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
	s, err := p.titles.series(ctx, tvdbID)
	if err != nil {
		p.log.Warn("TVDB lookup failed; Sonarr will search by title", "tvdbid", tvdbID, "err", err)
		return "", nil, nil
	}
	m, err := p.searchTitles(ctx, s, q)
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

type titleMatch struct {
	items       []provider.Item
	tvpTitle    string // the title that found items
	serialFound bool   // with or without the episode
}

// Suppress per-search logs here: feed rebuilds call this for every recent episode.
func (p *Provider) searchTitles(ctx context.Context, s series, q provider.Query) (titleMatch, error) {
	var m titleMatch
	tried := map[string]bool{}
	for _, title := range append(slices.Clone(s.polish), s.title) {
		norm := provider.NormalizeTitle(title)
		if norm == "" || tried[norm] {
			continue
		}
		tried[norm] = true
		serial, ok, err := p.findSerial(ctx, title)
		if err != nil {
			return titleMatch{}, err
		}
		if !ok {
			continue
		}
		m.serialFound = true
		items, err := p.serialEpisodes(ctx, serial, q, &s)
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

type series struct {
	title    string   // Sonarr's
	polish   []string // Wikidata's, possibly none
	episodes []tvdbEpisode
}

// Excludes specials.
type tvdbEpisode struct {
	season, episode int
	aired           time.Time // zero if not yet scheduled
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
	series  series
	err     error
	expires time.Time
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
	now := time.Now()
	l.mu.Lock()
	hit, ok := l.cache[tvdbID]
	l.mu.Unlock()
	if ok && now.Before(hit.expires) {
		return hit.series, hit.err
	}

	lookupCtx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	s, err := l.lookup(lookupCtx, tvdbID)
	ttl := cacheTTL
	if err != nil {
		if ctx.Err() != nil {
			return s, err // caller cancellation is not a lookup failure
		}
		ttl = failureTTL
	}
	l.mu.Lock()
	for k, v := range l.cache {
		if now.After(v.expires) {
			delete(l.cache, k)
		}
	}
	l.cache[tvdbID] = cachedSeries{series: s, err: err, expires: now.Add(ttl)}
	l.mu.Unlock()
	return s, err
}

func (l *titleLookup) lookup(ctx context.Context, tvdbID int) (series, error) {
	var show struct {
		Title    string `json:"title"`
		ImdbID   string `json:"imdbId"`
		Episodes []struct {
			SeasonNumber          int    `json:"seasonNumber"`
			EpisodeNumber         int    `json:"episodeNumber"`
			AbsoluteEpisodeNumber int    `json:"absoluteEpisodeNumber"`
			Title                 string `json:"title"`
			AirDateUtc            string `json:"airDateUtc"`
		} `json:"episodes"`
	}
	if err := l.get(ctx, l.skyhookURL+"/"+strconv.Itoa(tvdbID), &show); err != nil {
		return series{}, fmt.Errorf("skyhook: %w", err)
	}
	if show.Title == "" {
		return series{}, fmt.Errorf("skyhook: TVDB %d has no title", tvdbID)
	}
	s := series{title: show.Title}
	for _, e := range show.Episodes {
		if e.SeasonNumber <= 0 || e.EpisodeNumber <= 0 {
			continue // specials
		}
		aired, _ := time.Parse(time.RFC3339, e.AirDateUtc)
		s.episodes = append(s.episodes, tvdbEpisode{
			season:  e.SeasonNumber,
			episode: e.EpisodeNumber,
			aired:   aired,
			number:  agreed(max(e.AbsoluteEpisodeNumber, 0), titleNumber(e.Title)),
		})
	}

	// Fall back to IMDb when Wikidata has no TVDB match.
	claims := []string{"P4835=" + strconv.Itoa(tvdbID)}
	if isIMDbID(show.ImdbID) {
		claims = append(claims, "P345="+show.ImdbID)
	}
	for _, claim := range claims {
		titles, found, err := l.polishLabels(ctx, claim)
		if err != nil {
			return series{}, fmt.Errorf("wikidata: %w", err)
		}
		if found {
			s.polish = titles
			break
		}
	}
	return s, nil
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
	if err := l.get(ctx, l.wikidataURL+"?"+v.Encode(), &res); err != nil {
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

func (l *titleLookup) get(ctx context.Context, url string, out any) error {
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
