package tvp

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/combor/magnetowid/internal/provider"
)

func pin(season, episode int) provider.EpisodeNumber {
	return provider.EpisodeNumber{Season: season, Episode: episode}
}

func ep(season, episode int) provider.Query {
	return provider.Query{Kind: provider.Episode, Season: season, Episode: episode}
}

// Serve the soap's guide, and UA Ranczo, which searching for Ranczo also finds.
func overriddenProvider(t *testing.T, ov *provider.Overrides) *Provider {
	t.Helper()
	guide, _ := serveGuide(t, hundredsProgrammes)
	p := newProviderWith(t, func(key string) (string, bool) {
		switch key {
		case "/vods/serials/310247/seasons":
			return `[{"id":310248,"number":1}]`, true
		case "/vods/serials/310247/seasons/310248/episodes":
			return `[{"id":310249,"number":1}]`, true
		}
		return guide(key)
	})
	fakeTitles(t, p)
	p.SetOverrides(ov)
	return p
}

func TestSearchTVDBOverrides(t *testing.T) {
	// The Ranch (TVDB 5) is Ranczo: TVP's S2 continues S1's numbering at 14,
	// 381150 (14), 381138 (15) and paid 381151 (16).
	rule := provider.SeasonRule{Season: 2, SiteSeason: 2, Offset: 14}
	tests := []struct {
		name   string
		tvdbID int
		ov     provider.SeriesOverride
		q      provider.Query
		ids    []string
	}{
		{"titles instead of Wikidata's", 6, provider.SeriesOverride{Titles: []string{"Ranczo"}}, ep(1, 1), []string{"381046"}},
		{"id among the search results", 5, provider.SeriesOverride{ID: "310247"}, ep(1, 1), []string{"310249"}},
		{"season rule", 5, provider.SeriesOverride{Seasons: []provider.SeasonRule{rule}}, ep(2, 1), []string{"381138"}},
		{"season rule to a paid episode, without falling back", 5, provider.SeriesOverride{Seasons: []provider.SeasonRule{rule}}, ep(2, 2), nil},
		{"whole season by rule", 5, provider.SeriesOverride{Seasons: []provider.SeasonRule{rule}}, ep(2, 0), []string{"381138"}},
		{"any season", 5, provider.SeriesOverride{Seasons: []provider.SeasonRule{{Season: 2, Offset: 12}}}, ep(2, 2), []string{"381150"}},
		{"pin before rule", 5, provider.SeriesOverride{Seasons: []provider.SeasonRule{rule},
			Episodes: map[provider.EpisodeNumber]string{pin(2, 1): "381046"}}, ep(2, 1), []string{"381046"}},
		{"pinned episode left out of automatic matching", 5, provider.SeriesOverride{
			Episodes: map[provider.EpisodeNumber]string{pin(2, 2): "381150"}}, ep(2, 1), nil},
		{"pin and automatic matching in one season", 5, provider.SeriesOverride{
			Episodes: map[provider.EpisodeNumber]string{pin(2, 2): "381150"}}, ep(2, 0), []string{"381150"}},
		{"rule to an episode pinned elsewhere", 5, provider.SeriesOverride{Seasons: []provider.SeasonRule{{Season: 2, SiteSeason: 2, Offset: 13}},
			Episodes: map[provider.EpisodeNumber]string{pin(2, 2): "381150"}}, ep(2, 1), nil},
		{"pin to a missing episode", 5, provider.SeriesOverride{
			Episodes: map[provider.EpisodeNumber]string{pin(2, 1): "1"}}, ep(2, 1), nil},
		{"uncovered episode", 5, provider.SeriesOverride{Seasons: []provider.SeasonRule{rule}}, ep(1, 13), []string{"381054"}},
		// Automatic matching rejects TVP's 2025 for TVDB's 2016.
		{"soap numbers", 7, provider.SeriesOverride{Seasons: []provider.SeasonRule{{Season: 20, Offset: 1900}}}, ep(20, 1), []string{"11901"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := overriddenProvider(t, &provider.Overrides{Series: map[int]provider.SeriesOverride{tt.tvdbID: tt.ov}})
			_, items, err := p.SearchTVDB(context.Background(), tt.tvdbID, tt.q)
			if err != nil {
				t.Fatal(err)
			}
			var ids []string
			for _, it := range items {
				ids = append(ids, it.ID)
				if it.Season != tt.q.Season || (tt.q.Episode != 0 && it.Episode != tt.q.Episode) {
					t.Errorf("%s named S%02dE%02d", it.ID, it.Season, it.Episode)
				}
			}
			if !slices.Equal(ids, tt.ids) {
				t.Errorf("SearchTVDB found %v, want %v", ids, tt.ids)
			}
		})
	}
}

func TestFeedOverrides(t *testing.T) {
	p := overriddenProvider(t, nil)
	p.watch(5, "The Ranch")
	p.seriesFeed.mu.Lock()
	p.seriesFeed.stale = false
	p.seriesFeed.mu.Unlock()
	p.SetOverrides(&provider.Overrides{Series: map[int]provider.SeriesOverride{
		5: {Seasons: []provider.SeasonRule{{Season: 2, SiteSeason: 2, Offset: 14}}},
	}})
	if !p.seriesFeed.stale || !p.filmFeed.stale {
		t.Error("new overrides left the feeds fresh")
	}
	p.rebuildSeries(context.Background())
	if got, want := describe(feedReleases(p)), "381054 The Ranch S01E13, 381138 The Ranch S02E01"; got != want {
		t.Errorf("feed = %s; want %s", got, want)
	}

	// A release renumbered by a changed override is new to Sonarr.
	first := feedReleases(p)[1].Published
	p.SetOverrides(&provider.Overrides{Series: map[int]provider.SeriesOverride{5: {
		Seasons:  []provider.SeasonRule{{Season: 2, SiteSeason: 2, Offset: 14}},
		Episodes: map[provider.EpisodeNumber]string{pin(1, 13): "381138"},
	}}})
	p.rebuildSeries(context.Background())
	got := feedReleases(p)
	if describe(got) != "381138 The Ranch S01E13" {
		t.Fatalf("feed = %s", describe(got))
	}
	if !got[0].Published.After(first) {
		t.Errorf("S01E13 kept the date %v it had as S02E01", got[0].Published)
	}
}

func TestFilmOverrides(t *testing.T) {
	ov := &provider.Overrides{Films: map[string]provider.FilmOverride{
		provider.FilmKey("Hydro-Puzzle", 1970): {Titles: []string{"Hydrozagadka"}},
		// Złote runo is from 1996.
		provider.FilmKey("Golden Fleece", 2000): {Titles: []string{"Hydrozagadka"}, ID: "350232"},
	}}
	p := overriddenProvider(t, ov)
	for _, tt := range []struct {
		title string
		year  int
		ids   []string
	}{
		{"Hydro-Puzzle", 1970, []string{"296079"}},
		{"Golden Fleece", 2000, []string{"350232"}},
		{"Hydrozagadka", 1970, []string{"296079"}}, // no override
		{"Hydrozagadka", 1980, nil},
	} {
		items, err := p.Search(context.Background(), provider.Query{Kind: provider.Movie, Title: tt.title, Year: tt.year})
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, it := range items {
			ids = append(ids, it.ID)
		}
		if !slices.Equal(ids, tt.ids) {
			t.Errorf("%s (%d): found %v, want %v", tt.title, tt.year, ids, tt.ids)
		}
	}
}

func TestFilmFeedOverrides(t *testing.T) {
	p, _ := filmProvider(t, nil)
	p.watchFilm("Unwanted", 1990)
	p.SetOverrides(&provider.Overrides{Films: map[string]provider.FilmOverride{
		provider.FilmKey("Unwanted", 1990): {Titles: []string{"Niechciany"}},
		provider.FilmKey("Stary", 2018):    {ID: "5"}, // TVP says 2016
	}})
	p.rebuildFilms(context.Background())
	if p.filmFeed.stale {
		t.Error("the feed is stale after a rebuild")
	}
	got := describeFilms(filmReleases(p))
	if want := "1 Kler 2018, 2 Perfume: The Story of a Murderer 2006, 5 Stary 2018, 6 Unwanted 1990"; got != want {
		t.Errorf("feed = %s; want %s", got, want)
	}
}

func TestParseID(t *testing.T) {
	p := newProvider(t)
	for ref, want := range map[string]string{
		"381046":  "381046",
		" 00123 ": "123",
		"https://vod.tvp.pl/seriale,18/ranczo-odcinki,316445/odcinek-1,S01E01,381046": "381046",
		"https://vod.tvp.pl/seriale,18/ranczo-odcinki,316445":                         "316445",
		"https://vod.tvp.pl/seriale,18/ranczo-odcinki,316445/":                        "316445",
		"https://vod.tvp.pl/filmy-fabularne,136/hydrozagadka,296079?x=1":              "296079",
		"https://vod.tvp.pl/seriale,18/ranczo-odcinki":                                "",
		"https://example.com/ranczo,316445":                                           "",
		"0":                                                                           "",
		"-1":                                                                          "",
		"ranczo":                                                                      "",
	} {
		got, err := p.ParseID(ref)
		if got != want || (err == nil) != (want != "") {
			t.Errorf("ParseID(%q) = %q, %v; want %q", ref, got, err, want)
		}
	}
}

// Sonarr searches by title when its ID search finds nothing.
func TestTitleSearchAfterOverride(t *testing.T) {
	// Ranczo's S02E02 is TVP's paid episode 16; automatic numbering makes it 15.
	p := overriddenProvider(t, &provider.Overrides{Series: map[int]provider.SeriesOverride{
		81970: {Seasons: []provider.SeasonRule{{Season: 2, SiteSeason: 2, Offset: 14}}},
	}})
	ctx := context.Background()
	title := func(episode int) []string {
		t.Helper()
		items, err := p.Search(ctx, provider.Query{Kind: provider.Episode, Title: "Ranczo", Season: 2, Episode: episode})
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, it := range items {
			ids = append(ids, it.ID)
		}
		return ids
	}
	if _, items, err := p.SearchTVDB(ctx, 81970, ep(2, 2)); len(items) != 0 || err != nil {
		t.Fatalf("SearchTVDB = %v, %v", items, err)
	}
	if got := title(2); got != nil {
		t.Errorf("title search found %v, undoing the override", got)
	}
	if got := title(1); !slices.Equal(got, []string{"381138"}) {
		t.Errorf("title search for S02E01 found %v, want the override's 381138", got)
	}
}

// The Ranch (TVDB 5) is Ranczo, whose season 2 on TVP lists an unnumbered
// trailer, 999. Its S00E01 and S02E01 aired on 2007-07-01.
func TestSpecialsAndAirDates(t *testing.T) {
	special := provider.SeriesOverride{Episodes: map[provider.EpisodeNumber]string{pin(0, 1): "999"}}
	aired := provider.Query{Kind: provider.Episode, AirDate: "2007-07-01"}
	tests := []struct {
		name string
		ov   provider.SeriesOverride
		q    provider.Query
		want []string
	}{
		{"no automatic specials", provider.SeriesOverride{}, ep(0, 1), nil},
		{"pinned special", special, ep(0, 1), []string{"999 S00E01"}},
		{"season of pinned specials", special, ep(0, 0), []string{"999 S00E01"}},
		{"air date", provider.SeriesOverride{}, aired, []string{"381150 S02E01"}},
		{"air date with a pinned special", special, aired, []string{"381150 S02E01", "999 S00E01"}},
		{"air date with nothing", special, provider.Query{Kind: provider.Episode, AirDate: "2007-07-02"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := overriddenProvider(t, &provider.Overrides{Series: map[int]provider.SeriesOverride{5: tt.ov}})
			_, items, err := p.SearchTVDB(context.Background(), 5, tt.q)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, it := range items {
				got = append(got, fmt.Sprintf("%s S%02dE%02d", it.ID, it.Season, it.Episode))
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("SearchTVDB found %v, want %v", got, tt.want)
			}
			// Found on TVP, the series is watched, even without the episode.
			if !slices.Contains(p.watchedSeries.ids(), 5) {
				t.Error("the series isn't watched")
			}
		})
	}

	// Title searches have neither TVDB's air dates nor, without an override
	// for the title, its specials.
	p := overriddenProvider(t, nil)
	for _, q := range []provider.Query{{Season: 0, Episode: 1}, {AirDate: "2007-07-01"}} {
		q.Kind, q.Title = provider.Episode, "Ranczo"
		if items, err := p.Search(context.Background(), q); items != nil || err != nil {
			t.Errorf("Search(%+v) = %v, %v", q, items, err)
		}
	}
}

func TestFeedOffersPinnedSpecials(t *testing.T) {
	p := overriddenProvider(t, &provider.Overrides{Series: map[int]provider.SeriesOverride{
		5: {Episodes: map[provider.EpisodeNumber]string{pin(0, 1): "999"}},
	}})
	p.watch(5, "The Ranch")
	p.rebuildSeries(context.Background())
	if got, want := describe(feedReleases(p)), "381054 The Ranch S01E13, 381150 The Ranch S02E01, 999 The Ranch S00E01"; got != want {
		t.Errorf("feed = %s; want %s", got, want)
	}
}

func TestWholeSeasonOfPins(t *testing.T) {
	// The guide, which automatic matching would ask about season 27, is down.
	p := newProviderWith(t, func(key string) (string, bool) {
		return "500", strings.HasPrefix(key, "/lives/programmes?")
	})
	fakeTitles(t, p)
	pins := map[provider.EpisodeNumber]string{}
	for n, id := range map[int]string{1: "11901", 5: "11902", 6: "11941", 7: "11942", 8: "11943", 9: "11002"} {
		pins[pin(27, n)] = id
	}
	p.SetOverrides(&provider.Overrides{Series: map[int]provider.SeriesOverride{7: {Episodes: pins}}})
	_, items, err := p.SearchTVDB(context.Background(), 7, ep(27, 0))
	if err != nil || len(items) != 6 {
		t.Errorf("SearchTVDB = %+v, %v; want the 6 pinned episodes", items, err)
	}
}

func TestFailedRebuildDropsSupersededReleases(t *testing.T) {
	p := overriddenProvider(t, nil)
	p.watch(5, "The Ranch")
	p.rebuildSeries(context.Background())
	if got := describe(feedReleases(p)); got != "381054 The Ranch S01E13, 381150 The Ranch S02E01" {
		t.Fatalf("feed = %s", got)
	}
	// Unchanged series keep their releases for failed rebuilds.
	p.SetOverrides(&provider.Overrides{Films: map[string]provider.FilmOverride{provider.FilmKey("Kler", 2018): {ID: "1"}}})
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	p.rebuildSeries(cancelled)
	if got := describe(feedReleases(p)); got != "381054 The Ranch S01E13, 381150 The Ranch S02E01" {
		t.Errorf("after a film override and a failed rebuild, feed = %s", got)
	}
	p.SetOverrides(&provider.Overrides{Series: map[int]provider.SeriesOverride{
		5: {Seasons: []provider.SeasonRule{{Season: 2, SiteSeason: 2, Offset: 14}}},
	}})
	p.rebuildSeries(cancelled)
	if got := describe(feedReleases(p)); got != "" {
		t.Errorf("after a series override and a failed rebuild, feed = %s", got)
	}
}

func TestRebuildDuringOverrideChange(t *testing.T) {
	var p *Provider
	var once sync.Once
	p = newProviderWith(t, func(key string) (string, bool) {
		if strings.HasPrefix(key, "/vods/serials/316445/") {
			once.Do(func() {
				p.SetOverrides(&provider.Overrides{Series: map[int]provider.SeriesOverride{
					5: {Seasons: []provider.SeasonRule{{Season: 2, SiteSeason: 2, Offset: 14}}},
				}})
			})
		}
		return "", false
	})
	fakeTitles(t, p)
	p.watch(5, "The Ranch")
	p.rebuildSeries(context.Background())
	if got := describe(feedReleases(p)); got != "" {
		t.Errorf("a rebuild begun under superseded overrides published %s", got)
	}
	if !p.seriesFeed.stale {
		t.Error("the feed isn't stale")
	}
	p.rebuildSeries(context.Background())
	if got := describe(feedReleases(p)); got != "381054 The Ranch S01E13, 381138 The Ranch S02E01" {
		t.Errorf("feed = %s", got)
	}
}

// Wikidata fails for TVDB 3; the override's titles don't need it.
func TestOverrideTitlesWithoutWikidata(t *testing.T) {
	p := overriddenProvider(t, &provider.Overrides{Series: map[int]provider.SeriesOverride{3: {Titles: []string{"Ranczo"}}}})
	ctx := context.Background()
	if _, items, err := p.SearchTVDB(ctx, 3, ep(1, 1)); err != nil || len(items) != 1 || items[0].ID != "381046" {
		t.Errorf("SearchTVDB = %+v, %v; want 381046", items, err)
	}
	if id, ok := p.overriddenSeries("Lagging"); !ok || id != 3 {
		t.Errorf("overriddenSeries = %d, %v", id, ok)
	}
}
