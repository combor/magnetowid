package downloader

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/combor/vodarr/internal/nzb"
	"github.com/combor/vodarr/internal/provider"
)

type fakeProvider struct {
	mu       sync.Mutex
	resolves int
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
	if f.err != nil {
		return provider.Stream{}, f.err
	}
	return provider.Stream{URL: fmt.Sprintf("http://example.invalid/%s/%d.m3u8", id, f.resolves)}, nil
}

// fakeEngine fails the first `fail` calls, and every call for a stream URL
// containing failURL, then writes a small file.
type fakeEngine struct {
	mu      sync.Mutex
	calls   int
	fail    int
	failURL string
}

func (e *fakeEngine) Download(_ context.Context, s provider.Stream, out string, progress func(time.Duration, int64)) error {
	e.mu.Lock()
	e.calls++
	failing := e.calls <= e.fail || (e.failURL != "" && strings.Contains(s.URL, e.failURL))
	e.mu.Unlock()
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

func startQueueWithDelay(t *testing.T, p *fakeProvider, e Engine, retryDelay time.Duration) *Queue {
	t.Helper()
	q := New(t.TempDir(), provider.NewRegistry(p), e, slog.New(slog.NewTextHandler(io.Discard, nil)))
	q.retryDelay = func(int) time.Duration { return retryDelay }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { q.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return q
}

func waitFinished(t *testing.T, q *Queue, id string) Job {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, j := range q.Jobs() {
			if j.ID == id && (j.Status == StatusCompleted || j.Status == StatusFailed) {
				return j
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("job %s did not finish", id)
	return Job{}
}

func TestJobCompletes(t *testing.T) {
	q := startQueue(t, &fakeProvider{}, &fakeEngine{})
	id := q.Add("Ranczo.S01E01.1080p.WEB-DL.AAC.H.264-TVP", "Ranczo.S01E01.1080p.WEB-DL.AAC.H.264-TVP.nzb", "tv",
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
	first := waitFinished(t, q, q.Add("Movie.2020", "Movie.2020.nzb", "movies", ref))
	second := waitFinished(t, q, q.Add("Movie.2020", "Movie.2020.nzb", "movies", ref))
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
	j := waitFinished(t, q, q.Add("a", "a.nzb", "tv", nzb.Ref{Provider: "fake", ID: "1"}))
	if j.Status != StatusCompleted || j.Attempts != 4 || p.resolves != 4 || j.Error != "" {
		t.Fatalf("status = %s, attempts = %d, resolves = %d, error = %q", j.Status, j.Attempts, p.resolves, j.Error)
	}
}

func TestGivesUpAfterFiveRetries(t *testing.T) {
	p := &fakeProvider{}
	q := startQueue(t, p, &fakeEngine{fail: 100})
	j := waitFinished(t, q, q.Add("b", "b.nzb", "tv", nzb.Ref{Provider: "fake", ID: "1"}))
	if j.Status != StatusFailed || j.Attempts != 1+maxRetries || p.resolves != 1+maxRetries || j.Error != "boom" {
		t.Fatalf("status = %s, attempts = %d, resolves = %d, error = %q", j.Status, j.Attempts, p.resolves, j.Error)
	}
}

// A job waiting to retry must not hold up the rest of the queue.
func TestRetryWaitDoesNotBlockQueue(t *testing.T) {
	q := startQueueWithDelay(t, &fakeProvider{}, &fakeEngine{failURL: "/bad/"}, time.Hour)
	bad := q.Add("bad", "bad.nzb", "tv", nzb.Ref{Provider: "fake", ID: "bad"})
	good := waitFinished(t, q, q.Add("good", "good.nzb", "tv", nzb.Ref{Provider: "fake", ID: "good"}))
	if good.Status != StatusCompleted {
		t.Fatalf("good job: %s %q", good.Status, good.Error)
	}
	for _, j := range q.Jobs() {
		if j.ID == bad && (j.Status != StatusQueued || j.Attempts != 1 || time.Until(j.RetryAt) < 50*time.Minute || j.Error != "boom") {
			t.Fatalf("bad job = %+v", j)
		}
	}
}

func TestUnavailableIsNotRetried(t *testing.T) {
	p := &fakeProvider{err: fmt.Errorf("%w: DRM", provider.ErrUnavailable)}
	q := startQueue(t, p, &fakeEngine{})
	j := waitFinished(t, q, q.Add("c", "c.nzb", "tv", nzb.Ref{Provider: "fake", ID: "1"}))
	if j.Status != StatusFailed || p.resolves != 1 || j.Error != "content unavailable: DRM" {
		t.Fatalf("status = %s, resolves = %d, error = %q", j.Status, p.resolves, j.Error)
	}
}

func TestUnknownProviderFails(t *testing.T) {
	q := startQueue(t, &fakeProvider{}, &fakeEngine{})
	j := waitFinished(t, q, q.Add("d", "d.nzb", "tv", nzb.Ref{Provider: "gone", ID: "1"}))
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

// An importer may remove a completed folder but keep the job record; a new
// job with the same name must not reuse that path, or deleting the old
// record with its files would delete the new download.
func TestNoReuseOfRecordedStorage(t *testing.T) {
	q := startQueue(t, &fakeProvider{}, &fakeEngine{})
	ref := nzb.Ref{Provider: "fake", ID: "1"}
	first := waitFinished(t, q, q.Add("Show.S01E01", "Show.S01E01.nzb", "tv", ref))
	if err := os.RemoveAll(first.Storage); err != nil { // the importer moved it away
		t.Fatal(err)
	}
	second := waitFinished(t, q, q.Add("Show.S01E01", "Show.S01E01.nzb", "tv", ref))
	if second.Storage == first.Storage {
		t.Fatalf("second job reused %q", first.Storage)
	}
	q.Delete(first.ID, true)
	if _, err := os.Stat(filepath.Join(second.Storage, "Show.S01E01.mp4")); err != nil {
		t.Errorf("second job's file gone: %v", err)
	}
}
