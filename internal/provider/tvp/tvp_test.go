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
	// "Luka": S1 repeats a number, S2 has a gap, S3's start is unknown.
	"/vods/search/SERIAL?keyword=Luka":       `{"items":[{"type":"SERIAL","id":500,"title":"Luka"}]}`,
	"/vods/serials/500/seasons":              `[{"id":501,"number":1},{"id":502,"number":2},{"id":503,"number":3}]`,
	"/vods/serials/500/seasons/501/episodes": `[{"id":5011,"number":1},{"id":5012,"number":2},{"id":5099,"number":2},{"id":5013,"number":3}]`,
	"/vods/serials/500/seasons/502/episodes": `[{"id":5024,"number":4},{"id":5026,"number":6}]`,
	"/vods/serials/500/seasons/503/episodes": `[{"id":5038,"number":8},{"id":5039,"number":9}]`,
	// "Absolutny": numbers 1-14 in seasons 1-4, 6 and 7; 11 is listed twice
	// in season 4, 13 in seasons 6 and 7.
	"/vods/search/SERIAL?keyword=Absolutny":  `{"items":[{"type":"SERIAL","id":700,"title":"Absolutny"}]}`,
	"/vods/serials/700/seasons":              `[{"id":701,"number":1},{"id":702,"number":2},{"id":703,"number":3},{"id":704,"number":4},{"id":706,"number":6},{"id":707,"number":7}]`,
	"/vods/serials/700/seasons/701/episodes": `[{"id":7001,"number":1},{"id":7002,"number":2},{"id":7003,"number":3}]`,
	"/vods/serials/700/seasons/702/episodes": `[{"id":7004,"number":4},{"id":7005,"number":5},{"id":7006,"number":6}]`,
	"/vods/serials/700/seasons/703/episodes": `[{"id":7007,"number":7},{"id":7008,"number":8},{"id":7009,"number":9}]`,
	"/vods/serials/700/seasons/704/episodes": `[{"id":7010,"number":10},{"id":7011,"number":11},{"id":7099,"number":11}]`,
	"/vods/serials/700/seasons/706/episodes": `[{"id":7012,"number":12},{"id":7013,"number":13}]`,
	"/vods/serials/700/seasons/707/episodes": `[{"id":7113,"number":13},{"id":7114,"number":14}]`,
	// "Restart": numbering restarts in S2; S3 lacks its first episode.
	"/vods/search/SERIAL?keyword=Restart":    `{"items":[{"type":"SERIAL","id":800,"title":"Restart"}]}`,
	"/vods/serials/800/seasons":              `[{"id":801,"number":1},{"id":802,"number":2},{"id":803,"number":3}]`,
	"/vods/serials/800/seasons/801/episodes": `[{"id":8001,"number":1},{"id":8002,"number":2}]`,
	"/vods/serials/800/seasons/802/episodes": `[{"id":8011,"number":1},{"id":8012,"number":2},{"id":8013,"number":3}]`,
	"/vods/serials/800/seasons/803/episodes": `[{"id":8022,"number":2},{"id":8023,"number":3},{"id":8024,"number":4}]`,
	// "Sezony": per-season numbering, then a season starting at 4.
	"/vods/search/SERIAL?keyword=Sezony":     `{"items":[{"type":"SERIAL","id":900,"title":"Sezony"}]}`,
	"/vods/serials/900/seasons":              `[{"id":901,"number":1},{"id":902,"number":2},{"id":903,"number":3}]`,
	"/vods/serials/900/seasons/901/episodes": `[{"id":9001,"number":1},{"id":9002,"number":2},{"id":9003,"number":3}]`,
	"/vods/serials/900/seasons/902/episodes": `[{"id":9011,"number":1},{"id":9012,"number":2},{"id":9013,"number":3}]`,
	"/vods/serials/900/seasons/903/episodes": `[{"id":9024,"number":4},{"id":9025,"number":5}]`,
	// "Pusty": S2 is empty, S3 restarts, S4 starts at 4.
	"/vods/search/SERIAL?keyword=Pusty":        `{"items":[{"type":"SERIAL","id":1000,"title":"Pusty"}]}`,
	"/vods/serials/1000/seasons":               `[{"id":1001,"number":1},{"id":1002,"number":2},{"id":1003,"number":3},{"id":1004,"number":4}]`,
	"/vods/serials/1000/seasons/1001/episodes": `[{"id":10001,"number":1},{"id":10002,"number":2},{"id":10003,"number":3}]`,
	"/vods/serials/1000/seasons/1002/episodes": `[]`,
	"/vods/serials/1000/seasons/1003/episodes": `[{"id":10021,"number":1},{"id":10022,"number":2},{"id":10023,"number":3}]`,
	"/vods/serials/1000/seasons/1004/episodes": `[{"id":10034,"number":4},{"id":10035,"number":5},{"id":10036,"number":6}]`,
	// "Odcinki": every season restarts at 1.
	"/vods/search/SERIAL?keyword=Odcinki":    `{"items":[{"type":"SERIAL","id":600,"title":"Odcinki"}]}`,
	"/vods/serials/600/seasons":              `[{"id":601,"number":1},{"id":602,"number":2}]`,
	"/vods/serials/600/seasons/601/episodes": `[{"id":6011,"number":1}]`,
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

// Gaps, duplicates and unknown starts never shift episode numbers.
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
		{"Luka", 2, 1, "5024"}, // starts at 4, after season 1's 3
		{"Luka", 2, 2, ""},     // 5 is missing
		{"Luka", 2, 3, "5026"},
		{"Luka", 3, 1, ""}, // starts at 8, season 2 ended at 6
		{"Odcinki", 2, 2, "6022"},
		{"Odcinki", 1, 2, ""}, // numbering restarts: S2E2 is not S1E2
		// Absolute numbers (TVDB's Klan S15E2113).
		{"Absolutny", 2, 8, "7008"},  // above season 2's highest (6)
		{"Absolutny", 2, 5, ""},      // could be within season 2
		{"Absolutny", 5, 12, "7012"}, // no season 5: above season 4's highest
		{"Absolutny", 5, 13, ""},     // in seasons 6 and 7
		{"Absolutny", 6, 14, ""},     // season 7 overlaps season 6: unclear
		{"Restart", 1, 4, ""},        // restarts in S2: never reaches S3
		{"Sezony", 2, 4, ""},         // S2 restarts after S1: not absolute
		{"Pusty", 3, 5, ""},          // S3 restarts after S1; empty S2 in between
		{"Absolutny", 5, 11, ""},     // not above season 4's highest
		{"Absolutny", 1, 11, ""},     // listed twice
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
		{Kind: provider.Episode, Title: "The Mire", Season: 1}, // English title
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
	// A rate limit stays retryable.
	if _, err := p.Resolve(context.Background(), "429"); err == nil || errors.Is(err, provider.ErrUnavailable) {
		t.Errorf("Resolve(429) err = %v, want a retryable error", err)
	}
	if _, err := p.Resolve(context.Background(), "../etc"); err == nil {
		t.Error("non-numeric id accepted")
	}
}
