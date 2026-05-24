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

	agents         *listCacheEntry
	agentsInflight chan struct{}
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
	c.listCache.mu.Unlock()
}

// invalidateAgentsCache mirrors invalidateMCPCache for /v1/agents.
func (c *Client) invalidateAgentsCache() {
	if c.listCache == nil {
		return
	}
	c.listCache.mu.Lock()
	c.listCache.agents = nil
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
		<-ch
		c.listCache.mu.Lock()
		out := c.listCache.mcp
		c.listCache.mu.Unlock()
		if out == nil {
			// Inflight completed but cleared (rare race on close); fall
			// through to a direct fetch rather than serve nil.
			return c.ListMCPServers(ctx)
		}
		return out.mcp, out.err
	}
	ch := make(chan struct{})
	c.listCache.mcpInflight = ch
	c.listCache.mu.Unlock()

	entries, err := c.ListMCPServers(ctx)

	c.listCache.mu.Lock()
	c.listCache.mcp = &listCacheEntry{mcp: entries, err: err, fetched: time.Now()}
	c.listCache.mcpInflight = nil
	c.listCache.mu.Unlock()
	close(ch)
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
		<-ch
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
	c.listCache.mu.Unlock()

	entries, err := c.ListAgents(ctx)

	c.listCache.mu.Lock()
	c.listCache.agents = &listCacheEntry{agents: entries, err: err, fetched: time.Now()}
	c.listCache.agentsInflight = nil
	c.listCache.mu.Unlock()
	close(ch)
	return entries, err
}
