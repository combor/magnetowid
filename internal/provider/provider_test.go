package provider

import (
	"context"
	"encoding/json"
	"slices"
	"sync"
	"testing"
)

func TestNormalizeTitle(t *testing.T) {
	tests := []struct{ in, want string }{
		{"Ranczo", "ranczo"},
		{"M jak miłość", "m jak milosc"},
		{"Czterej pancerni i pies", "czterej pancerni i pies"},
		{"Hydro-Puzzle", "hydro puzzle"},
		{"Tom & Jerry", "tom and jerry"},
		{"Grey's Anatomy", "greys anatomy"},
		{"Mr. Robot", "mr robot"},
		{"  Wodecki Twist – Tylko w kinie ", "wodecki twist tylko w kinie"},
		{"Żółć", "zolc"},
		{"The", "the"},
	}
	for _, tt := range tests {
		if got := NormalizeTitle(tt.in); got != tt.want {
			t.Errorf("NormalizeTitle(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestNormalizeTitleMatchesArrCleaning(t *testing.T) {
	pairs := [][2]string{
		{"The Killing", "Killing"},
		{"Tom & Jerry", "Tom and Jerry"},
		{"Grey's Anatomy", "Greys Anatomy"},
	}
	for _, p := range pairs {
		if NormalizeTitle(p[0]) != NormalizeTitle(p[1]) {
			t.Errorf("NormalizeTitle(%q) = %q != NormalizeTitle(%q) = %q",
				p[0], NormalizeTitle(p[0]), p[1], NormalizeTitle(p[1]))
		}
	}
}

type named string

func (n named) Name() string                                  { return string(n) }
func (named) Search(context.Context, Query) ([]Item, error)   { return nil, nil }
func (named) Resolve(context.Context, string) (Stream, error) { return Stream{}, nil }

func TestRegistry(t *testing.T) {
	r := NewRegistry(named("tvp"), named("other"))
	if _, ok := r.Get("tvp"); !ok {
		t.Fatal("tvp not registered")
	}
	if _, ok := r.Get("missing"); ok {
		t.Fatal("unexpected provider")
	}
	if got := r.Names(); len(got) != 2 || got[0] != "other" || got[1] != "tvp" {
		t.Fatalf("Names() = %v", got)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("duplicate name did not panic")
		}
	}()
	NewRegistry(named("tvp"), named("tvp"))
}

func TestNormalizeTitleConcurrent(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				if got := NormalizeTitle("M jak miłość"); got != "m jak milosc" {
					t.Errorf("got %q", got)
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestSeriesOverrideTarget(t *testing.T) {
	o := SeriesOverride{
		Seasons:  []SeasonRule{{Season: 2, SiteSeason: 1, Offset: 13}, {Season: 3, Offset: 100}},
		Episodes: map[EpisodeNumber]string{{2, 5}: "pinned"},
	}
	tests := []struct {
		season, episode int
		want            Target
		ok              bool
	}{
		{2, 1, Target{Season: 1, Episode: 14}, true},
		{2, 5, Target{ID: "pinned"}, true}, // pins win over rules
		{3, 7, Target{Season: 0, Episode: 107}, true},
		{1, 1, Target{}, false},
	}
	for _, tt := range tests {
		if got, ok := o.Target(tt.season, tt.episode); got != tt.want || ok != tt.ok {
			t.Errorf("Target(%d, %d) = %+v, %v; want %+v, %v", tt.season, tt.episode, got, ok, tt.want, tt.ok)
		}
	}
	if !o.Pinned("pinned") || o.Pinned("other") {
		t.Error("Pinned is wrong")
	}
}

func TestEpisodeNumberJSON(t *testing.T) {
	var got map[EpisodeNumber]string
	if err := json.Unmarshal([]byte(`{"S01E05":"a","s10e123":"b"}`), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[EpisodeNumber{1, 5}] != "a" || got[EpisodeNumber{10, 123}] != "b" {
		t.Fatalf("got %v", got)
	}
	out, err := json.Marshal(got)
	if err != nil || string(out) != `{"S01E05":"a","S10E123":"b"}` {
		t.Errorf("Marshal = %s, %v", out, err)
	}
	for _, bad := range []string{`{"1x05":"a"}`, `{"S01":"a"}`, `{"S01E05 ":"a"}`} {
		if err := json.Unmarshal([]byte(bad), &got); err == nil {
			t.Errorf("Unmarshal(%s) succeeded", bad)
		}
	}
}

func TestOverridesLookup(t *testing.T) {
	var none *Overrides
	if _, ok := none.SeriesFor(1); ok {
		t.Error("nil overrides have a series")
	}
	if _, ok := none.Film("Cube", 1997); ok {
		t.Error("nil overrides have a film")
	}
	o := &Overrides{
		Series: map[int]SeriesOverride{83920: {ID: "292065"}},
		Films:  map[string]FilmOverride{FilmKey("The Sexmission", 1984): {Titles: []string{"Seksmisja"}}},
	}
	if s, ok := o.SeriesFor(83920); !ok || s.ID != "292065" {
		t.Errorf("SeriesFor = %+v, %v", s, ok)
	}
	// Radarr's title as it cleans titles.
	if f, ok := o.Film("sexmission", 1984); !ok || f.Titles[0] != "Seksmisja" {
		t.Errorf("Film = %+v, %v", f, ok)
	}
	if _, ok := o.Film("Sexmission", 1985); ok {
		t.Error("another year's film has an override")
	}
}

func TestOverridesChanges(t *testing.T) {
	before := &Overrides{
		Series: map[int]SeriesOverride{1: {ID: "a"}, 2: {Titles: []string{"B"}}, 3: {ID: "c"}},
		Films:  map[string]FilmOverride{"1990 x": {ID: "x"}},
	}
	after := &Overrides{
		Series: map[int]SeriesOverride{1: {ID: "a"}, 2: {Titles: []string{"B", "C"}}, 4: {ID: "d"}},
		Films:  map[string]FilmOverride{"1990 x": {ID: "x"}},
	}
	series, films := after.Changes(before)
	slices.Sort(series)
	if !slices.Equal(series, []int{2, 3, 4}) || films {
		t.Errorf("Changes = %v, %v; want [2 3 4], false", series, films)
	}
	var none *Overrides
	if series, films := none.Changes(before); len(series) != 3 || !films {
		t.Errorf("removing all: %v, %v", series, films)
	}
	if series, films := none.Changes(&Overrides{}); len(series) != 0 || films {
		t.Errorf("nil and empty differ: %v, %v", series, films)
	}
}
