// Package overrides keeps the user's corrections to automatic matching, gives
// each site its own, and serves them over HTTP.
package overrides

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"sync"

	bolt "go.etcd.io/bbolt"

	"github.com/combor/magnetowid/internal/provider"
)

// Keys are "<site> series <TVDB ID>" and "<site> film <provider.FilmKey>".
var bucket = []byte("overrides")

// ErrUnknownSite reports a site that isn't registered or takes no overrides.
var ErrUnknownSite = errors.New("no such site takes overrides")

// InvalidError reports an override the store refuses.
type InvalidError struct{ msg string }

func (e *InvalidError) Error() string { return e.msg }

func invalid(format string, args ...any) error {
	return &InvalidError{fmt.Sprintf(format, args...)}
}

// Store is safe for concurrent use.
type Store struct {
	db        *bolt.DB // nil keeps overrides in memory only
	providers *provider.Registry

	mu    sync.Mutex
	sites map[string]*provider.Overrides // replaced, never modified, once given to a site
}

// Open loads saved overrides and gives each site its own.
func Open(db *bolt.DB, providers *provider.Registry) (*Store, error) {
	s := &Store{db: db, providers: providers, sites: make(map[string]*provider.Overrides)}
	if db != nil {
		err := db.Update(func(tx *bolt.Tx) error {
			b, err := tx.CreateBucketIfNotExists(bucket)
			if err != nil {
				return err
			}
			return b.ForEach(func(k, v []byte) error {
				if err := s.load(string(k), v); err != nil {
					return fmt.Errorf("override %q: %w", k, err)
				}
				return nil
			})
		})
		if err != nil {
			return nil, fmt.Errorf("loading overrides from %s: %w", db.Path(), err)
		}
	}
	for _, name := range providers.Names() {
		s.notify(name)
	}
	return s, nil
}

// Overrides of sites no longer registered are kept.
func (s *Store) load(key string, v []byte) error {
	site, rest, _ := strings.Cut(key, " ")
	kind, id, _ := strings.Cut(rest, " ")
	o := s.sites[site]
	if o == nil {
		o = clone(nil)
		s.sites[site] = o
	}
	switch kind {
	case "series":
		tvdbID, err := strconv.Atoi(id)
		if err != nil {
			return errors.New("not a TVDB ID")
		}
		var so provider.SeriesOverride
		if err := json.Unmarshal(v, &so); err != nil {
			return err
		}
		o.Series[tvdbID] = so
	case "film":
		var fo provider.FilmOverride
		if err := json.Unmarshal(v, &fo); err != nil {
			return err
		}
		o.Films[id] = fo
	default:
		return errors.New("unknown kind")
	}
	return nil
}

func clone(o *provider.Overrides) *provider.Overrides {
	c := &provider.Overrides{Series: make(map[int]provider.SeriesOverride), Films: make(map[string]provider.FilmOverride)}
	if o != nil {
		maps.Copy(c.Series, o.Series)
		maps.Copy(c.Films, o.Films)
	}
	return c
}

// Call with s.mu held, or before s is shared.
func (s *Store) notify(site string) {
	if o, ok := s.overridable(site); ok {
		o.SetOverrides(s.sites[site])
	}
}

func (s *Store) overridable(site string) (provider.Overridable, bool) {
	p, ok := s.providers.Get(site)
	if !ok {
		return nil, false
	}
	o, ok := p.(provider.Overridable)
	return o, ok
}

// Sites returns the sites that take overrides, in sorted order.
func (s *Store) Sites() []string {
	var sites []string
	for _, name := range s.providers.Names() {
		if _, ok := s.overridable(name); ok {
			sites = append(sites, name)
		}
	}
	return sites
}

// Site returns a site's overrides, nil if none. Do not modify them.
func (s *Store) Site(site string) *provider.Overrides {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sites[site]
}

// PutSeries returns the override as saved, with IDs taken from any URLs.
func (s *Store) PutSeries(site string, tvdbID int, o provider.SeriesOverride) (provider.SeriesOverride, error) {
	parser, ok := s.overridable(site)
	if !ok {
		return provider.SeriesOverride{}, ErrUnknownSite
	}
	o, err := checkSeries(tvdbID, o, parser)
	if err != nil {
		return provider.SeriesOverride{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return o, s.write(site, seriesKey(site, tvdbID), o, func(c *provider.Overrides) { c.Series[tvdbID] = o })
}

// DeleteSeries reports whether the override existed.
func (s *Store) DeleteSeries(site string, tvdbID int) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sites[site].SeriesFor(tvdbID); !ok {
		return false, nil
	}
	return true, s.write(site, seriesKey(site, tvdbID), nil, func(c *provider.Overrides) { delete(c.Series, tvdbID) })
}

// PutFilm returns the override as saved, with IDs taken from any URLs.
func (s *Store) PutFilm(site, title string, year int, o provider.FilmOverride) (provider.FilmOverride, error) {
	parser, ok := s.overridable(site)
	if !ok {
		return provider.FilmOverride{}, ErrUnknownSite
	}
	o, err := checkFilm(title, year, o, parser)
	if err != nil {
		return provider.FilmOverride{}, err
	}
	key := provider.FilmKey(o.Title, o.Year)
	s.mu.Lock()
	defer s.mu.Unlock()
	return o, s.write(site, filmKey(site, key), o, func(c *provider.Overrides) { c.Films[key] = o })
}

// DeleteFilm reports whether the override existed.
func (s *Store) DeleteFilm(site, title string, year int) (bool, error) {
	key := provider.FilmKey(title, year)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sites[site].Film(title, year); !ok {
		return false, nil
	}
	return true, s.write(site, filmKey(site, key), nil, func(c *provider.Overrides) { delete(c.Films, key) })
}

func seriesKey(site string, tvdbID int) string { return site + " series " + strconv.Itoa(tvdbID) }

func filmKey(site, key string) string { return site + " film " + key }

// Save v, or delete the key if v is nil, then give the site its changed
// overrides. Call with s.mu held.
func (s *Store) write(site, key string, v any, change func(*provider.Overrides)) error {
	if s.db != nil {
		var data []byte
		if v != nil {
			var err error
			if data, err = json.Marshal(v); err != nil {
				return err
			}
		}
		err := s.db.Update(func(tx *bolt.Tx) error {
			b := tx.Bucket(bucket)
			if data == nil {
				return b.Delete([]byte(key))
			}
			return b.Put([]byte(key), data)
		})
		if err != nil {
			return fmt.Errorf("saving override %q: %w", key, err)
		}
	}
	c := clone(s.sites[site])
	change(c)
	s.sites[site] = c
	s.notify(site)
	return nil
}
