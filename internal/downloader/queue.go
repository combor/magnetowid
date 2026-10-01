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
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/combor/magnetowid/internal/nzb"
	"github.com/combor/magnetowid/internal/provider"
	"github.com/combor/magnetowid/internal/subtitles"
)

// Status is a SABnzbd job state.
type Status string

const (
	StatusQueued      Status = "Queued"
	StatusDownloading Status = "Downloading"
	StatusCompleted   Status = "Completed"
	StatusFailed      Status = "Failed"
)

const (
	maxRetries      = 5
	firstRetryDelay = 5 * time.Second
	maxRetryDelay   = 10 * time.Minute
)

// n is a one-based retry count.
func backoff(n int) time.Duration {
	d := firstRetryDelay
	for i := 1; i < n && d < maxRetryDelay; i++ {
		d *= 4
	}
	return min(d, maxRetryDelay)
}

var (
	errUnknownProvider = errors.New("unknown provider")
	// A provider outage, not an individual stream failure.
	errUnreachable = errors.New("provider unreachable")
	errPaused      = errors.New("paused")
)

// Engine may keep partial downloads in out's folder, which stays until the job
// finishes or is removed.
type Engine interface {
	Download(ctx context.Context, s provider.Stream, out string, progress func(done time.Duration, bytes int64)) error
}

// Job is a download snapshot; its JSON form is persisted.
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
	Priority int       `json:"priority,omitzero"` // -1 low, 0 normal, 1 high, 2 force
	// Separate from Status so older versions can still load paused jobs.
	Paused bool `json:"paused,omitzero"`

	startFraction float64 // when this attempt's progress began; not saved
}

// Assume 4 Mbit/s until ffmpeg reports progress.
const bytesPerSecond = 4_000_000 / 8

// EstimatedSize is the job's expected final size in bytes.
func (j Job) EstimatedSize() int64 {
	if j.Fraction > 0.01 && j.Bytes > 0 {
		return int64(float64(j.Bytes) / j.Fraction)
	}
	return int64(j.Ref.Duration) * bytesPerSecond
}

// TimeLeft is zero until a running download has made some progress.
func (j Job) TimeLeft(now time.Time) time.Duration {
	gained := j.Fraction - j.startFraction
	if j.Status != StatusDownloading || gained <= 0.01 {
		return 0
	}
	return time.Duration(float64(now.Sub(j.Started)) * (1 - j.Fraction) / gained)
}

// Unreachable reports whether the job's last attempt met its provider's
// outage, which Error then describes.
func (j Job) Unreachable() bool { return strings.HasPrefix(j.Error, errUnreachable.Error()+": ") }

// HistoryRetention is how long finished jobs are kept.
const HistoryRetention = 30 * 24 * time.Hour

const incompleteDir = ".incomplete"

// Queue downloads one job at a time and persists state changes.
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
	cancels map[string]context.CancelCauseFunc
	outages map[string]*outage // by provider name
	paused  bool
	wake    chan struct{}
}

type outage struct {
	n     int
	until time.Time
}

// New requires an absolute download path. Close db only after Run returns.
func New(dir string, db *bolt.DB, providers *provider.Registry, engine Engine, log *slog.Logger) (*Queue, error) {
	jobs, err := loadJobs(db)
	if err != nil {
		return nil, err
	}
	paused, err := loadPaused(db)
	if err != nil {
		return nil, err
	}
	q := &Queue{
		dir:        dir,
		providers:  providers,
		engine:     engine,
		log:        log,
		db:         db,
		retryDelay: backoff,
		jobs:       make(map[string]*Job),
		cancels:    make(map[string]context.CancelCauseFunc),
		outages:    make(map[string]*outage),
		paused:     paused,
		wake:       make(chan struct{}, 1),
	}
	for _, j := range jobs {
		q.jobs[j.ID] = j
		q.order = append(q.order, j.ID)
	}
	q.prune(time.Now())
	q.cleanWork()
	return q, nil
}

// cleanWork removes partial downloads that no job will continue.
func (q *Queue) cleanWork() {
	entries, err := os.ReadDir(filepath.Join(q.dir, incompleteDir))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		q.log.Warn("listing unfinished downloads", "err", err)
	}
	for _, e := range entries {
		if j := q.jobs[e.Name()]; j == nil || j.Status == StatusCompleted || j.Status == StatusFailed {
			q.removeWork(e.Name())
		}
	}
}

func (q *Queue) workDir(id string) string { return filepath.Join(q.dir, incompleteDir, id) }

func (q *Queue) removeWork(id string) {
	if err := os.RemoveAll(q.workDir(id)); err != nil {
		q.log.Warn("removing an unfinished download", "id", id, "err", err)
	}
}

func (q *Queue) Dir() string { return q.dir }

// Add returns a SABnzbd job ID only after saving the job. Higher priorities run first.
func (q *Queue) Add(name, nzbName, category string, priority int, paused bool, ref nzb.Ref) (string, error) {
	job := &Job{
		ID:       "SABnzbd_nzo_" + randomHex(8),
		Name:     SanitizeName(name),
		NZBName:  nzbName,
		Category: category,
		Ref:      ref,
		Status:   StatusQueued,
		Added:    time.Now(),
		Priority: priority,
		Paused:   paused,
	}
	q.mu.Lock()
	if err := q.put(job); err != nil {
		q.mu.Unlock()
		return "", err
	}
	q.jobs[job.ID] = job
	q.order = append(q.order, job.ID)
	q.mu.Unlock()
	q.log.Info("job queued", "id", job.ID, "name", job.Name, "category", category, "priority", priority,
		"paused", paused, "provider", ref.Provider, "ref", ref.ID)
	q.wakeUp()
	return job.ID, nil
}

func (q *Queue) wakeUp() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *Queue) Paused() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.paused
}

// Outages returns, by provider, when jobs held back by that provider's outage
// may run again.
func (q *Queue) Outages() map[string]time.Time {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make(map[string]time.Time, len(q.outages))
	for name, o := range q.outages {
		out[name] = o.until
	}
	return out
}

// SetPaused stops or resumes the queue. Stopped downloads continue where they
// left off. A save failure leaves the queue unchanged.
func (q *Queue) SetPaused(paused bool) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.paused == paused {
		return nil
	}
	if err := q.putPaused(paused); err != nil {
		return err
	}
	q.paused = paused
	if paused {
		for _, cancel := range q.cancels {
			cancel(errPaused)
		}
		q.log.Info("queue paused")
	} else {
		q.wakeUp()
		q.log.Info("queue resumed")
	}
	return nil
}

// PauseJobs returns affected IDs. Stopped downloads continue where they left
// off. On a save failure, earlier changes remain applied.
func (q *Queue) PauseJobs(ids ...string) ([]string, error) {
	return q.setJobsPaused(true, ids)
}

// ResumeJobs returns affected IDs, with the same partial-save behavior as PauseJobs.
func (q *Queue) ResumeJobs(ids ...string) ([]string, error) {
	return q.setJobsPaused(false, ids)
}

func (q *Queue) setJobsPaused(paused bool, ids []string) ([]string, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	var done []string
	// Wake resumed jobs even if a later save fails.
	defer func() {
		if !paused && len(done) > 0 {
			q.wakeUp()
		}
	}()
	for _, id := range ids {
		job, ok := q.jobs[id]
		if !ok || (job.Status != StatusQueued && job.Status != StatusDownloading) {
			continue
		}
		if job.Paused != paused {
			// Persist as queued so a crash cannot leave a job marked as running.
			saved := *job
			saved.Paused = paused
			if saved.Status == StatusDownloading {
				requeue(&saved)
			}
			if err := q.put(&saved); err != nil {
				return done, err
			}
			job.Paused = paused
			if !paused {
				q.log.Info("job resumed", "id", id, "name", job.Name)
			} else {
				if cancel := q.cancels[id]; cancel != nil {
					cancel(errPaused)
				}
				q.log.Info("job paused", "id", id, "name", job.Name)
			}
		}
		done = append(done, id)
	}
	return done, nil
}

// Jobs returns snapshots in insertion order.
func (q *Queue) Jobs() []Job {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]Job, 0, len(q.order))
	for _, id := range q.order {
		out = append(out, *q.jobs[id])
	}
	return out
}

// Delete cancels the job and optionally removes its files. It returns false for
// unknown IDs; a save failure leaves the job unchanged.
func (q *Queue) Delete(id string, deleteFiles bool) (bool, error) {
	return q.discard(id, deleteFiles, false)
}

// Cancel deletes an unfinished job. It returns false for unknown and finished
// jobs, which stay in history for import.
func (q *Queue) Cancel(id string) (bool, error) {
	return q.discard(id, false, true)
}

func (q *Queue) discard(id string, deleteFiles, unfinishedOnly bool) (bool, error) {
	q.mu.Lock()
	job, ok := q.jobs[id]
	if !ok || (unfinishedOnly && job.Status != StatusQueued && job.Status != StatusDownloading) {
		q.mu.Unlock()
		return false, nil
	}
	if err := q.remove(id); err != nil {
		q.mu.Unlock()
		return false, err
	}
	// A running job's worker removes its files once the engine stops.
	running := job.Status == StatusDownloading
	delete(q.jobs, id)
	for i, v := range q.order {
		if v == id {
			q.order = append(q.order[:i], q.order[i+1:]...)
			break
		}
	}
	cancel := q.cancels[id]
	// Hold the lock to prevent moveToComplete from reusing this path.
	if deleteFiles && job.Storage != "" {
		if err := os.RemoveAll(job.Storage); err != nil {
			q.log.Warn("removing job files", "id", id, "err", err)
		}
	}
	q.mu.Unlock()

	if cancel != nil {
		cancel(nil)
	}
	if !running {
		q.removeWork(id)
	}
	q.log.Info("job deleted", "id", id, "delete_files", deleteFiles)
	return true, nil
}

func (q *Queue) Run(ctx context.Context) {
	for {
		// Check before selecting work so cancellation cannot start another job.
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

// next selects the highest priority, oldest ready job. Otherwise wait is the
// time until the next retry or provider recovery attempt.
func (q *Queue) next() (id string, wait time.Duration, ok bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.paused {
		return "", 0, false
	}
	now := time.Now()
	var best *Job
	for _, id := range q.order {
		job := q.jobs[id]
		if job.Status != StatusQueued || job.Paused {
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
		if best == nil || job.Priority > best.Priority {
			best = job
		}
	}
	if best == nil {
		return "", wait, false
	}
	best.Status = StatusDownloading
	best.Started = now
	best.startFraction = best.Fraction
	best.Attempts++
	return best.ID, 0, true
}

func (q *Queue) process(parent context.Context, id string) {
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)

	q.mu.Lock()
	job, ok := q.jobs[id]
	if !ok {
		q.mu.Unlock()
		// Deleted after next selected it, so discard left the files to us.
		q.removeWork(id)
		return
	}
	q.cancels[id] = cancel
	// Catch pauses made after next selected this job, before a cancel function existed.
	if q.paused || job.Paused {
		cancel(errPaused)
	}
	j := *job
	q.mu.Unlock()

	q.log.Info("job started", "id", id, "name", j.Name)
	storage, err := q.run(ctx, id, j, q.workDir(id))
	// Finished and deleted jobs never run again, so their files can go unlocked.
	if !q.finish(parent, ctx, id, storage, err) {
		q.removeWork(id)
	}
}

// finish records an attempt's outcome. keep reports whether the job may
// continue its partial download.
func (q *Queue) finish(parent, ctx context.Context, id, storage string, err error) (keep bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.cancels, id)
	job, ok := q.jobs[id]
	if !ok {
		// Deleted while running.
		if storage != "" {
			os.RemoveAll(storage)
		}
		return false
	}
	// Persist outcomes, not running progress, so crashes leave jobs queued for retry.
	defer func() {
		if err := q.put(job); err != nil {
			q.log.Warn("database write failed", "err", err)
		}
		q.prune(time.Now())
	}()
	if err != nil && parent.Err() != nil {
		// Shutdown does not consume a retry.
		requeue(job)
		q.log.Info("job interrupted, requeued", "id", id, "name", job.Name)
		return true
	}
	if err != nil && errors.Is(context.Cause(ctx), errPaused) {
		requeue(job)
		q.log.Info("job stopped by a pause, requeued", "id", id, "name", job.Name)
		return true
	}
	if errors.Is(err, errUnreachable) {
		// Provider outages pause all its jobs without consuming retries.
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
		q.log.Warn("pausing provider's jobs", "provider", job.Ref.Provider,
			"retry_in", delay, "err", err)
		return true
	}
	delete(q.outages, job.Ref.Provider)
	job.Finished = time.Now()
	if err != nil {
		job.Error = err.Error()
		if retryable(err) && job.Attempts <= maxRetries {
			delay := q.retryDelay(job.Attempts)
			job.Status = StatusQueued
			job.RetryAt = job.Finished.Add(delay)
			q.log.Warn("job attempt failed, will retry", "id", id, "name", job.Name,
				"attempt", job.Attempts, "retry_in", delay, "err", err)
			return true
		}
		job.Status = StatusFailed
		q.log.Error("job failed", "id", id, "name", job.Name, "attempts", job.Attempts, "err", err)
		return false
	}
	job.Status = StatusCompleted
	job.Error = ""
	job.Fraction = 1
	job.Storage = storage
	q.log.Info("job completed", "id", id, "name", job.Name, "storage", storage, "bytes", job.Bytes)
	return false
}

// prune removes expired history but keeps downloaded files. Requires q.mu.
func (q *Queue) prune(now time.Time) {
	var old []string
	for _, id := range q.order {
		j := q.jobs[id]
		if (j.Status == StatusCompleted || j.Status == StatusFailed) && now.Sub(j.Finished) > HistoryRetention {
			old = append(old, id)
		}
	}
	if len(old) == 0 {
		return
	}
	// Keep failed removals in memory to retry next time.
	if err := q.remove(old...); err != nil {
		q.log.Warn("database write failed", "err", err)
		return
	}
	for _, id := range old {
		delete(q.jobs, id)
	}
	q.order = slices.DeleteFunc(q.order, func(id string) bool { return q.jobs[id] == nil })
	q.log.Info("forgot old finished jobs", "count", len(old))
}

// Requeue without consuming a retry. Progress stays with the partial download.
func requeue(job *Job) {
	job.Status = StatusQueued
	job.Attempts--
}

// offline distinguishes network failures from item-specific errors.
func offline(err error) bool {
	var op *net.OpError
	var ne net.Error
	return errors.As(err, &op) || (errors.As(err, &ne) && ne.Timeout())
}

func retryable(err error) bool {
	return !errors.Is(err, provider.ErrUnavailable) &&
		!errors.Is(err, errUnknownProvider)
}

func (q *Queue) run(ctx context.Context, id string, j Job, work string) (string, error) {
	if err := context.Cause(ctx); err != nil {
		return "", err
	}
	p, ok := q.providers.Get(j.Ref.Provider)
	if !ok {
		return "", fmt.Errorf("%w %q", errUnknownProvider, j.Ref.Provider)
	}
	if err := os.MkdirAll(work, 0o777); err != nil {
		return "", err
	}
	out := filepath.Join(work, j.Name+".mp4")
	duration := time.Duration(j.Ref.Duration) * time.Second
	first := true
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
		// The first report includes what earlier attempts saved.
		if first {
			job.startFraction, first = job.Fraction, false
		}
	}

	// Stream URLs expire, so resolve on every attempt.
	s, err := p.Resolve(ctx, j.Ref.ID)
	if err != nil && offline(err) {
		return "", fmt.Errorf("%w: %w", errUnreachable, err)
	}
	if err != nil {
		return "", err
	}
	if err := q.engine.Download(ctx, s, out, progress); err != nil {
		return "", err
	}
	subs, err := q.saveSubtitles(ctx, j, s, work)
	if err != nil {
		return "", err
	}
	return q.moveToComplete(out, subs, j.Category, j.Name)
}

// Subtitle failures are logged and skipped; only cancellation fails the download.
func (q *Queue) saveSubtitles(ctx context.Context, j Job, s provider.Stream, work string) ([]string, error) {
	var paths []string
	for _, sub := range s.Subtitles {
		path := filepath.Join(work, subtitleName(j.Name, sub))
		if slices.Contains(paths, path) {
			continue // more in the same language
		}
		srt, err := subtitles.Fetch(ctx, defaultClient, sub, s.Header)
		if err == nil {
			err = os.WriteFile(path, srt, 0o666)
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err != nil {
			q.log.Warn("can't save subtitles; keeping the video without them", "id", j.ID, "name", j.Name,
				"language", sub.Language, "err", err)
			continue
		}
		paths = append(paths, path)
	}
	return paths, nil
}

// Use the media-server convention: {video}.{language}[.sdh].srt.
func subtitleName(video string, s provider.Subtitle) string {
	name := video
	if s.Language != "" {
		name += "." + SanitizeName(s.Language)
	}
	if s.SDH {
		name += ".sdh"
	}
	return name + ".srt"
}

// Reserve a unique {dir}/{category}/{name} folder for the video and extras.
// Existing folders and job records reserve names; failed extras are skipped.
func (q *Queue) moveToComplete(out string, extras []string, category, name string) (string, error) {
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
		for _, f := range extras {
			if err := os.Rename(f, filepath.Join(dest, filepath.Base(f))); err != nil {
				q.log.Warn("can't move a download's extra file; leaving it out", "file", f, "err", err)
			}
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
