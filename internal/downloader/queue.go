// Package downloader runs download jobs one at a time.
package downloader

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/combor/vodarr/internal/nzb"
	"github.com/combor/vodarr/internal/provider"
)

// Status is a SABnzbd job state.
type Status string

const (
	StatusQueued      Status = "Queued"
	StatusDownloading Status = "Downloading"
	StatusCompleted   Status = "Completed"
	StatusFailed      Status = "Failed"
)

// Retry waits grow 4x from firstRetryDelay: 5s, 20s, 80s, 320s, 10m.
const (
	maxRetries      = 5
	firstRetryDelay = 5 * time.Second
	maxRetryDelay   = 10 * time.Minute
)

// backoff returns the wait before retry number n (1-based).
func backoff(n int) time.Duration {
	d := firstRetryDelay
	for i := 1; i < n && d < maxRetryDelay; i++ {
		d *= 4
	}
	return min(d, maxRetryDelay)
}

var errUnknownProvider = errors.New("unknown provider")

// Engine fetches a stream into out.
type Engine interface {
	Download(ctx context.Context, s provider.Stream, out string, progress func(done time.Duration, bytes int64)) error
}

// Job is a snapshot of one download. The JSON form is its database record.
type Job struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	NZBName  string    `json:"nzb_name"`
	Category string    `json:"category"`
	Ref      nzb.Ref   `json:"ref"`
	Status   Status    `json:"status"`
	Added    time.Time `json:"added"`
	Started  time.Time `json:"started,omitzero"`
	Finished time.Time `json:"finished,omitzero"`
	Fraction float64   `json:"fraction,omitzero"` // 0..1
	Bytes    int64     `json:"bytes,omitzero"`    // written so far, or final size
	Storage  string    `json:"storage,omitzero"`  // completed job folder
	Error    string    `json:"error,omitzero"`
	Attempts int       `json:"attempts,omitzero"`
	RetryAt  time.Time `json:"retry_at,omitzero"`
}

// historyRetention is how long finished jobs are kept. Sonarr/Radarr handle
// them within minutes and usually remove them themselves.
const historyRetention = 30 * 24 * time.Hour

// incompleteDir, under the download root, holds one work folder per running job.
const incompleteDir = ".incomplete"

// Queue runs jobs with a single worker. It keeps them in memory and saves
// them to the job database when added, deleted or done with an attempt.
type Queue struct {
	dir       string
	providers *provider.Registry
	engine    Engine
	log       *slog.Logger
	db        *bolt.DB

	retryDelay func(n int) time.Duration

	mu      sync.Mutex
	jobs    map[string]*Job
	order   []string
	cancels map[string]context.CancelFunc
	outages map[string]*outage // by provider name
	wake    chan struct{}
}

// outage pauses a provider's jobs after it couldn't be reached n times in a row.
type outage struct {
	n     int
	until time.Time
}

// New opens the queue that downloads into dir, which must be absolute, with
// the jobs saved there. Close it when done.
func New(dir string, providers *provider.Registry, engine Engine, log *slog.Logger) (*Queue, error) {
	db, jobs, err := openDB(dir)
	if err != nil {
		return nil, err
	}
	// Nothing is running yet, so any work folders are left over from a crash.
	if err := os.RemoveAll(filepath.Join(dir, incompleteDir)); err != nil {
		log.Warn("removing unfinished downloads", "err", err)
	}
	q := &Queue{
		dir:        dir,
		providers:  providers,
		engine:     engine,
		log:        log,
		db:         db,
		retryDelay: backoff,
		jobs:       make(map[string]*Job),
		cancels:    make(map[string]context.CancelFunc),
		outages:    make(map[string]*outage),
		wake:       make(chan struct{}, 1),
	}
	for _, j := range jobs {
		q.jobs[j.ID] = j
		q.order = append(q.order, j.ID)
	}
	q.prune(time.Now())
	return q, nil
}

// Dir returns the download root.
func (q *Queue) Dir() string { return q.dir }

// Add queues a job and returns its SABnzbd-style ID.
func (q *Queue) Add(name, nzbName, category string, ref nzb.Ref) string {
	job := &Job{
		ID:       "SABnzbd_nzo_" + randomHex(8),
		Name:     SanitizeName(name),
		NZBName:  nzbName,
		Category: category,
		Ref:      ref,
		Status:   StatusQueued,
		Added:    time.Now(),
	}
	q.mu.Lock()
	q.jobs[job.ID] = job
	q.order = append(q.order, job.ID)
	q.put(job)
	q.mu.Unlock()
	q.log.Info("job queued", "id", job.ID, "name", job.Name, "category", category, "provider", ref.Provider, "ref", ref.ID)
	select {
	case q.wake <- struct{}{}:
	default:
	}
	return job.ID
}

// Jobs returns snapshots of all jobs in the order they were added.
func (q *Queue) Jobs() []Job {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]Job, 0, len(q.order))
	for _, id := range q.order {
		out = append(out, *q.jobs[id])
	}
	return out
}

// Delete removes a job, cancelling it if running, and with deleteFiles its
// folder. It reports whether the job existed.
func (q *Queue) Delete(id string, deleteFiles bool) bool {
	q.mu.Lock()
	job, ok := q.jobs[id]
	if !ok {
		q.mu.Unlock()
		return false
	}
	delete(q.jobs, id)
	for i, v := range q.order {
		if v == id {
			q.order = append(q.order[:i], q.order[i+1:]...)
			break
		}
	}
	q.remove(id)
	cancel := q.cancels[id]
	// Under the lock, so moveToComplete can't reuse the path meanwhile.
	if deleteFiles && job.Storage != "" {
		if err := os.RemoveAll(job.Storage); err != nil {
			q.log.Warn("removing job files", "id", id, "err", err)
		}
	}
	q.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	q.log.Info("job deleted", "id", id, "delete_files", deleteFiles)
	return true
}

// Run processes queued jobs until ctx is cancelled.
func (q *Queue) Run(ctx context.Context) {
	for {
		// Checked first, so a cancelled queue doesn't start the next job.
		if ctx.Err() != nil {
			return
		}
		id, wait, ok := q.next()
		if ok {
			q.process(ctx, id)
			continue
		}
		var timer *time.Timer
		var fire <-chan time.Time
		if wait > 0 {
			timer = time.NewTimer(wait)
			fire = timer.C
		}
		select {
		case <-ctx.Done():
		case <-q.wake:
		case <-fire:
		}
		if timer != nil {
			timer.Stop()
		}
	}
}

// next starts the oldest ready job. If none is ready, wait is the time until
// the earliest retry or end of a provider pause.
func (q *Queue) next() (id string, wait time.Duration, ok bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	now := time.Now()
	for _, id := range q.order {
		job := q.jobs[id]
		if job.Status != StatusQueued {
			continue
		}
		until := job.RetryAt
		if o := q.outages[job.Ref.Provider]; o != nil && o.until.After(until) {
			until = o.until
		}
		if d := until.Sub(now); d > 0 {
			if wait == 0 || d < wait {
				wait = d
			}
			continue
		}
		job.Status = StatusDownloading
		job.Started = now
		job.Attempts++
		return id, 0, true
	}
	return "", wait, false
}

func (q *Queue) process(parent context.Context, id string) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	q.mu.Lock()
	job, ok := q.jobs[id]
	if !ok {
		q.mu.Unlock()
		return
	}
	q.cancels[id] = cancel
	j := *job
	q.mu.Unlock()

	q.log.Info("job started", "id", id, "name", j.Name)
	work := filepath.Join(q.dir, incompleteDir, id)
	storage, err := q.run(ctx, id, j, work)
	if rmErr := os.RemoveAll(work); rmErr != nil {
		q.log.Warn("removing work dir", "id", id, "err", rmErr)
	}

	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.cancels, id)
	job, ok = q.jobs[id]
	if !ok {
		// Deleted while running.
		if storage != "" {
			os.RemoveAll(storage)
		}
		return
	}
	// Every outcome below is saved; progress while running is not, so a
	// crash leaves the job Queued with its earlier attempts.
	defer func() {
		q.put(job)
		q.prune(time.Now())
	}()
	if err != nil && parent.Err() != nil {
		// Interrupted by shutdown, which is not the job's fault.
		requeue(job)
		q.log.Info("job interrupted, requeued", "id", id, "name", job.Name)
		return
	}
	if err != nil && offline(err) {
		// The provider is unreachable, so every job for it would fail the
		// same way. Pause them all rather than use up this job's retries.
		requeue(job)
		job.Error = err.Error()
		o := q.outages[job.Ref.Provider]
		if o == nil {
			o = &outage{}
			q.outages[job.Ref.Provider] = o
		}
		o.n++
		delay := q.retryDelay(o.n)
		o.until = time.Now().Add(delay)
		q.log.Warn("provider unreachable, pausing its jobs", "provider", job.Ref.Provider,
			"retry_in", delay, "err", err)
		return
	}
	delete(q.outages, job.Ref.Provider)
	job.Finished = time.Now()
	if err != nil {
		job.Error = err.Error()
		if retryable(err) && job.Attempts <= maxRetries {
			delay := q.retryDelay(job.Attempts)
			job.Status = StatusQueued
			job.RetryAt = job.Finished.Add(delay)
			job.Fraction, job.Bytes = 0, 0
			q.log.Warn("job attempt failed, will retry", "id", id, "name", job.Name,
				"attempt", job.Attempts, "retry_in", delay, "err", err)
			return
		}
		job.Status = StatusFailed
		q.log.Error("job failed", "id", id, "name", job.Name, "attempts", job.Attempts, "err", err)
		return
	}
	job.Status = StatusCompleted
	job.Error = ""
	job.Fraction = 1
	job.Storage = storage
	q.log.Info("job completed", "id", id, "name", job.Name, "storage", storage, "bytes", job.Bytes)
}

// prune forgets finished jobs older than historyRetention but leaves their
// files, which belong to Sonarr/Radarr. q.mu must be held.
func (q *Queue) prune(now time.Time) {
	var old []string
	kept := q.order[:0]
	for _, id := range q.order {
		j := q.jobs[id]
		if (j.Status == StatusCompleted || j.Status == StatusFailed) && now.Sub(j.Finished) > historyRetention {
			old = append(old, id)
			delete(q.jobs, id)
			continue
		}
		kept = append(kept, id)
	}
	q.order = kept
	if len(old) > 0 {
		q.remove(old...)
		q.log.Info("forgot old finished jobs", "count", len(old))
	}
}

// requeue returns a started job to the queue without counting the attempt.
func requeue(job *Job) {
	job.Status = StatusQueued
	job.Attempts--
	job.Fraction, job.Bytes = 0, 0
}

// offline reports whether err is a network failure (DNS, connection or
// timeout) rather than a problem with the item.
func offline(err error) bool {
	var op *net.OpError
	var ne net.Error
	return errors.As(err, &op) || (errors.As(err, &ne) && ne.Timeout())
}

// retryable reports whether a failed attempt is worth retrying.
func retryable(err error) bool {
	return !errors.Is(err, provider.ErrUnavailable) &&
		!errors.Is(err, errUnknownProvider)
}

// run makes one download attempt and returns the completed folder.
func (q *Queue) run(ctx context.Context, id string, j Job, work string) (string, error) {
	p, ok := q.providers.Get(j.Ref.Provider)
	if !ok {
		return "", fmt.Errorf("%w %q", errUnknownProvider, j.Ref.Provider)
	}
	if err := os.MkdirAll(work, 0o777); err != nil {
		return "", err
	}
	out := filepath.Join(work, j.Name+".mp4")
	duration := time.Duration(j.Ref.Duration) * time.Second
	progress := func(done time.Duration, bytes int64) {
		q.mu.Lock()
		defer q.mu.Unlock()
		job, ok := q.jobs[id]
		if !ok {
			return
		}
		job.Bytes = bytes
		if duration > 0 {
			job.Fraction = min(float64(done)/float64(duration), 0.99)
		}
	}

	// Stream URLs expire, so resolve on every attempt.
	s, err := p.Resolve(ctx, j.Ref.ID)
	if err != nil {
		return "", err
	}
	if err := q.engine.Download(ctx, s, out, progress); err != nil {
		return "", err
	}
	return q.moveToComplete(out, j.Category, j.Name)
}

// moveToComplete moves out to {dir}/{category}/{name}/{name}.mp4, trying
// name.1, name.2, ... if the folder exists or another job references it.
func (q *Queue) moveToComplete(out, category, name string) (string, error) {
	parent := q.dir
	if c := SanitizeName(category); category != "" && category != "*" && c != "" {
		parent = filepath.Join(q.dir, c)
	}
	if err := os.MkdirAll(parent, 0o777); err != nil {
		return "", err
	}
	reserved := map[string]bool{}
	q.mu.Lock()
	for _, j := range q.jobs {
		if j.Storage != "" {
			reserved[j.Storage] = true
		}
	}
	q.mu.Unlock()
	for i := 0; ; i++ {
		folder := name
		if i > 0 {
			folder += "." + strconv.Itoa(i)
		}
		dest := filepath.Join(parent, folder)
		if reserved[dest] {
			continue
		}
		err := os.Mkdir(dest, 0o777)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		if err := os.Rename(out, filepath.Join(dest, name+".mp4")); err != nil {
			os.Remove(dest)
			return "", err
		}
		return dest, nil
	}
}

// SanitizeName makes s safe as a single path element.
func SanitizeName(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || strings.ContainsRune(`/\:*?"<>|`, r) {
			return '_'
		}
		return r
	}, s)
	s = strings.Trim(s, " .")
	if s == "" {
		return "job"
	}
	return s
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}
