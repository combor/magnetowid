package tvp

import (
	"context"
	"net/url"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/combor/vodarr/internal/provider"
)

// The feeds offer new episodes of the watched series to Sonarr's RSS sync,
// and new films that Radarr has searched for to Radarr's.
//
// TVP doesn't list new episodes, and mapping a TVP episode back to TVDB's
// numbering can name it as another episode, so the series feed runs the
// search Sonarr would for each episode that TVDB says has just aired. TVP
// does list its newest products, films among them, so the film feed matches
// those against the films Radarr searched for.

const (
	recentWindow = 14 * 24 * time.Hour
	// airDateSlack lets in episodes whose TVDB air time is a little late.
	// TVP hides episodes until they are free, at broadcast.
	airDateSlack = 24 * time.Hour
	feedTTL      = 10 * time.Minute
	feedTimeout  = 5 * time.Minute
	// newestProducts is how many of TVP's newest products the film feed
	// reads: about two and a half weeks' worth, two thirds of them films.
	newestProducts = 100
	// filmsKey holds the film feed's releases, which one listing finds.
	filmsKey = 0
)

// feed holds the releases found by the last rebuild. RSS sync can't wait
// for TVP, so rebuilds run in the background.
type feed struct {
	rebuild func(context.Context) // rebuildSeries or rebuildFilms; replaced in tests

	mu        sync.Mutex
	found     map[int][]provider.Release // by TVDB ID, or under filmsKey
	firstSeen map[string]time.Time       // by TVP ID
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

// Recent returns the new releases found by the last rebuild at once, and
// starts a rebuild if that one is out of date. The first RSS sync after a
// start therefore gets none.
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

// publish replaces the feed's releases with those found, and keeps the ones
// found before under the keys that failed. tried are the series the rebuild
// got to. It returns how many releases the feed has.
//
// A release is dated when a rebuild first found it, not by TVP: TVP lists
// premieres days early as paid, and dates them then. Sonarr's RSS sync reads
// further pages only until one holds a release older than the newest it saw
// last time, so a release dated by TVP could sort onto a page it never reads
// once it turns free.
func (f *feed) publish(found map[int][]provider.Release, tried, failed []int) int {
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
		if rs := f.found[id]; len(rs) > 0 {
			found[id] = rs
			for _, r := range rs {
				firstSeen[r.ID] = r.Published
			}
		}
	}
	f.found, f.firstSeen, f.built = found, firstSeen, done
	return len(firstSeen)
}

// rebuildSeries looks for new episodes of every watched series. A series it
// can't look through keeps the releases found before.
func (p *Provider) rebuildSeries(ctx context.Context) {
	f := &p.seriesFeed
	f.mu.Lock()
	f.stale = false // before reading the list, so no series added later is missed
	ids := p.watchedSeries.ids()
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
	n := f.publish(found, tried, failed)
	p.log.Debug("rebuilt TVP series feed", "series", len(ids), "releases", n, "took", time.Since(now).Round(time.Second))
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

// rebuildFilms looks for the watched films among TVP's newest products. If
// it can't, the feed keeps the films found before.
func (p *Provider) rebuildFilms(ctx context.Context) {
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
	n := p.filmFeed.publish(found, nil, failed)
	p.log.Debug("rebuilt TVP film feed", "watched", len(watched), "releases", n, "took", time.Since(start).Round(time.Second))
}

// newFilms returns the free films among TVP's newest products that match a
// watched film, named as Radarr searched for them. Of several watched films
// that match, the one watched first names the release. A film whose original
// title can't be looked up is left out until the next rebuild.
func (p *Provider) newFilms(ctx context.Context, watched []watchRecord) ([]provider.Release, error) {
	var res struct {
		Items []product `json:"items"`
	}
	params := url.Values{"sort": {"createdAt"}, "order": {"desc"}, "maxResults": {strconv.Itoa(newestProducts)}}
	if err := p.get(ctx, "vods", params, &res); err != nil {
		return nil, err
	}
	normalized := make([]string, len(watched))
	for i, w := range watched {
		normalized[i] = provider.NormalizeTitle(w.Title)
	}
	match := func(v product) (watchRecord, bool) {
		for i, w := range watched {
			if filmMatches(v, normalized[i], w.Year) {
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
		// Radarr may have searched with the original title, which only
		// TVP's search gives. It is looked up only if the year could match.
		if !known && slices.ContainsFunc(watched, func(w watchRecord) bool { return yearFits(v, w.Year) }) {
			lookups = append(lookups, v)
		}
	}

	// Films not tried for longest go first, so lookups that keep failing
	// can't use up every rebuild's time. One cut short by the rebuild's end
	// wasn't tried.
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

// originalTitle looks the film's original title up with TVP's search: "" if
// it has none. found is false if the search doesn't list the film yet.
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
