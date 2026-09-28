package tvp

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/combor/vodarr/internal/provider"
)

// soapNow is around when Hundreds' recent episodes air, to the minute like
// the guide's times.
var soapNow = time.Now().UTC().Truncate(time.Minute)

// hundredsShow is Hundreds, TVP's Setki, a soap TVP keeps in blocks of 100.
// TVDB's absolute numbers and titles number S19 and S20, whose air dates are
// years from TVP's, and S1, which also counts from the first episode except
// where they disagree (E4). The guide (hundredsGuide) numbers S27.
func hundredsShow(now time.Time) string {
	aired := func(d time.Duration) string { return now.Add(d).Format(time.RFC3339) }
	return fmt.Sprintf(`{"tvdbId":7,"title":"Hundreds","episodes":[
		{"seasonNumber":0,"episodeNumber":1,"absoluteEpisodeNumber":1943},
		{"seasonNumber":1,"episodeNumber":1,"absoluteEpisodeNumber":1,"airDateUtc":"2000-11-04T18:45:00Z"},
		{"seasonNumber":1,"episodeNumber":2,"absoluteEpisodeNumber":2,"title":"Episode 2","airDateUtc":"2000-11-11T18:45:00Z"},
		{"seasonNumber":1,"episodeNumber":3,"airDateUtc":"2000-11-18T18:45:00Z"},
		{"seasonNumber":1,"episodeNumber":4,"absoluteEpisodeNumber":4,"title":"Episode 5","airDateUtc":"2000-11-25T18:45:00Z"},
		{"seasonNumber":19,"episodeNumber":1,"title":"Odcinek 1899","airDateUtc":"2025-05-05T18:45:00Z"},
		{"seasonNumber":19,"episodeNumber":2,"title":"Odcinek 1900","airDateUtc":"2025-05-06T18:45:00Z"},
		{"seasonNumber":20,"episodeNumber":1,"title":"1901","airDateUtc":"2016-09-05T18:45:00Z"},
		{"seasonNumber":20,"episodeNumber":2,"title":"1902","airDateUtc":"2016-09-06T18:45:00Z"},
		{"seasonNumber":27,"episodeNumber":1,"airDateUtc":%q},
		{"seasonNumber":27,"episodeNumber":5,"airDateUtc":%q},
		{"seasonNumber":27,"episodeNumber":6,"airDateUtc":%q},
		{"seasonNumber":27,"episodeNumber":7,"airDateUtc":%q},
		{"seasonNumber":27,"episodeNumber":8,"airDateUtc":%q},
		{"seasonNumber":27,"episodeNumber":9}]}`,
		aired(-30*24*time.Hour), aired(-8*24*time.Hour), aired(-7*24*time.Hour), aired(-24*time.Hour), aired(12*time.Hour))
}

// hundredsGuide is TVP's number for each of Hundreds' broadcasts in the
// guide, by Unix time: S27E05-E08. S27E01 aired before the guide's window.
var hundredsGuide = map[int64]int{
	soapNow.Add(-8 * 24 * time.Hour).Unix(): 1941,
	soapNow.Add(-7 * 24 * time.Hour).Unix(): 1942,
	soapNow.Add(-24 * time.Hour).Unix():     1943,
	soapNow.Add(12 * time.Hour).Unix():      1944,
}

func hundredsProgrammes(at time.Time) []programme {
	n, ok := hundredsGuide[at.Unix()]
	if !ok {
		return nil
	}
	return []programme{{Title: fmt.Sprintf("Setki - odc. %d", n), Since: at.Add(5 * time.Minute).Format(time.RFC3339)}}
}

// newSoapProvider is a provider that knows Hundreds, and the times its
// guide was asked about.
func newSoapProvider(t *testing.T) (*Provider, *guideRequests) {
	t.Helper()
	serve, guide := serveGuide(t, hundredsProgrammes)
	p := newProviderWith(t, serve)
	fakeTitles(t, p)
	return p, guide
}

func TestSearchTVDBSoap(t *testing.T) {
	p, guide := newSoapProvider(t)
	ep := func(season, episode int) provider.Query {
		return provider.Query{Kind: provider.Episode, Season: season, Episode: episode}
	}
	tests := []struct {
		name string
		q    provider.Query
		ids  []string
	}{
		{"TVDB's absolute numbers", ep(1, 2), []string{"11002"}},
		{"season 1 counting from the first episode", ep(1, 3), []string{"11003"}},
		{"sources that disagree", ep(1, 4), nil},
		{"whole season 1", ep(1, 0), []string{"11001", "11002", "11003"}},
		{"TVDB's titles", ep(19, 1), []string{"11899"}},
		{"whole season", ep(19, 0), []string{"11899", "11900"}},
		{"TVP's year too far from the air date", ep(20, 1), nil},
		// The special's absolute number doesn't count against the guide's.
		{"the guide", ep(27, 7), []string{"11943"}},
		{"whole season by the guide", ep(27, 0), []string{"11941", "11942", "11943"}},
		{"paid", ep(27, 8), nil},
		{"aired before the guide's window", ep(27, 1), nil},
		{"not yet scheduled", ep(27, 9), nil},
	}
	for _, tt := range tests {
		title, items, err := p.SearchTVDB(context.Background(), 7, tt.q)
		if err != nil {
			t.Errorf("%s: %v", tt.name, err)
			continue
		}
		var ids []string
		for _, it := range items {
			ids = append(ids, it.ID)
			if it.Season != tt.q.Season || (tt.q.Episode != 0 && it.Episode != tt.q.Episode) {
				t.Errorf("%s: %s named S%02dE%02d", tt.name, it.ID, it.Season, it.Episode)
			}
		}
		if title != "Hundreds" || !slices.Equal(ids, tt.ids) {
			t.Errorf("%s: SearchTVDB = %q, %v; want Hundreds, %v", tt.name, title, ids, tt.ids)
		}
	}
	old := soapNow.Add(-30 * 24 * time.Hour)
	if slices.ContainsFunc(guide.times(), old.Equal) {
		t.Errorf("asked the guide about %v, before its window", old)
	}
}

// Only a season with recent episodes needs the guide, so older seasons are
// found while it fails.
func TestSearchTVDBSoapGuideDown(t *testing.T) {
	p := newProviderWith(t, func(key string) (string, bool) {
		return "500", strings.HasPrefix(key, "/lives/programmes?")
	})
	fakeTitles(t, p)
	older := provider.Query{Kind: provider.Episode, Season: 19, Episode: 1}
	if _, items, err := p.SearchTVDB(context.Background(), 7, older); err != nil || len(items) != 1 || items[0].ID != "11899" {
		t.Errorf("S19E01 = %+v, %v; want 11899", items, err)
	}
	recent := provider.Query{Kind: provider.Episode, Season: 27, Episode: 7}
	if _, _, err := p.SearchTVDB(context.Background(), 7, recent); err == nil {
		t.Error("S27E07 gave no error with the guide down")
	}
}

// The feed offers a soap's new episodes, numbered by the guide.
func TestFeedRebuildSoap(t *testing.T) {
	p, _ := newSoapProvider(t)
	p.watch(7, "Hundreds")
	p.rebuildSeries(context.Background())
	want := "11941 Hundreds S27E05, 11942 Hundreds S27E06, 11943 Hundreds S27E07"
	if got := describe(feedReleases(p)); got != want {
		t.Errorf("feed = %s; want %s", got, want)
	}
}

func TestBlocks(t *testing.T) {
	tests := []struct {
		title       string
		first, last int
		ok          bool
	}{
		{"1–100", 1, 100, true},
		{"801-900", 801, 900, true},
		{"782–800", 782, 800, true},
		{"1901–", 1901, 0, true},
		{" 2901 - 3000 ", 2901, 3000, true},
		{"Sezon 1", 0, 0, false},
		{"Odcinki", 0, 0, false},
		{"Odcinki specjalne", 0, 0, false},
		{"", 0, 0, false},
	}
	for _, tt := range tests {
		bs := blocks([]product{{Title: tt.title}})
		ok := len(bs) == 1
		if ok != tt.ok || (ok && (bs[0].first != tt.first || bs[0].last != tt.last)) {
			t.Errorf("blocks(%q) = %+v, want %d–%d %v", tt.title, bs, tt.first, tt.last, tt.ok)
		}
	}
}

func TestTitleNumber(t *testing.T) {
	for title, want := range map[string]int{
		"1945":                     1945,
		"Odcinek 1378":             1378,
		"Episode 75":               75,
		"odc. 12":                  12,
		"Odcinek 846 (12.09.2011)": 846,
		"Odcinek 28981":            28981, // a typo, which checkNumbers drops
		"The 1983 Affair":          0,
		"Episode 5: The Return":    0,
		"":                         0,
	} {
		if got := titleNumber(title); got != want {
			t.Errorf("titleNumber(%q) = %d, want %d", title, got, want)
		}
	}
}

func TestAgreed(t *testing.T) {
	for _, tt := range []struct{ a, b, want int }{
		{5, 5, 5}, {5, 0, 5}, {0, 5, 5}, {0, 0, 0},
		{5, 6, conflicting},
		// A conflict stays one, whatever else agrees.
		{conflicting, 5, conflicting}, {5, conflicting, conflicting}, {conflicting, 0, conflicting},
	} {
		if got := agreed(tt.a, tt.b); got != tt.want {
			t.Errorf("agreed(%d, %d) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestCheckNumbers(t *testing.T) {
	var episodes []tvdbEpisode
	var numbers []int
	for _, e := range [][3]int{ // season, episode, number
		// A run numbered in order, listed out of order.
		{1, 2, 12}, {1, 1, 11}, {1, 3, 13},
		// 22 is claimed twice, which leaves 21 no neighbour.
		{2, 1, 21}, {2, 2, 22}, {2, 3, 22},
		// Neighbours without numbers.
		{3, 1, 0}, {3, 2, 32}, {3, 3, 0},
		// Neighbours at different offsets, as with a typo.
		{4, 1, 41}, {4, 2, 420},
		// A gap in TVDB's list keeps the offset.
		{5, 1, 51}, {5, 3, 53},
		// Neighbours in different seasons.
		{6, 1, 61}, {7, 2, 62},
	} {
		episodes = append(episodes, tvdbEpisode{season: e[0], episode: e[1]})
		numbers = append(numbers, e[2])
	}
	want := map[episodeKey]int{{1, 1}: 11, {1, 2}: 12, {1, 3}: 13, {5, 1}: 51, {5, 3}: 53}
	if got := checkNumbers(episodes, numbers); !maps.Equal(got, want) {
		t.Errorf("checkNumbers = %v, want %v", got, want)
	}
}
