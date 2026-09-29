package bbc

import (
	"context"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/combor/magnetowid/internal/provider"
	"github.com/combor/magnetowid/internal/store"
)

const newestFilms = `{"category_programmes":{"count":3,"elements":[
	{"id":"m00327ht","type":"programme_large","title":"Nickel Boys","tleo_type":"episode","initial_children":[
		{"id":"m00327ht","type":"episode","title":"Nickel Boys","tleo_type":"episode","release_date":"2024",
		 "release_date_time":"2024-01-01T00:00:00.000Z","versions":[{"id":"m00327hs","kind":"original",
		 "duration":{"value":"PT2H9M59.200S"},"availability":{"start":"2026-09-29T00:16:14Z"}}]}]},
	{"id":"b006pn88","type":"programme_large","title":"Arena","tleo_type":"brand","initial_children":[
		{"id":"m002z0zd","type":"episode","title":"Arena","subtitle":"Kim Novak's Vertigo","tleo_type":"brand"}]},
	{"id":"b0074t6w","type":"programme_large","title":"The 39 Steps","tleo_type":"episode","initial_children":[
		{"id":"b0074t6w","type":"episode","title":"The 39 Steps","tleo_type":"episode","release_date":"6 Jun 1935",
		 "release_date_time":"1935-06-06T00:00:00.000Z","versions":[{"id":"b0074t6v","kind":"editorial",
		 "duration":{"value":"PT1H22M29S"}}]}]}]}}`

func releases(rs []provider.Release) []string {
	var out []string
	for _, r := range rs {
		out = append(out, r.Title+" "+r.ID)
	}
	slices.Sort(out)
	return out
}

func TestSeriesFeed(t *testing.T) {
	var failing atomic.Bool
	p := newProviderWith(t, func(key string) (string, bool) {
		if failing.Load() && key == "/ibl/v1/new-search?q=Doctor Who" {
			return "500 oops", true
		}
		return "", false
	})
	p.cache.now = func() time.Time { return time.Now().Add(-apiCacheTTL) } // never cache
	ctx := context.Background()
	p.watch(449991, "Doctor Who (2023)")
	p.watch(83920, "Days of Honor") // not on iPlayer

	// Only S02E08 became available within two weeks.
	before := time.Now()
	p.rebuildSeries(ctx)
	rs := p.seriesFeed.recent()
	if got := releases(rs); !slices.Equal(got, []string{"Doctor Who (2023) m002d3lr"}) {
		t.Fatalf("feed %v", got)
	}
	if r := rs[0]; r.Season != 2 || r.Episode != 8 || r.Published.Before(before) {
		t.Errorf("release %+v", r)
	}
	// Dated when first found, and kept when iPlayer fails.
	first := rs[0].Published
	failing.Store(true)
	p.rebuildSeries(ctx)
	if rs := p.seriesFeed.recent(); len(rs) != 1 || !rs[0].Published.Equal(first) {
		t.Errorf("after a failure: %+v", rs)
	}
	failing.Store(false)
	p.rebuildSeries(ctx)
	if rs := p.seriesFeed.recent(); len(rs) != 1 || !rs[0].Published.Equal(first) {
		t.Errorf("after recovering: %+v", rs)
	}
}

func TestFilmFeed(t *testing.T) {
	p := newProviderWith(t, func(key string) (string, bool) {
		return newestFilms, key == "/ibl/v1/categories/films/programmes"
	})
	ctx := context.Background()
	p.rebuildFilms(ctx) // nothing watched: no request
	if rs := p.filmFeed.recent(); len(rs) != 0 {
		t.Fatalf("unwatched feed %v", releases(rs))
	}
	// Radarr's titles name releases; BBC's 1935 is a date, still the film's year.
	p.watchFilm("Nickel Boys", 2024)
	p.watchFilm("The 39 Steps", 1935)
	p.watchFilm("The 39 Steps", 1959)
	p.watchFilm("Vertigo", 1958)
	p.rebuildFilms(ctx)
	rs := p.filmFeed.recent()
	if got := releases(rs); !slices.Equal(got, []string{"Nickel Boys m00327ht", "The 39 Steps b0074t6w"}) {
		t.Fatalf("feed %v", got)
	}
	for _, r := range rs {
		if r.Kind != provider.Movie || r.Duration < time.Hour || (r.Title == "The 39 Steps" && r.Year != 1935) {
			t.Errorf("release %+v", r)
		}
	}
}

func TestRecentRebuildsInBackground(t *testing.T) {
	p := newProvider(t)
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	p.seriesFeed.rebuild = func(context.Context) {
		started <- struct{}{}
		<-release
	}
	if rs, err := p.Recent(context.Background(), provider.Episode); err != nil || len(rs) != 0 {
		t.Fatalf("first sync: %v, %v", rs, err)
	}
	<-started
	// One rebuild at a time.
	p.Recent(context.Background(), provider.Episode)
	close(release)
	select {
	case <-started:
		t.Error("a second rebuild started while the first ran")
	case <-time.After(50 * time.Millisecond):
	}
}

func openStore(t *testing.T, dir string) *bolt.DB {
	t.Helper()
	db, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestWatchListsPersist(t *testing.T) {
	dir := t.TempDir()
	db := openStore(t, dir)
	p := newProvider(t)
	var err error
	if p.watchedSeries, err = loadWatchList(db, seriesWatch); err != nil {
		t.Fatal(err)
	}
	if p.watchedFilms, err = loadWatchList(db, filmWatch); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.SearchTVDB(context.Background(), 449991, provider.Query{Kind: provider.Episode, Season: 1, Episode: 1}); err != nil {
		t.Fatal(err)
	}
	p.watchFilm("Nickel Boys", 2024)
	db.Close()

	db = openStore(t, dir)
	series, err := loadWatchList(db, seriesWatch)
	if err != nil {
		t.Fatal(err)
	}
	films, err := loadWatchList(db, filmWatch)
	if err != nil {
		t.Fatal(err)
	}
	if got := series.ids(); !slices.Equal(got, []int{449991}) {
		t.Errorf("series %v", got)
	}
	if got := films.all(); len(got) != 1 || got[0].Title != "Nickel Boys" || got[0].Year != 2024 {
		t.Errorf("films %+v", got)
	}

	// A corrupt entry fails startup rather than being dropped.
	db.Update(func(tx *bolt.Tx) error { return tx.Bucket(seriesWatch.bucket).Put([]byte("x"), []byte("{}")) })
	if _, err := loadWatchList(db, seriesWatch); err == nil {
		t.Error("loaded a watch list with a non-TVDB key")
	}
}
