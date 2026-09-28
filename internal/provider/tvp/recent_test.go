package tvp

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/combor/vodarr/internal/provider"
)

// feedReleases returns p's feed by episode ID, without starting a rebuild.
func feedReleases(p *Provider) []provider.Release {
	p.feed.mu.Lock()
	defer p.feed.mu.Unlock()
	var out []provider.Release
	for _, rs := range p.feed.series {
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

// The feed has the watched series' episodes that aired within the window
// and are free, named with Sonarr's title.
func TestFeedRebuild(t *testing.T) {
	p := newProvider(t)
	fakeTitles(t, p)
	p.watch(5, "The Ranch")
	p.watch(6, "Nowhere") // not on TVP
	p.rebuildFeed(context.Background())
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
	p.rebuildFeed(context.Background())
	before := feedReleases(p)
	if len(before) == 0 {
		t.Fatal("empty feed")
	}

	failing.Store(true)
	p.cache.now = func() time.Time { return time.Now().Add(apiCacheTTL) }
	p.rebuildFeed(context.Background())
	if after := feedReleases(p); !slices.Equal(after, before) {
		t.Errorf("feed after a failure = %+v, want %+v", after, before)
	}
}

// An episode that turns free is dated by the rebuild that found it, not by
// TVP, which dates premieres from when they were listed as paid. It is then
// newer than anything else in the feed, so it comes first in RSS sync.
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
	p.rebuildFeed(context.Background())
	first := feedReleases(p)
	if want := "381054 The Ranch S01E13, 381150 The Ranch S02E01"; describe(first) != want {
		t.Fatalf("feed = %s; want %s", describe(first), want)
	}

	free.Store(true)
	p.cache.now = func() time.Time { return time.Now().Add(apiCacheTTL) }
	p.rebuildFeed(context.Background())
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

// waitIdle waits for p's feed rebuild, if one is running, to finish.
func waitIdle(t *testing.T, p *Provider) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		p.feed.mu.Lock()
		building := p.feed.building
		p.feed.mu.Unlock()
		if !building {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("feed rebuild didn't finish")
		}
	}
}

// RSS sync never waits for TVP: Recent returns the last feed and rebuilds
// it in the background, one rebuild at a time.
func TestRecentRebuildsInBackground(t *testing.T) {
	p := newProvider(t)
	fakeTitles(t, p)
	p.feed.series = map[int][]provider.Release{1: {{Title: "Old", Item: provider.Item{ID: "1"}}}}
	var rebuilds atomic.Int32
	unblock := make(chan struct{})
	p.feed.rebuild = func(ctx context.Context) {
		rebuilds.Add(1)
		<-unblock
		p.rebuildFeed(ctx)
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
		waitIdle(t, p)
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

	p.feed.mu.Lock()
	p.feed.built = time.Now().Add(-feedTTL)
	p.feed.mu.Unlock()
	recent()
	check("feedTTL later", 3)

	if rs, err := p.Recent(context.Background(), provider.Movie); rs != nil || err != nil {
		t.Errorf("movies = %+v, %v", rs, err)
	}
	check("movies", 3)
}

// pathRecorder records the paths of requests.
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

// A rebuild that runs out of time goes on next time with the series it
// didn't reach, so the same ones aren't left out every time.
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
	p.rebuildFeed(ctx)

	fakeTitles(t, p) // forgets the titles, so the next rebuild looks each series up
	rec := &pathRecorder{next: http.DefaultTransport}
	p.titles.client = &http.Client{Transport: rec}
	p.rebuildFeed(context.Background())
	if len(rec.paths) == 0 || rec.paths[0] != "/shows/6" {
		t.Errorf("rebuild looked up %v, want Nowhere (6) first", rec.paths)
	}
}
