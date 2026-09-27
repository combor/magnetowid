package newznab

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/combor/vodarr/internal/nzb"
	"github.com/combor/vodarr/internal/probe"
	"github.com/combor/vodarr/internal/provider"
)

// streams serves the master playlists fake items resolve to.
var streams *httptest.Server

func TestMain(m *testing.M) {
	streams = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/1080.m3u8":
			io.WriteString(w, `#EXTM3U
#EXT-X-STREAM-INF:BANDWIDTH=3598429,RESOLUTION=1280x720,CODECS="avc1.64001f,mp4a.40.2"
720.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=5118260,AVERAGE-BANDWIDTH=4800000,RESOLUTION=1920x1080,CODECS="avc1.640029,mp4a.40.2"
1080.m3u8
`)
		case "/720.m3u8":
			io.WriteString(w, `#EXTM3U
#EXT-X-STREAM-INF:BANDWIDTH=3598429,RESOLUTION=1280x720,CODECS="avc1.64001f,mp4a.40.2"
720.m3u8
`)
		case "/stall.m3u8":
			<-r.Context().Done()
		default:
			http.Error(w, "boom", http.StatusInternalServerError)
		}
	}))
	code := m.Run()
	streams.Close()
	os.Exit(code)
}

type fakeProvider struct {
	got      provider.Query
	items    []provider.Item
	delay    time.Duration // before Search returns
	resolves atomic.Int32
}

func (f *fakeProvider) Name() string { return "fake" }

func (f *fakeProvider) Search(_ context.Context, q provider.Query) ([]provider.Item, error) {
	f.got = q
	time.Sleep(f.delay)
	return f.items, nil
}

// Resolve gives a 1080p stream, or by ID prefix: "720" a 720p one, "drm"
// none, "down" one whose playlist can't be fetched, and "stall" one whose
// playlist never comes.
func (f *fakeProvider) Resolve(_ context.Context, id string) (provider.Stream, error) {
	f.resolves.Add(1)
	switch {
	case strings.HasPrefix(id, "stall"):
		return provider.Stream{URL: streams.URL + "/stall.m3u8"}, nil
	case strings.HasPrefix(id, "720"):
		return provider.Stream{URL: streams.URL + "/720.m3u8"}, nil
	case strings.HasPrefix(id, "drm"):
		return provider.Stream{}, fmt.Errorf("%w: DRM-protected", provider.ErrUnavailable)
	case strings.HasPrefix(id, "down"):
		return provider.Stream{URL: streams.URL + "/down.m3u8"}, nil
	}
	return provider.Stream{URL: streams.URL + "/1080.m3u8"}, nil
}

// fakeTVDBProvider also searches by TVDB ID, returning title and its items.
type fakeTVDBProvider struct {
	fakeProvider
	gotTVDB int
	title   string
	err     error
}

func (f *fakeTVDBProvider) SearchTVDB(_ context.Context, tvdbID int, q provider.Query) (string, []provider.Item, error) {
	f.gotTVDB, f.got = tvdbID, q
	return f.title, f.items, f.err
}

func newServer(t *testing.T, p provider.Provider) *httptest.Server {
	t.Helper()
	return newServerWith(t, p, nil)
}

// newServerWith is newServer with the handler changed by set, if not nil.
func newServerWith(t *testing.T, p provider.Provider, set func(*Handler)) *httptest.Server {
	t.Helper()
	h := &Handler{
		Providers: provider.NewRegistry(p),
		APIKey:    "secret",
		Probe:     &probe.Prober{Client: streams.Client()},
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if set != nil {
		set(h)
	}
	mux := http.NewServeMux()
	mux.Handle("/{provider}/api", h)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func get(t *testing.T, srv *httptest.Server, path string, params url.Values) string {
	t.Helper()
	resp, err := http.Get(srv.URL + path + "?" + params.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return string(body)
}

// feedItem holds the fields Sonarr/Radarr read.
type feedItem struct {
	Title     string `xml:"title"`
	GUID      string `xml:"guid"`
	Link      string `xml:"link"`
	PubDate   string `xml:"pubDate"`
	Enclosure struct {
		URL    string `xml:"url,attr"`
		Length int64  `xml:"length,attr"`
	} `xml:"enclosure"`
	Attrs []struct {
		Name  string `xml:"name,attr"`
		Value string `xml:"value,attr"`
	} `xml:"http://www.newznab.com/DTD/2010/feeds/attributes/ attr"`
}

func parseFeed(t *testing.T, body string) []feedItem {
	t.Helper()
	var doc struct {
		Items []feedItem `xml:"channel>item"`
	}
	if err := xml.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("bad feed: %v\n%s", err, body)
	}
	return doc.Items
}

func attrValue(it feedItem, name string) string {
	for _, a := range it.Attrs {
		if a.Name == name {
			return a.Value
		}
	}
	return ""
}

func TestCaps(t *testing.T) {
	srv := newServer(t, &fakeProvider{})
	body := get(t, srv, "/fake/api", url.Values{"t": {"caps"}})
	for _, want := range []string{
		`<tv-search available="yes" supportedParams="q,season,ep" searchEngine="raw">`,
		`<search available="yes" supportedParams="q" searchEngine="raw">`,
		`<movie-search available="yes" supportedParams="q" searchEngine="raw">`,
		`<category id="5000" name="TV"><subcat id="5040" name="HD">`,
		`<category id="2000" name="Movies"><subcat id="2040" name="HD">`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("caps missing %s\n%s", want, body)
		}
	}

	body = get(t, newServer(t, &fakeTVDBProvider{}), "/fake/api", url.Values{"t": {"caps"}})
	if want := `<tv-search available="yes" supportedParams="q,season,ep,tvdbid" searchEngine="raw">`; !strings.Contains(body, want) {
		t.Errorf("caps missing %s\n%s", want, body)
	}
}

func TestAPIKeyAndUnknownProvider(t *testing.T) {
	srv := newServer(t, &fakeProvider{})
	body := get(t, srv, "/fake/api", url.Values{"t": {"tvsearch"}, "apikey": {"wrong"}})
	if !strings.Contains(body, `<error code="100"`) {
		t.Errorf("bad key: %s", body)
	}
	resp, err := http.Get(srv.URL + "/nope/api?t=caps")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown provider status = %d", resp.StatusCode)
	}
	body = get(t, srv, "/fake/api", url.Values{"t": {"bogus"}, "apikey": {"secret"}})
	if !strings.Contains(body, `<error code="202"`) {
		t.Errorf("unknown function: %s", body)
	}
}

func TestPlaceholderWithoutQuery(t *testing.T) {
	srv := newServer(t, &fakeProvider{})
	for _, params := range []url.Values{
		{"t": {"tvsearch"}, "cat": {"5000,5040"}},
		{"t": {"movie"}, "cat": {"2000,2040"}},
		{"t": {"search"}},
	} {
		params.Set("apikey", "secret")
		items := parseFeed(t, get(t, srv, "/fake/api", params))
		if len(items) != 1 || items[0].Title != "vodarr fake feed placeholder" {
			t.Errorf("%v: items = %+v", params, items)
		}
	}
}

func TestEpisodeSearch(t *testing.T) {
	fp := &fakeProvider{items: []provider.Item{{
		ID: "381150", Kind: provider.Episode, Season: 2, Episode: 1,
		Duration: 50 * time.Minute, Published: time.Date(2021, 1, 4, 16, 0, 0, 0, time.UTC),
	}}}
	srv := newServer(t, fp)
	items := parseFeed(t, get(t, srv, "/fake/api", url.Values{
		"t": {"tvsearch"}, "q": {"Ranczo"}, "season": {"2"}, "ep": {"1"}, "apikey": {"secret"},
	}))
	want := provider.Query{Kind: provider.Episode, Title: "Ranczo", Season: 2, Episode: 1}
	if fp.got != want {
		t.Fatalf("query = %+v, want %+v", fp.got, want)
	}
	if len(items) != 1 {
		t.Fatalf("items = %+v", items)
	}
	it := items[0]
	if it.Title != "Ranczo.S02E01.1080p.WEB-DL.AAC.H.264-FAKE" {
		t.Errorf("title = %q", it.Title)
	}
	if it.GUID != "fake:381150" {
		t.Errorf("guid = %q", it.GUID)
	}
	// The size is the duration at the stream's average bandwidth, 4.8 Mbit/s.
	if attrValue(it, "category") != "5040" || attrValue(it, "size") != "1800000000" || it.Enclosure.Length != 1800000000 {
		t.Errorf("attrs = %+v, enclosure = %+v", it.Attrs, it.Enclosure)
	}
	if _, err := time.Parse(time.RFC1123Z, it.PubDate); err != nil {
		t.Errorf("pubDate %q: %v", it.PubDate, err)
	}
	link, err := url.Parse(it.Link)
	if err != nil || link.Path != "/fake/api" || link.Query().Get("t") != "get" ||
		link.Query().Get("id") != "381150" || link.Query().Get("dur") != "3000" || link.Query().Get("apikey") != "secret" {
		t.Errorf("link = %q", it.Link)
	}
}

func TestEpisodeSearchNeedsSeason(t *testing.T) {
	fp := &fakeProvider{items: []provider.Item{{ID: "1", Kind: provider.Episode}}}
	srv := newServer(t, fp)
	for _, params := range []url.Values{
		{"t": {"tvsearch"}, "q": {"Ranczo"}},
		{"t": {"tvsearch"}, "q": {"Ranczo"}, "season": {"2024"}, "ep": {"09/26"}},
	} {
		params.Set("apikey", "secret")
		if items := parseFeed(t, get(t, srv, "/fake/api", params)); len(items) != 0 {
			t.Errorf("%v: items = %+v", params, items)
		}
	}
}

// Radarr searches with t=search and movie categories; the release keeps
// Radarr's year, not the site's.
func TestMovieSearchViaSearch(t *testing.T) {
	fp := &fakeProvider{items: []provider.Item{{ID: "296079", Kind: provider.Movie, Year: 1970}}}
	srv := newServer(t, fp)
	items := parseFeed(t, get(t, srv, "/fake/api", url.Values{
		"t": {"search"}, "q": {"Hydrozagadka 1971"}, "cat": {"2000,2040"}, "apikey": {"secret"},
	}))
	want := provider.Query{Kind: provider.Movie, Title: "Hydrozagadka", Year: 1971}
	if fp.got != want {
		t.Fatalf("query = %+v, want %+v", fp.got, want)
	}
	if len(items) != 1 || items[0].Title != "Hydrozagadka.1971.1080p.WEB-DL.AAC.H.264-FAKE" {
		t.Fatalf("items = %+v", items)
	}
	if attrValue(items[0], "category") != "2040" {
		t.Errorf("category = %q", attrValue(items[0], "category"))
	}
}

func TestGetReturnsNZB(t *testing.T) {
	srv := newServer(t, &fakeProvider{})
	resp, err := http.Get(srv.URL + "/fake/api?" + url.Values{
		"t": {"get"}, "id": {"296079"}, "dur": {"4520"}, "apikey": {"secret"},
	}.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "application/x-nzb" {
		t.Errorf("content type = %q", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	ref, err := nzb.Decode(body)
	if err != nil {
		t.Fatal(err)
	}
	if ref != (nzb.Ref{Provider: "fake", ID: "296079", Duration: 4520}) {
		t.Errorf("ref = %+v", ref)
	}
}

func TestReleaseTitle(t *testing.T) {
	hd := probe.Info{Width: 1920, Height: 1080, Codecs: "avc1.640029,mp4a.40.2"}
	tests := []struct {
		q    provider.Query
		it   provider.Item
		info probe.Info
		want string
	}{
		{provider.Query{Title: "The Killing"}, provider.Item{Kind: provider.Episode, Season: 1, Episode: 12}, hd,
			"The.Killing.S01E12.1080p.WEB-DL.AAC.H.264-TVP"},
		{provider.Query{Title: "Godland"}, provider.Item{Kind: provider.Movie, Year: 2022}, hd,
			"Godland.2022.1080p.WEB-DL.AAC.H.264-TVP"},
		{provider.Query{Title: "Hydrozagadka", Year: 1971}, provider.Item{Kind: provider.Movie, Year: 1970}, hd,
			"Hydrozagadka.1971.1080p.WEB-DL.AAC.H.264-TVP"},
		{provider.Query{Title: "Ranczo"}, provider.Item{Kind: provider.Episode, Season: 2, Episode: 1},
			probe.Info{Width: 1280, Height: 720, Codecs: "hvc1.1.6.L93.B0,ec-3"},
			"Ranczo.S02E01.720p.WEB-DL.DDP.H.265-TVP"},
		// Codecs the playlist doesn't name are left out.
		{provider.Query{Title: "Ranczo"}, provider.Item{Kind: provider.Episode, Season: 2, Episode: 1},
			probe.Info{Width: 1024, Height: 576},
			"Ranczo.S02E01.576p.WEB-DL-TVP"},
	}
	for _, tt := range tests {
		if got := ReleaseTitle("tvp", tt.q, tt.it, tt.info); got != tt.want {
			t.Errorf("ReleaseTitle(%+v, %+v, %+v) = %q, want %q", tt.q, tt.it, tt.info, got, tt.want)
		}
	}
}

// Releases are named with their stream's quality. Items that can't be
// downloaded, or whose quality can't be read, are left out.
func TestSearchNamesStreamQuality(t *testing.T) {
	fp := &fakeProvider{items: []provider.Item{
		{ID: "1", Kind: provider.Episode, Season: 1, Episode: 1},
		{ID: "720-2", Kind: provider.Episode, Season: 1, Episode: 2},
		{ID: "drm-3", Kind: provider.Episode, Season: 1, Episode: 3},
		{ID: "down-4", Kind: provider.Episode, Season: 1, Episode: 4},
		{ID: "5", Kind: provider.Episode, Season: 1, Episode: 5},
	}}
	items := parseFeed(t, get(t, newServer(t, fp), "/fake/api", url.Values{
		"t": {"tvsearch"}, "q": {"Ranczo"}, "season": {"1"}, "apikey": {"secret"},
	}))
	var titles []string
	for _, it := range items {
		titles = append(titles, it.Title)
	}
	want := []string{
		"Ranczo.S01E01.1080p.WEB-DL.AAC.H.264-FAKE",
		"Ranczo.S01E02.720p.WEB-DL.AAC.H.264-FAKE",
		"Ranczo.S01E05.1080p.WEB-DL.AAC.H.264-FAKE",
	}
	if strings.Join(titles, "\n") != strings.Join(want, "\n") {
		t.Errorf("titles:\n%s\nwant:\n%s", strings.Join(titles, "\n"), strings.Join(want, "\n"))
	}
}

// A stalled stream is left out once probing runs out of time, so the search
// still answers before Sonarr gives up on it. The time the provider's search
// took counts too.
func TestStalledStreamIsLeftOut(t *testing.T) {
	for name, tt := range map[string]struct {
		searchDelay, budget, answer, max time.Duration
	}{
		"probing budget": {0, 200 * time.Millisecond, time.Hour, 5 * time.Second},
		// Due 1.2 s after the request came, not after probing began (2.2 s).
		"answer after slow search": {time.Second, time.Hour, 1200 * time.Millisecond, 2 * time.Second},
	} {
		fp := &fakeProvider{delay: tt.searchDelay, items: []provider.Item{
			{ID: "1", Kind: provider.Episode, Season: 1, Episode: 1},
			{ID: "stall-2", Kind: provider.Episode, Season: 1, Episode: 2},
			{ID: "3", Kind: provider.Episode, Season: 1, Episode: 3},
		}}
		srv := newServerWith(t, fp, func(h *Handler) { h.probeBudget, h.answerWithin = tt.budget, tt.answer })
		start := time.Now()
		items := parseFeed(t, get(t, srv, "/fake/api", url.Values{
			"t": {"tvsearch"}, "q": {"Ranczo"}, "season": {"1"}, "apikey": {"secret"},
		}))
		if elapsed := time.Since(start); elapsed > tt.max {
			t.Errorf("%s: search took %v, want at most %v", name, elapsed, tt.max)
		}
		if len(items) != 2 || items[0].Title != "Ranczo.S01E01.1080p.WEB-DL.AAC.H.264-FAKE" ||
			items[1].Title != "Ranczo.S01E03.1080p.WEB-DL.AAC.H.264-FAKE" {
			t.Errorf("%s: items = %+v", name, items)
		}
	}
}

// A stalled stream times out in time for another item to fill its place, so
// the page stays full and Sonarr asks for the next one.
func TestStalledStreamIsReplaced(t *testing.T) {
	fp := &fakeProvider{}
	for i := 1; i <= 105; i++ {
		id := strconv.Itoa(i)
		if i == 50 {
			id = "stall-" + id
		}
		fp.items = append(fp.items, provider.Item{ID: id, Kind: provider.Episode, Season: 1, Episode: i})
	}
	srv := newServerWith(t, fp, func(h *Handler) {
		h.Probe.Timeout = 200 * time.Millisecond
		h.probeBudget = 10 * time.Second
	})
	start := time.Now()
	items := parseFeed(t, get(t, srv, "/fake/api", url.Values{
		"t": {"tvsearch"}, "q": {"Klan"}, "season": {"1"}, "offset": {"0"}, "limit": {"100"}, "apikey": {"secret"},
	}))
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("search took %v", elapsed)
	}
	if len(items) != 100 || items[99].GUID != "fake:101" {
		t.Errorf("%d items, want 100 ending with fake:101", len(items))
	}
}

// An offset past the provider's results gives an empty page without
// probing anything.
func TestOffsetPastResults(t *testing.T) {
	fp := &fakeProvider{items: []provider.Item{{ID: "1", Kind: provider.Episode, Season: 1, Episode: 1}}}
	items := parseFeed(t, get(t, newServer(t, fp), "/fake/api", url.Values{
		"t": {"tvsearch"}, "q": {"Klan"}, "season": {"1"}, "offset": {"100"}, "apikey": {"secret"},
	}))
	if len(items) != 0 || fp.resolves.Load() != 0 {
		t.Errorf("%d items after %d resolves, want none", len(items), fp.resolves.Load())
	}
}

// Items are left out before paging, so a full page stays full and Sonarr
// asks for the next one. Only as many are probed as the page needs.
func TestPagingAfterLeavingOut(t *testing.T) {
	fp := &fakeProvider{}
	for i := 1; i <= 150; i++ {
		id := strconv.Itoa(i)
		if i%15 == 0 {
			id = "drm-" + id
		}
		fp.items = append(fp.items, provider.Item{ID: id, Kind: provider.Episode, Season: 1, Episode: i})
	}
	srv := newServer(t, fp)
	for _, tt := range []struct {
		offset          string
		items, resolves int
		first           string
	}{
		// 100 readable ones are the first 107: 15, 30, … 105 are DRM.
		{"0", 100, 107, "fake:1"},
		// The first 107 are cached; the other 43 hold 40 readable ones.
		{"100", 40, 150, "fake:108"},
	} {
		items := parseFeed(t, get(t, srv, "/fake/api", url.Values{
			"t": {"tvsearch"}, "q": {"Klan"}, "season": {"1"}, "offset": {tt.offset}, "limit": {"100"}, "apikey": {"secret"},
		}))
		var first string
		if len(items) > 0 {
			first = items[0].GUID
		}
		if len(items) != tt.items || first != tt.first {
			t.Errorf("offset %s: %d items from %q, want %d from %q", tt.offset, len(items), first, tt.items, tt.first)
		}
		if n := int(fp.resolves.Load()); n != tt.resolves {
			t.Errorf("offset %s: %d resolves in all, want %d", tt.offset, n, tt.resolves)
		}
	}
}

func TestSplitYear(t *testing.T) {
	tests := []struct {
		in    string
		title string
		year  int
	}{
		{"Hydrozagadka 1971", "Hydrozagadka", 1971},
		{"Blade Runner 2049 2017", "Blade Runner 2049", 2017},
		{"Godland (2022)", "Godland", 2022},
		{"Ranczo", "Ranczo", 0},
		{"1917", "1917", 0},
	}
	for _, tt := range tests {
		title, year := splitYear(tt.in)
		if title != tt.title || year != tt.year {
			t.Errorf("splitYear(%q) = %q, %d; want %q, %d", tt.in, title, year, tt.title, tt.year)
		}
	}
}

// Sonarr asks for the next offset whenever a page is full.
func TestSearchPaging(t *testing.T) {
	fp := &fakeProvider{}
	for i := 1; i <= 150; i++ {
		fp.items = append(fp.items, provider.Item{ID: strconv.Itoa(i), Kind: provider.Episode, Season: 1, Episode: i})
	}
	srv := newServer(t, fp)
	for _, tt := range []struct {
		offset, limit string
		want          int
		first         string
	}{
		{"", "", 100, "fake:1"},
		{"0", "100", 100, "fake:1"},
		{"100", "100", 50, "fake:101"},
		{"200", "100", 0, ""},
		{"10", "5", 5, "fake:11"},
		{"0", "1000", 100, "fake:1"},
	} {
		items := parseFeed(t, get(t, srv, "/fake/api", url.Values{
			"t": {"tvsearch"}, "q": {"Klan"}, "season": {"1"}, "offset": {tt.offset}, "limit": {tt.limit}, "apikey": {"secret"},
		}))
		if len(items) != tt.want || (tt.want > 0 && items[0].GUID != tt.first) {
			t.Errorf("offset=%s limit=%s: %d items, first %+v", tt.offset, tt.limit, len(items), items)
		}
	}
}

// Behind a TLS-terminating reverse proxy, links must use the external URL.
func TestLinksBehindProxy(t *testing.T) {
	fp := &fakeProvider{items: []provider.Item{{ID: "1", Kind: provider.Episode, Season: 1, Episode: 1}}}
	srv := newServer(t, fp)
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/fake/api?"+url.Values{
		"t": {"tvsearch"}, "q": {"Ranczo"}, "season": {"1"}, "ep": {"1"}, "apikey": {"secret"},
	}.Encode(), nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "vodarr.example.com, internal:8484")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	items := parseFeed(t, string(body))
	if len(items) != 1 || !strings.HasPrefix(items[0].Link, "https://vodarr.example.com/fake/api?") {
		t.Fatalf("items = %+v", items)
	}
}

// Sonarr's first search for a series has only its TVDB ID. Releases are
// named with the title the provider returns.
func TestSearchByTVDBID(t *testing.T) {
	fp := &fakeTVDBProvider{title: "Days of Honor"}
	fp.items = []provider.Item{
		{ID: "720-1", Kind: provider.Episode, Season: 1, Episode: 2},
		{ID: "drm-2", Kind: provider.Episode, Season: 1, Episode: 2},
	}
	srv := newServer(t, fp)
	items := parseFeed(t, get(t, srv, "/fake/api", url.Values{
		"t": {"tvsearch"}, "tvdbid": {"83920"}, "season": {"1"}, "ep": {"2"}, "cat": {"5000,5040"}, "apikey": {"secret"},
	}))
	if len(items) != 1 || items[0].Title != "Days.of.Honor.S01E02.720p.WEB-DL.AAC.H.264-FAKE" {
		t.Fatalf("items = %+v", items)
	}
	want := provider.Query{Kind: provider.Episode, Season: 1, Episode: 2}
	if fp.gotTVDB != 83920 || fp.got != want {
		t.Errorf("SearchTVDB(%d, %+v), want (83920, %+v)", fp.gotTVDB, fp.got, want)
	}
}

// When a search by TVDB ID finds nothing, Sonarr searches by title. So it
// must never return the placeholder, which would count as a result.
func TestSearchByTVDBIDFindsNothing(t *testing.T) {
	item := []provider.Item{{ID: "1", Kind: provider.Episode, Season: 1, Episode: 1}}
	for name, p := range map[string]provider.Provider{
		"provider can't":   &fakeProvider{items: item},
		"nothing found":    &fakeTVDBProvider{title: "Ranczo"},
		"no title to name": &fakeTVDBProvider{fakeProvider: fakeProvider{items: item}},
	} {
		items := parseFeed(t, get(t, newServer(t, p), "/fake/api", url.Values{
			"t": {"tvsearch"}, "tvdbid": {"81970"}, "season": {"1"}, "ep": {"1"}, "apikey": {"secret"},
		}))
		if len(items) != 0 {
			t.Errorf("%s: items = %+v", name, items)
		}
	}
}

func TestSearchByTVDBIDError(t *testing.T) {
	fp := &fakeTVDBProvider{title: "Days of Honor", err: errors.New("tvp: HTTP 500")}
	body := get(t, newServer(t, fp), "/fake/api", url.Values{
		"t": {"tvsearch"}, "tvdbid": {"83920"}, "season": {"1"}, "apikey": {"secret"},
	})
	if !strings.Contains(body, `<error code="900" description="Search failed: tvp: HTTP 500"`) {
		t.Errorf("body = %s", body)
	}
}
