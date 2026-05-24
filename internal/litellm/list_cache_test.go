// SPDX-License-Identifier: Apache-2.0

package litellm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
)

// mockMCPListServer returns an httptest.Server serving /v1/mcp/server
// with the configured payload + counts the requests received.
func mockMCPListServer(t *testing.T, payload []MCPServerEntry) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var count atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_ = json.NewEncoder(w).Encode(payload)
	}))
	return srv, &count
}

// TestCachedListMCPServers_NilCachePassthrough — Client constructed
// without WithListCacheTTL (or ttl<=0) falls through to ListMCPServers
// on every call.
func TestCachedListMCPServers_NilCachePassthrough(t *testing.T) {
	srv, count := mockMCPListServer(t, []MCPServerEntry{{ServerID: "x", ServerName: "y"}})
	defer srv.Close()
	c := NewClient(srv.URL, "sk-test", logr.Discard())
	for i := 0; i < 3; i++ {
		if _, err := c.CachedListMCPServers(context.Background()); err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
	}
	if got := count.Load(); got != 3 {
		t.Errorf("nil cache should passthrough; want 3 upstream hits, got %d", got)
	}
}

// TestCachedListMCPServers_CacheHit — within TTL, subsequent calls
// return cached payload without hitting upstream.
func TestCachedListMCPServers_CacheHit(t *testing.T) {
	srv, count := mockMCPListServer(t, []MCPServerEntry{{ServerID: "x", ServerName: "y"}})
	defer srv.Close()
	c := NewClient(srv.URL, "sk-test", logr.Discard(), WithListCacheTTL(time.Second))
	for i := 0; i < 5; i++ {
		out, err := c.CachedListMCPServers(context.Background())
		if err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
		if len(out) != 1 || out[0].ServerName != "y" {
			t.Errorf("iter %d: unexpected payload %+v", i, out)
		}
	}
	if got := count.Load(); got != 1 {
		t.Errorf("cache hit: want 1 upstream hit (first call), got %d", got)
	}
}

// TestCachedListMCPServers_TTLExpiry — after TTL, next call refetches.
func TestCachedListMCPServers_TTLExpiry(t *testing.T) {
	srv, count := mockMCPListServer(t, []MCPServerEntry{{ServerID: "x", ServerName: "y"}})
	defer srv.Close()
	c := NewClient(srv.URL, "sk-test", logr.Discard(), WithListCacheTTL(50*time.Millisecond))
	if _, err := c.CachedListMCPServers(context.Background()); err != nil {
		t.Fatalf("first: %v", err)
	}
	time.Sleep(75 * time.Millisecond)
	if _, err := c.CachedListMCPServers(context.Background()); err != nil {
		t.Fatalf("post-ttl: %v", err)
	}
	if got := count.Load(); got != 2 {
		t.Errorf("ttl expiry: want 2 upstream hits, got %d", got)
	}
}

// TestCachedListMCPServers_Singleflight — N concurrent callers on a
// cold cache produce exactly ONE upstream fetch (in-flight dedup).
func TestCachedListMCPServers_Singleflight(t *testing.T) {
	var count atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		// Hold the response open long enough that 20 concurrent callers
		// definitely race past the cache-miss check.
		time.Sleep(40 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]MCPServerEntry{{ServerID: "x", ServerName: "y"}})
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "sk-test", logr.Discard(), WithListCacheTTL(time.Second))

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.CachedListMCPServers(context.Background()); err != nil {
				t.Errorf("concurrent: %v", err)
			}
		}()
	}
	wg.Wait()
	if got := count.Load(); got != 1 {
		t.Errorf("singleflight: want 1 upstream hit for 20 concurrent callers, got %d", got)
	}
}

// TestCachedListAgents_CacheHit — sanity-check the Agents variant
// mirrors the MCP shape (same single-flight + TTL).
func TestCachedListAgents_CacheHit(t *testing.T) {
	var count atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]AgentEntry{{AgentID: "a1", AgentName: "a"}})
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "sk-test", logr.Discard(), WithListCacheTTL(time.Second))
	for i := 0; i < 4; i++ {
		if _, err := c.CachedListAgents(context.Background()); err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
	}
	if got := count.Load(); got != 1 {
		t.Errorf("agents cache hit: want 1 upstream hit, got %d", got)
	}
}
