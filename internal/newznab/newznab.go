// Package newznab serves each provider as a Newznab indexer.
package newznab

import (
	"cmp"
	"context"
	"crypto/subtle"
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

	"github.com/combor/magnetowid/internal/nzb"
	"github.com/combor/magnetowid/internal/probe"
	"github.com/combor/magnetowid/internal/provider"
)

// Assume 4 Mbit/s when the stream omits bandwidth.
const bytesPerSecond = 4_000_000 / 8

const maxResults = 100

const probeWorkers = 4

// Leave time to respond before Sonarr/Radarr's 100-second timeout.
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
	Probe     *probe.Prober
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
	// Newznab capabilities are public.
	if t != "caps" && subtle.ConstantTimeCompare([]byte(q.Get("apikey")), []byte(h.APIKey)) != 1 {
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
		// A placeholder here would suppress Sonarr's fallback to title search.
		h.rss(w, r, p, movie, start)
		return
	}

	var query provider.Query
	if movie {
		title, year := splitYear(text)
		query = provider.Query{Kind: provider.Movie, Title: title, Year: year}
	} else {
		var ok bool
		if query, ok = episodeQuery(text, q.Get("season"), q.Get("ep")); !ok {
			writeXML(w, h.feed(p, nil))
			return
		}
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
		"year", query.Year, "season", query.Season, "episode", query.Episode, "airdate", query.AirDate, "offset", off, "results", len(pg),
		"unavailable", res.unavailable, "unreadable", res.unreadable)

	items := make([]item, 0, len(pg))
	for _, rel := range pg {
		items = append(items, h.release(r, p, query, rel.Item, rel.info))
	}
	writeXML(w, h.feed(p, items))
}

var dailyEpisode = regexp.MustCompile(`^(\d\d)/(\d\d)$`)

// Sonarr asks for specials as season 00, and for a daily series' episode by
// its air date, as season=2026&ep=09/29. ok=false answers with no results.
func episodeQuery(title, season, ep string) (q provider.Query, ok bool) {
	s, err := strconv.Atoi(season)
	if err != nil || s < 0 {
		return provider.Query{}, false
	}
	q = provider.Query{Kind: provider.Episode, Title: title, Season: s}
	if ep == "" {
		return q, true
	}
	if m := dailyEpisode.FindStringSubmatch(ep); m != nil {
		date := fmt.Sprintf("%04d-%s-%s", s, m[1], m[2])
		if _, err := time.Parse(time.DateOnly, date); err != nil {
			return provider.Query{}, false
		}
		return provider.Query{Kind: provider.Episode, Title: title, AirDate: date}, true
	}
	if q.Episode, err = strconv.Atoi(ep); err != nil || q.Episode <= 0 {
		return provider.Query{}, false
	}
	return q, true
}

// The unparseable placeholder keeps empty feeds valid for indexer tests and
// prevents Sonarr from reporting an RSS gap when real releases first appear.
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
	// Sort a copy with stable ties so concurrent requests cannot shift pages.
	releases := slices.Clone(cur.releases)
	failed := h.feeds.redate(feed, releases, cur.began)
	slices.SortFunc(releases, func(a, b provider.Release) int {
		return cmp.Or(b.Published.Compare(a.Published), failed[a.Key()].Compare(failed[b.Key()]), strings.Compare(a.ID, b.ID))
	})
	pg, res := h.probePage(r.Context(), start, p, releases, off, lim)
	h.feeds.update(feed, releases, pg, res, cur.began)
	h.Log.Debug("rss", "provider", p.Name(), "kind", kind, "offset", off, "releases", len(releases),
		"results", len(pg), "unavailable", res.unavailable, "unreadable", res.unreadable)
	items := make([]item, 0, len(pg)+1)
	for _, rel := range pg {
		items = append(items, h.release(r, p, provider.Query{Kind: rel.Kind, Title: rel.Title}, rel.Item, rel.info))
	}
	// Emit the placeholder only once so pagination terminates.
	if n := len(res.read); off <= n && n < off+lim {
		items = append(items, h.placeholder(r, p, movie))
	}
	writeXML(w, h.feed(p, items))
}

// Fill the page despite failed probes. Sonarr/Radarr stop paging at a short page.
func (h *Handler) probePage(ctx context.Context, start time.Time, p provider.Provider, releases []provider.Release, off, lim int) ([]probed, probeResult) {
	// Recheck cached probes at the end to determine whether the last page was full.
	want := off + lim
	if off < 0 || off > len(releases) {
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

// rssFeeds preserves pagination and tracks releases deferred by failed probes.
// Date them when first offered so Sonarr sees them before its RSS cutoff.
type rssFeeds struct {
	mu    sync.Mutex
	syncs map[string]feedSync
	held  map[string]map[string]heldRelease // then by provider.Release.Key
}

// Snapshot the feed at offset 0 so provider updates cannot shift later pages.
type feedSync struct {
	began    time.Time
	releases []provider.Release
}

type heldRelease struct {
	offered, failed time.Time
}

// Offset 0 starts a new sync.
func (fs *rssFeeds) current(feed string, off int) (feedSync, bool) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	cur, ok := fs.syncs[feed]
	return cur, ok && off != 0
}

// Use one timestamp throughout a sync to keep deferred releases on stable pages.
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

// Undelivered releases get this sync's timestamp and their last failure time.
// Probe the least recently tried first so persistent failures cannot starve others.
func (fs *rssFeeds) redate(feed string, releases []provider.Release, began time.Time) (failed map[string]time.Time) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	failed = make(map[string]time.Time)
	for i, r := range releases {
		held, ok := fs.held[feed][r.Key()]
		switch {
		case !ok:
		case held.offered.IsZero():
			releases[i].Published = began
			failed[r.Key()] = held.failed
		default:
			releases[i].Published = held.offered
		}
	}
	return failed
}

func (fs *rssFeeds) update(feed string, releases []provider.Release, offered []probed, res probeResult, began time.Time) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	prev := fs.held[feed]
	held := make(map[string]heldRelease)
	for _, r := range releases {
		if h, ok := prev[r.Key()]; ok {
			held[r.Key()] = h
		}
	}
	for _, r := range res.late {
		held[r.Key()] = heldRelease{failed: held[r.Key()].failed}
	}
	for _, r := range res.failed {
		held[r.Key()] = heldRelease{failed: began}
	}
	for _, r := range offered {
		if h, ok := held[r.Key()]; ok && h.offered.IsZero() {
			held[r.Key()] = heldRelease{offered: began}
		}
	}
	if fs.held == nil {
		fs.held = make(map[string]map[string]heldRelease)
	}
	fs.held[feed] = held
}

type probed struct {
	provider.Release
	info probe.Info
}

type probeResult struct {
	read                    []probed           // in order
	failed                  []provider.Release // unavailable or unreadable
	late                    []provider.Release // not probed in time
	unavailable, unreadable int
}

func (h *Handler) probeUntil(ctx context.Context, p provider.Provider, releases []provider.Release, want int) probeResult {
	var res probeResult
	var firstUnavailable error
	next := 0
	for next < len(releases) && len(res.read) < want && ctx.Err() == nil {
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
				// Warn once per search; log further failures at debug level.
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

// Budget exhaustion says nothing about stream availability.
var errLate = errors.New("not probed in time")

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

// No results or title makes Sonarr fall back to title search.
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

// Omit tvdbid: Sonarr will not auto-import releases matched only by ID.
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

// Keep the placeholder older than real releases so it cannot advance Sonarr's RSS cutoff.
var placeholderDate = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

func (h *Handler) placeholder(r *http.Request, p provider.Provider, movie bool) item {
	cat := catTVHD
	if movie {
		cat = catMoviesHD
	}
	link := h.baseURL(r, p) + "?" + url.Values{"t": {"get"}, "apikey": {h.APIKey}}.Encode()
	return item{
		Title:     "magnetowid " + p.Name() + " feed placeholder",
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

// Use the client's title and year for import matching, with the stream's language
// and quality so selection agrees with the downloaded file.
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
	parts := []string{title}
	if lang := info.LanguageName(); lang != "" {
		parts = append(parts, lang)
	}
	parts = append(parts, info.Resolution(), "WEB-DL")
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

// Honor reverse-proxy headers when constructing the public URL.
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

func firstValue(v string) string {
	first, _, _ := strings.Cut(v, ",")
	return strings.TrimSpace(first)
}

func pageBounds(offset, limit string) (off, lim int) {
	off, _ = strconv.Atoi(offset)
	lim, err := strconv.Atoi(limit)
	if err != nil || lim <= 0 || lim > maxResults {
		lim = maxResults
	}
	return off, lim
}

func page[T any](items []T, off, lim int) []T {
	if off < 0 || off >= len(items) {
		return nil
	}
	return items[off:min(off+lim, len(items))]
}

var trailingYear = regexp.MustCompile(`^(.*\S)\s+\(?((?:19|20)\d\d)\)?$`)

// Split Radarr's "Title 1971" query.
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

// SearchEngine=raw preserves title prefixes such as "The".
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

// Advertising tvdbid makes Sonarr try ID search before title search.
func caps(providerName string, tvdbSearch bool) capsDoc {
	tvParams := "q,season,ep"
	if tvdbSearch {
		tvParams += ",tvdbid"
	}
	return capsDoc{
		Server: capsServer{Title: "magnetowid " + providerName},
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
		Channel:   channel{Title: "magnetowid " + p.Name(), Items: items},
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
