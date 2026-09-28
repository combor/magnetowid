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

	"github.com/combor/vodarr/internal/provider"
	"github.com/combor/vodarr/internal/store"
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

// watchingProvider is newProvider with fake title lookups and its watch
// list in db.
func watchingProvider(t *testing.T, db *bolt.DB) *Provider {
	t.Helper()
	p := newProvider(t)
	fakeTitles(t, p)
	w, err := loadWatchList(db)
	if err != nil {
		t.Fatal(err)
	}
	p.watched = w
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
	w, err := loadWatchList(db)
	if err != nil {
		t.Fatal(err)
	}
	if ids := w.ids(); !slices.Equal(ids, []int{5}) {
		t.Fatalf("watched %v after reopening, want [5]", ids)
	}
	// A series already watched isn't saved again, so saving can't fail.
	db.Close()
	if isNew, err := w.add(5); isNew || err != nil {
		t.Errorf("add(5) = %v, %v; want false, nil", isNew, err)
	}
}

func TestCorruptWatchRecordFailsNew(t *testing.T) {
	db := openStore(t, t.TempDir())
	err := db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(watchBucket)
		if err != nil {
			return err
		}
		return b.Put([]byte("5"), []byte("{bad"))
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(http.DefaultClient, slog.New(slog.DiscardHandler), db); err == nil || !strings.Contains(err.Error(), "series 5") {
		t.Fatalf("err = %v", err)
	}
}

// A search doesn't fail because the watch list can't be saved.
func TestWatchSaveFailure(t *testing.T) {
	db := openStore(t, t.TempDir())
	p := watchingProvider(t, db)
	db.Close()
	title, found, err := p.SearchTVDB(context.Background(), 5, ranchS02E01)
	if err != nil || title != "The Ranch" || len(found) != 1 {
		t.Fatalf("SearchTVDB = %q, %+v, %v", title, found, err)
	}
	if ids := p.watched.ids(); !slices.Equal(ids, []int{5}) {
		t.Errorf("watched %v, want [5]", ids)
	}
}

// A newly watched series joins the feed at the next RSS sync, not feedTTL
// later.
func TestWatchingMarksFeedStale(t *testing.T) {
	p := newProvider(t)
	p.feed.built = time.Now()
	p.watch(5, "The Ranch")
	if !p.feed.stale {
		t.Error("feed not stale after watching a new series")
	}
	p.feed.stale = false
	p.watch(5, "The Ranch")
	if p.feed.stale {
		t.Error("feed stale after watching a series again")
	}
}
