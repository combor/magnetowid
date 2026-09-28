package tvp

import (
	"sync"
	"time"
)

// Reuse listings across season searches and feed rebuilds without delaying new episodes for long.
const apiCacheTTL = 10 * time.Minute

// Cache decoded responses to save memory. Safe for concurrent use.
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

// Cache successful fetches only. Returned values are shared and must not be modified.
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
