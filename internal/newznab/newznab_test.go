package newznab

import (
	"context"
	"encoding/xml"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/combor/vodarr/internal/nzb"
	"github.com/combor/vodarr/internal/provider"
)

type fakeProvider struct {
	got   provider.Query
	items []provider.Item
}

func (f *fakeProvider) Name() string { return "fake" }

func (f *fakeProvider) Search(_ context.Context, q provider.Query) ([]provider.Item, error) {
	f.got = q
	return f.items, nil
}

func (f *fakeProvider) Resolve(context.Context, string) (provider.Stream, error) {
	return provider.Stream{}, nil
}

func newServer(t *testing.T, p provider.Provider) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle("/{provider}/api", &Handler{
		Providers: provider.NewRegistry(p),
		APIKey:    "secret",
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
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
	if attrValue(it, "category") != "5040" || attrValue(it, "size") != "1500000000" || it.Enclosure.Length != 1500000000 {
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
	tests := []struct {
		q    provider.Query
		it   provider.Item
		want string
	}{
		{provider.Query{Title: "The Killing"}, provider.Item{Kind: provider.Episode, Season: 1, Episode: 12},
			"The.Killing.S01E12.1080p.WEB-DL.AAC.H.264-TVP"},
		{provider.Query{Title: "Godland"}, provider.Item{Kind: provider.Movie, Year: 2022},
			"Godland.2022.1080p.WEB-DL.AAC.H.264-TVP"},
		{provider.Query{Title: "Hydrozagadka", Year: 1971}, provider.Item{Kind: provider.Movie, Year: 1970},
			"Hydrozagadka.1971.1080p.WEB-DL.AAC.H.264-TVP"},
	}
	for _, tt := range tests {
		if got := ReleaseTitle("tvp", tt.q, tt.it); got != tt.want {
			t.Errorf("ReleaseTitle(%+v, %+v) = %q, want %q", tt.q, tt.it, got, tt.want)
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
