package bbc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/combor/magnetowid/internal/provider"
)

// Skyhook, Sonarr's metadata service, supplies Sonarr's titles and TVDB's
// episodes, which are matched to BBC's by title, air date and number.

const (
	skyhookURL       = "https://skyhook.sonarr.tv/v1/tvdb/shows/en"
	skyhookSearchURL = "https://skyhook.sonarr.tv/v1/tvdb/search/en/"
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
	client    *http.Client
	url       string
	searchURL string

	mu    sync.Mutex
	shows map[int]cachedLookup[series]
	ids   map[string]cachedLookup[int] // by normalized title; 0 if none
}

type cachedLookup[V any] struct {
	value   V
	err     error
	expires time.Time
}

func newTitleLookup(client *http.Client) *titleLookup {
	return &titleLookup{client: client, url: skyhookURL, searchURL: skyhookSearchURL,
		shows: make(map[int]cachedLookup[series]), ids: make(map[string]cachedLookup[int])}
}

func (l *titleLookup) series(ctx context.Context, tvdbID int) (series, error) {
	return remember(ctx, &l.mu, l.shows, tvdbID, func(ctx context.Context) (series, error) {
		return l.lookup(ctx, tvdbID)
	})
}

// Cache results for a day and failures for 5 minutes, but not caller cancellation.
func remember[K comparable, V any](ctx context.Context, mu *sync.Mutex, cache map[K]cachedLookup[V], key K, lookup func(context.Context) (V, error)) (V, error) {
	now := time.Now()
	mu.Lock()
	hit, ok := cache[key]
	mu.Unlock()
	if ok && now.Before(hit.expires) {
		return hit.value, hit.err
	}
	lookupCtx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	v, err := lookup(lookupCtx)
	ttl := cacheTTL
	if err != nil {
		if ctx.Err() != nil {
			return v, err
		}
		ttl = failureTTL
	}
	mu.Lock()
	for k, e := range cache {
		if now.After(e.expires) {
			delete(cache, k)
		}
	}
	cache[key] = cachedLookup[V]{value: v, err: err, expires: now.Add(ttl)}
	mu.Unlock()
	return v, err
}

// resolve returns the TVDB series named title as Sonarr cleans titles, e.g.
// "Doctor Who 2023" for Doctor Who (2023) or "Traitors" for The Traitors.
// Alternative titles count only if no main title matches. ok is false unless
// exactly one series matches: "Traitors" also names Channel 4's drama.
func (l *titleLookup) resolve(ctx context.Context, title string) (tvdbID int, ok bool, err error) {
	want := provider.NormalizeTitle(title)
	if want == "" {
		return 0, false, nil
	}
	id, err := remember(ctx, &l.mu, l.ids, want, func(ctx context.Context) (int, error) {
		var found []struct {
			TVDBID       int      `json:"tvdbId"`
			Title        string   `json:"title"`
			Alternatives []titled `json:"alternativeTitles"`
		}
		if err := l.get(ctx, l.searchURL+"?"+url.Values{"term": {title}}.Encode(), &found); err != nil {
			return 0, err
		}
		var byTitle, byAlias []int
		for _, s := range found {
			switch {
			case provider.NormalizeTitle(s.Title) == want:
				byTitle = append(byTitle, s.TVDBID)
			case slices.ContainsFunc(s.Alternatives, func(a titled) bool { return provider.NormalizeTitle(a.Title) == want }):
				byAlias = append(byAlias, s.TVDBID)
			}
		}
		if len(byTitle) == 0 {
			byTitle = byAlias
		}
		if len(byTitle) != 1 {
			return 0, nil
		}
		return byTitle[0], nil
	})
	return id, id > 0, err
}

func (l *titleLookup) lookup(ctx context.Context, tvdbID int) (series, error) {
	var show struct {
		Title        string   `json:"title"`
		Alternatives []titled `json:"alternativeTitles"`
		Episodes     []struct {
			SeasonNumber  int    `json:"seasonNumber"`
			EpisodeNumber int    `json:"episodeNumber"`
			Title         string `json:"title"`
			AirDate       string `json:"airDate"`
		} `json:"episodes"`
	}
	if err := l.get(ctx, l.url+"/"+strconv.Itoa(tvdbID), &show); err != nil {
		return series{}, err
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

type titled struct {
	Title string `json:"title"`
}

func (l *titleLookup) get(ctx context.Context, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "magnetowid (https://github.com/combor/magnetowid)")
	resp, err := l.client.Do(req)
	if err != nil {
		return fmt.Errorf("skyhook: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("skyhook: HTTP %d", resp.StatusCode)
	}
	// Skyhook lists every episode: about 2 MB for EastEnders.
	if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(out); err != nil {
		return fmt.Errorf("skyhook: %w", err)
	}
	return nil
}
