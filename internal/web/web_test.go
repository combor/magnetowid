package web

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/combor/magnetowid/internal/downloader"
	"github.com/combor/magnetowid/internal/nzb"
	"github.com/combor/magnetowid/internal/provider"
	"github.com/combor/magnetowid/internal/store"
)

type fakeProvider struct{}

func (fakeProvider) Name() string { return "fake" }
func (fakeProvider) Search(context.Context, provider.Query) ([]provider.Item, error) {
	return nil, nil
}
func (fakeProvider) Resolve(context.Context, string) (provider.Stream, error) {
	return provider.Stream{}, nil
}

type fakeEngine struct{}

func (fakeEngine) Download(context.Context, provider.Stream, string, func(time.Duration, int64)) error {
	return nil
}

// newUI serves the interface for a queue whose worker never runs.
func newUI(t *testing.T) (*httptest.Server, *downloader.Queue) {
	t.Helper()
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	q, err := downloader.New(filepath.Dir(db.Path()), db, provider.NewRegistry(fakeProvider{}), fakeEngine{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	// The other APIs' routes must not conflict.
	mux.Handle("/{provider}/api", http.NotFoundHandler())
	mux.Handle("/api", http.NotFoundHandler())
	mux.HandleFunc("GET /overrides", http.NotFound)
	mux.HandleFunc("GET /health", http.NotFound)
	(&Handler{Queue: q, APIKey: "key", Version: "1.2.3", Log: slog.New(slog.DiscardHandler)}).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, q
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
	srv, _ := newUI(t)

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
	srv, q := newUI(t)
	c := signIn(t, srv)
	add := func(name string) string {
		t.Helper()
		id, err := q.Add(name, name+".nzb", "tv", 0, false, nzb.Ref{Provider: "fake", ID: name})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	add("Tom & Jerry.S01E02.1080p.WEB-DL.AAC.H.264-FAKE")
	if _, err := q.PauseJobs(add("Ranczo.S02E01.1080p.WEB-DL.AAC.H.264-FAKE")); err != nil {
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

	// Refreshes carry only the changing part and the tab's title.
	r = get(t, srv, "/ui/queue", c, "HX-Request", "true")
	if r.status != http.StatusOK || !strings.HasPrefix(r.body, "<title>Paused — magnetowid</title>") ||
		strings.Contains(r.body, "<html") || !strings.Contains(r.body, "Ranczo") {
		t.Errorf("refresh = %d %s", r.status, r.body)
	}
}

func TestStaticFiles(t *testing.T) {
	srv, _ := newUI(t)
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

	for _, tc := range []struct {
		v     queueView
		state string
	}{
		{newQueueView(nil, false, nil, "dev", now), "idle"},
		{newQueueView(nil, true, nil, "dev", now), "paused"},
		{newQueueView([]downloader.Job{low}, false, nil, "dev", now), "waiting"},
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
		{newQueueView([]downloader.Job{failing}, false, nil, "", now), []string{"Queued</h2>", `data-state="waiting"`}},
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
