// Package downloader runs download jobs one at a time: resolve the stream
// with the job's provider, fetch it with an Engine, then move the result
// into {dir}/{category}/{name}/ for Sonarr/Radarr to import.
package downloader

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/combor/vodarr/internal/nzb"
	"github.com/combor/vodarr/internal/provider"
)

// Status mirrors the SABnzbd job states Sonarr/Radarr understand.
type Status string

const (
	StatusQueued      Status = "Queued"
	StatusDownloading Status = "Downloading"
	StatusCompleted   Status = "Completed"
	StatusFailed      Status = "Failed"
)

// Failed attempts are retried maxRetries times, waiting firstRetryDelay,
// then 4x longer each time (5s, 20s, 80s, 320s, ...), capped at maxRetryDelay.
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

// errUnknownProvider is permanent: retrying cannot help.
var errUnknownProvider = errors.New("unknown provider")

// Engine fetches a stream into out. progress reports how much media time has
// been written and the bytes written so far.
type Engine interface {
	Download(ctx context.Context, s provider.Stream, out string, progress func(done time.Duration, bytes int64)) error
}

// Job is a snapshot of one download.
type Job struct {
	ID       string
	Name     string // folder and file name, from the uploaded NZB name
	NZBName  string
	Category string
	Ref      nzb.Ref
	Status   Status
	Added    time.Time
	Started  time.Time
	Finished time.Time
	Fraction float64 // 0..1
	Bytes    int64   // written so far, or final size
	Storage  string  // completed job folder
	Error    string  // last failure, also kept while waiting to retry
	Attempts int
	RetryAt  time.Time // queued job waits until then
}

// Queue holds jobs in memory and runs them with a single worker.
type Queue struct {
	dir       string
	providers *provider.Registry
	engine    Engine
	log       *slog.Logger

	retryDelay func(n int) time.Duration // backoff; replaced in tests

	mu      sync.Mutex
	jobs    map[string]*Job
	order   []string // insertion order
	cancels map[string]context.CancelFunc
	wake    chan struct{}
}

// New returns a queue that downloads into dir, which must be absolute.
func New(dir string, providers *provider.Registry, engine Engine, log *slog.Logger) *Queue {
	return &Queue{
		dir:        dir,
		providers:  providers,
		engine:     engine,
		log:        log,
		retryDelay: backoff,
		jobs:       make(map[string]*Job),
		cancels:    make(map[string]context.CancelFunc),
		wake:       make(chan struct{}, 1),
	}
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

// Delete removes a job, cancelling it if it is running. With deleteFiles, a
// completed job's folder is removed too. It reports whether the job existed.
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
	cancel := q.cancels[id]
	// Remove files before unlocking: once the record is gone its path is no
	// longer reserved, and moveToComplete (which reads reservations under
	// this lock) must not hand it to a new job while it is still being
	// deleted.
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

// Run processes queued jobs until ctx is cancelled. Jobs waiting to retry
// don't block the others.
func (q *Queue) Run(ctx context.Context) {
	for {
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
		if ctx.Err() != nil {
			return
		}
	}
}

// next marks the oldest ready job as downloading and returns its ID. If no
// job is ready, it returns how long until the earliest retry (0 if none).
func (q *Queue) next() (id string, wait time.Duration, ok bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	now := time.Now()
	for _, id := range q.order {
		job := q.jobs[id]
		if job.Status != StatusQueued {
			continue
		}
		if until := job.RetryAt.Sub(now); until > 0 {
			if wait == 0 || until < wait {
				wait = until
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
	work := filepath.Join(q.dir, ".incomplete", id)
	storage, err := q.run(ctx, id, j, work)
	if rmErr := os.RemoveAll(work); rmErr != nil {
		q.log.Warn("removing work dir", "id", id, "err", rmErr)
	}

	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.cancels, id)
	job, ok = q.jobs[id]
	if !ok {
		// Deleted while running; don't leave its output behind.
		if storage != "" {
			os.RemoveAll(storage)
		}
		return
	}
	job.Finished = time.Now()
	if err != nil {
		job.Error = err.Error()
		if retryable(parent, err) && job.Attempts <= maxRetries {
			// Back to the queue; a retry resolves a fresh stream URL.
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

// retryable reports whether a failed attempt may succeed later: not when the
// content is unavailable (DRM, geo-blocked, paid), the provider is unknown,
// or vodarr is shutting down.
func retryable(parent context.Context, err error) bool {
	return parent.Err() == nil &&
		!errors.Is(err, provider.ErrUnavailable) &&
		!errors.Is(err, errUnknownProvider)
}

// run makes one download attempt into work and moves the file to its final
// folder, which it returns.
func (q *Queue) run(ctx context.Context, id string, j Job, work string) (string, error) {
	p, ok := q.providers.Get(j.Ref.Provider)
	if !ok {
		return "", fmt.Errorf("%w %q", errUnknownProvider, j.Ref.Provider)
	}
	if err := os.MkdirAll(work, 0o755); err != nil {
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

	// Resolve on every attempt: stream URLs expire and may point at a bad edge.
	s, err := p.Resolve(ctx, j.Ref.ID)
	if err != nil {
		return "", err
	}
	if err := q.engine.Download(ctx, s, out, progress); err != nil {
		return "", err
	}
	return q.moveToComplete(out, j.Category, j.Name)
}

// moveToComplete moves out to {dir}/{category}/{name}/{name}.mp4. If that
// folder exists, or a job record still points at it (an importer may have
// removed the folder but not the record), it tries name.1, name.2, ... like
// SABnzbd, so jobs never share files or delete each other's.
func (q *Queue) moveToComplete(out, category, name string) (string, error) {
	parent := q.dir
	if c := SanitizeName(category); category != "" && category != "*" && c != "" {
		parent = filepath.Join(q.dir, c)
	}
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", err
	}
	// Only the single worker creates folders, so this can't go stale.
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
		err := os.Mkdir(dest, 0o755)
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
