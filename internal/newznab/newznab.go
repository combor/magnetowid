// Package newznab exposes each provider as a Newznab indexer at
// /{provider}/api so Sonarr and Radarr can search it.
package newznab

import (
	"encoding/xml"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/combor/vodarr/internal/nzb"
	"github.com/combor/vodarr/internal/provider"
)

// bytesPerSecond estimates release size (4 Mbit/s); sites don't publish sizes.
const bytesPerSecond = 4_000_000 / 8

// maxResults is the page size advertised in caps.
const maxResults = 100

const (
	catMovies   = 2000
	catMoviesHD = 2040
	catTV       = 5000
	catTVHD     = 5040
)

// Handler serves the Newznab API for every registered provider. Mount it at
// "/{provider}/api".
type Handler struct {
	Providers *provider.Registry
	APIKey    string
	Log       *slog.Logger
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p, ok := h.Providers.Get(r.PathValue("provider"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	t := q.Get("t")
	// caps is public, as on most Newznab indexers; everything else needs the key.
	if t != "caps" && q.Get("apikey") != h.APIKey {
		writeError(w, 100, "Incorrect user credentials")
		return
	}
	switch t {
	case "caps":
		writeXML(w, caps(p.Name()))
	case "search", "tvsearch", "movie":
		h.search(w, r, p, t)
	case "get":
		h.get(w, r, p)
	default:
		writeError(w, 202, "No such function")
	}
}

func (h *Handler) search(w http.ResponseWriter, r *http.Request, p provider.Provider, t string) {
	q := r.URL.Query()
	movie := t == "movie" || (t == "search" && hasMovieCategory(q.Get("cat")))
	text := strings.TrimSpace(q.Get("q"))
	if text == "" {
		// RSS sync and the indexer test: the test fails on an empty feed, and
		// real items would risk wrong grabs, so return one unparseable item.
		writeXML(w, h.feed(p, []item{h.placeholder(r, p, movie)}))
		return
	}

	var query provider.Query
	if movie {
		title, year := splitYear(text)
		query = provider.Query{Kind: provider.Movie, Title: title, Year: year}
	} else {
		season, err := strconv.Atoi(q.Get("season"))
		if err != nil || season <= 0 {
			writeXML(w, h.feed(p, nil)) // episode search needs a season
			return
		}
		episode := 0
		if ep := q.Get("ep"); ep != "" {
			if episode, err = strconv.Atoi(ep); err != nil || episode <= 0 {
				writeXML(w, h.feed(p, nil)) // e.g. daily "MM/dd": not supported
				return
			}
		}
		query = provider.Query{Kind: provider.Episode, Title: text, Season: season, Episode: episode}
	}

	found, err := p.Search(r.Context(), query)
	if err != nil {
		h.Log.Error("search failed", "provider", p.Name(), "kind", query.Kind, "title", query.Title, "err", err)
		writeError(w, 900, "Search failed: "+err.Error())
		return
	}
	h.Log.Info("search", "provider", p.Name(), "kind", query.Kind, "title", query.Title,
		"year", query.Year, "season", query.Season, "episode", query.Episode, "results", len(found))

	// Page like a real indexer: Sonarr/Radarr ask for the next offset
	// whenever a page comes back full.
	found = page(found, q.Get("offset"), q.Get("limit"))
	items := make([]item, 0, len(found))
	for _, it := range found {
		items = append(items, h.release(r, p, query, it))
	}
	writeXML(w, h.feed(p, items))
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request, p provider.Provider) {
	q := r.URL.Query()
	id := q.Get("id")
	if id == "" {
		writeError(w, 200, "Missing parameter (id)")
		return
	}
	duration, _ := strconv.Atoi(q.Get("dur"))
	body, err := nzb.Encode(nzb.Ref{Provider: p.Name(), ID: id, Duration: duration})
	if err != nil {
		writeError(w, 900, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/x-nzb")
	w.Write(body)
}

// release turns a provider item into a Newznab item. The title reuses the
// *arr's own query title so the release parses back to the same series or
// movie; automatic imports are blocked for releases matched only by ID.
func (h *Handler) release(r *http.Request, p provider.Provider, q provider.Query, it provider.Item) item {
	secs := int(it.Duration / time.Second)
	link := h.nzbLink(r, p, it.ID, secs)
	size := int64(secs) * bytesPerSecond
	cat := catTVHD
	if it.Kind == provider.Movie {
		cat = catMoviesHD
	}
	return item{
		Title:     ReleaseTitle(p.Name(), q, it),
		GUID:      guid{IsPermaLink: false, Value: p.Name() + ":" + it.ID},
		Link:      link,
		PubDate:   pubDate(it.Published),
		Category:  categoryName(cat),
		Enclosure: enclosure{URL: link, Length: size, Type: "application/x-nzb"},
		Attrs: []attr{
			{Name: "category", Value: strconv.Itoa(cat)},
			{Name: "size", Value: strconv.FormatInt(size, 10)},
		},
	}
}

func (h *Handler) placeholder(r *http.Request, p provider.Provider, movie bool) item {
	cat := catTVHD
	if movie {
		cat = catMoviesHD
	}
	link := h.baseURL(r, p) + "?" + url.Values{"t": {"get"}, "apikey": {h.APIKey}}.Encode()
	return item{
		Title:     "vodarr " + p.Name() + " feed placeholder",
		GUID:      guid{IsPermaLink: false, Value: p.Name() + ":placeholder"},
		Link:      link,
		PubDate:   pubDate(time.Time{}),
		Category:  categoryName(cat),
		Enclosure: enclosure{URL: link, Length: 1, Type: "application/x-nzb"},
		Attrs: []attr{
			{Name: "category", Value: strconv.Itoa(cat)},
			{Name: "size", Value: "1"},
		},
	}
}

// ReleaseTitle builds a scene-style release name from the *arr's query title.
// Movies use the year from the query when present: Radarr needs the exact
// year and sites often differ by one.
func ReleaseTitle(providerName string, q provider.Query, it provider.Item) string {
	title := strings.Join(strings.Fields(q.Title), ".")
	group := strings.ToUpper(providerName)
	if it.Kind == provider.Movie {
		year := q.Year
		if year == 0 {
			year = it.Year
		}
		if year > 0 {
			title += "." + strconv.Itoa(year)
		}
		return title + ".1080p.WEB-DL.AAC.H.264-" + group
	}
	return fmt.Sprintf("%s.S%02dE%02d.1080p.WEB-DL.AAC.H.264-%s", title, it.Season, it.Episode, group)
}

func (h *Handler) nzbLink(r *http.Request, p provider.Provider, id string, secs int) string {
	v := url.Values{"t": {"get"}, "id": {id}, "apikey": {h.APIKey}}
	if secs > 0 {
		v.Set("dur", strconv.Itoa(secs))
	}
	return h.baseURL(r, p) + "?" + v.Encode()
}

// baseURL is the URL Sonarr/Radarr reached this indexer at, so NZB links
// work behind a reverse proxy that terminates TLS (X-Forwarded-Proto/Host).
// The headers only shape links in the caller's own response.
func (h *Handler) baseURL(r *http.Request, p provider.Provider) string {
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(firstValue(r.Header.Get("X-Forwarded-Proto")), "https") {
		scheme = "https"
	}
	host := r.Host
	if fh := firstValue(r.Header.Get("X-Forwarded-Host")); fh != "" {
		host = fh
	}
	return scheme + "://" + host + "/" + p.Name() + "/api"
}

// firstValue returns the first of a comma-separated header value, as set by
// a chain of proxies.
func firstValue(v string) string {
	first, _, _ := strings.Cut(v, ",")
	return strings.TrimSpace(first)
}

// page applies Newznab offset/limit; limit defaults to and is capped at the
// advertised maxResults.
func page(items []provider.Item, offset, limit string) []provider.Item {
	off, _ := strconv.Atoi(offset)
	lim, err := strconv.Atoi(limit)
	if err != nil || lim <= 0 || lim > maxResults {
		lim = maxResults
	}
	if off < 0 || off >= len(items) {
		return nil
	}
	return items[off:min(off+lim, len(items))]
}

var trailingYear = regexp.MustCompile(`^(.*\S)\s+\(?((?:19|20)\d\d)\)?$`)

// splitYear splits Radarr's "Title 1971" query into title and year.
func splitYear(s string) (string, int) {
	m := trailingYear.FindStringSubmatch(s)
	if m == nil {
		return s, 0
	}
	year, _ := strconv.Atoi(m[2])
	return m[1], year
}

func hasMovieCategory(cats string) bool {
	for _, c := range strings.Split(cats, ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(c)); err == nil && n >= 2000 && n < 3000 {
			return true
		}
	}
	return false
}

func categoryName(cat int) string {
	if cat/1000 == catMovies/1000 {
		return "Movies > HD"
	}
	return "TV > HD"
}

func pubDate(t time.Time) string {
	if t.IsZero() {
		t = time.Now()
	}
	return t.UTC().Format(time.RFC1123Z)
}

// --- XML documents ---

type capsDoc struct {
	XMLName    xml.Name      `xml:"caps"`
	Server     capsServer    `xml:"server"`
	Limits     capsLimits    `xml:"limits"`
	Searching  capsSearching `xml:"searching"`
	Categories []capsCat     `xml:"categories>category"`
}

type capsServer struct {
	Title string `xml:"title,attr"`
}

type capsLimits struct {
	Max     int `xml:"max,attr"`
	Default int `xml:"default,attr"`
}

type capsSearching struct {
	Search      capsSearch `xml:"search"`
	TVSearch    capsSearch `xml:"tv-search"`
	MovieSearch capsSearch `xml:"movie-search"`
}

// SearchEngine "raw" makes Sonarr/Radarr send titles as-is instead of cleaned
// ("The Killing" would otherwise arrive as "Killing").
type capsSearch struct {
	Available       string `xml:"available,attr"`
	SupportedParams string `xml:"supportedParams,attr"`
	SearchEngine    string `xml:"searchEngine,attr"`
}

type capsCat struct {
	ID      int       `xml:"id,attr"`
	Name    string    `xml:"name,attr"`
	Subcats []capsCat `xml:"subcat"`
}

func caps(providerName string) capsDoc {
	return capsDoc{
		Server: capsServer{Title: "vodarr " + providerName},
		Limits: capsLimits{Max: maxResults, Default: maxResults},
		Searching: capsSearching{
			Search:      capsSearch{Available: "yes", SupportedParams: "q", SearchEngine: "raw"},
			TVSearch:    capsSearch{Available: "yes", SupportedParams: "q,season,ep", SearchEngine: "raw"},
			MovieSearch: capsSearch{Available: "yes", SupportedParams: "q", SearchEngine: "raw"},
		},
		Categories: []capsCat{
			{ID: catMovies, Name: "Movies", Subcats: []capsCat{{ID: catMoviesHD, Name: "HD"}}},
			{ID: catTV, Name: "TV", Subcats: []capsCat{{ID: catTVHD, Name: "HD"}}},
		},
	}
}

type rss struct {
	XMLName   xml.Name `xml:"rss"`
	Version   string   `xml:"version,attr"`
	NewznabNS string   `xml:"xmlns:newznab,attr"`
	Channel   channel  `xml:"channel"`
}

type channel struct {
	Title string `xml:"title"`
	Items []item `xml:"item"`
}

type item struct {
	Title     string    `xml:"title"`
	GUID      guid      `xml:"guid"`
	Link      string    `xml:"link"`
	PubDate   string    `xml:"pubDate"`
	Category  string    `xml:"category"`
	Enclosure enclosure `xml:"enclosure"`
	Attrs     []attr    `xml:"newznab:attr"`
}

type guid struct {
	IsPermaLink bool   `xml:"isPermaLink,attr"`
	Value       string `xml:",chardata"`
}

type enclosure struct {
	URL    string `xml:"url,attr"`
	Length int64  `xml:"length,attr"`
	Type   string `xml:"type,attr"`
}

type attr struct {
	Name  string `xml:"name,attr"`
	Value string `xml:"value,attr"`
}

func (h *Handler) feed(p provider.Provider, items []item) rss {
	return rss{
		Version:   "2.0",
		NewznabNS: "http://www.newznab.com/DTD/2010/feeds/attributes/",
		Channel:   channel{Title: "vodarr " + p.Name(), Items: items},
	}
}

type errorDoc struct {
	XMLName     xml.Name `xml:"error"`
	Code        int      `xml:"code,attr"`
	Description string   `xml:"description,attr"`
}

func writeError(w http.ResponseWriter, code int, description string) {
	writeXML(w, errorDoc{Code: code, Description: description})
}

func writeXML(w http.ResponseWriter, v any) {
	body, err := xml.Marshal(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Write([]byte(xml.Header))
	w.Write(body)
}
