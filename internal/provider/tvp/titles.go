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

	"github.com/combor/vodarr/internal/provider"
)

// Sonarr's series titles are often English ("Days of Honor"), but TVP only
// knows Polish ones ("Czas honoru"). For Sonarr's search by TVDB ID, Wikidata
// gives the Polish title and Skyhook, where Sonarr gets its titles, the title
// the releases need. Skyhook also gives the air dates the feed goes by.

const (
	skyhookURL  = "https://skyhook.sonarr.tv/v1/tvdb/shows/en"
	wikidataURL = "https://www.wikidata.org/w/api.php"
	// botUserAgent identifies vodarr, as Wikidata requires.
	botUserAgent = "vodarr (https://github.com/combor/vodarr)"
	// lookupTimeout leaves Sonarr time to search by title if a lookup fails.
	lookupTimeout = 10 * time.Second
)

// Titles rarely change. A failed lookup is retried after failureTTL, not on
// every episode of a season search.
const (
	cacheTTL   = 24 * time.Hour
	failureTTL = 5 * time.Minute
)

// SearchTVDB searches TVP for the series with the TVDB ID by its Polish
// titles, then by Sonarr's. A series found on TVP is watched for new
// episodes, even if it lacks this one. A failed lookup is logged and finds
// nothing, so Sonarr searches by title.
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

// titleMatch is what searching TVP by a series' titles found.
type titleMatch struct {
	items       []provider.Item
	tvpTitle    string // the title that found items
	serialFound bool   // with or without the episode
}

// searchTitles searches TVP for the episode by the series' Polish titles,
// then by Sonarr's, until one finds it. It doesn't log, as the feed runs it
// for every new episode.
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
		items, err := p.serialEpisodes(ctx, serial, q)
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

// series holds a series' titles and aired episodes.
type series struct {
	title    string   // Sonarr's
	polish   []string // Wikidata's, possibly none
	episodes []airedEpisode
}

// airedEpisode is an episode with a TVDB air date, in TVDB's numbering.
type airedEpisode struct {
	season, episode int
	aired           time.Time
}

// titleLookup finds series titles by TVDB ID and caches them. It is safe
// for concurrent use.
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
			return s, err // the caller gave up; nothing is known about the series
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
			SeasonNumber  int    `json:"seasonNumber"`
			EpisodeNumber int    `json:"episodeNumber"`
			AirDateUtc    string `json:"airDateUtc"`
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
		aired, err := time.Parse(time.RFC3339, e.AirDateUtc)
		if e.SeasonNumber <= 0 || e.EpisodeNumber <= 0 || err != nil {
			continue // specials, and episodes not yet scheduled
		}
		s.episodes = append(s.episodes, airedEpisode{season: e.SeasonNumber, episode: e.EpisodeNumber, aired: aired})
	}

	// The TVDB ID finds the Wikidata item; the IMDb ID covers items without it.
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

// polishLabels returns the Polish labels of the Wikidata items with the
// claim, e.g. "P4835=83920", and whether there are any such items.
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

// isIMDbID reports whether s is an IMDb title ID, so it is safe in a search.
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
