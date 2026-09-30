package tvp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/combor/magnetowid/internal/provider"
)

// Wikidata supplies Polish search titles; Skyhook supplies Sonarr's release
// titles, air dates, and absolute episode numbers.

const (
	skyhookURL  = "https://skyhook.sonarr.tv/v1/tvdb/shows/en"
	wikidataURL = "https://www.wikidata.org/w/api.php"
	// Wikidata requires an identifying User-Agent.
	botUserAgent = "magnetowid (https://github.com/combor/magnetowid)"
	// Leave Sonarr time to fall back to title search.
	lookupTimeout = 10 * time.Second
)

// Cache stable titles longer than failures to avoid repeating failed season lookups.
const (
	cacheTTL   = 24 * time.Hour
	failureTTL = 5 * time.Minute
)

// Search Polish titles before Sonarr's. Watch matched series even when the
// episode is absent; failed lookups let Sonarr fall back to title search.
func (p *Provider) SearchTVDB(ctx context.Context, tvdbID int, q provider.Query) (string, []provider.Item, error) {
	ov, _ := p.overrides.Load().SeriesFor(tvdbID)
	s, err := p.titles.seriesFor(ctx, tvdbID, len(ov.Titles) == 0)
	if err != nil {
		p.log.Warn("TVDB lookup failed; Sonarr will search by title", "tvdbid", tvdbID, "err", err)
		return "", nil, nil
	}
	var m titleMatch
	if q.AirDate != "" {
		m, err = p.searchAired(ctx, s, ov, q)
	} else {
		m, err = p.searchTitles(ctx, s, ov, q)
	}
	if err != nil {
		return "", nil, err
	}
	if m.serialFound {
		p.watch(tvdbID, s.title)
	}
	if len(m.items) > 0 {
		p.log.Info("found TVP series by TVDB ID", "tvdbid", tvdbID, "title", s.title, "tvp_title", m.tvpTitle)
	}
	return s.title, m.items, nil
}

// Return the only overridden series with Sonarr's title. Only cached lookups
// count: Sonarr's ID search, which precedes its title search, made them.
func (p *Provider) overriddenSeries(title string) (int, bool) {
	o := p.overrides.Load()
	want := provider.NormalizeTitle(title)
	if o == nil || want == "" {
		return 0, false
	}
	found := 0
	for id := range o.Series {
		if s, ok := p.titles.cached(id); ok && provider.NormalizeTitle(s.title) == want {
			if found != 0 {
				return 0, false
			}
			found = id
		}
	}
	return found, found != 0
}

type titleMatch struct {
	items       []provider.Item
	tvpTitle    string // the title that found items
	serialFound bool   // with or without the episode
}

// Suppress per-search logs here: feed rebuilds call this for every recent episode.
func (p *Provider) searchTitles(ctx context.Context, s series, ov provider.SeriesOverride, q provider.Query) (titleMatch, error) {
	var m titleMatch
	for _, title := range searchable(s, ov) {
		serial, ok, err := p.findSerial(ctx, title, ov.ID)
		if err != nil {
			return titleMatch{}, err
		}
		if !ok {
			continue
		}
		m.serialFound = true
		items, err := p.serialEpisodes(ctx, serial, q, &s, ov)
		if err != nil {
			return titleMatch{}, err
		}
		if len(items) > 0 {
			m.items, m.tvpTitle = items, title
			return m, nil
		}
	}
	return m, nil
}

// Return the titles to search TVP for, each once. The override's titles
// replace the Polish titles and Sonarr's.
func searchable(s series, ov provider.SeriesOverride) []string {
	titles := append(slices.Clone(s.polish), s.title)
	if len(ov.Titles) > 0 {
		titles = ov.Titles
	}
	var out []string
	tried := map[string]bool{}
	for _, title := range titles {
		norm := provider.NormalizeTitle(title)
		if norm == "" || tried[norm] {
			continue
		}
		tried[norm] = true
		out = append(out, title)
	}
	return out
}

// A daily series' search gives an air date. Search for each TVDB episode
// aired that day, in TVDB's numbering. Without one, still find the serial,
// which a search for a missing episode watches.
func (p *Provider) searchAired(ctx context.Context, s series, ov provider.SeriesOverride, q provider.Query) (titleMatch, error) {
	var aired titleMatch
	searched := false
	for _, e := range s.all() {
		if !q.Wants(e.season, e.episode, e.date) || !placed(ov, e) {
			continue
		}
		searched = true
		m, err := p.searchTitles(ctx, s, ov, provider.Query{Kind: q.Kind, Season: e.season, Episode: e.episode})
		if err != nil {
			return titleMatch{}, err
		}
		aired.items = append(aired.items, m.items...)
		aired.serialFound = aired.serialFound || m.serialFound
		if m.tvpTitle != "" {
			aired.tvpTitle = m.tvpTitle
		}
	}
	if searched {
		return aired, nil
	}
	for _, title := range searchable(s, ov) {
		_, ok, err := p.findSerial(ctx, title, ov.ID)
		if err != nil {
			return titleMatch{}, err
		}
		if ok {
			aired.serialFound = true
			break
		}
	}
	return aired, nil
}

// Report whether TVP could have the episode: only overrides place specials.
func placed(ov provider.SeriesOverride, e tvdbEpisode) bool {
	_, ok := ov.Target(e.season, e.episode)
	return e.season != 0 || ok
}

type series struct {
	title    string   // Sonarr's
	polish   []string // Wikidata's, possibly none
	episodes []tvdbEpisode
	// Season 0, kept apart: only overrides place specials, and their
	// numbers would confuse checks on the others'.
	specials []tvdbEpisode
}

// all returns the episodes, then the specials.
func (s *series) all() []tvdbEpisode {
	return slices.Concat(s.episodes, s.specials)
}

type tvdbEpisode struct {
	season, episode int
	aired           time.Time // zero if not yet scheduled
	date            string    // local air date, e.g. "2026-09-29"; "" if unknown
	// Absolute number agreed by TVDB's number and numeric title;
	// zero if unknown, conflicting if they disagree.
	number int
}

// Safe for concurrent use.
type titleLookup struct {
	client      *http.Client
	skyhookURL  string
	wikidataURL string

	mu    sync.Mutex
	cache map[int]cachedSeries
}

type cachedSeries struct {
	series series
	err    error
	// err is Wikidata's: series has Skyhook's data, without Polish titles.
	noPolish bool
	expires  time.Time
}

func newTitleLookup(client *http.Client) *titleLookup {
	return &titleLookup{
		client:      client,
		skyhookURL:  skyhookURL,
		wikidataURL: wikidataURL,
		cache:       make(map[int]cachedSeries),
	}
}

func (l *titleLookup) series(ctx context.Context, tvdbID int) (series, error) {
	return l.seriesFor(ctx, tvdbID, true)
}

// Without polish, a Wikidata failure leaves out Polish titles instead of
// failing the lookup.
func (l *titleLookup) seriesFor(ctx context.Context, tvdbID int, polish bool) (series, error) {
	c := l.get(ctx, tvdbID)
	if c.noPolish && !polish {
		return c.series, nil
	}
	return c.series, c.err
}

func (l *titleLookup) get(ctx context.Context, tvdbID int) cachedSeries {
	now := time.Now()
	l.mu.Lock()
	hit, ok := l.cache[tvdbID]
	l.mu.Unlock()
	if ok && now.Before(hit.expires) {
		return hit
	}

	lookupCtx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	s, noPolish, err := l.lookup(lookupCtx, tvdbID)
	c := cachedSeries{series: s, err: err, noPolish: noPolish, expires: now.Add(cacheTTL)}
	if err != nil {
		if ctx.Err() != nil {
			return c // caller cancellation is not a lookup failure
		}
		c.expires = now.Add(failureTTL)
	}
	l.mu.Lock()
	for k, v := range l.cache {
		if now.After(v.expires) {
			delete(l.cache, k)
		}
	}
	l.cache[tvdbID] = c
	l.mu.Unlock()
	return c
}

// cached returns an unexpired lookup of Sonarr's title without making one.
func (l *titleLookup) cached(tvdbID int) (series, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	hit, ok := l.cache[tvdbID]
	return hit.series, ok && (hit.err == nil || hit.noPolish) && time.Now().Before(hit.expires)
}

// noPolish reports a Wikidata failure, which leaves s without Polish titles.
func (l *titleLookup) lookup(ctx context.Context, tvdbID int) (s series, noPolish bool, err error) {
	var show struct {
		Title    string `json:"title"`
		ImdbID   string `json:"imdbId"`
		Episodes []struct {
			SeasonNumber          int    `json:"seasonNumber"`
			EpisodeNumber         int    `json:"episodeNumber"`
			AbsoluteEpisodeNumber int    `json:"absoluteEpisodeNumber"`
			Title                 string `json:"title"`
			AirDate               string `json:"airDate"`
			AirDateUtc            string `json:"airDateUtc"`
		} `json:"episodes"`
	}
	if err := l.fetch(ctx, l.skyhookURL+"/"+strconv.Itoa(tvdbID), &show); err != nil {
		return series{}, false, fmt.Errorf("skyhook: %w", err)
	}
	if show.Title == "" {
		return series{}, false, fmt.Errorf("skyhook: TVDB %d has no title", tvdbID)
	}
	s = series{title: show.Title}
	for _, e := range show.Episodes {
		if e.SeasonNumber < 0 || e.EpisodeNumber <= 0 {
			continue
		}
		aired, _ := time.Parse(time.RFC3339, e.AirDateUtc)
		ep := tvdbEpisode{season: e.SeasonNumber, episode: e.EpisodeNumber, aired: aired, date: e.AirDate}
		if e.SeasonNumber == 0 {
			s.specials = append(s.specials, ep)
			continue
		}
		ep.number = agreed(max(e.AbsoluteEpisodeNumber, 0), titleNumber(e.Title))
		s.episodes = append(s.episodes, ep)
	}

	// Fall back to IMDb when Wikidata has no TVDB match.
	claims := []string{"P4835=" + strconv.Itoa(tvdbID)}
	if isIMDbID(show.ImdbID) {
		claims = append(claims, "P345="+show.ImdbID)
	}
	for _, claim := range claims {
		titles, found, err := l.polishLabels(ctx, claim)
		if err != nil {
			return s, true, fmt.Errorf("wikidata: %w", err)
		}
		if found {
			s.polish = titles
			break
		}
	}
	return s, false, nil
}

// claim is a Wikidata property match, e.g. P4835=83920. The boolean reports any matching items.
func (l *titleLookup) polishLabels(ctx context.Context, claim string) ([]string, bool, error) {
	v := url.Values{
		"action":        {"query"},
		"format":        {"json"},
		"formatversion": {"2"},
		"generator":     {"search"},
		"gsrsearch":     {"haswbstatement:" + claim},
		"gsrlimit":      {"5"},
		"prop":          {"entityterms"},
		"wbetterms":     {"label"},
		"uselang":       {"pl"}, // no fallback: an item without a Polish label has none
	}
	var res struct {
		Error *struct {
			Info string `json:"info"`
		} `json:"error"`
		Query struct {
			Pages []struct {
				Terms struct {
					Label []string `json:"label"`
				} `json:"entityterms"`
			} `json:"pages"`
		} `json:"query"`
	}
	if err := l.fetch(ctx, l.wikidataURL+"?"+v.Encode(), &res); err != nil {
		return nil, false, err
	}
	if res.Error != nil {
		return nil, false, errors.New(res.Error.Info)
	}
	var titles []string
	for _, p := range res.Query.Pages {
		titles = append(titles, p.Terms.Label...)
	}
	return titles, len(res.Query.Pages) > 0, nil
}

// Validate before embedding an IMDb ID in a Wikidata search.
func isIMDbID(s string) bool {
	digits, ok := strings.CutPrefix(s, "tt")
	if !ok || digits == "" {
		return false
	}
	for _, r := range digits {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func (l *titleLookup) fetch(ctx context.Context, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", botUserAgent)
	req.Header.Set("Accept", "application/json")
	resp, err := l.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	// Skyhook lists every episode: ~0.4 MB for a long soap.
	return json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(out)
}
