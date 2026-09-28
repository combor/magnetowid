package downloader

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/combor/magnetowid/internal/nzb"
	"github.com/combor/magnetowid/internal/provider"
	"github.com/combor/magnetowid/internal/store"
)

// fakeProvider can't be reached for the first offline resolves, then returns
// err if set.
type fakeProvider struct {
	mu       sync.Mutex
	resolves int
	offline  int
	err      error
}

func (f *fakeProvider) Name() string { return "fake" }

func (f *fakeProvider) Search(context.Context, provider.Query) ([]provider.Item, error) {
	return nil, nil
}

func (f *fakeProvider) Resolve(_ context.Context, id string) (provider.Stream, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resolves++
	if f.resolves <= f.offline {
		return provider.Stream{}, &url.Error{Op: "Get", URL: "http://example.invalid/", Err: &net.OpError{
			Op: "dial", Net: "tcp", Err: errors.New("connect: connection refused")}}
	}
	if f.err != nil {
		return provider.Stream{}, f.err
	}
	return provider.Stream{URL: fmt.Sprintf("http://example.invalid/%s/%d.m3u8", id, f.resolves)}, nil
}

// fakeEngine fails the first fail calls and any stream URL containing failURL,
// with err or else "boom".
type fakeEngine struct {
	mu      sync.Mutex
	calls   int
	fail    int
	failURL string
	err     error
}

func (e *fakeEngine) Download(_ context.Context, s provider.Stream, out string, progress func(time.Duration, int64)) error {
	e.mu.Lock()
	e.calls++
	failing := e.calls <= e.fail || (e.failURL != "" && strings.Contains(s.URL, e.failURL))
	e.mu.Unlock()
	if failing && e.err != nil {
		return e.err
	}
	if failing {
		return errors.New("boom")
	}
	progress(30*time.Second, 7)
	return os.WriteFile(out, []byte("media"), 0o644)
}

func startQueue(t *testing.T, p *fakeProvider, e Engine) *Queue {
	t.Helper()
	return startQueueWithDelay(t, p, e, 10*time.Millisecond)
}

// blockingEngine reports each download on started and holds it until cancelled.
type blockingEngine struct {
	started chan string
}

func (e *blockingEngine) Download(ctx context.Context, s provider.Stream, _ string, _ func(time.Duration, int64)) error {
	e.started <- s.URL
	<-ctx.Done()
	return ctx.Err()
}

// orderEngine records the streams it fetches and holds the first until
// release is closed.
type orderEngine struct {
	mu      sync.Mutex
	urls    []string
	started chan struct{}
	release chan struct{}
}

func (e *orderEngine) Download(_ context.Context, s provider.Stream, out string, _ func(time.Duration, int64)) error {
	e.mu.Lock()
	e.urls = append(e.urls, s.URL)
	first := len(e.urls) == 1
	e.mu.Unlock()
	if first {
		close(e.started)
		<-e.release
	}
	return os.WriteFile(out, []byte("media"), 0o644)
}

func startQueueWithDelay(t *testing.T, p *fakeProvider, e Engine, retryDelay time.Duration) *Queue {
	t.Helper()
	q := newQueue(t, t.TempDir(), p, e)
	q.retryDelay = func(int) time.Duration { return retryDelay }
	run(t, q)
	return q
}

// add queues a job named name with an NZB named after it.
func add(t *testing.T, q *Queue, name, category string, priority int, ref nzb.Ref) string {
	t.Helper()
	id, err := q.Add(name, name+".nzb", category, priority, false, ref)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// newQueue opens a queue and its database in dir, and closes the database
// when the test ends. Tests close q.db to stop saving, or to reopen dir.
func newQueue(t *testing.T, dir string, p provider.Provider, e Engine) *Queue {
	t.Helper()
	db, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	q, err := New(dir, db, provider.NewRegistry(p), e, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return q
}

// run starts q's worker and returns a function that stops it and waits.
func run(t *testing.T, q *Queue) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { q.Run(ctx); close(done) }()
	stop = func() { cancel(); <-done }
	t.Cleanup(stop)
	return stop
}

func waitFinished(t *testing.T, q *Queue, id string) Job {
	t.Helper()
	return waitJob(t, q, id, func(j Job) bool { return j.Status == StatusCompleted || j.Status == StatusFailed })
}

// waitJob polls until job id satisfies cond.
func waitJob(t *testing.T, q *Queue, id string, cond func(Job) bool) Job {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, j := range q.Jobs() {
			if j.ID == id && cond(j) {
				return j
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("job %s: condition not met", id)
	return Job{}
}

func TestJobCompletes(t *testing.T) {
	q := startQueue(t, &fakeProvider{}, &fakeEngine{})
	id := add(t, q, "Ranczo.S01E01.1080p.WEB-DL.AAC.H.264-TVP", "tv", 0,
		nzb.Ref{Provider: "fake", ID: "381046", Duration: 60})
	j := waitFinished(t, q, id)
	if j.Status != StatusCompleted {
		t.Fatalf("status = %s, error = %s", j.Status, j.Error)
	}
	wantDir := filepath.Join(q.Dir(), "tv", "Ranczo.S01E01.1080p.WEB-DL.AAC.H.264-TVP")
	if j.Storage != wantDir {
		t.Errorf("storage = %q, want %q", j.Storage, wantDir)
	}
	if _, err := os.Stat(filepath.Join(wantDir, "Ranczo.S01E01.1080p.WEB-DL.AAC.H.264-TVP.mp4")); err != nil {
		t.Error(err)
	}
	if j.Fraction != 1 || j.Bytes != 7 {
		t.Errorf("fraction = %v, bytes = %d", j.Fraction, j.Bytes)
	}
	if _, err := os.Stat(filepath.Join(q.Dir(), ".incomplete", id)); !os.IsNotExist(err) {
		t.Errorf("work dir not removed: %v", err)
	}
}

func TestSameNameGetsSuffix(t *testing.T) {
	q := startQueue(t, &fakeProvider{}, &fakeEngine{})
	ref := nzb.Ref{Provider: "fake", ID: "1"}
	first := waitFinished(t, q, add(t, q, "Movie.2020", "movies", 0, ref))
	second := waitFinished(t, q, add(t, q, "Movie.2020", "movies", 0, ref))
	if first.Storage == second.Storage {
		t.Fatalf("both jobs share %q", first.Storage)
	}
	if want := filepath.Join(q.Dir(), "movies", "Movie.2020.1"); second.Storage != want {
		t.Errorf("second storage = %q, want %q", second.Storage, want)
	}
	// Deleting one job with its files leaves the other intact.
	q.Delete(second.ID, true)
	if _, err := os.Stat(second.Storage); !os.IsNotExist(err) {
		t.Errorf("second folder still there: %v", err)
	}
	if _, err := os.Stat(filepath.Join(first.Storage, "Movie.2020.mp4")); err != nil {
		t.Errorf("first job's file gone: %v", err)
	}
}

func TestBackoffSchedule(t *testing.T) {
	want := []time.Duration{5 * time.Second, 20 * time.Second, 80 * time.Second, 320 * time.Second, 10 * time.Minute, 10 * time.Minute}
	for i, w := range want {
		if got := backoff(i + 1); got != w {
			t.Errorf("backoff(%d) = %v, want %v", i+1, got, w)
		}
	}
}

func TestRetriesWithFreshResolve(t *testing.T) {
	p := &fakeProvider{}
	q := startQueue(t, p, &fakeEngine{fail: 3})
	j := waitFinished(t, q, add(t, q, "a", "tv", 0, nzb.Ref{Provider: "fake", ID: "1"}))
	if j.Status != StatusCompleted || j.Attempts != 4 || p.resolves != 4 || j.Error != "" {
		t.Fatalf("status = %s, attempts = %d, resolves = %d, error = %q", j.Status, j.Attempts, p.resolves, j.Error)
	}
}

func TestGivesUpAfterFiveRetries(t *testing.T) {
	p := &fakeProvider{}
	q := startQueue(t, p, &fakeEngine{fail: 100})
	j := waitFinished(t, q, add(t, q, "b", "tv", 0, nzb.Ref{Provider: "fake", ID: "1"}))
	if j.Status != StatusFailed || j.Attempts != 1+maxRetries || p.resolves != 1+maxRetries || j.Error != "boom" {
		t.Fatalf("status = %s, attempts = %d, resolves = %d, error = %q", j.Status, j.Attempts, p.resolves, j.Error)
	}
}

// A job waiting to retry must not hold up the rest of the queue.
func TestRetryWaitDoesNotBlockQueue(t *testing.T) {
	q := startQueueWithDelay(t, &fakeProvider{}, &fakeEngine{failURL: "/bad/"}, time.Hour)
	bad := add(t, q, "bad", "tv", 0, nzb.Ref{Provider: "fake", ID: "bad"})
	good := waitFinished(t, q, add(t, q, "good", "tv", 0, nzb.Ref{Provider: "fake", ID: "good"}))
	if good.Status != StatusCompleted {
		t.Fatalf("good job: %s %q", good.Status, good.Error)
	}
	for _, j := range q.Jobs() {
		if j.ID == bad && (j.Status != StatusQueued || j.Attempts != 1 || time.Until(j.RetryAt) < 50*time.Minute || j.Error != "boom") {
			t.Fatalf("bad job = %+v", j)
		}
	}
}

// An outage longer than the retry budget doesn't fail the job.
func TestOutageUsesNoRetries(t *testing.T) {
	p := &fakeProvider{offline: maxRetries + 3}
	q := startQueue(t, p, &fakeEngine{})
	j := waitFinished(t, q, add(t, q, "a", "tv", 0, nzb.Ref{Provider: "fake", ID: "1"}))
	if j.Status != StatusCompleted || j.Attempts != 1 || p.resolves != maxRetries+4 || j.Error != "" {
		t.Fatalf("status = %s, attempts = %d, resolves = %d, error = %q", j.Status, j.Attempts, p.resolves, j.Error)
	}
}

// While a provider is unreachable, none of its jobs are tried.
func TestOutagePausesProvider(t *testing.T) {
	p := &fakeProvider{offline: 1}
	q := startQueueWithDelay(t, p, &fakeEngine{}, time.Hour)
	a := add(t, q, "a", "tv", 0, nzb.Ref{Provider: "fake", ID: "a"})
	b := add(t, q, "b", "tv", 0, nzb.Ref{Provider: "fake", ID: "b"})
	waitJob(t, q, a, func(j Job) bool { return j.Error != "" })
	time.Sleep(50 * time.Millisecond)
	for _, j := range q.Jobs() {
		if (j.ID == a || j.ID == b) && (j.Status != StatusQueued || j.Attempts != 0) {
			t.Errorf("job %s: status = %s, attempts = %d", j.Name, j.Status, j.Attempts)
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.resolves != 1 {
		t.Errorf("resolves = %d, want 1", p.resolves)
	}
}

// A stream host that can't be reached is that job's problem: it uses a retry
// and doesn't hold up the provider's other jobs.
func TestUnreachableStreamIsNotAnOutage(t *testing.T) {
	dial := &url.Error{Op: "Get", URL: "http://cdn.invalid/", Err: &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("refused")}}
	e := &fakeEngine{failURL: "/bad/", err: fmt.Errorf("fetching playlist: %w", dial)}
	q := startQueueWithDelay(t, &fakeProvider{}, e, time.Hour)
	bad := add(t, q, "bad", "tv", 0, nzb.Ref{Provider: "fake", ID: "bad"})
	good := waitFinished(t, q, add(t, q, "good", "tv", 0, nzb.Ref{Provider: "fake", ID: "good"}))
	if good.Status != StatusCompleted {
		t.Fatalf("good job: %s %q", good.Status, good.Error)
	}
	j := waitJob(t, q, bad, func(j Job) bool { return j.Error != "" })
	if j.Status != StatusQueued || j.Attempts != 1 || time.Until(j.RetryAt) < 50*time.Minute {
		t.Fatalf("bad job = %+v", j)
	}
}

func TestOffline(t *testing.T) {
	dial := &url.Error{Op: "Get", URL: "http://x/", Err: &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("refused")}}
	tests := []struct {
		err  error
		want bool
	}{
		{fmt.Errorf("tvp: %w", dial), true},
		{fmt.Errorf("fetching playlist: %w", &url.Error{Op: "Get", URL: "http://x/", Err: timeoutError{}}), true},
		{&url.Error{Op: "parse", URL: "::", Err: errors.New("missing protocol scheme")}, false},
		{errors.New("ffmpeg: exit status 1"), false},
		{fmt.Errorf("%w: DRM", provider.ErrUnavailable), false},
	}
	for _, tt := range tests {
		if got := offline(tt.err); got != tt.want {
			t.Errorf("offline(%v) = %v, want %v", tt.err, got, tt.want)
		}
	}
}

type timeoutError struct{}

func (timeoutError) Error() string   { return "Client.Timeout exceeded" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

func TestUnavailableIsNotRetried(t *testing.T) {
	p := &fakeProvider{err: fmt.Errorf("%w: DRM", provider.ErrUnavailable)}
	q := startQueue(t, p, &fakeEngine{})
	j := waitFinished(t, q, add(t, q, "c", "tv", 0, nzb.Ref{Provider: "fake", ID: "1"}))
	if j.Status != StatusFailed || p.resolves != 1 || j.Error != "content unavailable: DRM" {
		t.Fatalf("status = %s, resolves = %d, error = %q", j.Status, p.resolves, j.Error)
	}
}

// Shutdown puts the running job back and starts nothing else.
func TestShutdownRequeues(t *testing.T) {
	p := &fakeProvider{}
	e := &blockingEngine{started: make(chan string, 1)}
	q := newQueue(t, t.TempDir(), p, e)
	stop := run(t, q)
	add(t, q, "a", "tv", 0, nzb.Ref{Provider: "fake", ID: "a"})
	<-e.started
	add(t, q, "b", "tv", 0, nzb.Ref{Provider: "fake", ID: "b"})
	stop()
	for _, j := range q.Jobs() {
		if j.Status != StatusQueued || j.Attempts != 0 {
			t.Errorf("job %s: status = %s, attempts = %d", j.Name, j.Status, j.Attempts)
		}
	}
	if p.resolves != 1 {
		t.Errorf("resolves = %d, want 1", p.resolves)
	}
}

// Pausing a running job stops its download without using up an attempt;
// resumed, it starts again.
func TestPauseRunningJob(t *testing.T) {
	p := &fakeProvider{}
	e := &blockingEngine{started: make(chan string, 2)}
	q := newQueue(t, t.TempDir(), p, e)
	run(t, q)
	id := add(t, q, "a", "tv", 0, nzb.Ref{Provider: "fake", ID: "a"})
	<-e.started
	if ids, err := q.PauseJobs(id, "SABnzbd_nzo_gone"); err != nil || len(ids) != 1 || ids[0] != id {
		t.Fatalf("PauseJobs = %v, %v", ids, err)
	}
	waitJob(t, q, id, func(j Job) bool { return j.Status == StatusQueued && j.Paused && j.Attempts == 0 })
	select {
	case <-e.started:
		t.Fatal("a paused job started")
	case <-time.After(50 * time.Millisecond):
	}
	if _, err := q.ResumeJobs(id); err != nil {
		t.Fatal(err)
	}
	<-e.started
	waitJob(t, q, id, func(j Job) bool { return j.Status == StatusDownloading && !j.Paused && j.Attempts == 1 })
}

// A pause between the worker picking a job and starting its download stops
// the download too.
func TestPauseBeforeDownloadStarts(t *testing.T) {
	for name, pause := range map[string]func(q *Queue, id string) error{
		"job":   func(q *Queue, id string) error { _, err := q.PauseJobs(id); return err },
		"queue": func(q *Queue, _ string) error { return q.SetPaused(true) },
	} {
		e := &fakeEngine{}
		q := newQueue(t, t.TempDir(), &fakeProvider{}, e)
		id := add(t, q, "a", "tv", 0, nzb.Ref{Provider: "fake", ID: "a"})
		if picked, _, ok := q.next(); !ok || picked != id {
			t.Fatalf("%s: next = %q, %v", name, picked, ok)
		}
		if err := pause(q, id); err != nil {
			t.Fatal(err)
		}
		q.process(context.Background(), id)
		if j := q.Jobs()[0]; j.Status != StatusQueued || j.Attempts != 0 || e.calls != 0 {
			t.Errorf("%s: job %s after %d attempts, %d downloads", name, j.Status, j.Attempts, e.calls)
		}
	}
}

// Pausing the queue stops the running download and starts nothing until it
// is resumed, even after a restart.
func TestPauseQueue(t *testing.T) {
	dir := t.TempDir()
	e := &blockingEngine{started: make(chan string, 2)}
	q := newQueue(t, dir, &fakeProvider{}, e)
	stop := run(t, q)
	a := add(t, q, "a", "tv", 0, nzb.Ref{Provider: "fake", ID: "a"})
	<-e.started
	if err := q.SetPaused(true); err != nil {
		t.Fatal(err)
	}
	waitJob(t, q, a, func(j Job) bool { return j.Status == StatusQueued && !j.Paused && j.Attempts == 0 })
	add(t, q, "b", "tv", 0, nzb.Ref{Provider: "fake", ID: "b"})
	stop()
	q.db.Close()

	q = newQueue(t, dir, &fakeProvider{}, &fakeEngine{})
	run(t, q)
	if !q.Paused() {
		t.Fatal("the queue's pause was lost in a restart")
	}
	time.Sleep(50 * time.Millisecond)
	for _, j := range q.Jobs() {
		if j.Status != StatusQueued {
			t.Fatalf("job %s is %s while the queue is paused", j.Name, j.Status)
		}
	}
	if err := q.SetPaused(false); err != nil {
		t.Fatal(err)
	}
	for _, j := range q.Jobs() {
		waitFinished(t, q, j.ID)
	}
}

// A job paused while running loads paused and queued after a crash, as a
// running job isn't saved.
func TestPausedRunningJobSurvivesCrash(t *testing.T) {
	dir := t.TempDir()
	// Its download doesn't stop when paused, so the crash comes first.
	e := &orderEngine{started: make(chan struct{}), release: make(chan struct{})}
	q := newQueue(t, dir, &fakeProvider{}, e)
	run(t, q)
	t.Cleanup(func() { close(e.release) })
	id := add(t, q, "a", "tv", 0, nzb.Ref{Provider: "fake", ID: "a"})
	<-e.started
	if _, err := q.PauseJobs(id); err != nil {
		t.Fatal(err)
	}
	q.db.Close() // as if magnetowid died then

	q = newQueue(t, dir, &fakeProvider{}, &fakeEngine{})
	if jobs := q.Jobs(); len(jobs) != 1 || jobs[0].Status != StatusQueued || !jobs[0].Paused || jobs[0].Attempts != 0 {
		t.Fatalf("reloaded %+v", jobs)
	}
}

// Jobs survive a restart, deleted ones stay deleted, and folders stay reserved.
func TestJobsPersist(t *testing.T) {
	dir := t.TempDir()
	q := newQueue(t, dir, &fakeProvider{}, &fakeEngine{})
	stop := run(t, q)
	ref := nzb.Ref{Provider: "fake", ID: "1"}
	done := waitFinished(t, q, add(t, q, "Movie.2020", "movies", 0, ref))
	gone := waitFinished(t, q, add(t, q, "Other", "movies", 0, ref))
	q.Delete(gone.ID, true)
	stop()
	q.db.Close()
	// As if the importer had moved the file out.
	if err := os.RemoveAll(done.Storage); err != nil {
		t.Fatal(err)
	}

	q = newQueue(t, dir, &fakeProvider{}, &fakeEngine{})
	jobs := q.Jobs()
	if len(jobs) != 1 {
		t.Fatalf("got %d jobs, want 1: %+v", len(jobs), jobs)
	}
	j := jobs[0]
	if j.ID != done.ID || j.Status != StatusCompleted || j.Storage != done.Storage ||
		j.Bytes != done.Bytes || j.Attempts != 1 || !j.Added.Equal(done.Added) {
		t.Fatalf("reloaded %+v, want %+v", j, done)
	}
	run(t, q)
	again := waitFinished(t, q, add(t, q, "Movie.2020", "movies", 0, ref))
	if want := done.Storage + ".1"; again.Storage != want {
		t.Errorf("storage = %q, want %q", again.Storage, want)
	}
}

// A job interrupted by shutdown is queued again after a restart.
func TestInterruptedJobResumes(t *testing.T) {
	dir := t.TempDir()
	e := &blockingEngine{started: make(chan string, 1)}
	q := newQueue(t, dir, &fakeProvider{}, e)
	stop := run(t, q)
	id := add(t, q, "a", "tv", 0, nzb.Ref{Provider: "fake", ID: "a"})
	<-e.started
	stop()
	q.db.Close()

	q = newQueue(t, dir, &fakeProvider{}, &fakeEngine{})
	if jobs := q.Jobs(); len(jobs) != 1 || jobs[0].Status != StatusQueued || jobs[0].Attempts != 0 {
		t.Fatalf("reloaded %+v", jobs)
	}
	run(t, q)
	if j := waitFinished(t, q, id); j.Status != StatusCompleted || j.Attempts != 1 {
		t.Fatalf("status = %s, attempts = %d", j.Status, j.Attempts)
	}
}

// A job running when magnetowid dies loads as Queued, since running isn't
// saved.
func TestCrashedJobIsQueued(t *testing.T) {
	dir := t.TempDir()
	e := &blockingEngine{started: make(chan string, 1)}
	q := newQueue(t, dir, &fakeProvider{}, e)
	run(t, q)
	add(t, q, "a", "tv", 0, nzb.Ref{Provider: "fake", ID: "a"})
	<-e.started
	q.db.Close() // as if magnetowid died mid-download

	q2 := newQueue(t, dir, &fakeProvider{}, &fakeEngine{})
	if jobs := q2.Jobs(); len(jobs) != 1 || jobs[0].Status != StatusQueued || jobs[0].Attempts != 0 {
		t.Fatalf("reloaded %+v", jobs)
	}
}

func TestNewRemovesLeftoverWork(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, incompleteDir, "SABnzbd_nzo_x", "x.mp4")
	if err := os.MkdirAll(filepath.Dir(stale), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("partial"), 0o666); err != nil {
		t.Fatal(err)
	}
	newQueue(t, dir, &fakeProvider{}, &fakeEngine{})
	if _, err := os.Stat(filepath.Join(dir, incompleteDir)); !os.IsNotExist(err) {
		t.Errorf("leftover work not removed: %v", err)
	}
}

// Only finished jobs past the retention are forgotten, also on disk.
func TestPruneOldHistory(t *testing.T) {
	dir := t.TempDir()
	q := newQueue(t, dir, &fakeProvider{}, &fakeEngine{})
	now := time.Now()
	old := now.Add(-historyRetention - time.Hour)
	set := func(name string, status Status, finished time.Time) string {
		id := add(t, q, name, "tv", 0, nzb.Ref{Provider: "fake", ID: name})
		q.mu.Lock()
		defer q.mu.Unlock()
		q.jobs[id].Status, q.jobs[id].Finished = status, finished
		q.put(q.jobs[id])
		return id
	}
	set("old-completed", StatusCompleted, old)
	set("old-failed", StatusFailed, old)
	recent := set("recent", StatusCompleted, now.Add(-time.Hour))
	retrying := set("retrying", StatusQueued, old) // a failed attempt sets Finished
	q.mu.Lock()
	q.prune(now)
	q.mu.Unlock()
	q.db.Close()

	q = newQueue(t, dir, &fakeProvider{}, &fakeEngine{})
	var ids []string
	for _, j := range q.Jobs() {
		ids = append(ids, j.ID)
	}
	if len(ids) != 2 || ids[0] != recent || ids[1] != retrying {
		t.Fatalf("kept %v, want [%s %s]", ids, recent, retrying)
	}
}

// Higher priority jobs start first; equal ones in the order they were added.
func TestPriorityOrder(t *testing.T) {
	e := &orderEngine{started: make(chan struct{}), release: make(chan struct{})}
	q := startQueue(t, &fakeProvider{}, e)
	add := func(id string, priority int) string {
		return add(t, q, id, "tv", priority, nzb.Ref{Provider: "fake", ID: id})
	}
	add("running", 0)
	<-e.started
	low := add("low", -1)
	add("normal1", 0)
	add("high", 1)
	add("normal2", 0)
	close(e.release)
	waitFinished(t, q, low)

	want := []string{"running", "high", "normal1", "normal2", "low"}
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.urls) != len(want) {
		t.Fatalf("fetched %v", e.urls)
	}
	for i, id := range want {
		if !strings.Contains(e.urls[i], "/"+id+"/") {
			t.Errorf("download %d = %s, want %s", i, e.urls[i], id)
		}
	}
}

// Changes that can't be saved are refused and leave the queue as it was.
func TestUnsavedChangesAreRefused(t *testing.T) {
	q := newQueue(t, t.TempDir(), &fakeProvider{}, &fakeEngine{})
	id := add(t, q, "a", "tv", 0, nzb.Ref{Provider: "fake", ID: "a"})
	q.db.Close() // every database write fails from here on
	if _, err := q.Add("b", "b.nzb", "tv", 0, false, nzb.Ref{Provider: "fake", ID: "b"}); err == nil {
		t.Error("Add succeeded without saving")
	}
	if ok, err := q.Delete(id, false); ok || err == nil {
		t.Errorf("Delete = %v, %v; want an error", ok, err)
	}
	if jobs := q.Jobs(); len(jobs) != 1 || jobs[0].ID != id {
		t.Errorf("jobs = %+v, want only %s", jobs, id)
	}
}

func TestUnknownProviderFails(t *testing.T) {
	q := startQueue(t, &fakeProvider{}, &fakeEngine{})
	j := waitFinished(t, q, add(t, q, "d", "tv", 0, nzb.Ref{Provider: "gone", ID: "1"}))
	if j.Status != StatusFailed {
		t.Fatalf("status = %s", j.Status)
	}
}

func TestSanitizeName(t *testing.T) {
	tests := map[string]string{
		"Ranczo.S01E01.1080p-TVP": "Ranczo.S01E01.1080p-TVP",
		"a/b\\c:d":                "a_b_c_d",
		"..":                      "job",
		" .hidden. ":              "hidden",
		"":                        "job",
	}
	for in, want := range tests {
		if got := SanitizeName(in); got != want {
			t.Errorf("SanitizeName(%q) = %q, want %q", in, got, want)
		}
	}
}

// A folder the importer removed is not reused while its job record exists.
func TestNoReuseOfRecordedStorage(t *testing.T) {
	q := startQueue(t, &fakeProvider{}, &fakeEngine{})
	ref := nzb.Ref{Provider: "fake", ID: "1"}
	first := waitFinished(t, q, add(t, q, "Show.S01E01", "tv", 0, ref))
	if err := os.RemoveAll(first.Storage); err != nil {
		t.Fatal(err)
	}
	second := waitFinished(t, q, add(t, q, "Show.S01E01", "tv", 0, ref))
	if second.Storage == first.Storage {
		t.Fatalf("second job reused %q", first.Storage)
	}
	q.Delete(first.ID, true)
	if _, err := os.Stat(filepath.Join(second.Storage, "Show.S01E01.mp4")); err != nil {
		t.Errorf("second job's file gone: %v", err)
	}
}
