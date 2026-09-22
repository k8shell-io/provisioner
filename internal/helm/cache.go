// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package helm

import (
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// listCacheTTL bounds how long a cached Kubernetes/Helm listing is served
// before a request pays the cost of a fresh fetch. It exists to absorb
// bursts of near-duplicate read-only workspace lookups under concurrent
// load, not to serve stale data, so it is kept short relative to how long a
// workspace lifecycle operation actually takes.
const listCacheTTL = 2 * time.Second

type cacheEntry[V any] struct {
	value   V
	fetched time.Time
}

// ttlCache is a small in-memory, per-process cache with request coalescing:
// concurrent callers missing on the same key during a fetch share one
// upstream call (via singleflight) instead of each issuing their own. It
// backs the read-only workspace lookup RPCs' Kubernetes/Helm list calls;
// callers that must observe a just-completed mutation must not go through
// it.
type ttlCache[V any] struct {
	mu    sync.RWMutex
	byKey map[string]cacheEntry[V]
	group singleflight.Group
}

func newTTLCache[V any]() *ttlCache[V] {
	return &ttlCache[V]{byKey: make(map[string]cacheEntry[V])}
}

// getOrFetch returns the cached value for key if it's within listCacheTTL,
// otherwise calls fetch and caches the result. Concurrent calls for the same
// key during a miss are coalesced into a single fetch.
func (c *ttlCache[V]) getOrFetch(key string, fetch func() (V, error)) (V, error) {
	c.mu.RLock()
	entry, ok := c.byKey[key]
	c.mu.RUnlock()
	if ok && time.Since(entry.fetched) < listCacheTTL {
		return entry.value, nil
	}

	v, err, _ := c.group.Do(key, func() (interface{}, error) {
		value, err := fetch()
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		c.byKey[key] = cacheEntry[V]{value: value, fetched: time.Now()}
		c.mu.Unlock()
		return value, nil
	})
	if err != nil {
		var zero V
		return zero, err
	}
	return v.(V), nil
}
