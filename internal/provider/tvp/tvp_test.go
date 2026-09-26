package tvp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/combor/vodarr/internal/provider"
)

// Fixtures trimmed from real TVP responses (2026-09-26).
var fixtures = map[string]string{
	"/vods/search/SERIAL?keyword=Ranczo": `{"items":[
		{"type":"SERIAL","id":310247,"title":"UA Ranczo","year":2006,"payable":false},
		{"type":"SERIAL","id":316445,"title":"Ranczo","year":2006,"payable":false}]}`,
	"/vods/serials/316445/seasons": `[
		{"id":381045,"title":"Sezon 1","type":"SEASON","number":1},
		{"id":381149,"title":"Sezon 2","type":"SEASON","number":2}]`,
	"/vods/serials/316445/seasons/381045/episodes": `[
		{"type":"EPISODE","id":381046,"title":"odc. 1 – Spadek","number":1},
		{"type":"EPISODE","id":381054,"title":"odc. 13","number":13}]`,
	// Season 2 continues season 1's numbering, out of order, with a special.
	"/vods/serials/316445/seasons/381149/episodes": `[
		{"type":"EPISODE","id":381138,"title":"odc. 15 – Gmina to ja","number":15,"duration":3100,"since":"2007-07-09T00:02:00+02:00"},
		{"type":"EPISODE","id":381150,"title":"odc. 14 – Sztuka i władza","number":14,"duration":3189,"since":"2007-07-02T00:02:00+02:00"},
		{"type":"EPISODE","id":999,"title":"zapowiedź","number":-1,"duration":60},
		{"type":"EPISODE","id":381151,"title":"odc. 16 – Lokalna rewolucja","number":16,"duration":3000,"payable":true}]`,
	// "Luka": S1 has a duplicate number, S2 a gap, S3 doesn't start where
	// S2 ends (its first episode may be missing).
	"/vods/search/SERIAL?keyword=Luka":       `{"items":[{"type":"SERIAL","id":500,"title":"Luka"}]}`,
	"/vods/serials/500/seasons":              `[{"id":501,"number":1},{"id":502,"number":2},{"id":503,"number":3}]`,
	"/vods/serials/500/seasons/501/episodes": `[{"id":5011,"number":1},{"id":5012,"number":2},{"id":5099,"number":2},{"id":5013,"number":3}]`,
	"/vods/serials/500/seasons/502/episodes": `[{"id":5024,"number":4},{"id":5026,"number":6}]`,
	"/vods/serials/500/seasons/503/episodes": `[{"id":5038,"number":8},{"id":5039,"number":9}]`,
	// "Odcinki": every season restarts at 1.
	"/vods/search/SERIAL?keyword=Odcinki":    `{"items":[{"type":"SERIAL","id":600,"title":"Odcinki"}]}`,
	"/vods/serials/600/seasons":              `[{"id":601,"number":1},{"id":602,"number":2}]`,
	"/vods/serials/600/seasons/602/episodes": `[{"id":6021,"number":1},{"id":6022,"number":2}]`,
	"/vods/search/VOD?keyword=Hydrozagadka": `{"items":[
		{"type":"VOD","id":296079,"title":"Hydrozagadka","year":1970,"duration":4235,"payable":false,"since":"2020-09-23T11:20:00+02:00"},
		{"type":"VOD","id":350232,"title":"Złote runo","year":1996,"duration":5000,"payable":false}]}`,
	"/vods/search/VOD?keyword=Vanskabte+land": `{"items":[
		{"type":"VOD","id":1506041,"title":"Godland","originalTitle":"Vanskabte land","year":2022,"duration":8212}]}`,
	"/296079/videos/playlist?videoType=MOVIE": `{"sources":{
		"DASH":[{"src":"https://cdn.example/video.mpd"}],
		"HLS":[{"src":"https://cdn.example/video-fmp4.m3u8"}]}}`,
	"/2612854/videos/playlist?videoType=MOVIE": `{"sources":{"HLS":[{"src":"https://cdn.example/drm.m3u8"}]},
		"drm":{"WIDEVINE":{"src":"https://vod.tvp.pl/api/products/2612854/drm/widevine/external"}}}`,
}

func newProvider(t *testing.T) *Provider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("lang") != "pl" || q.Get("platform") != "BROWSER" {
			t.Errorf("missing lang/platform: %s", r.URL)
		}
		q.Del("lang")
		q.Del("platform")
		key := r.URL.Path
		if len(q) > 0 {
			key += "?" + q.Encode()
		}
		if key == "/429/videos/playlist?videoType=MOVIE" {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		if key == "/1747321/videos/playlist?videoType=MOVIE" {
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `{"code":"GEOIP_FILTER_FAILED"}`)
			return
		}
		body, ok := fixtures[key]
		if !ok {
			io.WriteString(w, `{"items":[]}`)
			return
		}
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	p := New(srv.Client(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	p.baseURL = srv.URL
	return p
}

func TestEpisodeByPositionInSeason(t *testing.T) {
	p := newProvider(t)
	items, err := p.Search(context.Background(), provider.Query{Kind: provider.Episode, Title: "Ranczo", Season: 2, Episode: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %+v", items)
	}
	it := items[0]
	if it.ID != "381150" || it.Season != 2 || it.Episode != 1 || it.Duration != 3189*time.Second || it.Published.Year() != 2007 {
		t.Errorf("item = %+v", it)
	}
}

func TestWholeSeasonSkipsPaidAndSpecials(t *testing.T) {
	p := newProvider(t)
	items, err := p.Search(context.Background(), provider.Query{Kind: provider.Episode, Title: "Ranczo", Season: 2})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, it := range items {
		got = append(got, fmt.Sprintf("%s@E%d", it.ID, it.Episode))
	}
	// 381151 (E03) is paid; the -1 special is not numbered.
	if strings.Join(got, ",") != "381150@E1,381138@E2" {
		t.Errorf("items = %v", got)
	}
}

// Gaps, duplicates and unknown season starts must never shift or guess
// episode numbers.
func TestEpisodeNumbering(t *testing.T) {
	p := newProvider(t)
	tests := []struct {
		title           string
		season, episode int
		want            string // item ID, "" for none
	}{
		{"Luka", 1, 1, "5011"},
		{"Luka", 1, 2, ""}, // two episodes numbered 2
		{"Luka", 1, 3, "5013"},
		{"Luka", 2, 1, "5024"}, // season 2 starts at 4, right after season 1's 3
		{"Luka", 2, 2, ""},     // number 5 is missing: not number 6
		{"Luka", 2, 3, "5026"},
		{"Luka", 3, 1, ""}, // starts at 8, season 2 ended at 6: unknown start
		{"Odcinki", 2, 2, "6022"},
	}
	for _, tt := range tests {
		items, err := p.Search(context.Background(), provider.Query{Kind: provider.Episode, Title: tt.title, Season: tt.season, Episode: tt.episode})
		if err != nil {
			t.Fatal(err)
		}
		got := ""
		if len(items) == 1 {
			got = items[0].ID
		} else if len(items) > 1 {
			got = fmt.Sprintf("%d items", len(items))
		}
		if got != tt.want {
			t.Errorf("%s S%02dE%02d = %q, want %q", tt.title, tt.season, tt.episode, got, tt.want)
		}
	}
}

func TestEpisodeNoMatch(t *testing.T) {
	p := newProvider(t)
	for _, q := range []provider.Query{
		{Kind: provider.Episode, Title: "Ranczo", Season: 9},   // no such season
		{Kind: provider.Episode, Title: "The Mire", Season: 1}, // English title: TVP doesn't know it
		{Kind: provider.Episode, Title: "Ranczo", Season: 2, Episode: 7},
	} {
		items, err := p.Search(context.Background(), q)
		if err != nil || len(items) != 0 {
			t.Errorf("%+v: items = %+v, err = %v", q, items, err)
		}
	}
}

func TestMovieYearTolerance(t *testing.T) {
	p := newProvider(t)
	items, err := p.Search(context.Background(), provider.Query{Kind: provider.Movie, Title: "Hydrozagadka", Year: 1971})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ID != "296079" || items[0].Year != 1970 {
		t.Fatalf("items = %+v", items)
	}
	items, _ = p.Search(context.Background(), provider.Query{Kind: provider.Movie, Title: "Hydrozagadka", Year: 1975})
	if len(items) != 0 {
		t.Errorf("year 1975 matched: %+v", items)
	}
}

func TestMovieMatchesOriginalTitle(t *testing.T) {
	p := newProvider(t)
	items, err := p.Search(context.Background(), provider.Query{Kind: provider.Movie, Title: "Vanskabte land", Year: 2022})
	if err != nil || len(items) != 1 || items[0].ID != "1506041" {
		t.Fatalf("items = %+v, err = %v", items, err)
	}
}

func TestResolve(t *testing.T) {
	p := newProvider(t)
	s, err := p.Resolve(context.Background(), "296079")
	if err != nil {
		t.Fatal(err)
	}
	if s.URL != "https://cdn.example/video-fmp4.m3u8" || s.Header.Get("User-Agent") == "" {
		t.Errorf("stream = %+v", s)
	}

	for id, want := range map[string]string{
		"2612854": "DRM-protected",
		"1747321": "GEOIP_FILTER_FAILED",
	} {
		_, err := p.Resolve(context.Background(), id)
		if !errors.Is(err, provider.ErrUnavailable) || !strings.Contains(err.Error(), want) {
			t.Errorf("Resolve(%s) err = %v, want ErrUnavailable containing %q", id, err, want)
		}
	}
	// A rate limit is temporary: it must stay retryable.
	if _, err := p.Resolve(context.Background(), "429"); err == nil || errors.Is(err, provider.ErrUnavailable) {
		t.Errorf("Resolve(429) err = %v, want a retryable error", err)
	}
	if _, err := p.Resolve(context.Background(), "../etc"); err == nil {
		t.Error("non-numeric id accepted")
	}
}
