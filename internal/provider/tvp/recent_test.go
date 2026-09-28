package tvp

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/combor/magnetowid/internal/provider"
)

func feedReleases(p *Provider) []provider.Release {
	p.seriesFeed.mu.Lock()
	defer p.seriesFeed.mu.Unlock()
	var out []provider.Release
	for _, rs := range p.seriesFeed.found {
		out = append(out, rs...)
	}
	slices.SortFunc(out, func(a, b provider.Release) int { return cmp.Compare(a.ID, b.ID) })
	return out
}

func describe(rs []provider.Release) string {
	var s []string
	for _, r := range rs {
		s = append(s, fmt.Sprintf("%s %s S%02dE%02d", r.ID, r.Title, r.Season, r.Episode))
	}
	return strings.Join(s, ", ")
}

func TestFeedRebuild(t *testing.T) {
	p := newProvider(t)
	fakeTitles(t, p)
	p.watch(5, "The Ranch")
	p.watch(6, "Nowhere") // not on TVP
	p.rebuildSeries(context.Background())
	got := feedReleases(p)
	if want := "381054 The Ranch S01E13, 381150 The Ranch S02E01"; describe(got) != want {
		t.Errorf("feed = %s; want %s", describe(got), want)
	}
	for _, r := range got {
		if time.Since(r.Published) > time.Minute {
			t.Errorf("%s published %v, want when first found", r.ID, r.Published)
		}
	}
}

func TestFeedKeepsReleasesOfFailedSeries(t *testing.T) {
	var failing atomic.Bool
	p := newProviderWith(t, func(key string) (string, bool) {
		return "500", failing.Load() && strings.HasPrefix(key, "/vods/search/")
	})
	fakeTitles(t, p)
	p.watch(5, "The Ranch")
	p.rebuildSeries(context.Background())
	before := feedReleases(p)
	if len(before) == 0 {
		t.Fatal("empty feed")
	}

	failing.Store(true)
	p.cache.now = func() time.Time { return time.Now().Add(apiCacheTTL) }
	p.rebuildSeries(context.Background())
	if after := feedReleases(p); !slices.Equal(after, before) {
		t.Errorf("feed after a failure = %+v, want %+v", after, before)
	}
}

// TVP dates premieres while still paid; use discovery time so free releases precede the RSS cutoff.
func TestFeedDatesReleasesWhenFirstSeen(t *testing.T) {
	var free atomic.Bool
	p := newProviderWith(t, func(key string) (string, bool) {
		return fmt.Sprintf(`[
			{"id":381150,"number":14,"since":"2007-07-02T00:02:00+02:00"},
			{"id":381138,"number":15,"since":"2007-07-09T00:02:00+02:00"},
			{"id":381151,"number":16,"payable":%t,"since":"2007-06-01T00:00:00+02:00"}]`, !free.Load()),
			key == "/vods/serials/316445/seasons/381149/episodes"
	})
	fakeTitles(t, p)
	p.watch(5, "The Ranch")
	p.rebuildSeries(context.Background())
	first := feedReleases(p)
	if want := "381054 The Ranch S01E13, 381150 The Ranch S02E01"; describe(first) != want {
		t.Fatalf("feed = %s; want %s", describe(first), want)
	}

	free.Store(true)
	p.cache.now = func() time.Time { return time.Now().Add(apiCacheTTL) }
	p.rebuildSeries(context.Background())
	second := feedReleases(p)
	if want := "381054 The Ranch S01E13, 381150 The Ranch S02E01, 381151 The Ranch S02E03"; describe(second) != want {
		t.Fatalf("feed = %s; want %s", describe(second), want)
	}
	for i, r := range first {
		if !second[i].Published.Equal(r.Published) {
			t.Errorf("%s published %v, then %v", r.ID, r.Published, second[i].Published)
		}
		if !second[2].Published.After(r.Published) {
			t.Errorf("newly free %s published %v, not after %s's %v", second[2].ID, second[2].Published, r.ID, r.Published)
		}
	}
}

func waitIdle(t *testing.T, f *feed) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		f.mu.Lock()
		building := f.building
		f.mu.Unlock()
		if !building {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("feed rebuild didn't finish")
		}
	}
}

func TestRecentRebuildsInBackground(t *testing.T) {
	p := newProvider(t)
	fakeTitles(t, p)
	p.seriesFeed.found = map[int][]provider.Release{1: {{Title: "Old", Item: provider.Item{ID: "1"}}}}
	var rebuilds atomic.Int32
	unblock := make(chan struct{})
	p.seriesFeed.rebuild = func(ctx context.Context) {
		rebuilds.Add(1)
		<-unblock
		p.rebuildSeries(ctx)
	}
	recent := func() []provider.Release {
		t.Helper()
		rs, err := p.Recent(context.Background(), provider.Episode)
		if err != nil {
			t.Fatal(err)
		}
		return rs
	}
	check := func(when string, n int32) {
		t.Helper()
		waitIdle(t, &p.seriesFeed)
		if got := rebuilds.Load(); got != n {
			t.Errorf("%s: %d rebuilds, want %d", when, got, n)
		}
	}

	for range 3 {
		if rs := recent(); len(rs) != 1 || rs[0].ID != "1" {
			t.Fatalf("releases while rebuilding = %+v", rs)
		}
	}
	close(unblock)
	check("first requests", 1)
	if rs := recent(); len(rs) != 0 {
		t.Errorf("releases = %+v, want none: nothing is watched", rs)
	}
	check("within feedTTL", 1)

	p.watch(5, "The Ranch")
	recent()
	check("after watching a series", 2)
	if rs := recent(); len(rs) != 2 {
		t.Errorf("releases = %s, want The Ranch's two", describe(rs))
	}

	p.seriesFeed.mu.Lock()
	p.seriesFeed.built = time.Now().Add(-feedTTL)
	p.seriesFeed.mu.Unlock()
	recent()
	check("feedTTL later", 3)

	var filmRebuilds atomic.Int32
	p.filmFeed.rebuild = func(context.Context) { filmRebuilds.Add(1) }
	p.filmFeed.found = map[int][]provider.Release{filmsKey: {{Title: "Kler", Item: provider.Item{ID: "9"}}}}
	if rs, err := p.Recent(context.Background(), provider.Movie); err != nil || len(rs) != 1 || rs[0].ID != "9" {
		t.Errorf("films = %+v, %v", rs, err)
	}
	waitIdle(t, &p.filmFeed)
	if got := filmRebuilds.Load(); got != 1 {
		t.Errorf("%d film rebuilds, want 1", got)
	}
	check("films", 3)
}

const newestProductsKey = "/vods?maxResults=100&order=desc&sort=createdAt"

// Trimmed TVP response (2026-09-28); %t controls whether Pachnidło is paid.
const newestProductsFixture = `{"meta":{"totalCount":6359,"firstResult":0,"maxResults":100},"items":[
	{"type":"VOD","id":1,"title":"Kler","year":2019,"duration":7980,"payable":false,"since":"2026-09-20T09:00:00+02:00"},
	{"type":"VOD","id":2,"title":"Pachnidło: Historia mordercy","year":2006,"duration":8820,"payable":%t},
	{"type":"VOD","id":3,"title":"Płatny","year":2024,"payable":true},
	{"type":"SERIAL","id":4,"title":"Serial","year":2024,"payable":false},
	{"type":"VOD","id":5,"title":"Stary","year":2016,"payable":false},
	{"type":"VOD","id":6,"title":"Niechciany","year":1990,"payable":false}]}`

// Only TVP's search gives a film's original title.
var pachnidloSearchKey = "/vods/search/VOD?" + url.Values{"keyword": {"Pachnidło: Historia mordercy"}}.Encode()

const pachnidloSearch = `{"items":[{"type":"VOD","id":2,"title":"Pachnidło: Historia mordercy",
	"originalTitle":"Perfume: The Story of a Murderer","year":2006,"duration":8820}]}`

// serve overrides TVP fixtures as in newProviderWith.
func filmProvider(t *testing.T, serve func(key string) (string, bool)) (*Provider, *pathRecorder) {
	t.Helper()
	p := newProviderWith(t, func(key string) (string, bool) {
		if serve != nil {
			if body, ok := serve(key); ok {
				return body, true
			}
		}
		switch key {
		case newestProductsKey:
			return fmt.Sprintf(newestProductsFixture, false), true
		case pachnidloSearchKey:
			return pachnidloSearch, true
		}
		return "", false
	})
	for _, f := range []struct {
		title string
		year  int
	}{
		{"Kler", 2018}, // TVP says 2019
		{"Perfume: The Story of a Murderer", 2006}, // TVP's original title
		{"Płatny", 2024},                           // paid
		{"Serial", 2024},                           // a series
		{"Stary", 2018},                            // TVP says 2016
	} {
		p.watchFilm(f.title, f.year)
	}
	rec := &pathRecorder{next: p.client.Transport}
	p.client = &http.Client{Transport: rec}
	return p, rec
}

func filmReleases(p *Provider) []provider.Release {
	p.filmFeed.mu.Lock()
	defer p.filmFeed.mu.Unlock()
	out := slices.Clone(p.filmFeed.found[filmsKey])
	slices.SortFunc(out, func(a, b provider.Release) int { return cmp.Compare(a.ID, b.ID) })
	return out
}

func describeFilms(rs []provider.Release) string {
	var s []string
	for _, r := range rs {
		s = append(s, fmt.Sprintf("%s %s %d", r.ID, r.Title, r.Year))
	}
	return strings.Join(s, ", ")
}

func (r *pathRecorder) count(path string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, p := range r.paths {
		if p == path {
			n++
		}
	}
	return n
}

func TestFilmFeedRebuild(t *testing.T) {
	p, rec := filmProvider(t, nil)
	p.rebuildFilms(context.Background())
	got := filmReleases(p)
	if want := "1 Kler 2018, 2 Perfume: The Story of a Murderer 2006"; describeFilms(got) != want {
		t.Errorf("feed = %s; want %s", describeFilms(got), want)
	}
	for _, r := range got {
		if r.Kind != provider.Movie || r.Duration == 0 || time.Since(r.Published) > time.Minute {
			t.Errorf("release = %+v, want a film published when first found", r)
		}
	}
	// Only Pachnidło has a matching year without a matching title, requiring one lookup.
	if n := rec.count("/vods/search/VOD"); n != 1 {
		t.Errorf("%d searches, want 1", n)
	}

	p.cache.now = func() time.Time { return time.Now().Add(apiCacheTTL) }
	p.rebuildFilms(context.Background())
	if n := rec.count("/vods/search/VOD"); n != 1 {
		t.Errorf("%d searches after a second rebuild, want the first one only", n)
	}
	if again := filmReleases(p); !slices.Equal(again, got) {
		t.Errorf("feed after a second rebuild = %s; want %s", describeFilms(again), describeFilms(got))
	}
}

func TestFilmFeedWithoutWatchedFilms(t *testing.T) {
	p := newProvider(t)
	rec := &pathRecorder{next: p.client.Transport}
	p.client = &http.Client{Transport: rec}
	p.rebuildFilms(context.Background())
	if len(rec.paths) != 0 {
		t.Errorf("requests %v, want none", rec.paths)
	}
	if got := filmReleases(p); len(got) != 0 {
		t.Errorf("feed = %s, want none", describeFilms(got))
	}
}

func TestFilmFeedKeepsFilmsWhenListingFails(t *testing.T) {
	var failing atomic.Bool
	p, _ := filmProvider(t, func(key string) (string, bool) {
		return "500", failing.Load() && key == newestProductsKey
	})
	p.rebuildFilms(context.Background())
	before := filmReleases(p)
	if len(before) == 0 {
		t.Fatal("empty feed")
	}
	failing.Store(true)
	p.rebuildFilms(context.Background())
	if after := filmReleases(p); !slices.Equal(after, before) {
		t.Errorf("feed after a failure = %s, want %s", describeFilms(after), describeFilms(before))
	}
}

func TestFilmFeedRetriesFailedLookups(t *testing.T) {
	var failing atomic.Bool
	failing.Store(true)
	p, _ := filmProvider(t, func(key string) (string, bool) {
		return "500", failing.Load() && key == pachnidloSearchKey
	})
	p.rebuildFilms(context.Background())
	if got, want := describeFilms(filmReleases(p)), "1 Kler 2018"; got != want {
		t.Errorf("feed = %s; want %s", got, want)
	}
	failing.Store(false)
	p.rebuildFilms(context.Background())
	if got, want := describeFilms(filmReleases(p)), "1 Kler 2018, 2 Perfume: The Story of a Murderer 2006"; got != want {
		t.Errorf("feed after the lookup works = %s; want %s", got, want)
	}
}

// Cancelled lookups must not count as attempts.
func TestFilmFeedLooksUpUntriedFilmsFirst(t *testing.T) {
	const wonnaKey = "/vods/search/VOD?keyword=Wonna"
	var mu sync.Mutex
	var searched []string
	var cancel context.CancelFunc // ends the rebuild when Wonna is looked up
	p, _ := filmProvider(t, func(key string) (string, bool) {
		switch key {
		case newestProductsKey:
			return strings.TrimSuffix(fmt.Sprintf(newestProductsFixture, false), "]}") +
				`,{"type":"VOD","id":7,"title":"Wonna","year":2006}]}`, true
		case pachnidloSearchKey, wonnaKey:
			mu.Lock()
			defer mu.Unlock()
			searched = append(searched, key)
			if key == pachnidloSearchKey {
				return "500", true
			}
			if cancel != nil {
				cancel()
				cancel = nil
			}
			return `{"items":[]}`, true // not listed yet
		}
		return "", false
	})
	lookups := func(ctx context.Context, cancelOnWonna context.CancelFunc) []string {
		t.Helper()
		mu.Lock()
		searched, cancel = nil, cancelOnWonna
		mu.Unlock()
		p.cache = newResponseCache() // so every lookup reaches TVP
		p.rebuildFilms(ctx)
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(searched)
	}

	ctx, c := context.WithCancel(context.Background())
	defer c()
	if got := lookups(ctx, c); !slices.Equal(got, []string{pachnidloSearchKey, wonnaKey}) {
		t.Fatalf("first rebuild looked up %v, want Pachnidło, then Wonna", got)
	}
	if _, ok := p.lookupTried[2]; !ok {
		t.Error("failed lookup not counted as tried")
	}
	if _, ok := p.lookupTried[7]; ok {
		t.Error("lookup cut short by the rebuild's end counted as tried")
	}
	if got := lookups(context.Background(), nil); !slices.Equal(got, []string{wonnaKey, pachnidloSearchKey}) {
		t.Errorf("second rebuild looked up %v, want Wonna first", got)
	}
	if !p.lookupTried[7].Before(p.lookupTried[2]) {
		t.Error("lookup that didn't find the film not counted as tried")
	}
}

func TestFilmFeedDatesFilmsWhenFirstSeen(t *testing.T) {
	var free atomic.Bool
	p, _ := filmProvider(t, func(key string) (string, bool) {
		return fmt.Sprintf(newestProductsFixture, !free.Load()), key == newestProductsKey
	})
	p.rebuildFilms(context.Background())
	first := filmReleases(p)
	if want := "1 Kler 2018"; describeFilms(first) != want {
		t.Fatalf("feed = %s; want %s", describeFilms(first), want)
	}
	free.Store(true)
	p.rebuildFilms(context.Background())
	second := filmReleases(p)
	if want := "1 Kler 2018, 2 Perfume: The Story of a Murderer 2006"; describeFilms(second) != want {
		t.Fatalf("feed = %s; want %s", describeFilms(second), want)
	}
	if !second[0].Published.Equal(first[0].Published) {
		t.Errorf("Kler published %v, then %v", first[0].Published, second[0].Published)
	}
	if !second[1].Published.After(first[0].Published) {
		t.Errorf("newly free film published %v, not after %v", second[1].Published, first[0].Published)
	}
}

type pathRecorder struct {
	next  http.RoundTripper
	mu    sync.Mutex
	paths []string
}

func (r *pathRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	r.paths = append(r.paths, req.URL.Path)
	r.mu.Unlock()
	return r.next.RoundTrip(req)
}

func TestFeedRebuildGoesOnWhereItStopped(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	p := newProviderWith(t, func(key string) (string, bool) {
		if key == "/vods/search/SERIAL?keyword=Rancho" {
			cancel() // out of time while on The Ranch
		}
		return "", false
	})
	fakeTitles(t, p)
	p.watch(2, "Twins")
	p.watch(5, "The Ranch")
	p.watch(6, "Nowhere")
	p.rebuildSeries(ctx)

	fakeTitles(t, p) // forgets the titles, so the next rebuild looks each series up
	rec := &pathRecorder{next: http.DefaultTransport}
	p.titles.client = &http.Client{Transport: rec}
	p.rebuildSeries(context.Background())
	if len(rec.paths) == 0 || rec.paths[0] != "/shows/6" {
		t.Errorf("rebuild looked up %v, want Nowhere (6) first", rec.paths)
	}
}
