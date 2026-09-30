// Package bbc is the provider for BBC iPlayer (https://www.bbc.co.uk/iplayer).
package bbc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"

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

// Return containers titled exactly as title, in search order.
func (p *Provider) findShows(ctx context.Context, title string) ([]programme, error) {
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
		if r.container() && provider.NormalizeTitle(r.Title) == want {
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
	want := provider.NormalizeTitle(q.Title)
	if want == "" {
		return nil, nil
	}
	found, err := p.search(ctx, q.Title)
	if err != nil {
		return nil, err
	}
	var items []provider.Item
	for _, r := range found {
		if filmMatches(r, want, q.Year) {
			items = append(items, filmItem(r, q.Year))
		}
	}
	// Radarr relies on RSS after searching. Watch even found films, which
	// probing may still reject.
	if q.Year > 0 {
		p.watchFilm(q.Title, q.Year)
	}
	return items, nil
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
