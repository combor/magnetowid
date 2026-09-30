package overrides

import (
	"cmp"
	"maps"
	"slices"
	"strings"

	"github.com/combor/magnetowid/internal/provider"
)

// Return the override to save: titles trimmed and deduplicated, seasons
// sorted, and site IDs taken from URLs.
func checkSeries(tvdbID int, o provider.SeriesOverride, site provider.Overridable) (provider.SeriesOverride, error) {
	if tvdbID <= 0 {
		return provider.SeriesOverride{}, invalid("TVDB ID %d is not positive", tvdbID)
	}
	var out provider.SeriesOverride
	var err error
	if out.Titles, err = checkTitles(o.Titles); err != nil {
		return provider.SeriesOverride{}, err
	}
	if o.ID != "" {
		if out.ID, err = site.ParseID(o.ID); err != nil {
			return provider.SeriesOverride{}, invalid("id: %v", err)
		}
	}
	seen := make(map[int]bool)
	for _, r := range o.Seasons {
		switch {
		case r.Season < 1:
			return provider.SeriesOverride{}, invalid("seasons: season %d is not a numbered TVDB season", r.Season)
		case r.SiteSeason < 0:
			return provider.SeriesOverride{}, invalid("seasons: site_season %d is negative", r.SiteSeason)
		case seen[r.Season]:
			return provider.SeriesOverride{}, invalid("seasons: season %d has two rules", r.Season)
		}
		seen[r.Season] = true
	}
	out.Seasons = slices.SortedFunc(slices.Values(o.Seasons), func(a, b provider.SeasonRule) int {
		return cmp.Compare(a.Season, b.Season)
	})
	if len(o.Episodes) > 0 {
		out.Episodes = make(map[provider.EpisodeNumber]string, len(o.Episodes))
		pinned := make(map[string]provider.EpisodeNumber)
		numbers := slices.SortedFunc(maps.Keys(o.Episodes), func(a, b provider.EpisodeNumber) int {
			return cmp.Or(cmp.Compare(a.Season, b.Season), cmp.Compare(a.Episode, b.Episode))
		})
		for _, n := range numbers {
			// Season 0 holds specials.
			if n.Season < 0 || n.Episode < 1 {
				return provider.SeriesOverride{}, invalid("episodes: %s is not a numbered TVDB episode", n)
			}
			id, err := site.ParseID(o.Episodes[n])
			if err != nil {
				return provider.SeriesOverride{}, invalid("episodes: %s: %v", n, err)
			}
			if other, dup := pinned[id]; dup {
				return provider.SeriesOverride{}, invalid("episodes: %s and %s both name %s", other, n, id)
			}
			pinned[id] = n
			out.Episodes[n] = id
		}
	}
	if len(out.Titles) == 0 && out.ID == "" && len(out.Seasons) == 0 && len(out.Episodes) == 0 {
		return provider.SeriesOverride{}, invalid("an override needs titles, an id, seasons or episodes; DELETE removes one")
	}
	return out, nil
}

// The title and year come from the request's path; the override may repeat them.
func checkFilm(title string, year int, o provider.FilmOverride, site provider.Overridable) (provider.FilmOverride, error) {
	title = strings.TrimSpace(title)
	switch {
	case provider.NormalizeTitle(title) == "":
		return provider.FilmOverride{}, invalid("film title %q has no letters or digits", title)
	case year <= 0:
		return provider.FilmOverride{}, invalid("year %d is not positive", year)
	case (o.Title != "" || o.Year != 0) && provider.FilmKey(o.Title, o.Year) != provider.FilmKey(title, year):
		return provider.FilmOverride{}, invalid("the override is for %q (%d), not %q (%d)", o.Title, o.Year, title, year)
	}
	out := provider.FilmOverride{Title: title, Year: year}
	var err error
	if out.Titles, err = checkTitles(o.Titles); err != nil {
		return provider.FilmOverride{}, err
	}
	if o.ID != "" {
		if out.ID, err = site.ParseID(o.ID); err != nil {
			return provider.FilmOverride{}, invalid("id: %v", err)
		}
	}
	if len(out.Titles) == 0 && out.ID == "" {
		return provider.FilmOverride{}, invalid("an override needs titles or an id; DELETE removes one")
	}
	return out, nil
}

func checkTitles(titles []string) ([]string, error) {
	var out []string
	seen := make(map[string]bool)
	for _, t := range titles {
		t = strings.TrimSpace(t)
		n := provider.NormalizeTitle(t)
		if n == "" {
			return nil, invalid("titles: %q has no letters or digits", t)
		}
		if !seen[n] {
			seen[n] = true
			out = append(out, t)
		}
	}
	return out, nil
}
