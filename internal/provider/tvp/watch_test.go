package tvp

import (
	"context"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/combor/magnetowid/internal/provider"
	"github.com/combor/magnetowid/internal/store"
)

func openStore(t *testing.T, dir string) *bolt.DB {
	t.Helper()
	db, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func watchingProvider(t *testing.T, db *bolt.DB) *Provider {
	t.Helper()
	p := newProvider(t)
	fakeTitles(t, p)
	w, err := loadWatchList(db, seriesWatch)
	if err != nil {
		t.Fatal(err)
	}
	p.watchedSeries = w
	return p
}

var ranchS02E01 = provider.Query{Kind: provider.Episode, Season: 2, Episode: 1}

func TestWatchListPersists(t *testing.T) {
	dir := t.TempDir()
	db := openStore(t, dir)
	p := watchingProvider(t, db)
	if _, found, err := p.SearchTVDB(context.Background(), 5, ranchS02E01); err != nil || len(found) != 1 {
		t.Fatalf("found %+v, err = %v", found, err)
	}
	db.Close()

	db = openStore(t, dir)
	w, err := loadWatchList(db, seriesWatch)
	if err != nil {
		t.Fatal(err)
	}
	if ids := w.ids(); !slices.Equal(ids, []int{5}) {
		t.Fatalf("watched %v after reopening, want [5]", ids)
	}
	// A series already watched isn't saved again, so saving can't fail.
	db.Close()
	if isNew, err := w.add("5", watchRecord{}); isNew || err != nil {
		t.Errorf("add(5) = %v, %v; want false, nil", isNew, err)
	}
}

func TestCorruptWatchRecordFailsNew(t *testing.T) {
	const added = `{"added":"2026-09-28T12:00:00Z"}`
	for _, tt := range []struct {
		kind       watchKind
		key, value string
		want       string
	}{
		{seriesWatch, "5", "{bad", "series 5"},
		{seriesWatch, "five", added, "not a TVDB ID"},
		{filmWatch, "2018 kler", added, "no title or year"},
	} {
		db := openStore(t, t.TempDir())
		err := db.Update(func(tx *bolt.Tx) error {
			b, err := tx.CreateBucketIfNotExists(tt.kind.bucket)
			if err != nil {
				return err
			}
			return b.Put([]byte(tt.key), []byte(tt.value))
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := New(http.DefaultClient, slog.New(slog.DiscardHandler), db); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s %q: err = %v, want %q", tt.kind.name, tt.key, err, tt.want)
		}
	}
}

func TestFilmSearchWatches(t *testing.T) {
	p := newProviderWith(t, func(key string) (string, bool) {
		switch key {
		case "/vods/search/VOD?keyword=P%C5%82atny":
			return `{"items":[{"type":"VOD","id":3,"title":"Płatny","year":2024,"payable":true}]}`, true
		case "/vods/search/VOD?keyword=Awaria":
			return "500", true
		}
		return "", false
	})
	for _, tt := range []struct {
		title   string
		year    int
		watched bool
	}{
		{"Kler", 2018, true},         // not on TVP
		{"Płatny", 2024, true},       // paid
		{"Hydrozagadka", 1971, true}, // found, but its stream may be unreadable
		{"Nieznany", 0, false},       // no year to tell films apart
		{"Awaria", 2020, false},      // search failed
	} {
		_, err := p.Search(context.Background(), provider.Query{Kind: provider.Movie, Title: tt.title, Year: tt.year})
		if failed := tt.title == "Awaria"; (err != nil) != failed {
			t.Errorf("%s: err = %v", tt.title, err)
		}
		r, watched := p.watchedFilms.records[filmKey(tt.title, tt.year)]
		if watched != tt.watched {
			t.Errorf("%s %d: watched = %v, want %v", tt.title, tt.year, watched, tt.watched)
		}
		if watched && (r.Title != tt.title || r.Year != tt.year || r.Added.IsZero()) {
			t.Errorf("%s %d: record = %+v", tt.title, tt.year, r)
		}
	}
}

func TestFilmWatchListPersists(t *testing.T) {
	dir := t.TempDir()
	db := openStore(t, dir)
	p := newProvider(t)
	w, err := loadWatchList(db, filmWatch)
	if err != nil {
		t.Fatal(err)
	}
	p.watchedFilms = w
	if _, err := p.Search(context.Background(), provider.Query{Kind: provider.Movie, Title: "Kler", Year: 2018}); err != nil {
		t.Fatal(err)
	}
	db.Close()

	db = openStore(t, dir)
	if w, err = loadWatchList(db, filmWatch); err != nil {
		t.Fatal(err)
	}
	if rs := w.all(); len(rs) != 1 || rs[0].Title != "Kler" || rs[0].Year != 2018 || rs[0].Added.IsZero() {
		t.Fatalf("watched %+v after reopening, want Kler (2018)", rs)
	}
	series, err := loadWatchList(db, seriesWatch)
	if err != nil || len(series.ids()) != 0 {
		t.Errorf("watched series %v, %v; want none", series.ids(), err)
	}
	// A film already watched isn't saved again, so saving can't fail.
	db.Close()
	if isNew, err := w.add(filmKey("Kler", 2018), watchRecord{Title: "Kler", Year: 2018}); isNew || err != nil {
		t.Errorf("add = %v, %v; want false, nil", isNew, err)
	}
}

func TestWatchSaveFailure(t *testing.T) {
	db := openStore(t, t.TempDir())
	p := watchingProvider(t, db)
	db.Close()
	title, found, err := p.SearchTVDB(context.Background(), 5, ranchS02E01)
	if err != nil || title != "The Ranch" || len(found) != 1 {
		t.Fatalf("SearchTVDB = %q, %+v, %v", title, found, err)
	}
	if ids := p.watchedSeries.ids(); !slices.Equal(ids, []int{5}) {
		t.Errorf("watched %v, want [5]", ids)
	}
}

func TestWatchingMarksFeedStale(t *testing.T) {
	p := newProvider(t)
	p.seriesFeed.built = time.Now()
	p.watch(5, "The Ranch")
	if !p.seriesFeed.stale {
		t.Error("feed not stale after watching a new series")
	}
	p.seriesFeed.stale = false
	p.watch(5, "The Ranch")
	if p.seriesFeed.stale {
		t.Error("feed stale after watching a series again")
	}
}
