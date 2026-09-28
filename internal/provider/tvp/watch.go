package tvp

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/combor/vodarr/internal/provider"
)

// watchKind is what a watch list holds, and the bucket it is saved in, by a
// key and a watchRecord.
type watchKind struct {
	name   string // for errors
	bucket []byte
	check  func(key string, r watchRecord) error // a failure fails startup
}

// seriesWatch holds the series Sonarr has searched for by TVDB ID, keyed by
// the ID in decimal.
var seriesWatch = watchKind{
	name:   "series",
	bucket: []byte("tvp-watch"),
	check: func(key string, _ watchRecord) error {
		if _, err := strconv.Atoi(key); err != nil {
			return errors.New("not a TVDB ID")
		}
		return nil
	},
}

// filmWatch holds the films Radarr has searched for, keyed by filmKey.
var filmWatch = watchKind{
	name:   "film",
	bucket: []byte("tvp-watch-films"),
	check: func(_ string, r watchRecord) error {
		if r.Title == "" || r.Year <= 0 {
			return errors.New("no title or year")
		}
		return nil
	},
}

// filmKey tells films apart as Radarr's search does, by title and year.
func filmKey(title string, year int) string {
	return strconv.Itoa(year) + " " + provider.NormalizeTitle(title)
}

// watchRecord is JSON so fields can be added.
type watchRecord struct {
	Added time.Time `json:"added"`
	// For films, the title and year Radarr searched with, which name the
	// release.
	Title string `json:"title,omitempty"`
	Year  int    `json:"year,omitempty"`
}

// watchList is what Sonarr or Radarr has searched for, whose new releases
// the feeds offer. Entries stay on it: a series that has ended has no new
// episodes, so it costs no TVP requests, and a film only has to be matched
// against TVP's newest. It is safe for concurrent use.
type watchList struct {
	db   *bolt.DB // nil keeps the list in memory only
	kind watchKind

	mu      sync.Mutex
	records map[string]watchRecord
}

// loadWatchList loads the list saved in db.
func loadWatchList(db *bolt.DB, kind watchKind) (*watchList, error) {
	w := &watchList{db: db, kind: kind, records: make(map[string]watchRecord)}
	if db == nil {
		return w, nil
	}
	err := db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(kind.bucket)
		if err != nil {
			return err
		}
		return b.ForEach(func(k, v []byte) error {
			var r watchRecord
			if err := json.Unmarshal(v, &r); err != nil {
				return fmt.Errorf("%s %s: %w", kind.name, k, err)
			}
			if err := kind.check(string(k), r); err != nil {
				return fmt.Errorf("%s %q: %w", kind.name, k, err)
			}
			w.records[string(k)] = r
			return nil
		})
	})
	if err != nil {
		return nil, fmt.Errorf("loading TVP watch list from %s: %w", db.Path(), err)
	}
	return w, nil
}

// add watches r under key and reports whether the key is new to the list. A
// new one is watched even if saving it fails, until vodarr restarts.
func (w *watchList) add(key string, r watchRecord) (bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.records[key]; ok {
		return false, nil
	}
	r.Added = time.Now()
	w.records[key] = r
	if w.db == nil {
		return true, nil
	}
	v, err := json.Marshal(r)
	if err == nil {
		err = w.db.Update(func(tx *bolt.Tx) error {
			return tx.Bucket(w.kind.bucket).Put([]byte(key), v)
		})
	}
	if err != nil {
		return true, fmt.Errorf("saving watched %s %s: %w", w.kind.name, key, err)
	}
	return true, nil
}

// ids returns the watched TVDB IDs of a series list in order.
func (w *watchList) ids() []int {
	w.mu.Lock()
	defer w.mu.Unlock()
	ids := make([]int, 0, len(w.records))
	for k := range w.records {
		if id, err := strconv.Atoi(k); err == nil {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids
}

// all returns the records, oldest first.
func (w *watchList) all() []watchRecord {
	w.mu.Lock()
	defer w.mu.Unlock()
	keys := make([]string, 0, len(w.records))
	for k := range w.records {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b string) int {
		if c := w.records[a].Added.Compare(w.records[b].Added); c != 0 {
			return c
		}
		return cmp.Compare(a, b)
	})
	rs := make([]watchRecord, len(keys))
	for i, k := range keys {
		rs[i] = w.records[k]
	}
	return rs
}

// watch adds the series to the watch list. A new one makes the feed stale,
// so it joins the feed at the next RSS sync.
func (p *Provider) watch(tvdbID int, title string) {
	isNew, err := p.watchedSeries.add(strconv.Itoa(tvdbID), watchRecord{})
	if !isNew {
		return
	}
	p.seriesFeed.markStale()
	p.log.Info("watching TVP series for new episodes", "tvdbid", tvdbID, "title", title)
	if err != nil {
		p.log.Warn("can't save a watched TVP series; watching it until vodarr restarts", "tvdbid", tvdbID, "err", err)
	}
}

// watchFilm adds a film Radarr searched for to the watch list. The feed isn't
// made stale: TVP has just been searched for the film.
func (p *Provider) watchFilm(title string, year int) {
	isNew, err := p.watchedFilms.add(filmKey(title, year), watchRecord{Title: title, Year: year})
	if !isNew {
		return
	}
	p.log.Info("watching TVP for a film", "title", title, "year", year)
	if err != nil {
		p.log.Warn("can't save a watched film; watching it until vodarr restarts", "title", title, "year", year, "err", err)
	}
}
