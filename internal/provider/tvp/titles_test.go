package tvp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/combor/magnetowid/internal/provider"
)

// Skyhook shows, trimmed from real responses (2026-09-27) except the IDs
// below 100.
var shows = map[string]string{
	"/shows/83920":  `{"tvdbId":83920,"title":"Days of Honor","imdbId":"tt1331034","alternativeTitles":[{"title":"Days of Honour"}],"episodes":[]}`,
	"/shows/325988": `{"tvdbId":325988,"title":"War Girls","imdbId":"tt6466540"}`,
	"/shows/340175": `{"tvdbId":340175,"title":"The Crown of the Kings","imdbId":"tt7817856"}`,
	"/shows/81970":  `{"tvdbId":81970,"title":"Ranczo"}`,
	"/shows/1":      `{"tvdbId":1,"title":"Bad ID","imdbId":"tt1 OR haswbstatement:P31=Q5"}`,
	"/shows/2":      `{"tvdbId":2,"title":"Twins"}`,
	"/shows/3":      `{"tvdbId":3,"title":"Lagging"}`,
	"/shows/4":      `{"tvdbId":4,"title":""}`,
	"/shows/5":      ranchShow(time.Now()),
	"/shows/6":      `{"tvdbId":6,"title":"Nowhere"}`,
	"/shows/7":      hundredsShow(soapNow),
}

// Recent fixture episodes: S01E13 (due in 12 hours), S02E01, and paid S02E03.
func ranchShow(now time.Time) string {
	aired := func(d time.Duration) string { return now.Add(d).UTC().Format(time.RFC3339) }
	return fmt.Sprintf(`{"tvdbId":5,"title":"The Ranch","episodes":[
		{"seasonNumber":0,"episodeNumber":1,"airDateUtc":%q},
		{"seasonNumber":1,"episodeNumber":1,"airDateUtc":%q},
		{"seasonNumber":1,"episodeNumber":13,"airDateUtc":%q},
		{"seasonNumber":2,"episodeNumber":1,"airDateUtc":%q},
		{"seasonNumber":2,"episodeNumber":2,"airDateUtc":%q},
		{"seasonNumber":2,"episodeNumber":3,"airDateUtc":%q},
		{"seasonNumber":2,"episodeNumber":4}]}`,
		aired(-time.Hour), aired(3*24*time.Hour), aired(12*time.Hour),
		aired(-2*24*time.Hour), aired(-20*24*time.Hour), aired(-24*time.Hour))
}

// Polish labels of the Wikidata items with each claim; "" is an item
// without one.
var items = map[string][]string{
	"haswbstatement:P4835=83920":    {"Czas honoru"},
	"haswbstatement:P4835=325988":   {""},
	"haswbstatement:P345=tt6466540": {"never asked: the TVDB ID found an item"},
	"haswbstatement:P345=tt7817856": {"Korona królów"},
	"haswbstatement:P4835=81970":    {"Ranczo"},
	"haswbstatement:P4835=2":        {"Pierwszy", "Drugi"},
	"haswbstatement:P4835=5":        {"Rancho", "RANCHO", "Ranczo"},
	"haswbstatement:P4835=6":        {"Nieznany"},
	"haswbstatement:P4835=7":        {"Setki"},
}

func fakeTitles(t *testing.T, p *Provider) *atomic.Int32 {
	t.Helper()
	var requests atomic.Int32
	skyhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		checkBotUserAgent(t, r)
		body, ok := shows[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, body)
	}))
	wikidata := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		checkBotUserAgent(t, r)
		q := r.URL.Query()
		if q.Get("uselang") != "pl" || q.Get("generator") != "search" || q.Get("prop") != "entityterms" {
			t.Errorf("unexpected Wikidata query %s", r.URL.RawQuery)
		}
		search := q.Get("gsrsearch")
		if search == "haswbstatement:P4835=3" {
			io.WriteString(w, `{"error":{"code":"maxlag","info":"Waiting for a database server"}}`)
			return
		}
		labels, ok := items[search]
		if !ok {
			io.WriteString(w, `{"batchcomplete":true}`)
			return
		}
		type page struct {
			Terms struct {
				Label []string `json:"label"`
			} `json:"entityterms"`
		}
		var res struct {
			Query struct {
				Pages []page `json:"pages"`
			} `json:"query"`
		}
		for _, l := range labels {
			var p page
			if l != "" {
				p.Terms.Label = []string{l}
			}
			res.Query.Pages = append(res.Query.Pages, p)
		}
		json.NewEncoder(w).Encode(res)
	}))
	t.Cleanup(skyhook.Close)
	t.Cleanup(wikidata.Close)
	p.titles = newTitleLookup(http.DefaultClient)
	p.titles.skyhookURL = skyhook.URL + "/shows"
	p.titles.wikidataURL = wikidata.URL + "/w/api.php"
	return &requests
}

func checkBotUserAgent(t *testing.T, r *http.Request) {
	if ua := r.Header.Get("User-Agent"); ua != botUserAgent {
		t.Errorf("User-Agent = %q", ua)
	}
}

type keywordRecorder struct {
	next     http.RoundTripper
	mu       sync.Mutex
	keywords []string
}

func (r *keywordRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	if kw := req.URL.Query().Get("keyword"); kw != "" {
		r.mu.Lock()
		r.keywords = append(r.keywords, kw)
		r.mu.Unlock()
	}
	return r.next.RoundTrip(req)
}

func TestSearchTVDB(t *testing.T) {
	episode := provider.Query{Kind: provider.Episode, Season: 2, Episode: 1}
	tests := []struct {
		name     string
		tvdbID   int
		q        provider.Query
		title    string
		ids      []string
		searched []string
		watched  bool
	}{
		// Titles normalizing alike are searched once.
		{"Polish title", 5, episode, "The Ranch", []string{"381150"}, []string{"Rancho", "Ranczo"}, true},
		{"same as Sonarr's", 81970, episode, "Ranczo", []string{"381150"}, []string{"Ranczo"}, true},
		// Sonarr's title comes last.
		{"not on TVP", 6, episode, "Nowhere", nil, []string{"Nieznany", "Nowhere"}, false},
		{"episode not on TVP", 5, provider.Query{Kind: provider.Episode, Season: 9, Episode: 1}, "The Ranch", nil,
			[]string{"Rancho", "Ranczo", "The Ranch"}, true},
		{"unknown series", 404, episode, "", nil, nil, false},
	}
	for _, tt := range tests {
		// Use a fresh provider to avoid cached responses from previous cases.
		p := newProvider(t)
		fakeTitles(t, p)
		rec := &keywordRecorder{next: p.client.Transport}
		p.client = &http.Client{Transport: rec}
		title, found, err := p.SearchTVDB(context.Background(), tt.tvdbID, tt.q)
		if err != nil {
			t.Errorf("%s: %v", tt.name, err)
			continue
		}
		var ids []string
		for _, it := range found {
			ids = append(ids, it.ID)
		}
		if title != tt.title || !slices.Equal(ids, tt.ids) {
			t.Errorf("%s: SearchTVDB(%d) = %q, %v; want %q, %v", tt.name, tt.tvdbID, title, ids, tt.title, tt.ids)
		}
		if !slices.Equal(rec.keywords, tt.searched) {
			t.Errorf("%s: searched TVP for %q, want %q", tt.name, rec.keywords, tt.searched)
		}
		if watched := slices.Contains(p.watchedSeries.ids(), tt.tvdbID); watched != tt.watched {
			t.Errorf("%s: watched = %v, want %v", tt.name, watched, tt.watched)
		}
	}
}

func TestTitleLookup(t *testing.T) {
	p := newProvider(t)
	fakeTitles(t, p)
	tests := []struct {
		tvdbID int
		title  string
		polish []string
	}{
		{83920, "Days of Honor", []string{"Czas honoru"}},
		// Found by IMDb ID when no item has the TVDB ID.
		{340175, "The Crown of the Kings", []string{"Korona królów"}},
		// The TVDB ID's item has no Polish label, so the IMDb ID isn't tried.
		{325988, "War Girls", nil},
		// A malformed IMDb ID isn't searched for.
		{1, "Bad ID", nil},
		{2, "Twins", []string{"Pierwszy", "Drugi"}},
	}
	for _, tt := range tests {
		s, err := p.titles.series(context.Background(), tt.tvdbID)
		if err != nil {
			t.Errorf("series(%d): %v", tt.tvdbID, err)
			continue
		}
		if s.title != tt.title || !slices.Equal(s.polish, tt.polish) {
			t.Errorf("series(%d) = %q, %q; want %q, %q", tt.tvdbID, s.title, s.polish, tt.title, tt.polish)
		}
	}
}

func TestTitleLookupErrors(t *testing.T) {
	p := newProvider(t)
	fakeTitles(t, p)
	for id, want := range map[int]string{
		404: "skyhook: HTTP 404",
		3:   "wikidata: Waiting for a database server",
		4:   "skyhook: TVDB 4 has no title",
	} {
		if _, err := p.titles.series(context.Background(), id); err == nil || err.Error() != want {
			t.Errorf("series(%d) error = %v, want %q", id, err, want)
		}
	}
}

func TestTitleLookupCaches(t *testing.T) {
	p := newProvider(t)
	requests := fakeTitles(t, p)
	for range 3 {
		if s, err := p.titles.series(context.Background(), 83920); err != nil || s.title != "Days of Honor" {
			t.Fatalf("series = %+v, %v", s, err)
		}
	}
	if n := requests.Load(); n != 2 {
		t.Errorf("%d requests for three lookups, want 2", n)
	}
	// A failure is cached too, so a season's episodes don't each retry it.
	for range 3 {
		if _, err := p.titles.series(context.Background(), 404); err == nil || !strings.Contains(err.Error(), "HTTP 404") {
			t.Fatalf("err = %v", err)
		}
	}
	if n := requests.Load(); n != 3 {
		t.Errorf("%d requests after three failed lookups, want 3", n)
	}
}

func TestTitleLookupCancelledIsNotCached(t *testing.T) {
	p := newProvider(t)
	requests := fakeTitles(t, p)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.titles.series(ctx, 83920); err == nil {
		t.Fatal("cancelled lookup succeeded")
	}
	if s, err := p.titles.series(context.Background(), 83920); err != nil || s.title != "Days of Honor" {
		t.Fatalf("series = %+v, %v", s, err)
	}
	if n := requests.Load(); n != 2 {
		t.Errorf("%d requests, want 2", n)
	}
}
