package tvp

import (
	"sync"
	"time"
)

// apiCacheTTL covers a season search's per-episode fallback and a feed
// rebuild, which ask for the same lists again, and is short enough that new
// episodes show up soon.
const apiCacheTTL = 10 * time.Minute

// responseCache keeps decoded TVP responses, which are much smaller than the
// JSON. It is safe for concurrent use.
type responseCache struct {
	now func() time.Time // for tests

	mu      sync.Mutex
	entries map[string]cacheEntry
}

type cacheEntry struct {
	value   any
	expires time.Time
}

func newResponseCache() *responseCache {
	return &responseCache{now: time.Now, entries: make(map[string]cacheEntry)}
}

// cached returns the value cached under key, or what fetch returns, which
// is cached unless it is an error. The value is shared, so callers must not
// modify it.
func cached[T any](c *responseCache, key string, fetch func() (T, error)) (T, error) {
	now := c.now()
	c.mu.Lock()
	hit, ok := c.entries[key]
	c.mu.Unlock()
	if ok && now.Before(hit.expires) {
		return hit.value.(T), nil
	}
	v, err := fetch()
	if err != nil {
		return v, err
	}
	c.mu.Lock()
	for k, e := range c.entries {
		if !now.Before(e.expires) {
			delete(c.entries, k)
		}
	}
	c.entries[key] = cacheEntry{value: v, expires: now.Add(apiCacheTTL)}
	c.mu.Unlock()
	return v, nil
}
