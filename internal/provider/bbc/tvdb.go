package bbc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/combor/magnetowid/internal/provider"
)

// Skyhook, Sonarr's metadata service, supplies Sonarr's titles and TVDB's
// episodes, which are matched to BBC's by title, air date and number.

const (
	skyhookURL = "https://skyhook.sonarr.tv/v1/tvdb/shows/en"
	// Leave Sonarr time to fall back to title search.
	lookupTimeout = 10 * time.Second
	cacheTTL      = 24 * time.Hour
	failureTTL    = 5 * time.Minute
)

// A failed lookup returns no results, so Sonarr falls back to title search.
// Series found on iPlayer are watched for new episodes, even without the
// requested one.
func (p *Provider) SearchTVDB(ctx context.Context, tvdbID int, q provider.Query) (string, []provider.Item, error) {
	s, err := p.titles.series(ctx, tvdbID)
	if err != nil {
		p.log.Warn("TVDB lookup failed; Sonarr will search by title", "tvdbid", tvdbID, "err", err)
		return "", nil, nil
	}
	matched, found, err := p.matchSeries(ctx, s)
	if err != nil || !found {
		return "", nil, err
	}
	p.watch(tvdbID, s.title)
	var items []provider.Item
	for _, m := range matched {
		if m.tvdb.season == q.Season && (q.Episode == 0 || m.tvdb.episode == q.Episode) {
			items = append(items, episodeItem(m.bbc, m.tvdb.season, m.tvdb.episode))
		}
	}
	return s.title, items, nil
}

// Search Sonarr's title first: TVDB's aliases can name other versions, e.g.
// "The Traitors" for The Traitors (US). Of programmes with the same title,
// choose the one matching most episodes. found reports whether iPlayer has
// the series, even if no episode matches.
func (p *Provider) matchSeries(ctx context.Context, s series) (matched []pair, found bool, err error) {
	for _, title := range s.searchTitles() {
		shows, err := p.findShows(ctx, title)
		if err != nil || len(shows) == 0 {
			if err != nil {
				return nil, false, err
			}
			continue
		}
		for _, show := range shows {
			eps, err := p.episodes(ctx, show.ID)
			if err != nil {
				return nil, false, err
			}
			if m := match(s.episodes, eps); len(m) > len(matched) {
				matched = m
			}
		}
		return matched, true, nil
	}
	return nil, false, nil
}

type series struct {
	title    string // Sonarr's
	aliases  []string
	episodes []tvdbEpisode
}

type tvdbEpisode struct {
	season, episode int // season 0 holds specials
	title           string
	aired           string // local date, e.g. "2025-05-31"; "" if unknown
}

var (
	// TVDB qualifies titles, e.g. "Doctor Who (2023)" or "The Office (UK)";
	// iPlayer doesn't. Other countries' qualifiers stay: BBC has "The Traitors US".
	qualifier = regexp.MustCompile(`\s*\((?:\d{4}|UK|GB)\)$`)
	country   = regexp.MustCompile(`\([A-Z]{2}\)$`)
)

// Another country's version, e.g. "Ghosts (US)", must not find the UK's
// through aliases such as "Ghosts".
func (s series) searchTitles() []string {
	all := []string{s.title}
	if !country.MatchString(qualifier.ReplaceAllString(s.title, "")) {
		all = append(all, s.aliases...)
	}
	var titles []string
	seen := map[string]bool{}
	for _, t := range all {
		t = qualifier.ReplaceAllString(t, "")
		if n := provider.NormalizeTitle(t); n != "" && !seen[n] {
			seen[n] = true
			titles = append(titles, t)
		}
	}
	return titles
}

// Safe for concurrent use.
type titleLookup struct {
	client *http.Client
	url    string

	mu    sync.Mutex
	cache map[int]cachedSeries
}

type cachedSeries struct {
	series  series
	err     error
	expires time.Time
}

func newTitleLookup(client *http.Client) *titleLookup {
	return &titleLookup{client: client, url: skyhookURL, cache: make(map[int]cachedSeries)}
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
		Title        string `json:"title"`
		Alternatives []struct {
			Title string `json:"title"`
		} `json:"alternativeTitles"`
		Episodes []struct {
			SeasonNumber  int    `json:"seasonNumber"`
			EpisodeNumber int    `json:"episodeNumber"`
			Title         string `json:"title"`
			AirDate       string `json:"airDate"`
		} `json:"episodes"`
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, l.url+"/"+strconv.Itoa(tvdbID), nil)
	if err != nil {
		return series{}, err
	}
	req.Header.Set("User-Agent", "magnetowid (https://github.com/combor/magnetowid)")
	resp, err := l.client.Do(req)
	if err != nil {
		return series{}, fmt.Errorf("skyhook: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return series{}, fmt.Errorf("skyhook: HTTP %d", resp.StatusCode)
	}
	// Skyhook lists every episode: about 2 MB for EastEnders.
	if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&show); err != nil {
		return series{}, fmt.Errorf("skyhook: %w", err)
	}
	if show.Title == "" {
		return series{}, fmt.Errorf("skyhook: TVDB %d has no title", tvdbID)
	}
	s := series{title: show.Title}
	for _, a := range show.Alternatives {
		if a.Title != "" {
			s.aliases = append(s.aliases, a.Title)
		}
	}
	for _, e := range show.Episodes {
		if e.SeasonNumber < 0 || e.EpisodeNumber <= 0 {
			continue
		}
		s.episodes = append(s.episodes, tvdbEpisode{
			season: e.SeasonNumber, episode: e.EpisodeNumber,
			title: strings.TrimSpace(e.Title), aired: e.AirDate,
		})
	}
	return s, nil
}
