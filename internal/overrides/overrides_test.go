package overrides

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	bolt "go.etcd.io/bbolt"

	"github.com/combor/magnetowid/internal/provider"
	"github.com/combor/magnetowid/internal/store"
)

// site takes IDs of lowercase letters, or its URLs, https://site.example/<id>.
type site struct {
	name string
	mu   sync.Mutex
	got  []*provider.Overrides
}

func (s *site) Name() string                                                  { return s.name }
func (*site) Search(context.Context, provider.Query) ([]provider.Item, error) { return nil, nil }
func (*site) Resolve(context.Context, string) (provider.Stream, error)        { return provider.Stream{}, nil }
func (s *site) ParseID(ref string) (string, error) {
	ref = strings.TrimPrefix(ref, "https://site.example/")
	if ref == "" || strings.Trim(ref, "abcdefghijklmnopqrstuvwxyz") != "" {
		return "", errors.New("not an ID")
	}
	return ref, nil
}

func (s *site) SetOverrides(o *provider.Overrides) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, o)
}

// last returns the overrides last given and how many times they were given.
func (s *site) last() (*provider.Overrides, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.got) == 0 {
		return nil, 0
	}
	return s.got[len(s.got)-1], len(s.got)
}

// plain takes no overrides.
type plain struct{}

func (plain) Name() string                                                    { return "plain" }
func (plain) Search(context.Context, provider.Query) ([]provider.Item, error) { return nil, nil }
func (plain) Resolve(context.Context, string) (provider.Stream, error)        { return provider.Stream{}, nil }

func TestStorePersists(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	one, two := &site{name: "one"}, &site{name: "two"}
	s, err := Open(db, provider.NewRegistry(one, two, plain{}))
	if err != nil {
		t.Fatal(err)
	}
	if o, n := one.last(); o != nil || n != 1 {
		t.Errorf("at startup, one got %v, %d times", o, n)
	}

	saved, err := s.PutSeries("one", 83920, provider.SeriesOverride{
		Titles:   []string{" Czas honoru ", "czas honoru"},
		Episodes: map[provider.EpisodeNumber]string{{Season: 1, Episode: 5}: "https://site.example/abc"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := provider.SeriesOverride{Titles: []string{"Czas honoru"}, Episodes: map[provider.EpisodeNumber]string{{Season: 1, Episode: 5}: "abc"}}
	if !reflect.DeepEqual(saved, want) {
		t.Errorf("PutSeries = %+v, want %+v", saved, want)
	}
	if o, _ := one.last(); !reflect.DeepEqual(o.Series[83920], want) {
		t.Errorf("one got %+v", o)
	}
	if o, n := two.last(); o != nil || n != 1 {
		t.Errorf("two got %v, %d times, for one's change", o, n)
	}
	if _, err := s.PutFilm("two", "Face/Off", 1997, provider.FilmOverride{ID: "xyz"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutSeries("plain", 1, provider.SeriesOverride{ID: "x"}); !errors.Is(err, ErrUnknownSite) {
		t.Errorf("PutSeries for a site without overrides: %v", err)
	}
	db.Close()

	db, err = store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	one, two = &site{name: "one"}, &site{name: "two"}
	s, err = Open(db, provider.NewRegistry(one, two))
	if err != nil {
		t.Fatal(err)
	}
	if o, _ := one.last(); !reflect.DeepEqual(o.Series[83920], want) {
		t.Errorf("after reopening, one got %+v", o)
	}
	if f, ok := s.Site("two").Film("Face/Off", 1997); !ok || f.ID != "xyz" || f.Title != "Face/Off" || f.Year != 1997 {
		t.Errorf("after reopening, two's film is %+v, %v", f, ok)
	}

	before, _ := one.last()
	if found, err := s.DeleteSeries("one", 83920); !found || err != nil {
		t.Fatalf("DeleteSeries = %v, %v", found, err)
	}
	if o, _ := one.last(); len(o.Series) != 0 {
		t.Errorf("after deleting, one got %+v", o)
	}
	if len(before.Series) != 1 {
		t.Error("deleting modified the overrides one had before")
	}
	if found, err := s.DeleteSeries("one", 83920); found || err != nil {
		t.Errorf("deleting again = %v, %v", found, err)
	}
	if found, err := s.DeleteFilm("two", "face off", 1997); !found || err != nil {
		t.Errorf("DeleteFilm with Radarr's cleaned title = %v, %v", found, err)
	}
}

func TestStoreRefusesCorruptEntries(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, key := range []string{"one series x", "one show 1"} {
		t.Run(key, func(t *testing.T) {
			db.Update(func(tx *bolt.Tx) error {
				b, _ := tx.CreateBucketIfNotExists(bucket)
				b.ForEach(func(k, _ []byte) error { return b.Delete(k) })
				return b.Put([]byte(key), []byte(`{}`))
			})
			if _, err := Open(db, provider.NewRegistry(&site{name: "one"})); err == nil || !strings.Contains(err.Error(), key) {
				t.Errorf("Open = %v", err)
			}
		})
	}
}

func TestCheckSeries(t *testing.T) {
	ep := func(season, episode int) provider.EpisodeNumber {
		return provider.EpisodeNumber{Season: season, Episode: episode}
	}
	tests := []struct {
		name   string
		tvdbID int
		o      provider.SeriesOverride
		want   string // in the error; "" for none
	}{
		{"titles", 1, provider.SeriesOverride{Titles: []string{"Klan"}}, ""},
		{"no TVDB ID", 0, provider.SeriesOverride{Titles: []string{"Klan"}}, "not positive"},
		{"empty", 1, provider.SeriesOverride{}, "needs titles"},
		{"blank title", 1, provider.SeriesOverride{Titles: []string{"Klan", " – "}}, "no letters"},
		{"bad ID", 1, provider.SeriesOverride{ID: "https://elsewhere.example/abc"}, "id: not an ID"},
		{"special season", 1, provider.SeriesOverride{Seasons: []provider.SeasonRule{{Season: 0}}}, "season 0"},
		{"negative site season", 1, provider.SeriesOverride{Seasons: []provider.SeasonRule{{Season: 1, SiteSeason: -1}}}, "negative"},
		{"two rules", 1, provider.SeriesOverride{Seasons: []provider.SeasonRule{{Season: 1}, {Season: 1, Offset: 2}}}, "two rules"},
		{"special episode", 1, provider.SeriesOverride{Episodes: map[provider.EpisodeNumber]string{ep(0, 1): "abc"}}, "S00E01"},
		{"bad episode ID", 1, provider.SeriesOverride{Episodes: map[provider.EpisodeNumber]string{ep(1, 1): "12"}}, "S01E01: not an ID"},
		{"two pins", 1, provider.SeriesOverride{Episodes: map[provider.EpisodeNumber]string{
			ep(1, 2): "abc", ep(1, 1): "https://site.example/abc"}}, "S01E01 and S01E02 both name abc"},
	}
	for _, tt := range tests {
		_, err := checkSeries(tt.tvdbID, tt.o, &site{})
		if tt.want == "" && err != nil || tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)) {
			t.Errorf("%s: %v, want %q", tt.name, err, tt.want)
		}
	}
	o, err := checkSeries(1, provider.SeriesOverride{Seasons: []provider.SeasonRule{{Season: 3}, {Season: 1}}}, &site{})
	if err != nil || o.Seasons[0].Season != 1 {
		t.Errorf("seasons out of order: %+v, %v", o, err)
	}
}

func TestCheckFilm(t *testing.T) {
	tests := []struct {
		name  string
		title string
		year  int
		o     provider.FilmOverride
		want  string
	}{
		{"titles", "Sexmission", 1984, provider.FilmOverride{Titles: []string{"Seksmisja"}}, ""},
		{"the same film", "Sexmission", 1984, provider.FilmOverride{Title: "The Sexmission", Year: 1984, ID: "abc"}, ""},
		{"another film", "Sexmission", 1984, provider.FilmOverride{Title: "Cube", Year: 1984, ID: "abc"}, "not"},
		{"another year", "Sexmission", 1984, provider.FilmOverride{Year: 1985, ID: "abc"}, "not"},
		{"no year", "Sexmission", 0, provider.FilmOverride{ID: "abc"}, "not positive"},
		{"blank title", "-", 1984, provider.FilmOverride{ID: "abc"}, "no letters"},
		{"empty", "Sexmission", 1984, provider.FilmOverride{Title: "Sexmission", Year: 1984}, "needs titles"},
		{"bad ID", "Sexmission", 1984, provider.FilmOverride{ID: "1"}, "id: not an ID"},
	}
	for _, tt := range tests {
		_, err := checkFilm(tt.title, tt.year, tt.o, &site{})
		if tt.want == "" && err != nil || tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)) {
			t.Errorf("%s: %v, want %q", tt.name, err, tt.want)
		}
	}
}
