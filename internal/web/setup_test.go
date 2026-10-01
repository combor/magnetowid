package web

import (
	"bytes"
	"context"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/combor/magnetowid/internal/downloader"
	"github.com/combor/magnetowid/internal/nzb"
	"github.com/combor/magnetowid/internal/provider"
)

func TestSetupPage(t *testing.T) {
	srv, q, _ := newUI(t)
	if r := get(t, srv, "/ui/setup", nil); r.status != http.StatusSeeOther || r.header.Get("Location") != "/ui/login" {
		t.Fatalf("signed out = %d to %q", r.status, r.header.Get("Location"))
	}
	page, _ := signedIn(t, srv)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	r := page("/ui/setup")
	if missing := lacks(r.body,
		"<title>Setup — magnetowid</title>",
		`<h1 class="visually-hidden">Setup</h1>`,
		`<a class="tab" href="/ui/setup" aria-current="page">Setup</a>`,
		`hx-get="/ui/setup" hx-trigger="every 5s"`,
		`<code class="value">`+u.Hostname()+`</code>`,
		`<code class="value">`+u.Port()+`</code>`,
		`<code class="value">tv</code><span class="for">in Sonarr</span>`,
		`<code class="value">movies</code><span class="for">in Radarr</span>`,
		`<code class="value">`+srv.URL+`/fake</code><span class="for">fake</span>`,
		`<code class="value">/api</code>`,
		`<code class="value">5000, 5040</code><span class="for">in Sonarr</span>`,
		`<code class="value">2000, 2040</code><span class="for">in Radarr</span>`,
		`<li class="row" id="site-fake">`,
		"No downloads yet",
		"<span>overrides</span>",
		"<dd>v1.2.3</dd>",
		`<code class="value">`+q.Dir()+`</code>`,
		"<dd>tv, movies</dd>",
	); r.status != http.StatusOK || missing != nil {
		t.Errorf("setup page = %d, lacks %q:\n%s", r.status, missing, r.body)
	}
	// The page names the API key without showing it.
	if strings.Contains(r.body, `"key"`) || strings.Contains(r.body, ">key<") || strings.Contains(r.body, "Use SSL") {
		t.Errorf("setup page shows the API key or SSL:\n%s", r.body)
	}

	r = page("/ui/setup", "X-Forwarded-Proto", "https", "X-Forwarded-Host", "magnetowid.example.com")
	if missing := lacks(r.body,
		`<code class="value">magnetowid.example.com</code>`,
		`<code class="value">443</code>`,
		"<dt>Use SSL</dt>",
		`<code class="value">https://magnetowid.example.com/fake</code>`,
	); missing != nil {
		t.Errorf("setup page behind a proxy lacks %q:\n%s", missing, r.body)
	}

	add(t, q, "Ranczo.S02E01.1080p.WEB-DL.AAC.H.264-FAKE", "a")
	finish(t, q)
	r = page("/ui/setup", "HX-Request", "true")
	if missing := lacks(r.body, "<title>Setup — magnetowid</title>", `<span class="badge badge-ok">Working</span>`,
		"<span>last download just now</span>"); missing != nil || strings.Contains(r.body, "<html") {
		t.Errorf("refreshed setup page lacks %q, or is a whole page:\n%s", missing, r.body)
	}
}

type fullProvider struct{ fakeProvider }

func (fullProvider) Name() string { return "full" }
func (fullProvider) SearchTVDB(context.Context, int, provider.Query) (string, []provider.Item, error) {
	return "", nil, nil
}
func (fullProvider) Recent(context.Context, provider.Kind) ([]provider.Release, error) {
	return nil, nil
}

func TestSetupView(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	providers := provider.NewRegistry(fakeProvider{}, fullProvider{})
	job := func(site string, status downloader.Status) downloader.Job {
		return downloader.Job{Name: "x", Status: status, Ref: nzb.Ref{Provider: site}}
	}
	done := job("fake", downloader.StatusCompleted)
	done.Finished = now.Add(-2 * time.Hour)
	older := done
	older.Finished = now.Add(-3 * 24 * time.Hour)
	failed := job("fake", downloader.StatusFailed)
	failed.Finished = now
	held := job("fake", downloader.StatusQueued)
	held.Error = "provider unreachable: dial tcp: <no route>"
	waiting := job("fake", downloader.StatusQueued)
	waiting.Error = "boom"
	retried := held
	retried.Status = downloader.StatusDownloading
	paused := held
	paused.Paused = true
	soon, past := now.Add(90*time.Second), now.Add(-time.Second)

	for _, tc := range []struct {
		name   string
		jobs   []downloader.Job
		outage time.Time
		paused bool
		want   siteStatus
	}{
		{"unused", nil, time.Time{}, false,
			siteStatus{Name: "fake", Status: "No downloads yet", Tone: "neutral", Facts: []string{"overrides"}}},
		{"only failures", []downloader.Job{failed, job("full", downloader.StatusCompleted)}, time.Time{}, false,
			siteStatus{Name: "fake", Status: "No downloads yet", Tone: "neutral", Facts: []string{"overrides"}}},
		{"working", []downloader.Job{older, done, failed, waiting}, time.Time{}, false,
			siteStatus{Name: "fake", Status: "Working", Tone: "ok", Facts: []string{"last download 2 h ago", "overrides"}}},
		{"outage", []downloader.Job{done, waiting, held, paused}, soon, false,
			siteStatus{Name: "fake", Status: "Unreachable", Tone: "danger", Error: held.Error,
				Facts: []string{"trying again in 2 min", "2 downloads waiting", "overrides"}}},
		{"outage being retried", []downloader.Job{retried}, past, false,
			siteStatus{Name: "fake", Status: "Unreachable", Tone: "danger", Error: held.Error,
				Facts: []string{"trying again now", "1 download waiting", "overrides"}}},
		{"outage with the queue paused", []downloader.Job{held}, soon, true,
			siteStatus{Name: "fake", Status: "Unreachable", Tone: "danger", Error: held.Error,
				Facts: []string{"tries again when the queue resumes", "1 download waiting", "overrides"}}},
		{"outage whose jobs were removed", []downloader.Job{done}, soon, false,
			siteStatus{Name: "fake", Status: "Unreachable", Tone: "danger",
				Facts: []string{"trying again in 2 min", "overrides"}}},
		{"stale outage", []downloader.Job{done, paused}, past, false,
			siteStatus{Name: "fake", Status: "Working", Tone: "ok", Facts: []string{"last download 2 h ago", "overrides"}}},
	} {
		outages := map[string]time.Time{}
		if !tc.outage.IsZero() {
			outages["fake"] = tc.outage
		}
		c := newChrome(tc.jobs, tc.paused, "", "setup")
		v := newSetupView(c, "http://nas:8484", providers, []string{"tv", "movies"}, "/downloads", tc.jobs, outages, now)
		if len(v.Sites) != 2 || !reflect.DeepEqual(v.Sites[0], tc.want) {
			t.Errorf("%s: sites = %+v, want fake as %+v", tc.name, v.Sites, tc.want)
		}
	}

	v := newSetupView(chrome{}, "http://nas", providers, []string{"series", "films"}, "/downloads",
		[]downloader.Job{held}, map[string]time.Time{"fake": soon}, now)
	if v.Host != "nas" || v.Port != "80" || v.SSL || v.ByApp() {
		t.Errorf("address = %q %q, ssl = %v, by app = %v", v.Host, v.Port, v.SSL, v.ByApp())
	}
	if want := []setupValue{{Text: "series"}, {Text: "films"}}; !reflect.DeepEqual(v.Categories, want) {
		t.Errorf("categories = %+v", v.Categories)
	}
	if want := []setupValue{{"http://nas/fake", "fake"}, {"http://nas/full", "full"}}; !reflect.DeepEqual(v.Indexers, want) {
		t.Errorf("indexers = %+v", v.Indexers)
	}
	if want := []string{"No downloads yet", "TVDB ID search", "RSS", "overrides"}; v.Sites[1].Status != want[0] ||
		!reflect.DeepEqual(v.Sites[1].Facts, want[1:]) {
		t.Errorf("full site = %+v", v.Sites[1])
	}
	if v := newSetupView(chrome{}, "https://[::1]:8484", providers, nil, "", nil, nil, now); v.Host != "::1" || v.Port != "8484" || !v.SSL {
		t.Errorf("IPv6 address = %q %q, ssl = %v", v.Host, v.Port, v.SSL)
	}

	var b bytes.Buffer
	if err := setupPage.ExecuteTemplate(&b, "layout", v); err != nil {
		t.Fatal(err)
	}
	if missing := lacks(b.String(),
		`<span class="badge badge-danger">Unreachable</span>`,
		"provider unreachable: dial tcp: &lt;no route&gt;",
		`<code class="value">series</code></li><li><code class="value">films</code></li>`,
		"One for each app.",
		`<code class="value">80</code>`,
	); missing != nil {
		t.Errorf("setup page lacks %q:\n%s", missing, b.String())
	}
}
