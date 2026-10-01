package bbc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/combor/magnetowid/internal/hls"
	"github.com/combor/magnetowid/internal/provider"
)

// Fixtures trimmed from real iPlayer, media selector and Skyhook responses
// (2026-09-29). {srv} is the fake server's URL; {recent} and {old} are
// availability dates 2 and 30 days ago.
var fixtures = map[string]string{
	"/ibl/v1/new-search?q=Doctor Who": `{"new_search":{"results":[
		{"id":"p0gglvqn","type":"programme","title":"Doctor Who","tleo_type":"brand","count":4},
		{"id":"m002hrtl","type":"programme","title":"Doctor Who: Unleashed","tleo_type":"brand","count":10}]}}`,
	"/ibl/v1/programmes/p0gglvqn/episodes": `{"programme_episodes":{"count":4,"elements":[
		{"id":"m001sx3h","type":"episode","title":"Doctor Who","subtitle":"Specials: 1. The Star Beast","original_title":"The Star Beast",
		 "tleo_type":"brand","parent_position":1,"release_date_time":"2023-11-25T00:00:00.000Z",
		 "versions":[{"id":"m001sx3f","kind":"original","duration":{"value":"PT57M8.480S"},
		   "availability":{"start":"2024-12-09T19:00:00Z"},"first_broadcast_date_time":"2023-11-25T18:30:00.000Z"}]},
		{"id":"m001z8bz","type":"episode","title":"Doctor Who","subtitle":"Season 1: 1. Space Babies","original_title":"Space Babies",
		 "tleo_type":"brand","parent_position":1,"release_date_time":"2024-05-11T00:00:00.000Z",
		 "versions":[{"id":"m001z8bw","kind":"original","duration":{"value":"PT46M"},"availability":{"start":"{old}"}}]},
		{"id":"m002d3lr","type":"episode","title":"Doctor Who","subtitle":"Season 2: 8. The Reality War","original_title":"The Reality War",
		 "tleo_type":"brand","parent_position":8,"release_date_time":"2025-05-31T00:00:00.000Z",
		 "versions":[{"id":"p0lfnh4v","kind":"audio-described","duration":{"value":"PT1H6M40.120S"},"availability":{"start":"{recent}"}},
		   {"id":"m002d3vx","kind":"original","duration":{"value":"PT1H6M40.213333S"},"availability":{"start":"{recent}"},
		    "first_broadcast_date_time":"2025-05-31T17:50:00.000Z"}]},
		{"id":"m0026d24","type":"episode","title":"Doctor Who","subtitle":"Joy to the World","tleo_type":"brand","parent_position":4,
		 "release_date_time":"2024-12-25T00:00:00.000Z","versions":[{"id":"m0026f1h","kind":"original","duration":{"value":"PT1H"}}]}]}}`,
	"/skyhook/449991": `{"title":"Doctor Who (2023)","alternativeTitles":[{"title":"Doctor Who (2024)"}],"episodes":[
		{"seasonNumber":0,"episodeNumber":1,"title":"The Star Beast","airDate":"2023-11-25"},
		{"seasonNumber":0,"episodeNumber":5,"title":"Joy to the World","airDate":"2024-12-25"},
		{"seasonNumber":1,"episodeNumber":1,"title":"Space Babies","airDate":"2024-05-11"},
		{"seasonNumber":2,"episodeNumber":8,"title":"The Reality War (2)","airDate":"2025-05-31"}]}`,
	// Only the classic series' TVDB titles.
	"/skyhook/76107": `{"title":"Doctor Who","episodes":[
		{"seasonNumber":1,"episodeNumber":1,"title":"An Unearthly Child","airDate":"1963-11-23"}]}`,
	// Sonarr drops parentheses and a leading "The" from titles it searches.
	"/skyhook-search/?term=Doctor Who":      doctorWhos,
	"/skyhook-search/?term=Doctor Who 2023": doctorWhos,
	"/skyhook-search/?term=Doctor Who 2024": doctorWhos,
	"/skyhook-search/?term=Traitors": `[{"tvdbId":358884,"title":"Traitors"},
		{"tvdbId":420543,"title":"The Traitors","alternativeTitles":[{"title":"The Traitors UK"}]}]`,
	"/skyhook-search/?term=Unknown":      `[]`,
	"/skyhook/83920":                     `{"title":"Days of Honor","episodes":[{"seasonNumber":1,"episodeNumber":1,"title":"Episode 1"}]}`,
	"/ibl/v1/new-search?q=Days of Honor": `{"new_search":{"results":[]}}`,

	"/ibl/v1/episodes/m002d3lr": `{"episodes":[{"id":"m002d3lr","type":"episode_large","title":"Doctor Who",
		"versions":[{"id":"p0lfnh4v","kind":"audio-described"},{"id":"m002d3vx","kind":"original"}]}]}`,
	"/ms/mediaset/iptv-all/vpid/m002d3vx/format/json": `{"media":[
		{"kind":"video","type":"video/mp4","width":"1920","height":"1080","bitrate":"8490","connection":[
			{"priority":"10","protocol":"https","supplier":"mf_akamai","transferFormat":"hls","href":"{srv}/akamai/master.m3u8?__gda__=1"},
			{"priority":"10","protocol":"https","supplier":"mf_akamai","transferFormat":"dash","href":"{srv}/akamai/master.mpd"},
			{"priority":"30","protocol":"https","supplier":"mf_cloudfront","transferFormat":"hls","href":"{srv}/cf/vf.ism.hlsv2.ism/iptv_hd_abr_v1_hls_master.m3u8?Expires=1"},
			{"priority":"30","protocol":"http","supplier":"mf_cloudfront","transferFormat":"hls","href":"http://insecure.example/master.m3u8"}]},
		{"kind":"captions","type":"application/ttaf+xml","connection":[
			{"priority":"10","protocol":"http","transferFormat":"plain","href":"http://insecure.example/sub.xml"}]},
		{"kind":"captions","type":"application/ttaf+xml","connection":[
			{"priority":"30","protocol":"https","transferFormat":"plain","href":"{srv}/sub/cf.xml"},
			{"priority":"10","protocol":"https","transferFormat":"plain","href":"{srv}/sub/akamai.xml"}]}]}`,
	"/cf/vf.ism.hlsv2.ism/iptv_hd_abr_v1_hls_master.m3u8": `#EXTM3U
#EXT-X-VERSION:2
#EXT-X-STREAM-INF:BANDWIDTH=1013000,CODECS="mp4a.40.2,avc1.4D401F",RESOLUTION=704x396,FRAME-RATE=25
vf.ism.hlsv2-audio_eng_1=128000-video=827000.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=5510000,CODECS="mp4a.40.2,avc1.640020",RESOLUTION=1280x720,FRAME-RATE=50
vf.ism.hlsv2-audio_eng_1=128000-video=5070000.m3u8
`,
	"/cf/vf.ism.hlsv2.ism/vf.ism.hlsv2-audio_eng_1=128000-video=12000000.m3u8": `#EXTM3U
#EXT-X-TARGETDURATION:8
#EXTINF:8, no desc
vf.ism.hlsv2-audio_eng_1=128000-video=12000000-1.ts
`,

	// A film iPlayer lacks in 1080p, and only for PCs.
	"/ibl/v1/new-search?q=Nickel Boys": `{"new_search":{"results":[
		{"id":"m00327ht","type":"episode","title":"Nickel Boys","original_title":"Nickel Boys","tleo_type":"episode",
		 "release_date":"2024","release_date_time":"2024-01-01T00:00:00.000Z",
		 "versions":[{"id":"m00327hs","kind":"original","duration":{"value":"PT2H9M59.200S"},"availability":{"start":"2026-09-29T00:16:14Z"}}]},
		{"id":"b0078nh3","type":"episode","title":"Wonder Boys","tleo_type":"episode","release_date":"2000","release_date_time":"2000-01-01T00:00:00.000Z"},
		{"id":"m00nb0ys","type":"programme","title":"Nickel Boys","tleo_type":"brand"},
		{"id":"m0remake","type":"episode","title":"Nickel Boys","tleo_type":"episode","release_date":"1990","release_date_time":"1990-01-01T00:00:00.000Z"}]}}`,
	"/ibl/v1/episodes/m00327ht":                       `{"episodes":[{"id":"m00327ht","versions":[{"id":"m00327hs","kind":"original"}]}]}`,
	"/ms/mediaset/iptv-all/vpid/m00327hs/format/json": `404 {"result":"selectionunavailable"}`,
	"/ms/mediaset/pc/vpid/m00327hs/format/json": `{"media":[{"kind":"video","height":"720","bitrate":"3000","connection":[
		{"priority":"10","protocol":"https","transferFormat":"hls","href":"{srv}/pc/master.m3u8"}]}]}`,
	"/pc/master.m3u8": "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=3000000,RESOLUTION=1280x720\nvideo=2812000.m3u8\n",

	"/ibl/v1/episodes/b00000gb":                       `{"episodes":[{"id":"b00000gb","versions":[{"id":"b00000gv","kind":"original"}]}]}`,
	"/ms/mediaset/iptv-all/vpid/b00000gv/format/json": `403 {"result":"geolocation"}`,
	"/ibl/v1/episodes/b00000dn":                       `{"episodes":[{"id":"b00000dn","versions":[{"id":"b00000dv","kind":"original"}]}]}`,
	"/ms/mediaset/iptv-all/vpid/b00000dv/format/json": `503 <html>Service Unavailable</html>`,
	"/ibl/v1/episodes/b00000jn":                       `{"episodes":[{"id":"b00000jn","versions":[{"id":"b00000jv","kind":"original"}]}]}`,
	"/ms/mediaset/iptv-all/vpid/b00000jv/format/json": `503 {"result":"internalerror"}`,
	"/ibl/v1/episodes/b00000dd":                       `{"episodes":[{"id":"b00000dd","versions":[{"id":"b0000dv2","kind":"audio-described"}]}]}`,
	"/ibl/v1/episodes/b00000zz":                       `{"episodes":[]}`,
	"/ibl/v1/episodes/b0000404":                       `404 {"error":{"details":"Not Found","http_response_code":404}}`,
}

const doctorWhos = `[
	{"tvdbId":76107,"title":"Doctor Who","alternativeTitles":[{"title":"Doctor Who (1963)"}]},
	{"tvdbId":449991,"title":"Doctor Who (2023)","alternativeTitles":[{"title":"Doctor Who (2024)"}]},
	{"tvdbId":78804,"title":"Doctor Who (2005)"}]`

func newProvider(t *testing.T) *Provider {
	t.Helper()
	return newProviderWith(t, nil)
}

// A fixture starting with a status code, e.g. "404 {...}", sets the status.
// serve overrides fixtures.
func newProviderWith(t *testing.T, serve func(key string) (string, bool)) *Provider {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.URL.Path
		for _, param := range []string{"q", "term"} {
			if v := r.URL.Query().Get(param); v != "" {
				key += "?" + param + "=" + v
			}
		}
		if r.UserAgent() != userAgent && !strings.HasPrefix(key, "/skyhook") {
			t.Errorf("%s: User-Agent %q", key, r.UserAgent())
		}
		body, ok := "", false
		if serve != nil {
			body, ok = serve(key)
		}
		if !ok {
			body, ok = fixtures[key]
		}
		if !ok {
			http.NotFound(w, r)
			return
		}
		body = strings.NewReplacer("{srv}", srv.URL,
			"{recent}", time.Now().Add(-48*time.Hour).UTC().Format(time.RFC3339),
			"{old}", time.Now().Add(-30*24*time.Hour).UTC().Format(time.RFC3339)).Replace(body)
		var status int
		if n, _ := fmt.Sscanf(body, "%d ", &status); n == 1 {
			w.WriteHeader(status)
			body = body[4:]
		}
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	p, err := New(srv.Client(), slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err != nil {
		t.Fatal(err)
	}
	p.iblURL = srv.URL + "/ibl/v1"
	p.selectorURL = srv.URL + "/ms"
	p.titles.url = srv.URL + "/skyhook"
	p.titles.searchURL = srv.URL + "/skyhook-search/"
	return p
}

func ids(items []provider.Item) []string {
	var out []string
	for _, it := range items {
		out = append(out, fmt.Sprintf("%s S%02dE%02d", it.ID, it.Season, it.Episode))
	}
	return out
}

func TestSearchTVDB(t *testing.T) {
	p := newProvider(t)
	ctx := context.Background()
	title, items, err := p.SearchTVDB(ctx, 449991, provider.Query{Kind: provider.Episode, Season: 2, Episode: 8})
	if err != nil || title != "Doctor Who (2023)" || !slices.Equal(ids(items), []string{"m002d3lr S02E08"}) {
		t.Fatalf("SearchTVDB = %q, %v, %v", title, ids(items), err)
	}
	it := items[0]
	if it.Duration != time.Hour+6*time.Minute+40213333*time.Microsecond || it.Published.IsZero() ||
		it.Title != "Doctor Who – Season 2: 8. The Reality War" {
		t.Errorf("item %+v", it)
	}
	if got := p.watchedSeries.ids(); !slices.Equal(got, []int{449991}) {
		t.Errorf("watching %v", got)
	}

	// A season search returns the season's matches.
	_, items, err = p.SearchTVDB(ctx, 449991, provider.Query{Kind: provider.Episode, Season: 1})
	if err != nil || !slices.Equal(ids(items), []string{"m001z8bz S01E01"}) {
		t.Errorf("season 1: %v, %v", ids(items), err)
	}

	// Specials match by title and air date.
	for q, want := range map[provider.Query][]string{
		{Kind: provider.Episode, Season: 0, Episode: 1}: {"m001sx3h S00E01"},
		{Kind: provider.Episode, Season: 0}:             {"m001sx3h S00E01", "m0026d24 S00E05"},
	} {
		if _, items, err = p.SearchTVDB(ctx, 449991, q); err != nil || !slices.Equal(ids(items), want) {
			t.Errorf("S%02dE%02d: %v, %v; want %v", q.Season, q.Episode, ids(items), err, want)
		}
	}

	// A daily series' search gives an air date, local to the UK.
	for date, want := range map[string][]string{
		"2023-11-25": {"m001sx3h S00E01"},
		"2025-05-31": {"m002d3lr S02E08"},
		"2025-06-01": nil,
	} {
		_, items, err = p.SearchTVDB(ctx, 449991, provider.Query{Kind: provider.Episode, AirDate: date})
		if err != nil || !slices.Equal(ids(items), want) {
			t.Errorf("aired %s: %v, %v; want %v", date, ids(items), err, want)
		}
	}

	// The classic series shares the title; nothing matches, but the programme is found.
	title, items, err = p.SearchTVDB(ctx, 76107, provider.Query{Kind: provider.Episode, Season: 1, Episode: 1})
	if err != nil || title != "Doctor Who" || len(items) != 0 {
		t.Errorf("classic: %q, %v, %v", title, ids(items), err)
	}

	// Absent from iPlayer, or unknown to Skyhook: Sonarr searches by title.
	for _, id := range []int{83920, 1} {
		if title, items, err := p.SearchTVDB(ctx, id, provider.Query{Kind: provider.Episode, Season: 1, Episode: 1}); title != "" || items != nil || err != nil {
			t.Errorf("TVDB %d: %q, %v, %v", id, title, ids(items), err)
		}
	}
}

func TestSearchTitles(t *testing.T) {
	tests := []struct {
		s    series
		want []string
	}{
		{series{title: "Doctor Who (2023)", aliases: []string{"Doctor Who (2024)"}}, []string{"Doctor Who"}},
		{series{title: "The Office (UK)"}, []string{"The Office"}},
		{series{title: "The Traitors (US)", aliases: []string{"The Traitors"}}, []string{"The Traitors (US)"}},
		{series{title: "Borgen", aliases: []string{"Borgen - Power & Glory"}}, []string{"Borgen", "Borgen - Power & Glory"}},
	}
	for _, tt := range tests {
		if got := tt.s.searchTitles(); !slices.Equal(got, tt.want) {
			t.Errorf("searchTitles(%q) = %q, want %q", tt.s.title, got, tt.want)
		}
	}
}

// Title searches find the TVDB series first and use its numbering.
func TestSearchEpisodesByTitle(t *testing.T) {
	p := newProviderWith(t, func(key string) (string, bool) {
		return "500 oops", key == "/skyhook-search/?term=Broken"
	})
	ctx := context.Background()
	tests := []struct {
		title           string
		season, episode int
		want            []string
	}{
		{"Doctor Who 2023", 2, 8, []string{"m002d3lr S02E08"}},
		{"Doctor Who 2023", 1, 0, []string{"m001z8bz S01E01"}},
		{"Doctor Who 2023", 0, 1, []string{"m001sx3h S00E01"}},
		// An alternative title, when no main title matches.
		{"Doctor Who 2024", 2, 8, []string{"m002d3lr S02E08"}},
		// Classic Doctor Who has no S01E01 on iPlayer; BBC's own is 2024's.
		{"Doctor Who", 1, 1, nil},
		// Channel 4's Traitors and BBC's The Traitors.
		{"Traitors", 1, 1, nil},
		{"Unknown", 1, 1, nil},
		{"Broken", 1, 1, nil},
	}
	for _, tt := range tests {
		items, err := p.Search(ctx, provider.Query{Kind: provider.Episode, Title: tt.title, Season: tt.season, Episode: tt.episode})
		if err != nil || !slices.Equal(ids(items), tt.want) {
			t.Errorf("%q S%02dE%02d: %v, %v; want %v", tt.title, tt.season, tt.episode, ids(items), err, tt.want)
		}
	}
	items, err := p.Search(ctx, provider.Query{Kind: provider.Episode, Title: "Doctor Who 2023", AirDate: "2025-05-31"})
	if err != nil || !slices.Equal(ids(items), []string{"m002d3lr S02E08"}) {
		t.Errorf("by air date: %v, %v", ids(items), err)
	}
	// Found series are watched, as by ID.
	if got := p.watchedSeries.ids(); !slices.Equal(got, []int{76107, 449991}) {
		t.Errorf("watching %v", got)
	}
}

func TestSearchFilms(t *testing.T) {
	p := newProvider(t)
	ctx := context.Background()
	for _, year := range []int{2024, 2025, 0} {
		items, err := p.Search(ctx, provider.Query{Kind: provider.Movie, Title: "Nickel Boys", Year: year})
		want := []string{"m00327ht"}
		if year == 0 {
			want = []string{"m00327ht", "m0remake"}
		}
		var got []string
		for _, it := range items {
			got = append(got, it.ID)
		}
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("%d: %v, %v; want %v", year, got, err, want)
		}
		if year == 2025 && (items[0].Year != 2025 || items[0].Kind != provider.Movie || items[0].Duration < 2*time.Hour) {
			t.Errorf("item %+v", items[0])
		}
	}
	if items, err := p.Search(ctx, provider.Query{Kind: provider.Movie, Title: "Nickel Boys", Year: 2022}); len(items) != 0 || err != nil {
		t.Errorf("2022: %+v, %v", items, err)
	}
	// Searches with a year are watched for RSS.
	if got := p.watchedFilms.all(); len(got) != 3 || got[0].Title != "Nickel Boys" || got[0].Year != 2024 {
		t.Errorf("watching %+v", got)
	}
}

func TestResolve(t *testing.T) {
	p := newProviderWith(t, func(key string) (string, bool) {
		if strings.HasPrefix(key, "/akamai/") {
			return "403 Forbidden", true
		}
		return "", false
	})
	ctx := context.Background()
	s, err := p.Resolve(ctx, "m002d3lr")
	if err != nil {
		t.Fatal(err)
	}
	// Akamai refused the playlist; the next CDN served it.
	if !strings.HasSuffix(s.URL, "/cf/vf.ism.hlsv2.ism/iptv_hd_abr_v1_hls_master.m3u8?Expires=1") || s.Header.Get("User-Agent") != userAgent {
		t.Errorf("stream %+v", s)
	}
	// The stream is fetched the way the API is reached, e.g. through BBC's proxy.
	if s.Transport == nil || s.Transport != p.client.Transport {
		t.Errorf("stream transport = %v, want the client's", s.Transport)
	}
	// Each CDN's copy, in priority order.
	srv := strings.TrimSuffix(s.URL, "/cf/vf.ism.hlsv2.ism/iptv_hd_abr_v1_hls_master.m3u8?Expires=1")
	if want := []provider.Subtitle{
		{URL: srv + "/sub/akamai.xml", Format: provider.TTML, Language: "eng", SDH: true},
		{URL: srv + "/sub/cf.xml", Format: provider.TTML, Language: "eng", SDH: true},
	}; !slices.Equal(s.Subtitles, want) {
		t.Errorf("subtitles %+v, want %+v", s.Subtitles, want)
	}
	m, ok, err := hls.Load(ctx, http.DefaultClient, s)
	if err != nil || !ok {
		t.Fatalf("master: %v, %v", ok, err)
	}
	want := hls.Variant{URI: strings.TrimSuffix(s.URL, "iptv_hd_abr_v1_hls_master.m3u8?Expires=1") + "vf.ism.hlsv2-audio_eng_1=128000-video=12000000.m3u8",
		Width: 1920, Height: 1080, Bandwidth: 8490000 + 5510000 - 5070000, Codecs: "mp4a.40.2,avc1.640020"}
	if m.Video != want {
		t.Errorf("best variant %+v, want %+v", m.Video, want)
	}

	// Without the 1080p rendition, the listed variants remain.
	p = newProviderWith(t, func(key string) (string, bool) {
		if strings.HasSuffix(key, "video=12000000.m3u8") {
			return "404 <html>Not Found</html>", true
		}
		return "", false
	})
	s, err = p.Resolve(ctx, "m002d3lr")
	if err != nil {
		t.Fatal(err)
	}
	if m, _, _ := hls.Load(ctx, http.DefaultClient, s); m.Video.Height != 720 {
		t.Errorf("without 1080p: %+v", m.Video)
	}

	// An added variant would lose separate audio.
	p = newProviderWith(t, func(key string) (string, bool) {
		if strings.HasSuffix(key, "iptv_hd_abr_v1_hls_master.m3u8") {
			return "#EXTM3U\n" +
				`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aac",LANGUAGE="en",DEFAULT=YES,URI="audio_eng=128000.m3u8"` + "\n" +
				`#EXT-X-STREAM-INF:BANDWIDTH=5510000,RESOLUTION=1280x720,AUDIO="aac"` + "\nvf-video=5070000.m3u8\n", true
		}
		if strings.HasSuffix(key, "/vf-video=12000000.m3u8") {
			return "#EXTM3U\n#EXTINF:8,\nvf-video=12000000-1.ts\n", true
		}
		return "", false
	})
	s, err = p.Resolve(ctx, "m002d3lr")
	if err != nil {
		t.Fatal(err)
	}
	if m, _, _ := hls.Load(ctx, http.DefaultClient, s); m.Video.Height != 720 || m.Audio == "" {
		t.Errorf("separate audio: %+v", m)
	}
}

func TestResolveFallsBackToPCs(t *testing.T) {
	p := newProvider(t)
	s, err := p.Resolve(context.Background(), "m00327ht")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(s.URL, "/pc/master.m3u8") || len(s.Subtitles) != 0 || strings.Contains(s.Playlist, "12000000") {
		t.Errorf("stream %+v", s)
	}
}

func TestResolveErrors(t *testing.T) {
	p := newProvider(t)
	tests := []struct {
		id          string
		unavailable bool
		msg         string
	}{
		{"b00000gb", true, "only to the UK"},
		{"b00000dn", false, "HTTP 503"},
		{"b00000jn", false, "HTTP 503"}, // a JSON reason doesn't make it permanent
		{"b00000dd", true, "no main version"},
		{"b0000404", true, "HTTP 404"},
		{"b00000zz", true, "not available"},
		{"../../x", false, "invalid id"},
	}
	for _, tt := range tests {
		_, err := p.Resolve(context.Background(), tt.id)
		if err == nil || errors.Is(err, provider.ErrUnavailable) != tt.unavailable || !strings.Contains(err.Error(), tt.msg) {
			t.Errorf("%s: %v", tt.id, err)
		}
	}
}
