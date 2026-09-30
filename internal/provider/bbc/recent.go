package bbc

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/combor/magnetowid/internal/provider"
)

// Feeds offer episodes of watched series that iPlayer made available within
// two weeks, and watched films among the 200 it added most recently.

const (
	recentWindow = 14 * 24 * time.Hour
	feedTTL      = 10 * time.Minute
	feedTimeout  = 5 * time.Minute
	filmsKey     = 0
)

// Rebuild in the background because RSS requests cannot wait for iPlayer.
type feed struct {
	rebuild func(context.Context) // rebuildSeries or rebuildFilms; replaced in tests

	mu        sync.Mutex
	found     map[int][]provider.Release // by TVDB ID, or under filmsKey
	firstSeen map[string]time.Time       // by release key
	tried     map[int]time.Time          // when a rebuild last began on each series
	built     time.Time
	stale     bool // series added since the last rebuild
	building  bool
	// Counts override changes, which discard rebuilds begun before them.
	generation int
}

func (f *feed) markStale() {
	f.mu.Lock()
	f.stale = true
	f.mu.Unlock()
}

// Drop releases found under superseded overrides, which a failed rebuild
// would otherwise keep offering.
func (f *feed) forget(keys ...int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, k := range keys {
		delete(f.found, k)
	}
	if len(keys) > 0 {
		f.generation++
	}
}

// Recent returns cached releases and starts stale rebuilds asynchronously.
// The first sync after startup is empty.
func (p *Provider) Recent(_ context.Context, kind provider.Kind) ([]provider.Release, error) {
	switch kind {
	case provider.Episode:
		return p.seriesFeed.recent(), nil
	case provider.Movie:
		return p.filmFeed.recent(), nil
	}
	return nil, nil
}

func (f *feed) recent() []provider.Release {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.building && (f.stale || time.Since(f.built) >= feedTTL) {
		f.building = true
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), feedTimeout)
			defer cancel()
			f.rebuild(ctx)
			f.mu.Lock()
			f.building = false
			f.mu.Unlock()
		}()
	}
	var out []provider.Release
	for _, rs := range f.found {
		out = append(out, rs...)
	}
	return out
}

// Keep previous releases for failed keys; tried lists the series reached.
// Date new releases when discovered: a series watched after an episode became
// available would otherwise put it behind Sonarr's RSS cutoff.
// A rebuild begun in an earlier generation publishes nothing; the feed stays
// stale, so the next sync rebuilds it.
func (f *feed) publish(found map[int][]provider.Release, tried, failed []int, generation int) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	if generation != f.generation {
		return 0
	}
	// New releases must postdate anything served during this rebuild.
	done := time.Now()
	if f.tried == nil {
		f.tried = make(map[int]time.Time)
	}
	for _, id := range tried {
		f.tried[id] = done
	}
	firstSeen := make(map[string]time.Time)
	for _, rs := range found {
		for i := range rs {
			seen, ok := f.firstSeen[rs[i].Key()]
			if !ok {
				seen = done
			}
			rs[i].Published = seen
			firstSeen[rs[i].Key()] = seen
		}
	}
	for _, id := range failed {
		if rs := f.found[id]; len(rs) > 0 {
			found[id] = rs
			for _, r := range rs {
				firstSeen[r.Key()] = r.Published
			}
		}
	}
	f.found, f.firstSeen, f.built = found, firstSeen, done
	return len(firstSeen)
}

func (p *Provider) rebuildSeries(ctx context.Context) {
	f := &p.seriesFeed
	f.mu.Lock()
	f.stale = false // before reading the list, so no series added later is missed
	generation := f.generation
	ids := p.watchedSeries.ids()
	// Try the least recently checked series first to avoid starvation on timeouts.
	slices.SortStableFunc(ids, func(a, b int) int { return f.tried[a].Compare(f.tried[b]) })
	f.mu.Unlock()

	start := time.Now()
	found := make(map[int][]provider.Release)
	var failed, tried []int
	for _, id := range ids {
		if ctx.Err() != nil {
			failed = append(failed, id)
			continue
		}
		tried = append(tried, id)
		rs, err := p.recentEpisodes(ctx, id, start)
		if err != nil {
			if ctx.Err() == nil {
				p.log.Warn("can't look for new episodes of a BBC series; offering the ones found before",
					"tvdbid", id, "err", err)
			}
			failed = append(failed, id)
			continue
		}
		if len(rs) > 0 {
			found[id] = rs
		}
	}
	if ctx.Err() != nil {
		p.log.Warn("ran out of time looking for new BBC episodes; the series left go first next time",
			"series", len(ids), "tried", len(tried))
	}
	n := f.publish(found, tried, failed, generation)
	p.log.Debug("rebuilt BBC series feed", "series", len(ids), "releases", n, "took", time.Since(start).Round(time.Second))
}

func (p *Provider) recentEpisodes(ctx context.Context, tvdbID int, now time.Time) ([]provider.Release, error) {
	s, err := p.titles.series(ctx, tvdbID)
	if err != nil {
		return nil, err
	}
	ov, _ := p.overrides.Load().SeriesFor(tvdbID)
	matched, _, err := p.matchSeries(ctx, s, ov)
	if err != nil {
		return nil, err
	}
	var out []provider.Release
	for _, m := range matched {
		if since := m.bbc.available(); since.After(now.Add(-recentWindow)) && !since.After(now) {
			it := episodeItem(m.bbc, m.tvdb.season, m.tvdb.episode)
			out = append(out, provider.Release{Title: s.title, Item: it})
		}
	}
	return out, nil
}

func (p *Provider) rebuildFilms(ctx context.Context) {
	p.filmFeed.mu.Lock()
	p.filmFeed.stale = false // before reading overrides, so no later change is missed
	generation := p.filmFeed.generation
	p.filmFeed.mu.Unlock()
	start := time.Now()
	watched := p.watchedFilms.all()
	found := make(map[int][]provider.Release)
	var failed []int
	if len(watched) > 0 {
		films, err := p.newestFilms(ctx)
		switch {
		case err != nil:
			p.log.Warn("can't look for new BBC films; offering the ones found before", "err", err)
			failed = []int{filmsKey}
		default:
			if rs := matchFilms(films, watched, p.overrides.Load()); len(rs) > 0 {
				found[filmsKey] = rs
			}
		}
	}
	n := p.filmFeed.publish(found, nil, failed, generation)
	p.log.Debug("rebuilt BBC film feed", "watched", len(watched), "releases", n, "took", time.Since(start).Round(time.Second))
}

// Match each film to the oldest matching watch entry.
func matchFilms(films []programme, watched []watchRecord, o *provider.Overrides) []provider.Release {
	wants := make([]filmWant, len(watched))
	for i, w := range watched {
		wants[i] = newFilmWant(o, w.Title, w.Year)
	}
	var out []provider.Release
	for _, f := range films {
		for i, w := range watched {
			if wants[i].matches(f) {
				out = append(out, provider.Release{Title: w.Title, Item: filmItem(f, w.Year)})
				break
			}
		}
	}
	return out
}
