package tvp

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/combor/vodarr/internal/provider"
)

// The feed offers new episodes of the watched series to Sonarr's RSS sync.
// TVP doesn't list new episodes, and mapping a TVP episode back to TVDB's
// numbering can name it as another episode, so the feed runs the search
// Sonarr would for each episode that TVDB says has just aired.

const (
	recentWindow = 14 * 24 * time.Hour
	// airDateSlack lets in episodes whose TVDB air time is a little late.
	// TVP hides episodes until they are free, at broadcast.
	airDateSlack = 24 * time.Hour
	feedTTL      = 10 * time.Minute
	feedTimeout  = 5 * time.Minute
)

// feed holds the releases found by the last rebuild. RSS sync can't wait
// for TVP, so rebuilds run in the background.
type feed struct {
	rebuild func(context.Context) // Provider.rebuildFeed; replaced in tests

	mu        sync.Mutex
	series    map[int][]provider.Release // by TVDB ID
	firstSeen map[string]time.Time       // by TVP episode ID
	tried     map[int]time.Time          // when a rebuild last began on each series
	built     time.Time                  // when the last rebuild finished
	stale     bool                       // a series was added since then
	building  bool
}

func (f *feed) markStale() {
	f.mu.Lock()
	f.stale = true
	f.mu.Unlock()
}

// Recent returns the new episodes found by the last rebuild at once, and
// starts a rebuild if that one is out of date. The first RSS sync after a
// start therefore gets none.
func (p *Provider) Recent(_ context.Context, kind provider.Kind) ([]provider.Release, error) {
	if kind != provider.Episode {
		return nil, nil
	}
	f := &p.feed
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
	for _, rs := range f.series {
		out = append(out, rs...)
	}
	return out, nil
}

// rebuildFeed looks for new episodes of every watched series. A series it
// can't look through keeps the releases found before.
//
// A release is dated when a rebuild first found it, not by TVP: TVP lists
// premieres days early as paid, and dates them then. Sonarr's RSS sync reads
// further pages only until one holds a release older than the newest it saw
// last time, so an episode dated by TVP could sort onto a page it never reads
// once it turns free.
func (p *Provider) rebuildFeed(ctx context.Context) {
	f := &p.feed
	f.mu.Lock()
	f.stale = false // before reading the list, so no series added later is missed
	ids := p.watched.ids()
	// Series not tried for longest go first, so a rebuild that runs out of
	// time doesn't leave out the same ones every time.
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

	f.mu.Lock()
	defer f.mu.Unlock()
	// The time this rebuild finished is after anything offered while it ran,
	// so its new releases are newer than any Sonarr has seen.
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
			seen, ok := f.firstSeen[rs[i].ID]
			if !ok {
				seen = done
			}
			rs[i].Published = seen
			firstSeen[rs[i].ID] = seen
		}
	}
	for _, id := range failed {
		if rs := f.series[id]; len(rs) > 0 {
			found[id] = rs
			for _, r := range rs {
				firstSeen[r.ID] = r.Published
			}
		}
	}
	f.series, f.firstSeen, f.built = found, firstSeen, done
	p.log.Debug("rebuilt TVP feed", "series", len(ids), "releases", len(firstSeen), "took", time.Since(now).Round(time.Second))
}

// recentEpisodes searches for the series' episodes that aired within
// recentWindow of now.
func (p *Provider) recentEpisodes(ctx context.Context, tvdbID int, now time.Time) ([]provider.Release, error) {
	s, err := p.titles.series(ctx, tvdbID)
	if err != nil {
		return nil, err
	}
	var out []provider.Release
	for _, e := range s.episodes {
		if e.aired.Before(now.Add(-recentWindow)) || e.aired.After(now.Add(airDateSlack)) {
			continue
		}
		m, err := p.searchTitles(ctx, s, provider.Query{Kind: provider.Episode, Season: e.season, Episode: e.episode})
		if err != nil {
			return nil, err
		}
		for _, it := range m.items {
			out = append(out, provider.Release{Title: s.title, Item: it})
		}
	}
	return out, nil
}
