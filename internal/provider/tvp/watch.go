package tvp

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
)

// watchBucket holds the watched series: the TVDB ID in decimal, and a
// watchRecord.
var watchBucket = []byte("tvp-watch")

// watchRecord is JSON so fields can be added.
type watchRecord struct {
	Added time.Time `json:"added"`
}

// watchList is the series, by TVDB ID, that Sonarr has searched for and TVP
// has, whose new episodes the feed offers. Series stay on it: one that has
// ended has no new episodes, so it costs no TVP requests. It is safe for
// concurrent use.
type watchList struct {
	db *bolt.DB // nil keeps the list in memory only

	mu    sync.Mutex
	added map[int]time.Time
}

// loadWatchList loads the list saved in db.
func loadWatchList(db *bolt.DB) (*watchList, error) {
	w := &watchList{db: db, added: make(map[int]time.Time)}
	if db == nil {
		return w, nil
	}
	err := db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(watchBucket)
		if err != nil {
			return err
		}
		return b.ForEach(func(k, v []byte) error {
			id, err := strconv.Atoi(string(k))
			if err != nil {
				return fmt.Errorf("series %q: not a TVDB ID", k)
			}
			var r watchRecord
			if err := json.Unmarshal(v, &r); err != nil {
				return fmt.Errorf("series %s: %w", k, err)
			}
			w.added[id] = r.Added
			return nil
		})
	})
	if err != nil {
		return nil, fmt.Errorf("loading watched TVP series from %s: %w", db.Path(), err)
	}
	return w, nil
}

// add watches the series and reports whether it is new to the list. A new
// one is watched even if saving it fails, until vodarr restarts.
func (w *watchList) add(tvdbID int) (bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.added[tvdbID]; ok {
		return false, nil
	}
	now := time.Now()
	w.added[tvdbID] = now
	if w.db == nil {
		return true, nil
	}
	v, err := json.Marshal(watchRecord{Added: now})
	if err == nil {
		err = w.db.Update(func(tx *bolt.Tx) error {
			return tx.Bucket(watchBucket).Put([]byte(strconv.Itoa(tvdbID)), v)
		})
	}
	if err != nil {
		return true, fmt.Errorf("saving watched series %d: %w", tvdbID, err)
	}
	return true, nil
}

// ids returns the watched TVDB IDs in order.
func (w *watchList) ids() []int {
	w.mu.Lock()
	defer w.mu.Unlock()
	ids := make([]int, 0, len(w.added))
	for id := range w.added {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// watch adds the series to the watch list. A new one makes the feed stale,
// so it joins the feed at the next RSS sync.
func (p *Provider) watch(tvdbID int, title string) {
	isNew, err := p.watched.add(tvdbID)
	if !isNew {
		return
	}
	p.feed.markStale()
	p.log.Info("watching TVP series for new episodes", "tvdbid", tvdbID, "title", title)
	if err != nil {
		p.log.Warn("can't save a watched TVP series; watching it until vodarr restarts", "tvdbid", tvdbID, "err", err)
	}
}
