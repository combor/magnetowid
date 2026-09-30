// Package tvp is the provider for TVP VOD (https://vod.tvp.pl).
package tvp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/combor/magnetowid/internal/provider"
)

const (
	defaultBaseURL = "https://vod.tvp.pl/api/products"
	userAgent      = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130 Safari/537.36"
)

type Provider struct {
	client        *http.Client
	baseURL       string
	titles        *titleLookup
	cache         *responseCache
	watchedSeries *watchList
	watchedFilms  *watchList
	seriesFeed    feed
	filmFeed      feed
	overrides     atomic.Pointer[provider.Overrides]
	// Film title caches are owned by rebuildFilms, which runs serially.
	originals   map[int64]string
	lookupTried map[int64]time.Time
	log         *slog.Logger
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
		baseURL:       defaultBaseURL,
		titles:        newTitleLookup(client),
		cache:         newResponseCache(),
		watchedSeries: watchedSeries,
		watchedFilms:  watchedFilms,
		originals:     make(map[int64]string),
		lookupTried:   make(map[int64]time.Time),
		log:           log,
	}
	p.seriesFeed.rebuild = p.rebuildSeries
	p.filmFeed.rebuild = p.rebuildFilms
	return p, nil
}

func (p *Provider) Name() string { return "tvp" }

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

// TVP pages end with the product ID, e.g.
// https://vod.tvp.pl/seriale,18/ranczo-odcinki,316445/odcinek-1,S01E01,381046.
var pageID = regexp.MustCompile(`,(\d+)/?$`)

func (p *Provider) ParseID(ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if u, err := url.Parse(ref); err == nil && u.Host != "" {
		m := pageID.FindStringSubmatch(u.Path)
		if u.Host != "vod.tvp.pl" || m == nil {
			return "", fmt.Errorf("%q is not a TVP VOD programme page", ref)
		}
		ref = m[1]
	}
	id, err := strconv.ParseInt(ref, 10, 64)
	if err != nil || id <= 0 {
		return "", fmt.Errorf("%q is neither a TVP VOD ID nor a vod.tvp.pl URL", ref)
	}
	return strconv.FormatInt(id, 10), nil
}

type product struct {
	ID            int64  `json:"id"`
	Type          string `json:"type"`
	Title         string `json:"title"`
	OriginalTitle string `json:"originalTitle"`
	Year          int    `json:"year"`
	Number        int    `json:"number"`
	Duration      int    `json:"duration"` // seconds
	Payable       bool   `json:"payable"`
	Since         string `json:"since"`
}

func (p *Provider) Search(ctx context.Context, q provider.Query) ([]provider.Item, error) {
	switch q.Kind {
	case provider.Episode:
		return p.searchEpisodes(ctx, q)
	case provider.Movie:
		return p.searchMovies(ctx, q)
	}
	return nil, nil
}

func (p *Provider) searchEpisodes(ctx context.Context, q provider.Query) ([]provider.Item, error) {
	if q.Season <= 0 {
		return nil, nil
	}
	// Sonarr searches by title when its ID search finds nothing, even if an
	// override decided that; the override decides the title search too.
	if tvdbID, ok := p.overriddenSeries(q.Title); ok {
		_, items, err := p.SearchTVDB(ctx, tvdbID, q)
		return items, err
	}
	serial, ok, err := p.findSerial(ctx, q.Title, "")
	if err != nil || !ok {
		return nil, err
	}
	return p.serialEpisodes(ctx, serial, q, nil, provider.SeriesOverride{})
}

// id, if given, picks the serial from the search results.
func (p *Provider) findSerial(ctx context.Context, title, id string) (product, bool, error) {
	serials, err := p.search(ctx, "SERIAL", title)
	if err != nil {
		return product{}, false, err
	}
	serial, ok := p.pick(serials, title, id)
	return serial, ok, nil
}

// tv and ov are zero for title-only searches, which have no TVDB ID.
func (p *Provider) serialEpisodes(ctx context.Context, serial product, q provider.Query, tv *series, ov provider.SeriesOverride) ([]provider.Item, error) {
	seasons, err := p.seasons(ctx, serial.ID)
	if err != nil {
		return nil, err
	}
	items, covered, all, err := p.overridden(ctx, serial, seasons, q, tv, ov)
	if err != nil {
		return nil, err
	}
	if _, ruled := ov.Rule(q.Season); ruled || all {
		return items, nil
	}
	found, err := p.automatic(ctx, serial, seasons, q, tv)
	if err != nil {
		return nil, err
	}
	for _, it := range found {
		if !covered[it.Episode] && !ov.Pinned(it.ID) {
			items = append(items, it)
		}
	}
	return items, nil
}

// Return the episodes the override places; covered holds the TVDB episode
// numbers it decides, found or not, and all reports whether it decides every
// requested episode.
func (p *Provider) overridden(ctx context.Context, serial product, seasons []product, q provider.Query, tv *series, ov provider.SeriesOverride) (items []provider.Item, covered map[int]bool, all bool, err error) {
	covered = make(map[int]bool)
	if tv == nil {
		return nil, covered, false, nil
	}
	numbers := []int{q.Episode}
	if q.Episode == 0 {
		numbers = nil
		for _, e := range tv.episodes {
			if e.season == q.Season {
				numbers = append(numbers, e.episode)
			}
		}
	}
	for _, n := range numbers {
		t, ok := ov.Target(q.Season, n)
		if !ok {
			continue
		}
		covered[n] = true
		e, found, err := p.target(ctx, serial.ID, seasons, t, ov)
		if err != nil {
			return nil, nil, false, err
		}
		if !found {
			p.log.Debug("TVP has no free episode where an override puts a TVDB episode",
				"serial", serial.Title, "season", q.Season, "episode", n, "target", t)
			continue
		}
		items = append(items, episodeItem(serial, e, q.Season, n))
	}
	return items, covered, len(numbers) > 0 && len(covered) == len(numbers), nil
}

// Return the free episode at the target, if TVP has exactly one there.
// Season rules skip episodes pinned to other TVDB episodes.
func (p *Provider) target(ctx context.Context, serialID int64, seasons []product, t provider.Target, ov provider.SeriesOverride) (product, bool, error) {
	var match []product
	for _, s := range seasons {
		if t.ID == "" && t.Season != 0 && s.Number != t.Season {
			continue
		}
		eps, err := p.episodes(ctx, serialID, s.ID)
		if err != nil {
			return product{}, false, err
		}
		for _, e := range eps {
			if t.ID != "" && strconv.FormatInt(e.ID, 10) == t.ID {
				return e, !e.Payable, nil
			}
			if t.ID == "" && e.Number == t.Episode && !ov.Pinned(strconv.FormatInt(e.ID, 10)) {
				match = append(match, e)
			}
		}
	}
	if len(match) != 1 || match[0].Payable {
		return product{}, false, nil
	}
	return match[0], true, nil
}

// TVP can number across seasons, e.g. Ranczo S2 is 14–26. tv is nil for title-only searches.
func (p *Provider) automatic(ctx context.Context, serial product, seasons []product, q provider.Query, tv *series) ([]provider.Item, error) {
	var err error
	bs := blocks(seasons)
	if len(bs) > 0 && tv != nil {
		items, numbered, err := p.byNumber(ctx, serial, bs, q, tv)
		if err != nil || numbered {
			return items, err
		}
	}
	var episodes []product
	if season, ok := findSeason(seasons, q.Season); ok {
		if episodes, err = p.episodes(ctx, serial.ID, season.ID); err != nil {
			return nil, err
		}
		// Block positions are not TVDB seasons: M jak miłość block 20
		// starts at 1901, but S20E1 is 1452. Only season 1 can use block 1.
		if len(bs) == 0 || (q.Season == 1 && tv == nil) {
			items, err := p.bySeason(ctx, serial, seasons, q, episodes)
			if err != nil || len(items) > 0 || q.Episode == 0 {
				return items, err
			}
		}
	}
	if q.Episode == 0 {
		return nil, nil
	}
	return p.byAbsoluteNumber(ctx, serial, seasons, q, episodes)
}

// Use start+N-1 so missing or duplicate episode numbers cannot shift later matches.
func (p *Provider) bySeason(ctx context.Context, serial product, seasons []product, q provider.Query, episodes []product) ([]provider.Item, error) {
	start, ok, err := p.seasonStart(ctx, serial.ID, seasons, q.Season, episodes)
	if err != nil {
		return nil, err
	}
	if !ok {
		p.log.Warn("can't tell where a TVP season's numbering starts; skipping",
			"serial", serial.Title, "season", q.Season)
		return nil, nil
	}
	count := map[int]int{}
	for _, e := range episodes {
		count[e.Number]++
	}
	var items []provider.Item
	for _, e := range episodes {
		ep := e.Number - start + 1
		if count[e.Number] > 1 || ep < 1 || (q.Episode != 0 && ep != q.Episode) || e.Payable {
			continue
		}
		items = append(items, episodeItem(serial, e, q.Season, ep))
	}
	return items, nil
}

// Accept absolute numbers only above the requested season's range, with
// continuous numbering across seasons and exactly one match.
// Example: Klan S15E2113 maps to TVP episode 2113 in season 22.
func (p *Provider) byAbsoluteNumber(ctx context.Context, serial product, seasons []product, q provider.Query, seasonEpisodes []product) ([]provider.Item, error) {
	sorted := append([]product(nil), seasons...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Number < sorted[j].Number })

	// The requested season sets the ceiling; if TVP lacks it, the closest below.
	idx, ceil := -1, seasonEpisodes
	for i := len(sorted) - 1; i >= 0; i-- {
		if len(ceil) > 0 && sorted[i].Number == q.Season {
			idx = i
			break
		}
		if len(ceil) == 0 && sorted[i].Number < q.Season {
			eps, err := p.episodes(ctx, serial.ID, sorted[i].ID)
			if err != nil {
				return nil, err
			}
			if len(eps) > 0 {
				idx, ceil = i, eps
				break
			}
		}
	}
	if idx < 0 {
		return nil, nil
	}
	lastMax := ceil[len(ceil)-1].Number
	if q.Episode <= lastMax {
		return nil, nil
	}

	// Reject numbering that restarts or overlaps across seasons.
	for i := idx - 1; i >= 0; i-- {
		prev, err := p.episodes(ctx, serial.ID, sorted[i].ID)
		if err != nil {
			return nil, err
		}
		if len(prev) == 0 {
			continue
		}
		if ceil[0].Number <= prev[len(prev)-1].Number {
			return nil, nil
		}
		break
	}
	var match []product
	for _, s := range sorted[idx+1:] {
		eps, err := p.episodes(ctx, serial.ID, s.ID)
		if err != nil {
			return nil, err
		}
		if len(eps) == 0 {
			continue
		}
		if eps[0].Number <= lastMax {
			if hasNumber(eps, q.Episode) {
				return nil, nil
			}
			break
		}
		if eps[0].Number > q.Episode {
			break
		}
		for _, e := range eps {
			if e.Number == q.Episode {
				match = append(match, e)
			}
		}
		lastMax = eps[len(eps)-1].Number
	}
	if len(match) != 1 || match[0].Payable {
		return nil, nil
	}
	p.log.Info("matched TVP episode by absolute number", "serial", serial.Title,
		"season", q.Season, "episode", q.Episode)
	return []provider.Item{episodeItem(serial, match[0], q.Season, q.Episode)}, nil
}

func hasNumber(eps []product, n int) bool {
	for _, e := range eps {
		if e.Number == n {
			return true
		}
	}
	return false
}

func episodeItem(serial, e product, season, episode int) provider.Item {
	return provider.Item{
		ID:        strconv.FormatInt(e.ID, 10),
		Kind:      provider.Episode,
		Title:     serial.Title + " – " + e.Title,
		Year:      e.Year,
		Season:    season,
		Episode:   episode,
		Duration:  time.Duration(e.Duration) * time.Second,
		Published: parseTime(e.Since),
	}
}

// A season starts at 1 or immediately after the previous season's last episode.
func (p *Provider) seasonStart(ctx context.Context, serialID int64, seasons []product, number int, episodes []product) (start int, ok bool, err error) {
	if len(episodes) == 0 {
		return 0, false, nil
	}
	first := episodes[0].Number
	if first == 1 {
		return 1, true, nil
	}
	prev, found := findSeason(seasons, number-1)
	if !found {
		return 0, false, nil
	}
	prevEpisodes, err := p.episodes(ctx, serialID, prev.ID)
	if err != nil || len(prevEpisodes) == 0 {
		return 0, false, err
	}
	if first == prevEpisodes[len(prevEpisodes)-1].Number+1 {
		return first, true, nil
	}
	return 0, false, nil
}

func findSeason(seasons []product, number int) (product, bool) {
	for _, s := range seasons {
		if s.Number == number {
			return s, true
		}
	}
	return product{}, false
}

// The returned slice is shared; do not modify it.
func (p *Provider) seasons(ctx context.Context, serialID int64) ([]product, error) {
	path := fmt.Sprintf("vods/serials/%d/seasons", serialID)
	return cached(p.cache, path, func() ([]product, error) {
		var seasons []product
		err := p.get(ctx, path, nil, &seasons)
		return seasons, err
	})
}

// Episodes are sorted by number, without specials. The returned slice is shared.
func (p *Provider) episodes(ctx context.Context, serialID, seasonID int64) ([]product, error) {
	path := fmt.Sprintf("vods/serials/%d/seasons/%d/episodes", serialID, seasonID)
	return cached(p.cache, path, func() ([]product, error) {
		var all []product
		if err := p.get(ctx, path, nil, &all); err != nil {
			return nil, err
		}
		numbered := all[:0]
		for _, e := range all {
			if e.Number > 0 {
				numbered = append(numbered, e)
			}
		}
		sort.SliceStable(numbered, func(i, j int) bool { return numbered[i].Number < numbered[j].Number })
		return numbered, nil
	})
}

func (p *Provider) searchMovies(ctx context.Context, q provider.Query) ([]provider.Item, error) {
	want := p.filmWant(q.Title, q.Year)
	var items []provider.Item
	seen := make(map[int64]bool)
	for _, title := range want.titles {
		vods, err := p.search(ctx, "VOD", title)
		if err != nil {
			return nil, err
		}
		for _, v := range vods {
			if !v.Payable && !seen[v.ID] && want.matches(v) {
				seen[v.ID] = true
				items = append(items, movieItem(v, v.Year))
			}
		}
	}
	// Radarr relies on RSS after searching. Watch even found films,
	// because probing may still reject their streams.
	if q.Year > 0 && provider.NormalizeTitle(q.Title) != "" {
		p.watchFilm(q.Title, q.Year)
	}
	return items, nil
}

// filmWant is what a Radarr film matches: the override's product, or its
// titles or Radarr's, from about the same year.
type filmWant struct {
	id         string
	titles     []string // to search
	normalized []string
	year       int
	overrides  *provider.Overrides
}

func (p *Provider) filmWant(title string, year int) filmWant {
	o := p.overrides.Load()
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

// Products pinned to other films are theirs alone.
func (w filmWant) matches(v product) bool {
	id := strconv.FormatInt(v.ID, 10)
	if w.id != "" {
		return id == w.id
	}
	return !w.overrides.FilmPinned(id) && slices.ContainsFunc(w.normalized, func(n string) bool { return filmMatches(v, n, w.year) })
}

// Allow a one-year discrepancy in TVP dates; year=0 is unknown.
func filmMatches(v product, normalized string, year int) bool {
	return titleMatches(v, normalized) && yearFits(v, year)
}

func yearFits(v product, year int) bool {
	return year == 0 || v.Year == 0 || abs(v.Year-year) <= 1
}

// Use the provider's year when the query omits it.
func movieItem(v product, year int) provider.Item {
	return provider.Item{
		ID:        strconv.FormatInt(v.ID, 10),
		Kind:      provider.Movie,
		Title:     v.Title,
		Year:      year,
		Duration:  time.Duration(v.Duration) * time.Second,
		Published: parseTime(v.Since),
	}
}

// An id overrides title matching.
func (p *Provider) pick(serials []product, title, id string) (product, bool) {
	want := provider.NormalizeTitle(title)
	var matches []product
	for _, s := range serials {
		if s.Payable {
			continue
		}
		if (id != "" && strconv.FormatInt(s.ID, 10) == id) || (id == "" && titleMatches(s, want)) {
			matches = append(matches, s)
		}
	}
	if len(matches) == 0 {
		return product{}, false
	}
	if len(matches) > 1 {
		p.log.Warn("several TVP serials match; using the first", "title", title, "count", len(matches))
	}
	return matches[0], true
}

func titleMatches(v product, normalized string) bool {
	return provider.NormalizeTitle(v.Title) == normalized ||
		(v.OriginalTitle != "" && provider.NormalizeTitle(v.OriginalTitle) == normalized)
}

// The returned slice is shared; do not modify it.
func (p *Provider) search(ctx context.Context, kind, keyword string) ([]product, error) {
	path, params := "vods/search/"+kind, url.Values{"keyword": {keyword}}
	return cached(p.cache, path+"?"+params.Encode(), func() ([]product, error) {
		var res struct {
			Items []product `json:"items"`
		}
		err := p.get(ctx, path, params, &res)
		return res.Items, err
	})
}

// Stream URLs embed the requester's IP and expire; resolve at download time, never cache.
func (p *Provider) Resolve(ctx context.Context, id string) (provider.Stream, error) {
	if _, err := strconv.ParseInt(id, 10, 64); err != nil {
		return provider.Stream{}, fmt.Errorf("tvp: invalid id %q", id)
	}
	var pl struct {
		Sources map[string][]struct {
			Src string `json:"src"`
		} `json:"sources"`
		DRM       json.RawMessage    `json:"drm"`
		Subtitles []playlistSubtitle `json:"subtitles"`
	}
	err := p.get(ctx, id+"/videos/playlist", url.Values{"videoType": {"MOVIE"}}, &pl)
	var apiErr *apiError
	if errors.As(err, &apiErr) && apiErr.permanent() {
		return provider.Stream{}, fmt.Errorf("%w: tvp %s", provider.ErrUnavailable, apiErr.reason())
	}
	if err != nil {
		return provider.Stream{}, err
	}
	if len(pl.DRM) > 0 && string(pl.DRM) != "null" {
		return provider.Stream{}, fmt.Errorf("%w: DRM-protected", provider.ErrUnavailable)
	}
	hls := pl.Sources["HLS"]
	if len(hls) == 0 || hls[0].Src == "" {
		return provider.Stream{}, fmt.Errorf("%w: no HLS source", provider.ErrUnavailable)
	}
	s := provider.Stream{URL: hls[0].Src, Header: http.Header{"User-Agent": {userAgent}}}
	for _, sub := range pl.Subtitles {
		if sub.URL != "" {
			s.Subtitles = append(s.Subtitles, sub.subtitle())
		}
	}
	return s, nil
}

// DLA_NIESLYSZACYCH in the language name marks SDH; isoCode gives the language code.
type playlistSubtitle struct {
	URL      string `json:"url"`
	Language string `json:"language"`
	ISOCode  string `json:"isoCode"` // ISO 639-2
}

func (s playlistSubtitle) subtitle() provider.Subtitle {
	return provider.Subtitle{
		URL:      s.URL,
		Format:   provider.TTML,
		Language: strings.ToLower(s.ISOCode),
		SDH:      strings.Contains(s.Language, "NIESLYSZACYCH"),
	}
}

type apiError struct {
	status int
	code   string
}

func (e *apiError) Error() string { return "tvp: " + e.reason() }

func (e *apiError) permanent() bool {
	return e.status >= 400 && e.status < 500 &&
		e.status != http.StatusRequestTimeout && e.status != http.StatusTooManyRequests
}

func (e *apiError) reason() string {
	if e.code != "" {
		return fmt.Sprintf("%s (HTTP %d)", e.code, e.status)
	}
	return fmt.Sprintf("HTTP %d", e.status)
}

func (p *Provider) get(ctx context.Context, path string, params url.Values, out any) error {
	if params == nil {
		params = url.Values{}
	}
	params.Set("lang", "pl")
	params.Set("platform", "BROWSER")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/"+path+"?"+params.Encode(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("tvp: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("tvp: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var e struct {
			Code string `json:"code"`
		}
		json.Unmarshal(body, &e)
		return &apiError{status: resp.StatusCode, code: e.Code}
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("tvp: decoding %s: %w", path, err)
	}
	return nil
}

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
