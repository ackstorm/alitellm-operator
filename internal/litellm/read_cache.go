// SPDX-License-Identifier: MIT

package litellm

import (
	"context"
	"sync"
	"time"
)

// DefaultReadCacheTTL is the freshness window for the catalog GETs that
// every CR of a kind reads on each safety-relist tick (GET /model/info,
// GET /v2/guardrails/list). A relist burst drains serially in ~30s, so a
// 5s window collapses 165 per-model reads into a handful of full-list
// reads, while out-of-band drift is still seen within one TTL.
const DefaultReadCacheTTL = 5 * time.Second

// readCache memoizes successful GET bodies by path for ttl. Any write
// through the Client (see makeRequest) drops the whole cache, so a
// read-back after the operator's own mutation is always live. The lock
// is held across the fetch: concurrent readers of an expired path wait
// for one upstream call instead of each issuing their own.
//
// ponytail: one global lock serializes cached reads across paths; fine
// at two cached paths and serial relist bursts — key the lock per path
// if a third hot path shows contention.
type readCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	entries map[string]readCacheEntry
}

type readCacheEntry struct {
	body    []byte
	fetched time.Time
}

// WithReadCacheTTL enables the per-Client GET cache used by getCached.
// ttl <= 0 disables it (every read goes upstream).
func WithReadCacheTTL(ttl time.Duration) ClientOption {
	return func(c *Client) {
		if ttl > 0 {
			c.readCache = &readCache{ttl: ttl, entries: map[string]readCacheEntry{}}
		} else {
			c.readCache = nil
		}
	}
}

// getCached issues GET path through the read cache. Errors are never
// cached. The returned slice is shared across callers: decode it, never
// mutate it.
func (c *Client) getCached(ctx context.Context, path string) ([]byte, error) {
	rc := c.readCache
	if rc == nil {
		return c.makeRequest(ctx, "GET", path, nil)
	}
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if e, ok := rc.entries[path]; ok && time.Since(e.fetched) < rc.ttl {
		return e.body, nil
	}
	body, err := c.makeRequest(ctx, "GET", path, nil)
	if err != nil {
		return nil, err
	}
	rc.entries[path] = readCacheEntry{body: body, fetched: time.Now()}
	return body, nil
}

// dropReadCache empties the read cache. makeRequest defers it on every
// non-GET, so it runs once the write has completed: a read that raced the
// write either already stored (and is dropped here) or still holds mu and
// is dropped as soon as it stores.
func (c *Client) dropReadCache() {
	rc := c.readCache
	if rc == nil {
		return
	}
	rc.mu.Lock()
	clear(rc.entries)
	rc.mu.Unlock()
}
