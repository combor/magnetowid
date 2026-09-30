// Package bbc is the provider for BBC iPlayer (https://www.bbc.co.uk/iplayer).
package bbc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"

	bolt "go.etcd.io/bbolt"

	"github.com/combor/magnetowid/internal/provider"
)

const (
	defaultIBLURL      = "https://ibl.api.bbc.co.uk/ibl/v1"
	defaultSelectorURL = "https://open.live.bbc.co.uk/mediaselector/6/select/version/2.0"
	userAgent          = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130 Safari/537.36"
)

type Provider struct {
	client      *http.Client
	iblURL      string
	selectorURL string
	titles      *titleLookup
	cache       *responseCache

	watchedSeries *watchList
	watchedFilms  *watchList
	seriesFeed    feed
	filmFeed      feed
	overrides     atomic.Pointer[provider.Overrides]
	log           *slog.Logger
}

// A nil db keeps watch lists in memory only.
func New(client *http.Client, log *slog.Logger, db *bolt.DB) (*Provider, error) {
	watchedSeries, err := loadWatchList(db, seriesWatch)
	if err != nil {
		return nil, err
	}
	watchedFilms, err := loadWatchList(db, filmWatch)
	if err != nil {
		return nil, err
	}
	p := &Provider{
		client:        client,
		iblURL:        defaultIBLURL,
		selectorURL:   defaultSelectorURL,
		titles:        newTitleLookup(client),
		cache:         newResponseCache(),
		watchedSeries: watchedSeries,
		watchedFilms:  watchedFilms,
		log:           log,
	}
	p.seriesFeed.rebuild = p.rebuildSeries
	p.filmFeed.rebuild = p.rebuildFilms
	return p, nil
}

func (p *Provider) Name() string { return "bbc" }

// Feeds rebuild with new overrides at the next RSS sync.
func (p *Provider) SetOverrides(o *provider.Overrides) {
	series, films := o.Changes(p.overrides.Swap(o))
	p.seriesFeed.forget(series...)
	if films {
		p.filmFeed.forget(filmsKey)
	}
	p.seriesFeed.markStale()
	p.filmFeed.markStale()
}

// BBC pages name programmes and episodes by PID, e.g.
// https://www.bbc.co.uk/iplayer/episode/m002d3lr/doctor-who-season-2-8-the-reality-war,
// https://www.bbc.co.uk/iplayer/episodes/p0gglvqn/doctor-who or
// https://www.bbc.co.uk/programmes/m002d3lr.
var pagePID = regexp.MustCompile(`^/(?:iplayer/episodes?|programmes)/([^/]+)`)

func (p *Provider) ParseID(ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if u, err := url.Parse(ref); err == nil && u.Host != "" {
		m := pagePID.FindStringSubmatch(u.Path)
		if (u.Host != "www.bbc.co.uk" && u.Host != "bbc.co.uk") || m == nil {
			return "", fmt.Errorf("%q is not a BBC programme page", ref)
		}
		ref = m[1]
	}
	if !pidPattern.MatchString(ref) {
		return "", fmt.Errorf("%q is neither a BBC programme ID nor a bbc.co.uk URL", ref)
	}
	return ref, nil
}

func (p *Provider) Search(ctx context.Context, q provider.Query) ([]provider.Item, error) {
	switch q.Kind {
	case provider.Episode:
		return p.searchEpisodes(ctx, q)
	case provider.Movie:
		return p.searchFilms(ctx, q)
	}
	return nil, nil
}

// Episodes always get TVDB's numbering: a title search first finds the TVDB
// series of that title, then searches as SearchTVDB does. BBC's numbering
// alone would make 2024's Space Babies classic Doctor Who's S01E01.
func (p *Provider) searchEpisodes(ctx context.Context, q provider.Query) ([]provider.Item, error) {
	tvdbID, ok, err := p.titles.resolve(ctx, q.Title)
	if err != nil {
		p.log.Warn("TVDB title lookup failed; no results", "title", q.Title, "err", err)
		return nil, nil
	}
	if !ok {
		p.log.Debug("the title names no single TVDB series; no results", "title", q.Title)
		return nil, nil
	}
	_, items, err := p.SearchTVDB(ctx, tvdbID, q)
	return items, err
}

// Return containers titled exactly as title, in search order, or the one
// with the given id.
func (p *Provider) findShows(ctx context.Context, title, id string) ([]programme, error) {
	want := provider.NormalizeTitle(title)
	if want == "" {
		return nil, nil
	}
	found, err := p.search(ctx, title)
	if err != nil {
		return nil, err
	}
	var shows []programme
	for _, r := range found {
		if r.container() && ((id != "" && r.ID == id) || (id == "" && provider.NormalizeTitle(r.Title) == want)) {
			shows = append(shows, r)
		}
	}
	return shows, nil
}

func episodeItem(e programme, season, episode int) provider.Item {
	return provider.Item{
		ID:        e.ID,
		Kind:      provider.Episode,
		Title:     e.Title + " – " + e.Subtitle,
		Season:    season,
		Episode:   episode,
		Duration:  e.duration(),
		Published: e.available(),
	}
}

// Films are one-offs matched by title and year.
func (p *Provider) searchFilms(ctx context.Context, q provider.Query) ([]provider.Item, error) {
	if provider.NormalizeTitle(q.Title) == "" {
		return nil, nil
	}
	want := newFilmWant(p.overrides.Load(), q.Title, q.Year)
	var items []provider.Item
	seen := make(map[string]bool)
	for _, title := range want.titles {
		found, err := p.search(ctx, title)
		if err != nil {
			return nil, err
		}
		for _, r := range found {
			if !seen[r.ID] && want.matches(r) {
				seen[r.ID] = true
				items = append(items, filmItem(r, q.Year))
			}
		}
	}
	// Radarr relies on RSS after searching. Watch even found films, which
	// probing may still reject.
	if q.Year > 0 {
		p.watchFilm(q.Title, q.Year)
	}
	return items, nil
}

// filmWant is what a Radarr film matches: the override's programme, or its
// titles or Radarr's, from about the same year.
type filmWant struct {
	id         string
	titles     []string // to search
	normalized []string
	year       int
	overrides  *provider.Overrides
}

func newFilmWant(o *provider.Overrides, title string, year int) filmWant {
	ov, _ := o.Film(title, year)
	w := filmWant{id: ov.ID, titles: []string{title}, year: year, overrides: o}
	if len(ov.Titles) > 0 {
		w.titles = ov.Titles
	}
	for _, t := range w.titles {
		if n := provider.NormalizeTitle(t); n != "" {
			w.normalized = append(w.normalized, n)
		}
	}
	return w
}

// Films are one-offs, even when chosen by ID: the film feed lists no others.
// Programmes pinned to other films are theirs alone.
func (w filmWant) matches(r programme) bool {
	if w.id != "" {
		return r.ID == w.id && r.oneOff()
	}
	return !w.overrides.FilmPinned(r.ID) && slices.ContainsFunc(w.normalized, func(n string) bool { return filmMatches(r, n, w.year) })
}

// BBC's year is usually the release year, sometimes the UK premiere's. Allow
// a one-year discrepancy; year 0 is unknown.
func filmMatches(r programme, normalized string, year int) bool {
	if !r.oneOff() {
		return false
	}
	if provider.NormalizeTitle(r.Title) != normalized && provider.NormalizeTitle(r.OriginalTitle) != normalized {
		return false
	}
	y := r.year()
	return year == 0 || y == 0 || abs(y-year) <= 1
}

// Use the client's year, if given, for import matching.
func filmItem(r programme, year int) provider.Item {
	if year == 0 {
		year = r.year()
	}
	return provider.Item{
		ID:        r.ID,
		Kind:      provider.Movie,
		Title:     r.Title,
		Year:      year,
		Duration:  r.duration(),
		Published: r.available(),
	}
}

// BBC programme IDs (PIDs) are 8 or more lowercase letters and digits.
var pidPattern = regexp.MustCompile(`^[0-9b-df-hj-np-tv-z]{8,15}$`)

type apiError struct {
	status int
	url    string
}

func (e *apiError) Error() string {
	u, _, _ := strings.Cut(e.url, "?") // keep tokens out of logs
	return fmt.Sprintf("bbc: HTTP %d from %s", e.status, u)
}

func (e *apiError) permanent() bool {
	return e.status >= 400 && e.status < 500 &&
		e.status != http.StatusRequestTimeout && e.status != http.StatusTooManyRequests
}

func (p *Provider) get(ctx context.Context, url string, out any) error {
	body, err := p.fetch(ctx, url, 8<<20)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, out); err != nil {
		u, _, _ := strings.Cut(url, "?")
		return fmt.Errorf("bbc: decoding %s: %w", u, err)
	}
	return nil
}

// Error responses return their body with an *apiError.
func (p *Provider) fetch(ctx context.Context, url string, limit int64) ([]byte, error) {
	body, _, err := p.fetchFinal(ctx, url, limit)
	return body, err
}

// Also return the URL after redirects.
func (p *Provider) fetchFinal(ctx context.Context, url string, limit int64) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("bbc: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return nil, "", fmt.Errorf("bbc: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return body, "", &apiError{status: resp.StatusCode, url: url}
	}
	return body, resp.Request.URL.String(), nil
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
