package tvp

import (
	"context"
	"maps"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/combor/vodarr/internal/provider"
)

// requestCounter counts requests by path.
type requestCounter struct {
	next http.RoundTripper
	mu   sync.Mutex
	n    map[string]int
}

func (c *requestCounter) RoundTrip(req *http.Request) (*http.Response, error) {
	c.mu.Lock()
	c.n[req.URL.Path]++
	c.mu.Unlock()
	return c.next.RoundTrip(req)
}

func (c *requestCounter) counts() map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return maps.Clone(c.n)
}

func countRequests(p *Provider) *requestCounter {
	c := &requestCounter{next: p.client.Transport, n: make(map[string]int)}
	p.client = &http.Client{Transport: c}
	return c
}

// A season search's per-episode fallback asks for the same lists again;
// TVP answers each once. Stream URLs are never reused.
func TestResponseCache(t *testing.T) {
	p := newProvider(t)
	requests := countRequests(p)
	now := time.Now()
	p.cache.now = func() time.Time { return now }
	ctx := context.Background()
	search := func() {
		t.Helper()
		for _, q := range []provider.Query{
			{Kind: provider.Episode, Title: "Ranczo", Season: 2},
			{Kind: provider.Episode, Title: "Ranczo", Season: 2, Episode: 1},
			{Kind: provider.Episode, Title: "Ranczo", Season: 2, Episode: 2},
			{Kind: provider.Movie, Title: "Hydrozagadka"},
		} {
			if items, err := p.Search(ctx, q); err != nil || len(items) == 0 {
				t.Fatalf("%+v: items = %+v, err = %v", q, items, err)
			}
		}
		if _, err := p.Resolve(ctx, "296079"); err != nil {
			t.Fatal(err)
		}
	}
	want := func(lists, playlists int) map[string]int {
		return map[string]int{
			"/vods/search/SERIAL":                          lists,
			"/vods/search/VOD":                             lists,
			"/vods/serials/316445/seasons":                 lists,
			"/vods/serials/316445/seasons/381045/episodes": lists,
			"/vods/serials/316445/seasons/381149/episodes": lists,
			"/296079/videos/playlist":                      playlists,
		}
	}

	search()
	search()
	if got := requests.counts(); !maps.Equal(got, want(1, 2)) {
		t.Errorf("requests = %v, want %v", got, want(1, 2))
	}
	now = now.Add(apiCacheTTL)
	search()
	if got := requests.counts(); !maps.Equal(got, want(2, 3)) {
		t.Errorf("after expiry, requests = %v, want %v", got, want(2, 3))
	}
}

func TestResponseCacheSkipsErrors(t *testing.T) {
	p := newProviderWith(t, func(key string) (string, bool) {
		return "500", key == "/vods/search/SERIAL?keyword=Awaria"
	})
	requests := countRequests(p)
	for range 2 {
		if _, err := p.Search(context.Background(), provider.Query{Kind: provider.Episode, Title: "Awaria", Season: 1}); err == nil {
			t.Fatal("no error")
		}
	}
	if n := requests.counts()["/vods/search/SERIAL"]; n != 2 {
		t.Errorf("%d requests, want 2", n)
	}
}
