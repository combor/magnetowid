package bbc

import (
	"context"
	"slices"
	"testing"

	"github.com/combor/magnetowid/internal/provider"
)

func pin(season, episode int) provider.EpisodeNumber {
	return provider.EpisodeNumber{Season: season, Episode: episode}
}

func TestMatchOverrides(t *testing.T) {
	check := func(name string, tv []tvdbEpisode, eps []programme, ov provider.SeriesOverride, want ...string) {
		t.Helper()
		slices.Sort(want)
		if got := pairs(match(tv, eps, ov)); !slices.Equal(got, want) {
			t.Errorf("%s:\n got %v\nwant %v", name, got, want)
		}
	}

	tv := []tvdbEpisode{tvdb(1, 1, "Alpha", "2024-01-01"), tvdb(1, 2, "Beta", "2024-01-08")}
	eps := []programme{
		ep("a", "Series 1: 1. Alpha", "Alpha", "2024-01-01", ""),
		ep("b", "Series 1: 2. Beta", "Beta", "2024-01-08", ""),
		ep("x", "Series 1: 3. Gamma", "Gamma", "2024-01-15", ""),
	}
	check("pin", tv, eps, provider.SeriesOverride{Episodes: map[provider.EpisodeNumber]string{pin(1, 2): "x"}},
		"S01E01=a", "S01E02=x")
	check("pin reserves its BBC episode", tv, eps, provider.SeriesOverride{Episodes: map[provider.EpisodeNumber]string{pin(1, 2): "a"}},
		"S01E02=a")
	check("pin to a missing episode", tv, eps, provider.SeriesOverride{Episodes: map[provider.EpisodeNumber]string{pin(1, 2): "gone"}},
		"S01E01=a")

	// TVDB's season 5 is BBC's series 4; BBC's series 5 is another.
	tv = []tvdbEpisode{tvdb(5, 1, "Episode 1", ""), tvdb(5, 2, "Episode 2", "")}
	eps = []programme{
		ep("s4e1", "Series 4: Episode 1", "", "", ""),
		ep("s4e2", "Series 4: Episode 2", "", "", ""),
		ep("s5e1", "Series 5: Episode 1", "", "", ""),
		ep("special", "Specials: Christmas", "", "", ""),
	}
	check("automatic", tv, eps, provider.SeriesOverride{}, "S05E01=s5e1")
	check("rule", tv, eps, provider.SeriesOverride{Seasons: []provider.SeasonRule{{Season: 5, SiteSeason: 4}}},
		"S05E01=s4e1", "S05E02=s4e2")
	check("rule to missing episodes", tv, eps, provider.SeriesOverride{Seasons: []provider.SeasonRule{{Season: 5, SiteSeason: 4, Offset: 10}}})
	check("any series", tv, eps, provider.SeriesOverride{Seasons: []provider.SeasonRule{{Season: 5, Offset: 1}}},
		"S05E01=s4e2")
	check("ambiguous in any series", tv, eps, provider.SeriesOverride{Seasons: []provider.SeasonRule{{Season: 5}}},
		"S05E02=s4e2")
	// Specials have no number.
	check("before the first episode", tv, eps, provider.SeriesOverride{Seasons: []provider.SeasonRule{{Season: 5, Offset: -1}}})
	check("pinned special", append(tv, tvdb(0, 3, "Festive Special", "")), eps,
		provider.SeriesOverride{Episodes: map[provider.EpisodeNumber]string{pin(0, 3): "special"}},
		"S00E03=special", "S05E01=s5e1")
}

func TestSearchTVDBOverrides(t *testing.T) {
	p := newProvider(t)
	ctx := context.Background()
	// iPlayer lacks Days of Honor; say it is Doctor Who.
	p.SetOverrides(&provider.Overrides{Series: map[int]provider.SeriesOverride{83920: {
		Titles:   []string{"Doctor Who"},
		Episodes: map[provider.EpisodeNumber]string{pin(1, 1): "m001z8bz"},
	}}})
	if !p.seriesFeed.stale || !p.filmFeed.stale {
		t.Error("new overrides left the feeds fresh")
	}
	title, items, err := p.SearchTVDB(ctx, 83920, provider.Query{Kind: provider.Episode, Season: 1, Episode: 1})
	if err != nil || title != "Days of Honor" || !slices.Equal(ids(items), []string{"m001z8bz S01E01"}) {
		t.Errorf("SearchTVDB = %q, %v, %v", title, ids(items), err)
	}

	shows, err := p.findShows(ctx, "Doctor Who", "m002hrtl")
	if err != nil || len(shows) != 1 || shows[0].ID != "m002hrtl" {
		t.Errorf("findShows by ID = %+v, %v", shows, err)
	}
}

func TestFilmOverrides(t *testing.T) {
	p := newProviderWith(t, func(key string) (string, bool) {
		return `{"new_search":{"results":[{"id":"m002z0zd","type":"episode","title":"Arena",
			"subtitle":"Kim Novak's Vertigo","tleo_type":"brand"}]}}`, key == "/ibl/v1/new-search?q=Arena"
	})
	ctx := context.Background()
	p.SetOverrides(&provider.Overrides{Films: map[string]provider.FilmOverride{
		provider.FilmKey("Nickel Boys", 2022):         {ID: "m0remake"}, // iPlayer says 1990
		provider.FilmKey("Nickel Boys (film)", 2024):  {Titles: []string{"Nickel Boys"}},
		provider.FilmKey("Nickel Boys", 2020):         {ID: "m00nb0ys"},                            // a programme, not a film
		provider.FilmKey("Kim Novak's Vertigo", 2025): {Titles: []string{"Arena"}, ID: "m002z0zd"}, // a brand's episode
	}})
	for _, tt := range []struct {
		title string
		year  int
		want  []string
	}{
		{"Nickel Boys", 2022, []string{"m0remake"}},
		{"Nickel Boys (film)", 2024, []string{"m00327ht"}},
		{"Nickel Boys", 2020, nil},
		{"Kim Novak's Vertigo", 2025, nil},
	} {
		items, err := p.Search(ctx, provider.Query{Kind: provider.Movie, Title: tt.title, Year: tt.year})
		var got []string
		for _, it := range items {
			got = append(got, it.ID)
			if it.Year != tt.year {
				t.Errorf("%s (%d): release year %d", tt.title, tt.year, it.Year)
			}
		}
		if err != nil || !slices.Equal(got, tt.want) {
			t.Errorf("%s (%d): %v, %v; want %v", tt.title, tt.year, got, err, tt.want)
		}
	}
}

func TestFilmFeedOverrides(t *testing.T) {
	p := newProviderWith(t, func(key string) (string, bool) {
		return newestFilms, key == "/ibl/v1/categories/films/programmes"
	})
	p.watchFilm("The Thirty-Nine Steps", 1935)
	p.watchFilm("Nickel Boys", 2022)
	p.SetOverrides(&provider.Overrides{Films: map[string]provider.FilmOverride{
		provider.FilmKey("The Thirty-Nine Steps", 1935): {Titles: []string{"The 39 Steps"}},
		provider.FilmKey("Nickel Boys", 2022):           {ID: "m00327ht"},
	}})
	p.rebuildFilms(context.Background())
	if got := releases(p.filmFeed.recent()); !slices.Equal(got, []string{"Nickel Boys m00327ht", "The Thirty-Nine Steps b0074t6w"}) {
		t.Errorf("feed %v", got)
	}
}

func TestParseID(t *testing.T) {
	p := newProvider(t)
	for ref, want := range map[string]string{
		"m002d3lr":   "m002d3lr",
		" b0074t6w ": "b0074t6w",
		"https://www.bbc.co.uk/iplayer/episode/m002d3lr/doctor-who-season-2-8-the-reality-war": "m002d3lr",
		"https://www.bbc.co.uk/iplayer/episodes/p0gglvqn/doctor-who?seriesId=p0gglvqn":         "p0gglvqn",
		"https://www.bbc.co.uk/programmes/m002d3lr":                                            "m002d3lr",
		"https://bbc.co.uk/iplayer/episode/m002d3lr":                                           "m002d3lr",
		"https://www.bbc.co.uk/iplayer/episode/M002D3LR":                                       "",
		"https://www.bbc.co.uk/iplayer":                                                        "",
		"https://example.com/iplayer/episode/m002d3lr":                                         "",
		"doctor who": "",
		"a0000000":   "",
	} {
		got, err := p.ParseID(ref)
		if got != want || (err == nil) != (want != "") {
			t.Errorf("ParseID(%q) = %q, %v; want %q", ref, got, err, want)
		}
	}
}

func TestMatchOverridesInAnyOrder(t *testing.T) {
	eps := []programme{ep("a", "Series 1: Episode 1", "", "", ""), ep("b", "Series 2: Episode 1", "", "", "")}
	ov := provider.SeriesOverride{Seasons: []provider.SeasonRule{{Season: 1, SiteSeason: 1}, {Season: 2}}}
	for _, tv := range [][]tvdbEpisode{
		{tvdb(1, 1, "", ""), tvdb(2, 1, "", "")},
		{tvdb(2, 1, "", ""), tvdb(1, 1, "", "")},
	} {
		// Episode 1 of any series is ambiguous.
		if got := pairs(match(tv, eps, ov)); !slices.Equal(got, []string{"S01E01=a"}) {
			t.Errorf("TVDB S%02dE01 first: %v", tv[0].season, got)
		}
	}
}

// A film pinned by ID is the pin's alone.
func TestPinnedFilmIsReserved(t *testing.T) {
	p := newProviderWith(t, func(key string) (string, bool) {
		return newestFilms, key == "/ibl/v1/categories/films/programmes"
	})
	ctx := context.Background()
	p.watchFilm("Nickel Boys", 2024)
	p.watchFilm("Nickel Boys", 2022)
	p.SetOverrides(&provider.Overrides{Films: map[string]provider.FilmOverride{
		provider.FilmKey("Nickel Boys", 2022): {ID: "m00327ht"},
	}})
	p.rebuildFilms(ctx)
	rs := p.filmFeed.recent()
	if len(rs) != 1 || rs[0].ID != "m00327ht" || rs[0].Year != 2022 {
		t.Errorf("feed %+v; want m00327ht as the 2022 film", rs)
	}
	items, err := p.Search(ctx, provider.Query{Kind: provider.Movie, Title: "Nickel Boys", Year: 2024})
	if err != nil || len(items) != 0 {
		t.Errorf("search for the 2024 film = %+v, %v; want nothing", items, err)
	}
}
