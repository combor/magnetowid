package web

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/combor/magnetowid/internal/downloader"
	"github.com/combor/magnetowid/internal/nzb"
	"github.com/combor/magnetowid/internal/overrides"
	"github.com/combor/magnetowid/internal/provider"
	"github.com/combor/magnetowid/internal/store"
	bolt "go.etcd.io/bbolt"
)

type fakeProvider struct{}

func (fakeProvider) Name() string { return "fake" }
func (fakeProvider) Search(context.Context, provider.Query) ([]provider.Item, error) {
	return nil, nil
}

// Resolving "gone" fails permanently.
func (fakeProvider) Resolve(_ context.Context, id string) (provider.Stream, error) {
	if id == "gone" {
		return provider.Stream{}, provider.ErrUnavailable
	}
	return provider.Stream{}, nil
}

// IDs are lowercase letters, and pages https://site.example/<id>.
func (fakeProvider) ParseID(ref string) (string, error) {
	ref = strings.TrimPrefix(ref, "https://site.example/")
	if ref == "" || strings.Trim(ref, "abcdefghijklmnopqrstuvwxyz") != "" {
		return "", errors.New("not an ID")
	}
	return ref, nil
}

func (fakeProvider) SetOverrides(*provider.Overrides) {}

type fakeEngine struct{}

func (fakeEngine) Download(_ context.Context, _ provider.Stream, out string, progress func(time.Duration, int64)) error {
	progress(time.Second, 5)
	return os.WriteFile(out, []byte("media"), 0o666)
}

// newUI serves the interface for a queue whose worker runs only in finish.
func newUI(t *testing.T) (*httptest.Server, *downloader.Queue, *bolt.DB) {
	t.Helper()
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	providers := provider.NewRegistry(fakeProvider{})
	q, err := downloader.New(filepath.Dir(db.Path()), db, providers, fakeEngine{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	o, err := overrides.Open(db, providers)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	// The other APIs' routes must not conflict.
	mux.Handle("/{provider}/api", http.NotFoundHandler())
	mux.Handle("/api", http.NotFoundHandler())
	mux.HandleFunc("GET /overrides", http.NotFound)
	mux.HandleFunc("GET /health", http.NotFound)
	(&Handler{Queue: q, Overrides: o, APIKey: "key", Version: "1.2.3", Log: slog.New(slog.DiscardHandler)}).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, q, db
}

// finish runs the worker until every job has finished.
func finish(t *testing.T, q *downloader.Queue) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { q.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if !slices.ContainsFunc(q.Jobs(), unfinished) {
			return
		}
	}
	t.Fatalf("jobs did not finish: %+v", q.Jobs())
}

func unfinished(j downloader.Job) bool {
	return j.Status == downloader.StatusQueued || j.Status == downloader.StatusDownloading
}

func add(t *testing.T, q *downloader.Queue, name, ref string) string {
	t.Helper()
	id, err := q.Add(name, name+".nzb", "tv", 0, false, nzb.Ref{Provider: "fake", ID: ref})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func job(q *downloader.Queue, id string) (downloader.Job, bool) {
	for _, j := range q.Jobs() {
		if j.ID == id {
			return j, true
		}
	}
	return downloader.Job{}, false
}

type response struct {
	status int
	header http.Header
	body   string
}

func do(t *testing.T, srv *httptest.Server, req *http.Request) response {
	t.Helper()
	client := *srv.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return response{resp.StatusCode, resp.Header, string(b)}
}

func get(t *testing.T, srv *httptest.Server, path string, cookie *http.Cookie, header ...string) response {
	t.Helper()
	req, err := http.NewRequest("GET", srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	return do(t, srv, req)
}

func post(t *testing.T, srv *httptest.Server, path string, form url.Values, header ...string) response {
	t.Helper()
	req, err := http.NewRequest("POST", srv.URL+path, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	return do(t, srv, req)
}

func signIn(t *testing.T, srv *httptest.Server) *http.Cookie {
	t.Helper()
	r := post(t, srv, "/ui/login", url.Values{"apikey": {"key"}})
	if r.status != http.StatusSeeOther || r.header.Get("Location") != "/ui/" {
		t.Fatalf("signing in = %d to %q", r.status, r.header.Get("Location"))
	}
	c, err := http.ParseSetCookie(r.header.Get("Set-Cookie"))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestSignIn(t *testing.T) {
	srv, _, _ := newUI(t)

	if r := get(t, srv, "/", nil); r.status != http.StatusSeeOther || r.header.Get("Location") != "/ui/" {
		t.Errorf("GET / = %d to %q", r.status, r.header.Get("Location"))
	}
	if r := get(t, srv, "/ui/", nil); r.status != http.StatusSeeOther || r.header.Get("Location") != "/ui/login" {
		t.Errorf("signed-out GET /ui/ = %d to %q", r.status, r.header.Get("Location"))
	}
	// htmx would put a redirected page inside the current one.
	if r := get(t, srv, "/ui/queue", nil, "HX-Request", "true"); r.status != http.StatusOK || r.header.Get("HX-Redirect") != "/ui/login" || r.body != "" {
		t.Errorf("signed-out refresh = %d, HX-Redirect %q, body %q", r.status, r.header.Get("HX-Redirect"), r.body)
	}
	if r := get(t, srv, "/ui/login", nil); r.status != http.StatusOK || !strings.Contains(r.body, `name="apikey"`) {
		t.Errorf("GET /ui/login = %d %s", r.status, r.body)
	}

	r := post(t, srv, "/ui/login", url.Values{"apikey": {"wrong"}})
	if r.status != http.StatusUnauthorized || !strings.Contains(r.body, `aria-invalid="true"`) || r.header.Get("Set-Cookie") != "" {
		t.Errorf("wrong key = %d, Set-Cookie %q, %s", r.status, r.header.Get("Set-Cookie"), r.body)
	}
	if r := post(t, srv, "/ui/login", url.Values{"apikey": {"key"}}, "Sec-Fetch-Site", "cross-site"); r.status != http.StatusForbidden {
		t.Errorf("cross-site sign-in = %d", r.status)
	}

	c := signIn(t, srv)
	if c.Name != cookieName || c.Path != "/ui" || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.MaxAge <= 0 || strings.Contains(c.Value, "key") {
		t.Errorf("session cookie %+v", c)
	}
	if r := get(t, srv, "/ui/", c); r.status != http.StatusOK || !strings.Contains(r.body, "The queue is empty") {
		t.Errorf("signed-in GET /ui/ = %d %s", r.status, r.body)
	}
	if r := get(t, srv, "/ui/login", c); r.status != http.StatusSeeOther || r.header.Get("Location") != "/ui/" {
		t.Errorf("signed-in GET /ui/login = %d to %q", r.status, r.header.Get("Location"))
	}

	// Sessions lapse on the server, and changing the API key signs everyone out.
	for name, value := range map[string]string{
		"another key": (&Handler{APIKey: "old key"}).session(time.Now().Add(time.Hour)),
		"expired":     (&Handler{APIKey: "key"}).session(time.Now().Add(-time.Minute)),
		"extended":    strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10) + c.Value[strings.Index(c.Value, "."):],
		"malformed":   "nonsense",
	} {
		if r := get(t, srv, "/ui/", &http.Cookie{Name: cookieName, Value: value}); r.status != http.StatusSeeOther {
			t.Errorf("session from %s = %d", name, r.status)
		}
	}

	r = post(t, srv, "/ui/logout", nil)
	out, err := http.ParseSetCookie(r.header.Get("Set-Cookie"))
	if r.status != http.StatusSeeOther || r.header.Get("Location") != "/ui/login" || err != nil || out.Name != cookieName || out.MaxAge >= 0 {
		t.Errorf("sign-out = %d to %q, cookie %+v, %v", r.status, r.header.Get("Location"), out, err)
	}
}

func TestQueuePage(t *testing.T) {
	srv, q, _ := newUI(t)
	c := signIn(t, srv)
	add(t, q, "Tom & Jerry.S01E02.1080p.WEB-DL.AAC.H.264-FAKE", "1")
	if _, err := q.PauseJobs(add(t, q, "Ranczo.S02E01.1080p.WEB-DL.AAC.H.264-FAKE", "2")); err != nil {
		t.Fatal(err)
	}
	if err := q.SetPaused(true); err != nil {
		t.Fatal(err)
	}

	r := get(t, srv, "/ui/", c)
	for _, want := range []string{
		"<title>Paused — magnetowid</title>",
		"Downloads are paused.",
		`hx-get="/ui/queue"`,
		// Actions queue with refreshes, so responses land in order.
		`hx-sync:inherited="#app:queue all"`,
		"Tom &amp; Jerry",
		`<span class="episode">S02E01</span>`,
		`<span class="badge badge-warn">Paused</span>`,
		`<span class="pill-label">Paused</span>`,
		"v1.2.3",
	} {
		if !strings.Contains(r.body, want) {
			t.Errorf("queue page lacks %q:\n%s", want, r.body)
		}
	}
	if r.header.Get("Cache-Control") != "no-store" || !strings.Contains(r.header.Get("Content-Security-Policy"), "default-src 'self'") {
		t.Errorf("queue page headers %v", r.header)
	}
	// htmx focuses the first autofocus element after every swap.
	if strings.Contains(r.body, "autofocus") || !strings.Contains(r.body, `class="confirm-notice"`) {
		t.Errorf("queue page confirmations:\n%s", r.body)
	}

	// Refreshes carry only the changing part and the tab's title.
	r = get(t, srv, "/ui/queue", c, "HX-Request", "true")
	if r.status != http.StatusOK || !strings.HasPrefix(r.body, "<title>Paused — magnetowid</title>") ||
		strings.Contains(r.body, "<html") || !strings.Contains(r.body, "Ranczo") || r.header.Get("Vary") != "HX-Request" {
		t.Errorf("refresh = %d %v %s", r.status, r.header, r.body)
	}
	if r := get(t, srv, "/ui/queue", c); r.status != http.StatusOK || !strings.Contains(r.body, "<html") {
		t.Errorf("GET /ui/queue = %d %s", r.status, r.body)
	}
}

func TestQueueActions(t *testing.T) {
	srv, q, db := newUI(t)
	c := signIn(t, srv)
	cookie := c.Name + "=" + c.Value
	id := add(t, q, "Ranczo.S02E01.1080p.WEB-DL.AAC.H.264-FAKE", "1")
	other := add(t, q, "Ranczo.S02E02.1080p.WEB-DL.AAC.H.264-FAKE", "2")

	// Plain forms redirect to the page.
	for _, tc := range []struct {
		path  string
		check func() bool
	}{
		{"/ui/queue/pause", q.Paused},
		{"/ui/queue/resume", func() bool { return !q.Paused() }},
		{"/ui/queue/" + id + "/pause", func() bool { j, _ := job(q, id); return j.Paused }},
		{"/ui/queue/" + id + "/resume", func() bool { j, _ := job(q, id); return !j.Paused }},
		{"/ui/queue/" + other + "/delete", func() bool { _, ok := job(q, other); return !ok }},
	} {
		r := post(t, srv, tc.path, nil, "Cookie", cookie)
		if r.status != http.StatusSeeOther || r.header.Get("Location") != "/ui/" || !tc.check() {
			t.Errorf("POST %s = %d to %q; applied %v", tc.path, r.status, r.header.Get("Location"), tc.check())
		}
	}

	// htmx requests get the new state.
	r := post(t, srv, "/ui/queue/pause", nil, "Cookie", cookie, "HX-Request", "true")
	if r.status != http.StatusOK || !strings.HasPrefix(r.body, "<title>Paused — magnetowid</title>") ||
		!strings.Contains(r.body, "Resume<span class=\"wide\"> queue</span>") || strings.Contains(r.body, "<html") {
		t.Errorf("htmx pause = %d %s", r.status, r.body)
	}
	r = post(t, srv, "/ui/queue/"+id+"/pause", nil, "Cookie", cookie, "HX-Request", "true")
	if r.status != http.StatusOK || !strings.Contains(r.body, `action="/ui/queue/`+id+`/resume"`) {
		t.Errorf("htmx job pause = %d %s", r.status, r.body)
	}
	// An unknown job is not an error.
	if r := post(t, srv, "/ui/queue/SABnzbd_nzo_gone/delete", nil, "Cookie", cookie, "HX-Request", "true"); r.status != http.StatusOK {
		t.Errorf("removing an unknown job = %d %s", r.status, r.body)
	}

	if r := post(t, srv, "/ui/queue/resume", nil, "Cookie", cookie, "Sec-Fetch-Site", "cross-site"); r.status != http.StatusForbidden || !q.Paused() {
		t.Errorf("cross-site resume = %d, paused %v", r.status, q.Paused())
	}
	if r := post(t, srv, "/ui/queue/resume", nil); r.status != http.StatusSeeOther || r.header.Get("Location") != "/ui/login" || !q.Paused() {
		t.Errorf("signed-out resume = %d to %q", r.status, r.header.Get("Location"))
	}
	if r := post(t, srv, "/ui/queue/resume", nil, "HX-Request", "true"); r.header.Get("HX-Redirect") != "/ui/login" || !q.Paused() {
		t.Errorf("signed-out htmx resume = %d, HX-Redirect %q", r.status, r.header.Get("HX-Redirect"))
	}

	// A failed save gets a notice that refreshes don't replace.
	db.Close()
	r = post(t, srv, "/ui/queue/resume", nil, "Cookie", cookie, "HX-Request", "true")
	if r.status != http.StatusInternalServerError || r.header.Get("HX-Retarget") != "#notice" || r.header.Get("HX-Reswap") != "innerHTML" ||
		!strings.HasPrefix(r.body, `<p class="toast" role="alert">`) || !strings.Contains(r.body, "Couldn’t resume the queue.") {
		t.Errorf("failed htmx resume = %d %v %s", r.status, r.header, r.body)
	}
	r = post(t, srv, "/ui/queue/resume", nil, "Cookie", cookie)
	if r.status != http.StatusInternalServerError || !strings.Contains(r.body, "<html") || !strings.Contains(r.body, "Couldn’t resume the queue.") {
		t.Errorf("failed resume = %d %s", r.status, r.body)
	}
	// A failed removal's notice goes into its confirmation.
	r = post(t, srv, "/ui/queue/"+id+"/delete", nil, "Cookie", cookie, "HX-Request", "true")
	if r.status != http.StatusInternalServerError || r.header.Get("HX-Retarget") != "#remove-"+id+"-notice" ||
		!strings.Contains(r.body, "Couldn’t remove the download.") {
		t.Errorf("failed htmx removal = %d %v %s", r.status, r.header, r.body)
	}
}

func TestHistoryPage(t *testing.T) {
	srv, q, _ := newUI(t)
	c := signIn(t, srv)
	cookie := c.Name + "=" + c.Value
	kept := add(t, q, "Ranczo.S02E01.1080p.WEB-DL.AAC.H.264-FAKE", "1")
	deleted := add(t, q, "Ranczo.S02E02.1080p.WEB-DL.AAC.H.264-FAKE", "2")
	failed := add(t, q, "Seksmisja.1984.1080p.WEB-DL.AAC.H.264-FAKE", "gone")
	finish(t, q)
	add(t, q, "Queued.S01E01", "3")

	r := get(t, srv, "/ui/history", c)
	for _, want := range []string{
		"<title>History — magnetowid</title>",
		`hx-get="/ui/history" hx-trigger="every 5s"`,
		`<a class="tab" href="/ui/history" aria-current="page">History<span class="tab-count">3</span></a>`,
		`<span class="badge badge-ok">Completed</span>`,
		`<span class="badge badge-danger">Failed</span>`,
		"content unavailable",
		"Seksmisja",
		`action="/ui/history/` + deleted + `/delete"`,
		`name="files" value="1"`,
	} {
		if !strings.Contains(r.body, want) {
			t.Errorf("history lacks %q:\n%s", want, r.body)
		}
	}
	if strings.Contains(r.body, "Queued</span>") {
		t.Errorf("history shows a queued job:\n%s", r.body)
	}
	if r := get(t, srv, "/ui/history", c, "HX-Request", "true"); !strings.HasPrefix(r.body, "<title>History") || strings.Contains(r.body, "<html") {
		t.Errorf("history refresh = %d %s", r.status, r.body)
	}

	storage := func(id string) string { j, _ := job(q, id); return j.Storage }
	keptDir, deletedDir := storage(kept), storage(deleted)
	r = post(t, srv, "/ui/history/"+kept+"/delete", nil, "Cookie", cookie)
	if _, err := os.Stat(keptDir); r.status != http.StatusSeeOther || r.header.Get("Location") != "/ui/history" || err != nil {
		t.Errorf("removing without files = %d to %q; files %v", r.status, r.header.Get("Location"), err)
	}
	r = post(t, srv, "/ui/history/"+deleted+"/delete", url.Values{"files": {"1"}}, "Cookie", cookie, "HX-Request", "true")
	if _, err := os.Stat(deletedDir); r.status != http.StatusOK || !os.IsNotExist(err) || !strings.HasPrefix(r.body, "<title>History") {
		t.Errorf("removing with files = %d, files %v, %s", r.status, err, r.body)
	}
	for _, id := range []string{kept, deleted} {
		if _, ok := job(q, id); ok {
			t.Errorf("job %s still in history", id)
		}
	}
	if _, ok := job(q, failed); !ok {
		t.Error("the failed job went too")
	}
}

func TestStaticFiles(t *testing.T) {
	srv, _, _ := newUI(t)
	r := get(t, srv, "/ui/static/htmx-4.0.0.min.js", nil)
	if r.status != http.StatusOK || !strings.HasPrefix(r.header.Get("Content-Type"), "text/javascript") ||
		r.header.Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(r.body, "htmx") {
		t.Fatalf("htmx = %d %v", r.status, r.header)
	}
	tag := r.header.Get("ETag")
	if r := get(t, srv, "/ui/static/htmx-4.0.0.min.js", nil, "If-None-Match", tag); tag == "" || r.status != http.StatusNotModified {
		t.Errorf("revalidating ETag %q = %d", tag, r.status)
	}
	for _, name := range []string{"style.css", "icon.svg"} {
		if r := get(t, srv, "/ui/static/"+name, nil); r.status != http.StatusOK {
			t.Errorf("%s = %d", name, r.status)
		}
	}
}

func TestHistoryView(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	finished := func(name string, status downloader.Status, ago, took time.Duration) downloader.Job {
		return downloader.Job{ID: name, Name: name, Category: "tv", Status: status, Ref: nzb.Ref{Provider: "tvp"},
			Finished: now.Add(-ago), Started: now.Add(-ago - took), Bytes: 1_240_000_000, Attempts: 1}
	}
	older := finished("Ranczo.S02E01.1080p.WEB-DL.AAC.H.264-TVP", downloader.StatusCompleted, 3*time.Hour, 14*time.Minute)
	older.Storage = "/downloads/tv/Ranczo.S02E01.1080p.WEB-DL.AAC.H.264-TVP"
	failed := finished("failed", downloader.StatusFailed, 2*time.Hour, 30*time.Second)
	failed.Attempts, failed.Error = 6, "content unavailable"
	newer := finished("newer", downloader.StatusCompleted, time.Minute, 65*time.Minute)
	running := downloader.Job{ID: "running", Status: downloader.StatusDownloading}

	v := newHistoryView([]downloader.Job{older, running, failed, newer}, false, "1.4.0", now)
	var got []string
	for _, j := range v.Jobs {
		got = append(got, fmt.Sprintf("%s: %s %s, %q %q %q, %d attempts, %q", j.ID, j.Tone, j.Status, j.Size, j.Took, j.Finished, j.Attempt, j.Storage))
	}
	want := []string{
		`newer: ok Completed, "1.2 GB" "1 h 5 min" "1 min ago", 0 attempts, ""`,
		`failed: danger Failed, "" "" "2 h ago", 6 attempts, ""`,
		`Ranczo.S02E01.1080p.WEB-DL.AAC.H.264-TVP: ok Completed, "1.2 GB" "14 min" "3 h ago", 0 attempts, "/downloads/tv/Ranczo.S02E01.1080p.WEB-DL.AAC.H.264-TVP"`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("history:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if j := v.Jobs[2]; j.Label() != "Ranczo S02E01" || j.Error != "" {
		t.Errorf("job = %+v", j)
	}
	if v.Queued != 1 || v.Finished != 3 || v.State() != "downloading" || v.Refresh() != "/ui/history" || v.Every() != "5s" || v.KeptDays() != 30 {
		t.Errorf("chrome = %+v, state %q", v.chrome, v.State())
	}
}

func TestQueueView(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	job := func(name string, status downloader.Status) downloader.Job {
		return downloader.Job{ID: name, Name: name, Category: "tv", Status: status, Added: now.Add(-5 * time.Minute),
			Ref: nzb.Ref{Provider: "tvp", Duration: 2700}}
	}
	running := job("Ranczo.S02E01.1080p.WEB-DL.AAC.H.264-TVP", downloader.StatusDownloading)
	running.Started, running.Fraction, running.Bytes, running.Attempts = now.Add(-6*time.Minute), 0.42, 520_000_000, 2
	low := job("low", downloader.StatusQueued)
	low.Priority = -1
	high := job("high", downloader.StatusQueued)
	high.Priority = 1
	retrying := job("retrying", downloader.StatusQueued)
	retrying.RetryAt, retrying.Error = now.Add(3*time.Minute+10*time.Second), "stream gone"
	paused := job("paused", downloader.StatusQueued)
	paused.Paused, paused.Priority = true, 2
	offline := job("offline", downloader.StatusQueued)
	offline.Ref.Provider, offline.Error = "bbc", "provider unreachable"

	v := newQueueView([]downloader.Job{
		job("done", downloader.StatusCompleted), low, running, retrying, high, paused, offline, job("failed", downloader.StatusFailed),
	}, false, map[string]time.Time{"bbc": now.Add(2 * time.Minute), "tvp": now.Add(-time.Minute)}, "1.4.0", now)

	a := v.Active
	if a == nil || a.Title != "Ranczo" || a.Episode != "S02E01" || a.Percent != 42 || a.Status != "Downloading" ||
		a.Size != "520 MB of ~1.2 GB" || a.Left != "about 8 min left" || a.Attempt != 2 || a.Added != "5 min ago" {
		t.Errorf("active = %+v", a)
	}
	var next []string
	for _, j := range v.Next {
		next = append(next, fmt.Sprintf("%d %s: %s %s", j.Position, j.Name, j.Tone, j.Status))
	}
	// Jobs that can run come first, in the worker's order; the rest follow,
	// unnumbered, by priority too.
	want := []string{
		"1 high: neutral Queued",
		"2 low: neutral Queued",
		"0 paused: warn Paused",
		"0 retrying: danger Retrying in 4 min",
		"0 offline: danger Retrying in 2 min",
	}
	if strings.Join(next, "\n") != strings.Join(want, "\n") {
		t.Errorf("next:\n%s\nwant:\n%s", strings.Join(next, "\n"), strings.Join(want, "\n"))
	}
	if v.Next[0].Priority != "High priority" || v.Next[1].Priority != "Low priority" {
		t.Errorf("next = %+v", v.Next)
	}
	if v.State() != "downloading" || v.Title() != "42% · Ranczo S02E01 — magnetowid" || v.Version != "v1.4.0" {
		t.Errorf("state %q, title %q, version %q", v.State(), v.Title(), v.Version)
	}

	// A download stopping for a pause is shown as queued.
	stopping := running
	stopping.Paused = true
	for _, v := range []queueView{
		newQueueView([]downloader.Job{stopping, low}, false, nil, "", now),
		newQueueView([]downloader.Job{running, low}, true, nil, "", now),
	} {
		if v.Active != nil || len(v.Next) != 2 || v.Next[0].Running || v.State() == "downloading" {
			t.Errorf("stopping download: active %+v, next %+v, state %q", v.Active, v.Next, v.State())
		}
	}

	for _, tc := range []struct {
		v     queueView
		state string
	}{
		{newQueueView(nil, false, nil, "dev", now), "idle"},
		{newQueueView(nil, true, nil, "dev", now), "paused"},
		{newQueueView([]downloader.Job{low}, false, nil, "dev", now), "waiting"},
		{newQueueView([]downloader.Job{paused}, false, nil, "dev", now), "waiting"},
	} {
		if tc.v.State() != tc.state || tc.v.Version != "dev" {
			t.Errorf("state %q, version %q; want %q", tc.v.State(), tc.v.Version, tc.state)
		}
	}
}

// Every state the page can show must render.
func TestRenderStates(t *testing.T) {
	now := time.Now()
	running := downloader.Job{Name: "Hydrozagadka.1971.Polish.1080p.WEB-DL.AAC.H.264-TVP", Status: downloader.StatusDownloading,
		Started: now, Ref: nzb.Ref{Provider: "tvp"}}
	failing := downloader.Job{Name: "x", Status: downloader.StatusQueued, RetryAt: now.Add(time.Minute), Error: "<b>boom</b>"}
	paused := downloader.Job{ID: "p1", Name: "Paused.S01E01.1080p-TVP", Status: downloader.StatusQueued, Paused: true}
	partial := paused
	partial.Fraction = 0.4
	for _, tc := range []struct {
		v    queueView
		want []string
	}{
		{newQueueView(nil, false, nil, "", now), []string{"The queue is empty", `data-state="idle"`}},
		{newQueueView(nil, true, nil, "", now), []string{"The queue is empty", "Downloads are paused."}},
		{newQueueView([]downloader.Job{running}, false, nil, "", now), []string{"Now downloading", "Hydrozagadka</span>", ">1971<",
			"<span>Polish</span><span>1080p</span><span>WEB-DL</span><span>AAC</span><span>H.264</span>",
			`value="0"`, "estimating…"}},
		{newQueueView([]downloader.Job{running, failing}, false, nil, "", now), []string{"Up next", "Retrying in", "&lt;b&gt;boom&lt;/b&gt;"}},
		{newQueueView([]downloader.Job{failing}, false, nil, "", now), []string{"Queued</h2>", `data-state="waiting"`,
			`action="/ui/queue/pause"`, "Pause<span", `action="/ui/queue//pause"`, "Remove it from the queue?"}},
		{newQueueView([]downloader.Job{running}, false, nil, "", now), []string{"Stop and remove this download?",
			"Its partial download is deleted."}},
		{newQueueView([]downloader.Job{partial}, false, nil, "", now), []string{"Paused at 40%", "Remove it from the queue?",
			"Its partial download is deleted."}},
		{newQueueView(nil, true, nil, "", now), []string{"Resume<span", `action="/ui/queue/resume"`}},
		{newQueueView([]downloader.Job{paused}, false, nil, "", now), []string{`action="/ui/queue/p1/resume"`, "Resume Paused S01E01"}},
	} {
		var b bytes.Buffer
		if err := queuePage.ExecuteTemplate(&b, "layout", tc.v); err != nil {
			t.Fatal(err)
		}
		for _, want := range tc.want {
			if !strings.Contains(b.String(), want) {
				t.Errorf("page for %+v lacks %q", tc.v, want)
			}
		}
	}

	done := downloader.Job{ID: "d1", Name: "Cube.1997.1080p-TVP", Status: downloader.StatusCompleted, Storage: "/downloads/movies/Cube"}
	gone := downloader.Job{ID: "f1", Name: "x", Status: downloader.StatusFailed, Error: "<b>boom</b>"}
	for _, tc := range []struct {
		v    historyView
		want []string
		not  []string
	}{
		{newHistoryView(nil, false, "", now), []string{"No finished downloads", "for 30 days"}, nil},
		{newHistoryView([]downloader.Job{done}, false, "", now), []string{"Cube", "/downloads/movies/Cube", "hasn’t imported it yet"}, nil},
		{newHistoryView([]downloader.Job{gone}, false, "", now), []string{"&lt;b&gt;boom&lt;/b&gt;", `action="/ui/history/f1/delete"`},
			[]string{`name="files"`, "hasn’t imported"}},
	} {
		var b bytes.Buffer
		if err := historyPage.ExecuteTemplate(&b, "layout", tc.v); err != nil {
			t.Fatal(err)
		}
		for _, want := range tc.want {
			if !strings.Contains(b.String(), want) {
				t.Errorf("history for %+v lacks %q", tc.v, want)
			}
		}
		for _, not := range tc.not {
			if strings.Contains(b.String(), not) {
				t.Errorf("history for %+v has %q", tc.v, not)
			}
		}
	}
}

func TestSplitName(t *testing.T) {
	for _, tc := range []struct{ name, title, episode, specs string }{
		{"Ranczo.S02E01.1080p.WEB-DL.AAC.H.264-TVP", "Ranczo", "S02E01", "1080p WEB-DL AAC H.264"},
		{"Hydrozagadka.1971.Polish.1080p.WEB-DL.AAC.H.264-TVP", "Hydrozagadka", "1971", "Polish 1080p WEB-DL AAC H.264"},
		{"Doctor.Who.2005.S01E01.720p.WEB-DL.DDP.H.265-BBC", "Doctor Who 2005", "S01E01", "720p WEB-DL DDP H.265"},
		{"1917.2019.2160p.WEB-DL.AV1-TVP", "1917", "2019", "2160p WEB-DL AV1"},
		{"Blade.Runner.2049.2017.1080p.WEB-DL-TVP", "Blade Runner 2049", "2017", "1080p WEB-DL"},
		{"The.Daily.Show.S2026E187.1080p.WEB-DL-BBC", "The Daily Show", "S2026E187", "1080p WEB-DL"},
		{"Some release name", "Some release name", "", ""},
	} {
		title, episode, specs := splitName(tc.name)
		if title != tc.title || episode != tc.episode || strings.Join(specs, " ") != tc.specs {
			t.Errorf("splitName(%q) = %q, %q, %q", tc.name, title, episode, specs)
		}
	}
}

func TestFormatting(t *testing.T) {
	for _, tc := range []struct{ got, want string }{
		{size(0), "less than 1 MB"},
		{size(520_400_000), "520 MB"},
		{size(1_240_000_000), "1.2 GB"},
		{timeLeft(0), "estimating…"},
		{timeLeft(40 * time.Second), "less than a minute left"},
		{timeLeft(8*time.Minute + 20*time.Second), "about 8 min left"},
		{timeLeft(2 * time.Hour), "about 2 h left"},
		{timeLeft(65 * time.Minute), "about 1 h 5 min left"},
		{duration(20 * time.Second), "less than a minute"},
		{duration(14*time.Minute + 10*time.Second), "14 min"},
		{duration(71 * time.Minute), "1 h 11 min"},
		{ago(10 * time.Second), "just now"},
		{ago(5 * time.Minute), "5 min ago"},
		{ago(3 * time.Hour), "3 h ago"},
		{ago(30 * time.Hour), "yesterday"},
		{ago(80 * time.Hour), "3 days ago"},
		{until(200 * time.Millisecond), "1 s"},
		{until(40 * time.Second), "40 s"},
		{until(3*time.Minute + time.Second), "4 min"},
		{displayVersion("1.4.0"), "v1.4.0"},
		{displayVersion("dev"), "dev"},
	} {
		if tc.got != tc.want {
			t.Errorf("got %q, want %q", tc.got, tc.want)
		}
	}
}
