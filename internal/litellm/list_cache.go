// SPDX-License-Identifier: Apache-2.0

package litellm

import (
	"context"
	"sync"
	"time"
)

// DefaultListCacheTTL is the default freshness window for the in-memory
// LIST cache used by the vanish-probe hot paths. 30s is short enough that
// out-of-band drift is detected on the second safety-relist tick after
// the deletion at worst (safety-relist cadence is 5m+); it is long
// enough that 26 MCPServer CRs reconciling within their jitter window
// share a single upstream LIST instead of issuing 26.
const DefaultListCacheTTL = 30 * time.Second

// listCacheEntry holds a single cached LIST result + the wall time the
// fetch completed. err is preserved so callers see the same outcome
// every concurrent caller saw (ErrNotFound, transient, success).
type listCacheEntry struct {
	mcp     []MCPServerEntry
	agents  []AgentEntry
	err     error
	fetched time.Time
}

// listCacheStore is the per-Client memoization layer wrapping the
// LIST endpoints that vanish-probes consult. It dedupes concurrent
// callers via a single-flight pattern (mu-guarded in-flight channel)
// so the thundering herd at TTL expiry produces at most ONE upstream
// fetch per endpoint.
//
// The store is intentionally rolled in-house instead of pulling
// golang.org/x/sync/singleflight — keeps the dep surface unchanged
// (govulncheck gate sensitive). Implementation is ~50 lines per
// endpoint and only used in this package.
type listCacheStore struct {
	mu  sync.Mutex
	ttl time.Duration

	mcp         *listCacheEntry
	mcpInflight chan struct{}
	// mcpEpoch is a monotonic counter bumped on every MCP invalidation.
	// The single-flight leader captures it before its upstream fetch and
	// skips caching its (now stale) snapshot if the epoch advanced during
	// the call — see CachedListMCPServers (#60).
	mcpEpoch uint64

	agents         *listCacheEntry
	agentsInflight chan struct{}
	// agentsEpoch mirrors mcpEpoch for the /v1/agents endpoint.
	agentsEpoch uint64
}

// CachedListMCPServers returns the cached ListMCPServers result if
// fresher than the configured TTL; otherwise issues a single upstream
// LIST that all concurrent callers wait on. On error, the error is
// cached too (callers re-fetch only after TTL expires).
//
// Designed for vanish-probe consumers (mcpserver_controller.go Step 7b).
// Direct callers that need fresh data (finalizer drain, orphan
// adoption) keep using ListMCPServers.
//
// nil cache (Client constructed with WithListCacheTTL(0)) falls
// through to ListMCPServers — preserves test paths that want fresh
// state every call.
// invalidateMCPCache drops any cached ListMCPServers entry. Called by
// CreateMCPServer / UpdateMCPServer / DeleteMCPServer so the operator's
// own mutations are visible to the very next vanish-probe rather than
// suffering the TTL window. Safe to call when listCache is nil.
func (c *Client) invalidateMCPCache() {
	if c.listCache == nil {
		return
	}
	c.listCache.mu.Lock()
	c.listCache.mcp = nil
	c.listCache.mcpEpoch++
	c.listCache.mu.Unlock()
}

// invalidateAgentsCache mirrors invalidateMCPCache for /v1/agents.
func (c *Client) invalidateAgentsCache() {
	if c.listCache == nil {
		return
	}
	c.listCache.mu.Lock()
	c.listCache.agents = nil
	c.listCache.agentsEpoch++
	c.listCache.mu.Unlock()
}

func (c *Client) CachedListMCPServers(ctx context.Context) ([]MCPServerEntry, error) {
	if c.listCache == nil {
		return c.ListMCPServers(ctx)
	}
	c.listCache.mu.Lock()
	if c.listCache.mcp != nil && time.Since(c.listCache.mcp.fetched) < c.listCache.ttl {
		out := c.listCache.mcp
		c.listCache.mu.Unlock()
		return out.mcp, out.err
	}
	if c.listCache.mcpInflight != nil {
		ch := c.listCache.mcpInflight
		c.listCache.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		c.listCache.mu.Lock()
		out := c.listCache.mcp
		c.listCache.mu.Unlock()
		if out == nil {
			// Inflight completed but the cache is empty: the leader either
			// panicked (its store never ran) or a mid-flight invalidation
			// made it skip the store (#60). Fetch directly rather than
			// serve nil.
			return c.ListMCPServers(ctx)
		}
		return out.mcp, out.err
	}
	ch := make(chan struct{})
	c.listCache.mcpInflight = ch
	startEpoch := c.listCache.mcpEpoch
	c.listCache.mu.Unlock()

	// Reset the in-flight marker and wake all waiters even if ListMCPServers
	// panics (JSON nil-deref, io.ReadAll OOM, transport panic) or ctx is
	// canceled. Without this defer a leader failure strands every waiter on
	// <-ch forever — operator-wide deadlock until pod restart (#51).
	defer func() {
		c.listCache.mu.Lock()
		c.listCache.mcpInflight = nil
		c.listCache.mu.Unlock()
		close(ch)
	}()

	entries, err := c.ListMCPServers(ctx)

	c.listCache.mu.Lock()
	// Store only if no invalidation landed during the fetch. A mid-flight
	// Create/Update/Delete bumps mcpEpoch; caching the pre-mutation snapshot
	// here would clobber that invalidation and let a vanish-probe delete a
	// just-created server (#60). The leader still returns its own result to
	// its caller below — it just doesn't poison the shared cache.
	if c.listCache.mcpEpoch == startEpoch {
		c.listCache.mcp = &listCacheEntry{mcp: entries, err: err, fetched: time.Now()}
	}
	c.listCache.mu.Unlock()
	return entries, err
}

// CachedListAgents mirrors CachedListMCPServers for ListAgents.
// Used by a2aagent_controller.go Step 8b vanish-probe.
func (c *Client) CachedListAgents(ctx context.Context) ([]AgentEntry, error) {
	if c.listCache == nil {
		return c.ListAgents(ctx)
	}
	c.listCache.mu.Lock()
	if c.listCache.agents != nil && time.Since(c.listCache.agents.fetched) < c.listCache.ttl {
		out := c.listCache.agents
		c.listCache.mu.Unlock()
		return out.agents, out.err
	}
	if c.listCache.agentsInflight != nil {
		ch := c.listCache.agentsInflight
		c.listCache.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		c.listCache.mu.Lock()
		out := c.listCache.agents
		c.listCache.mu.Unlock()
		if out == nil {
			return c.ListAgents(ctx)
		}
		return out.agents, out.err
	}
	ch := make(chan struct{})
	c.listCache.agentsInflight = ch
	startEpoch := c.listCache.agentsEpoch
	c.listCache.mu.Unlock()

	// Reset the in-flight marker and wake all waiters even if ListAgents
	// panics or ctx is canceled — otherwise waiters strand forever (#51).
	defer func() {
		c.listCache.mu.Lock()
		c.listCache.agentsInflight = nil
		c.listCache.mu.Unlock()
		close(ch)
	}()

	entries, err := c.ListAgents(ctx)

	c.listCache.mu.Lock()
	// Skip the store if an invalidation advanced the epoch mid-flight (#60).
	if c.listCache.agentsEpoch == startEpoch {
		c.listCache.agents = &listCacheEntry{agents: entries, err: err, fetched: time.Now()}
	}
	c.listCache.mu.Unlock()
	return entries, err
}
