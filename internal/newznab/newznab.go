// Package newznab serves each provider as a Newznab indexer.
package newznab

import (
	"cmp"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/combor/vodarr/internal/nzb"
	"github.com/combor/vodarr/internal/probe"
	"github.com/combor/vodarr/internal/provider"
)

// bytesPerSecond estimates release size (4 Mbit/s) when the stream gives no
// bandwidth.
const bytesPerSecond = 4_000_000 / 8

// maxResults is the page size advertised in caps.
const maxResults = 100

// probeWorkers bounds the streams one search probes at once.
const probeWorkers = 4

// Sonarr and Radarr give up on a request after 100 s, so probing stops after
// probeBudget or answerWithin after the request came, whichever is first.
// Streams not read by then are left out.
const (
	probeBudget  = 45 * time.Second
	answerWithin = 90 * time.Second
)

const (
	catMovies   = 2000
	catMoviesHD = 2040
	catTV       = 5000
	catTVHD     = 5040
)

// Handler serves the Newznab API. Mount it at "/{provider}/api".
type Handler struct {
	Providers *provider.Registry
	APIKey    string
	Probe     *probe.Prober // reads each release's quality
	Log       *slog.Logger

	feeds rssFeeds

	// For tests; 0 is the constant of the same name.
	probeBudget, answerWithin time.Duration
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p, ok := h.Providers.Get(r.PathValue("provider"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	t := q.Get("t")
	// caps is public, as on most Newznab indexers.
	if t != "caps" && q.Get("apikey") != h.APIKey {
		writeError(w, 100, "Incorrect user credentials")
		return
	}
	switch t {
	case "caps":
		_, byTVDB := p.(provider.TVDBSearcher)
		writeXML(w, caps(p.Name(), byTVDB))
	case "search", "tvsearch", "movie":
		h.search(w, r, p, t)
	case "get":
		h.get(w, r, p)
	default:
		writeError(w, 202, "No such function")
	}
}

func (h *Handler) search(w http.ResponseWriter, r *http.Request, p provider.Provider, t string) {
	start := time.Now()
	q := r.URL.Query()
	movie := t == "movie" || (t == "search" && hasMovieCategory(q.Get("cat")))
	text := strings.TrimSpace(q.Get("q"))
	tvdbID, _ := strconv.Atoi(q.Get("tvdbid"))
	if text == "" && (movie || tvdbID <= 0) {
		// A search by TVDB ID never gets the feed's placeholder: Sonarr would
		// skip its title search.
		h.rss(w, r, p, movie, start)
		return
	}

	var query provider.Query
	if movie {
		title, year := splitYear(text)
		query = provider.Query{Kind: provider.Movie, Title: title, Year: year}
	} else {
		season, err := strconv.Atoi(q.Get("season"))
		if err != nil || season <= 0 {
			writeXML(w, h.feed(p, nil))
			return
		}
		episode := 0
		if ep := q.Get("ep"); ep != "" {
			if episode, err = strconv.Atoi(ep); err != nil || episode <= 0 {
				writeXML(w, h.feed(p, nil)) // daily "MM/dd" is not supported
				return
			}
		}
		query = provider.Query{Kind: provider.Episode, Title: text, Season: season, Episode: episode}
	}

	var found []provider.Item
	var err error
	if query.Title == "" {
		query.Title, found, err = searchTVDB(r.Context(), p, tvdbID, query)
	} else {
		found, err = p.Search(r.Context(), query)
	}
	if err != nil {
		h.Log.Error("search failed", "provider", p.Name(), "kind", query.Kind, "title", query.Title, "tvdbid", tvdbID, "err", err)
		writeError(w, 900, "Search failed: "+err.Error())
		return
	}
	off, lim := pageBounds(q.Get("offset"), q.Get("limit"))
	releases := make([]provider.Release, len(found))
	for i, it := range found {
		releases[i] = provider.Release{Title: query.Title, Item: it}
	}
	pg, res := h.probePage(r.Context(), start, p, releases, off, lim)
	h.Log.Info("search", "provider", p.Name(), "kind", query.Kind, "title", query.Title, "tvdbid", tvdbID,
		"year", query.Year, "season", query.Season, "episode", query.Episode, "offset", off, "results", len(pg),
		"unavailable", res.unavailable, "unreadable", res.unreadable)

	items := make([]item, 0, len(pg))
	for _, rel := range pg {
		items = append(items, h.release(r, p, query, rel.Item, rel.info))
	}
	writeXML(w, h.feed(p, items))
}

// rss serves RSS sync and the indexer test: the provider's new releases,
// newest first. The indexer test fails on an empty feed, so a placeholder
// stands in when there are none; being unparseable, it is never grabbed.
func (h *Handler) rss(w http.ResponseWriter, r *http.Request, p provider.Provider, movie bool, start time.Time) {
	kind := provider.Episode
	if movie {
		kind = provider.Movie
	}
	q := r.URL.Query()
	off, lim := pageBounds(q.Get("offset"), q.Get("limit"))
	feed := p.Name() + " " + kind.String()
	cur, ok := h.feeds.current(feed, off)
	if !ok {
		var releases []provider.Release
		if rl, ok := p.(provider.RecentLister); ok {
			var err error
			if releases, err = rl.Recent(r.Context(), kind); err != nil {
				// Error 900 would count against the indexer and pause its searches too.
				h.Log.Warn("listing new releases failed", "provider", p.Name(), "kind", kind, "err", err)
				writeXML(w, h.feed(p, []item{h.placeholder(r, p, movie)}))
				return
			}
		}
		cur = h.feeds.begin(feed, releases)
	}
	// Ties are broken so pages don't shift between requests. The list is
	// shared, so it is sorted as a copy.
	releases := slices.Clone(cur.releases)
	failed := h.feeds.redate(feed, releases, cur.began)
	slices.SortFunc(releases, func(a, b provider.Release) int {
		return cmp.Or(b.Published.Compare(a.Published), failed[a.ID].Compare(failed[b.ID]), strings.Compare(a.ID, b.ID))
	})
	pg, res := h.probePage(r.Context(), start, p, releases, off, lim)
	h.feeds.update(feed, releases, pg, res, cur.began)
	h.Log.Debug("rss", "provider", p.Name(), "kind", kind, "offset", off, "releases", len(releases),
		"results", len(pg), "unavailable", res.unavailable, "unreadable", res.unreadable)
	if len(pg) == 0 && off == 0 {
		writeXML(w, h.feed(p, []item{h.placeholder(r, p, movie)}))
		return
	}
	items := make([]item, 0, len(pg))
	for _, rel := range pg {
		items = append(items, h.release(r, p, provider.Query{Kind: rel.Kind, Title: rel.Title}, rel.Item, rel.info))
	}
	writeXML(w, h.feed(p, items))
}

// probePage probes releases in order until the page from off, lim long, is
// full, and returns it with what probing found. Sonarr/Radarr ask for the
// next offset whenever a page is full. Releases left out while probing
// mustn't shorten a full page, so enough are probed to fill it, and no more.
func (h *Handler) probePage(ctx context.Context, start time.Time, p provider.Provider, releases []provider.Release, off, lim int) ([]probed, probeResult) {
	want := off + lim
	if off < 0 || off >= len(releases) {
		want = 0
	}
	deadline := time.Now().Add(cmp.Or(h.probeBudget, probeBudget))
	if answerBy := start.Add(cmp.Or(h.answerWithin, answerWithin)); answerBy.Before(deadline) {
		deadline = answerBy
	}
	probeCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	res := h.probeUntil(probeCtx, p, releases, want)
	if probeCtx.Err() != nil && ctx.Err() == nil {
		h.Log.Warn("ran out of time reading release qualities; leaving the rest out", "provider", p.Name(),
			"took", time.Since(start).Round(time.Second), "read", len(res.read))
	}
	return page(res.read, off, lim), res
}

// rssFeeds keeps what RSS sync needs across requests, by feed: the sync in
// progress, and the releases held back because their streams couldn't be
// read in time. Sonarr reads further pages only until one holds the newest
// release it saw before, so a release read later could stay on a page it
// never reads. Instead, a held release comes first until it is offered, and
// is dated then.
type rssFeeds struct {
	mu    sync.Mutex
	syncs map[string]feedSync
	held  map[string]map[string]heldRelease // then by release ID
}

// feedSync is an RSS sync, which asks for offset 0 first and pages on from
// there. Its pages come from the releases listed when it began, so they don't
// shift if the provider's list changes meanwhile.
type feedSync struct {
	began    time.Time
	releases []provider.Release
}

// heldRelease is when a held release was offered, zero until then, and when
// a sync last found its stream unreadable.
type heldRelease struct {
	offered, failed time.Time
}

// current returns the sync in progress that asks for the page at off.
// Offset 0 begins a new one.
func (fs *rssFeeds) current(feed string, off int) (feedSync, bool) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	cur, ok := fs.syncs[feed]
	return cur, ok && off != 0
}

// begin begins a sync of the releases. Held releases are dated by when it
// began, so pages don't shift while Sonarr reads them.
func (fs *rssFeeds) begin(feed string, releases []provider.Release) feedSync {
	cur := feedSync{began: time.Now(), releases: releases}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if fs.syncs == nil {
		fs.syncs = make(map[string]feedSync)
	}
	fs.syncs[feed] = cur
	return cur
}

// redate dates the held releases by when they were offered, or by when this
// sync began if they haven't been yet. For the latter, it returns when they
// last failed: those not tried for longest are probed first, so releases that
// keep failing can't use up every sync's probing time.
func (fs *rssFeeds) redate(feed string, releases []provider.Release, began time.Time) (failed map[string]time.Time) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	failed = make(map[string]time.Time)
	for i, r := range releases {
		held, ok := fs.held[feed][r.ID]
		switch {
		case !ok:
		case held.offered.IsZero():
			releases[i].Published = began
			failed[r.ID] = held.failed
		default:
			releases[i].Published = held.offered
		}
	}
	return failed
}

// update holds the releases left out, and records when held ones were
// offered or failed: when this sync began. It forgets releases gone from the
// feed.
func (fs *rssFeeds) update(feed string, releases []provider.Release, offered []probed, res probeResult, began time.Time) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	prev := fs.held[feed]
	held := make(map[string]heldRelease)
	for _, r := range releases {
		if h, ok := prev[r.ID]; ok {
			held[r.ID] = h
		}
	}
	for _, r := range res.late {
		held[r.ID] = heldRelease{failed: held[r.ID].failed}
	}
	for _, r := range res.failed {
		held[r.ID] = heldRelease{failed: began}
	}
	for _, r := range offered {
		if h, ok := held[r.ID]; ok && h.offered.IsZero() {
			held[r.ID] = heldRelease{offered: began}
		}
	}
	if fs.held == nil {
		fs.held = make(map[string]map[string]heldRelease)
	}
	fs.held[feed] = held
}

// probed is a release with the quality of its stream.
type probed struct {
	provider.Release
	info probe.Info
}

// probeResult is what probing releases found.
type probeResult struct {
	read                    []probed           // in order
	failed                  []provider.Release // can't be downloaded, or their quality read
	late                    []provider.Release // not probed in time
	unavailable, unreadable int
}

// probeUntil probes releases in order until want of them have a readable
// quality. Releases that can't be downloaded (DRM, paid, region-blocked) or
// whose quality can't be read are left out and counted, as are those not
// probed in time.
func (h *Handler) probeUntil(ctx context.Context, p provider.Provider, releases []provider.Release, want int) probeResult {
	var res probeResult
	var firstUnavailable error
	next := 0
	for next < len(releases) && len(res.read) < want && ctx.Err() == nil {
		// Just enough for the page if all of them can be read.
		batch := releases[next:min(next+want-len(res.read), len(releases))]
		next += len(batch)
		infos, errs := h.probeBatch(ctx, p, batch)
		for i, rel := range batch {
			switch err := errs[i]; {
			case err == nil:
				res.read = append(res.read, probed{Release: rel, info: infos[i]})
				continue
			case errors.Is(err, errLate):
				res.late = append(res.late, rel)
				continue
			case errors.Is(err, provider.ErrUnavailable):
				if res.unavailable == 0 {
					firstUnavailable = err
				}
				res.unavailable++
				h.Log.Debug("release unavailable", "provider", p.Name(), "id", rel.ID, "title", rel.Item.Title, "err", err)
			default:
				// The first one is a warning: the site or network may be failing.
				if res.unreadable == 0 && ctx.Err() == nil {
					h.Log.Warn("can't read a release's quality; leaving it out", "provider", p.Name(), "id", rel.ID, "title", rel.Item.Title, "err", err)
				} else {
					h.Log.Debug("can't read a release's quality", "provider", p.Name(), "id", rel.ID, "title", rel.Item.Title, "err", err)
				}
				res.unreadable++
			}
			res.failed = append(res.failed, rel)
		}
	}
	if len(res.read) < want && ctx.Err() != nil {
		res.late = append(res.late, releases[next:]...)
	}
	if res.unavailable > 0 {
		h.Log.Info("left out releases that can't be downloaded", "provider", p.Name(), "count", res.unavailable,
			"first_reason", firstUnavailable)
	}
	return res
}

// errLate marks a probe cut short because probing ran out of time, which
// says nothing about the stream.
var errLate = errors.New("not probed in time")

// probeBatch probes releases, probeWorkers at a time.
func (h *Handler) probeBatch(ctx context.Context, p provider.Provider, releases []provider.Release) ([]probe.Info, []error) {
	infos := make([]probe.Info, len(releases))
	errs := make([]error, len(releases))
	sem := make(chan struct{}, probeWorkers)
	var wg sync.WaitGroup
	for i, it := range releases {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			infos[i], errs[i] = h.Probe.Probe(ctx, p, it.ID)
			if errs[i] != nil && ctx.Err() != nil {
				errs[i] = errLate
			}
		})
	}
	wg.Wait()
	return infos, errs
}

// searchTVDB serves Sonarr's search by TVDB ID alone. It finds nothing if
// the provider can't search by TVDB ID or returns no title to name releases
// with; Sonarr then searches by title.
func searchTVDB(ctx context.Context, p provider.Provider, tvdbID int, q provider.Query) (string, []provider.Item, error) {
	ts, ok := p.(provider.TVDBSearcher)
	if !ok {
		return "", nil, nil
	}
	title, found, err := ts.SearchTVDB(ctx, tvdbID, q)
	if err != nil || title == "" {
		return "", nil, err
	}
	return title, found, nil
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

// release turns a provider item into a Newznab item. It has no tvdbid attr:
// Sonarr would match by it when the title doesn't, and it won't auto-import
// a release matched only by ID.
func (h *Handler) release(r *http.Request, p provider.Provider, q provider.Query, it provider.Item, info probe.Info) item {
	secs := int(it.Duration / time.Second)
	link := h.nzbLink(r, p, it.ID, secs)
	size := int64(secs) * bytesPerSecond
	if info.Bandwidth > 0 {
		size = int64(secs) * info.Bandwidth / 8
	}
	cat := catTVHD
	if it.Kind == provider.Movie {
		cat = catMoviesHD
	}
	return item{
		Title:     ReleaseTitle(p.Name(), q, it, info),
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

// placeholderDate is long ago, so the placeholder is never newer than a
// release Sonarr hasn't read: RSS sync reads further pages only until one
// holds a release older than the newest it saw before.
var placeholderDate = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

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
		PubDate:   pubDate(placeholderDate),
		Category:  categoryName(cat),
		Enclosure: enclosure{URL: link, Length: 1, Type: "application/x-nzb"},
		Attrs: []attr{
			{Name: "category", Value: strconv.Itoa(cat)},
			{Name: "size", Value: "1"},
		},
	}
}

// ReleaseTitle builds a release name from the *arr's own query title and
// year, so the release matches by title: Sonarr/Radarr won't auto-import a
// release matched only by ID. The quality is the stream's, so the one they
// grab by is the one they record on import.
func ReleaseTitle(providerName string, q provider.Query, it provider.Item, info probe.Info) string {
	title := strings.Join(strings.Fields(q.Title), ".")
	if it.Kind == provider.Movie {
		year := q.Year
		if year == 0 {
			year = it.Year
		}
		if year > 0 {
			title += "." + strconv.Itoa(year)
		}
	} else {
		title += fmt.Sprintf(".S%02dE%02d", it.Season, it.Episode)
	}
	parts := []string{title, info.Resolution(), "WEB-DL"}
	for _, codec := range []string{info.AudioCodec(), info.VideoCodec()} {
		if codec != "" {
			parts = append(parts, codec)
		}
	}
	return strings.Join(parts, ".") + "-" + strings.ToUpper(providerName)
}

func (h *Handler) nzbLink(r *http.Request, p provider.Provider, id string, secs int) string {
	v := url.Values{"t": {"get"}, "id": {id}, "apikey": {h.APIKey}}
	if secs > 0 {
		v.Set("dur", strconv.Itoa(secs))
	}
	return h.baseURL(r, p) + "?" + v.Encode()
}

// baseURL is the URL the caller reached this indexer at, honouring a
// TLS-terminating reverse proxy.
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

// firstValue returns the first entry of a comma-separated header value.
func firstValue(v string) string {
	first, _, _ := strings.Cut(v, ",")
	return strings.TrimSpace(first)
}

// pageBounds parses Newznab offset and limit, capping limit at maxResults.
func pageBounds(offset, limit string) (off, lim int) {
	off, _ = strconv.Atoi(offset)
	lim, err := strconv.Atoi(limit)
	if err != nil || lim <= 0 || lim > maxResults {
		lim = maxResults
	}
	return off, lim
}

// page returns the items from off, at most lim of them.
func page[T any](items []T, off, lim int) []T {
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

// SearchEngine "raw" makes Sonarr/Radarr send titles uncleaned ("The
// Killing", not "Killing").
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

// caps advertises tvdbid for providers that can search by it. Sonarr then
// searches by ID first, and by title only if that finds nothing.
func caps(providerName string, tvdbSearch bool) capsDoc {
	tvParams := "q,season,ep"
	if tvdbSearch {
		tvParams += ",tvdbid"
	}
	return capsDoc{
		Server: capsServer{Title: "vodarr " + providerName},
		Limits: capsLimits{Max: maxResults, Default: maxResults},
		Searching: capsSearching{
			Search:      capsSearch{Available: "yes", SupportedParams: "q", SearchEngine: "raw"},
			TVSearch:    capsSearch{Available: "yes", SupportedParams: tvParams, SearchEngine: "raw"},
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
