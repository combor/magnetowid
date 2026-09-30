package tvp

import (
	"context"
	"net/url"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/combor/magnetowid/internal/provider"
)

// TVP has no new-episode feed. Search recently aired TVDB episodes using Sonarr's
// numbering; reversing TVP numbering can select the wrong episode. For films,
// match TVP's newest products against Radarr's watch list.

const (
	recentWindow = 14 * 24 * time.Hour
	// Allow late TVDB air times; TVP hides episodes until broadcast.
	airDateSlack = 24 * time.Hour
	feedTTL      = 10 * time.Minute
	feedTimeout  = 5 * time.Minute
	// About two and a half weeks of products, roughly two thirds films.
	newestProducts = 100
	filmsKey       = 0
)

// Rebuild in the background because RSS requests cannot wait for TVP.
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
// Date new releases when discovered: TVP dates paid premieres early, which
// would put them behind Sonarr's RSS cutoff when they become free.
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

	now := time.Now()
	found := make(map[int][]provider.Release)
	var failed, tried []int
	for _, id := range ids {
		if ctx.Err() != nil {
			failed = append(failed, id)
			continue
		}
		tried = append(tried, id)
		rs, err := p.recentEpisodes(ctx, id, now)
		if err != nil {
			if ctx.Err() == nil {
				p.log.Warn("can't look for new episodes of a TVP series; offering the ones found before",
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
		p.log.Warn("ran out of time looking for new TVP episodes; the series left go first next time",
			"series", len(ids), "tried", len(tried))
	}
	n := f.publish(found, tried, failed, generation)
	p.log.Debug("rebuilt TVP series feed", "series", len(ids), "releases", n, "took", time.Since(now).Round(time.Second))
}

func (p *Provider) recentEpisodes(ctx context.Context, tvdbID int, now time.Time) ([]provider.Release, error) {
	ov, _ := p.overrides.Load().SeriesFor(tvdbID)
	s, err := p.titles.seriesFor(ctx, tvdbID, len(ov.Titles) == 0)
	if err != nil {
		return nil, err
	}
	var out []provider.Release
	for _, e := range s.episodes {
		if e.aired.Before(now.Add(-recentWindow)) || e.aired.After(now.Add(airDateSlack)) {
			continue
		}
		m, err := p.searchTitles(ctx, s, ov, provider.Query{Kind: provider.Episode, Season: e.season, Episode: e.episode})
		if err != nil {
			return nil, err
		}
		for _, it := range m.items {
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
		rs, err := p.newFilms(ctx, watched)
		switch {
		case err != nil:
			p.log.Warn("can't look for new TVP films; offering the ones found before", "err", err)
			failed = []int{filmsKey}
		case len(rs) > 0:
			found[filmsKey] = rs
		}
	}
	n := p.filmFeed.publish(found, nil, failed, generation)
	p.log.Debug("rebuilt TVP film feed", "watched", len(watched), "releases", n, "took", time.Since(start).Round(time.Second))
}

// Match free films to the oldest matching watch entry. Defer films whose
// original title cannot be fetched until the next rebuild.
func (p *Provider) newFilms(ctx context.Context, watched []watchRecord) ([]provider.Release, error) {
	var res struct {
		Items []product `json:"items"`
	}
	params := url.Values{"sort": {"createdAt"}, "order": {"desc"}, "maxResults": {strconv.Itoa(newestProducts)}}
	if err := p.get(ctx, "vods", params, &res); err != nil {
		return nil, err
	}
	wants := make([]filmWant, len(watched))
	for i, w := range watched {
		wants[i] = p.filmWant(w.Title, w.Year)
	}
	match := func(v product) (watchRecord, bool) {
		for i, w := range watched {
			if wants[i].matches(v) {
				return w, true
			}
		}
		return watchRecord{}, false
	}

	var out []provider.Release
	listed := make(map[int64]bool)
	var lookups []product
	for _, v := range res.Items {
		if v.Type != "VOD" || v.Payable {
			continue
		}
		listed[v.ID] = true
		original, known := p.originals[v.ID]
		v.OriginalTitle = original
		if w, ok := match(v); ok {
			out = append(out, provider.Release{Title: w.Title, Item: movieItem(v, w.Year)})
			continue
		}
		// Only search provides original titles. Skip lookups when no watched year matches.
		if !known && slices.ContainsFunc(watched, func(w watchRecord) bool { return yearFits(v, w.Year) }) {
			lookups = append(lookups, v)
		}
	}

	// Try the least recently checked films first; cancelled lookups do not count as attempts.
	slices.SortStableFunc(lookups, func(a, b product) int { return p.lookupTried[a.ID].Compare(p.lookupTried[b.ID]) })
	var lookupErr error
	failed := 0
	for _, v := range lookups {
		if ctx.Err() != nil {
			break
		}
		original, found, err := p.originalTitle(ctx, v)
		if found {
			p.originals[v.ID] = original
			delete(p.lookupTried, v.ID)
			v.OriginalTitle = original
			if w, ok := match(v); ok {
				out = append(out, provider.Release{Title: w.Title, Item: movieItem(v, w.Year)})
			}
			continue
		}
		if ctx.Err() != nil {
			continue
		}
		p.lookupTried[v.ID] = time.Now()
		if err != nil {
			lookupErr = err
			failed++
		}
	}
	if failed > 0 {
		p.log.Warn("can't look up the original titles of new TVP films; trying them again next time",
			"films", failed, "err", lookupErr)
	}
	if ctx.Err() != nil {
		p.log.Warn("ran out of time looking up the original titles of new TVP films; the films left go first next time")
	}
	for id := range p.originals {
		if !listed[id] {
			delete(p.originals, id)
		}
	}
	for id := range p.lookupTried {
		if !listed[id] {
			delete(p.lookupTried, id)
		}
	}
	return out, nil
}

// found=false means TVP search does not list the film yet; an empty title means none was given.
func (p *Provider) originalTitle(ctx context.Context, v product) (original string, found bool, err error) {
	vods, err := p.search(ctx, "VOD", v.Title)
	if err != nil {
		return "", false, err
	}
	for _, r := range vods {
		if r.ID == v.ID {
			return r.OriginalTitle, true, nil
		}
	}
	return "", false, nil
}
