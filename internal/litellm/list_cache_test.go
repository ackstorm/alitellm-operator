// SPDX-License-Identifier: Apache-2.0

package litellm

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
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

// jsonResp builds a 200 application/json *http.Response carrying body.
// Used by the fault-injection RoundTrippers below so a test can return a
// valid LIST payload without standing up an httptest.Server.
func jsonResp(body []byte) *http.Response {
	return &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(bytes.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}
}

// blockThenPanicRT makes the FIRST RoundTrip (the single-flight leader)
// wait on release and then panic, simulating a leader that hangs on a
// stuck upstream and then dies (transport panic / nil-deref / OOM).
// Every subsequent RoundTrip (a waiter's direct-fetch fallback) succeeds
// with payload. Lets a test assert that a leader panic UNBLOCKS waiters
// instead of stranding them (#51).
type blockThenPanicRT struct {
	release chan struct{}
	calls   atomic.Int64
	payload []byte
}

func (rt *blockThenPanicRT) RoundTrip(_ *http.Request) (*http.Response, error) {
	if rt.calls.Add(1) == 1 {
		<-rt.release
		panic("synthetic leader panic")
	}
	return jsonResp(rt.payload), nil
}

// blockOnceRT makes the FIRST RoundTrip wait on release and then succeed;
// later calls succeed immediately. Lets a test park the leader in-flight,
// fire an invalidation, then release the leader and assert the leader's
// now-stale snapshot did NOT repopulate the cache (#60).
type blockOnceRT struct {
	release chan struct{}
	calls   atomic.Int64
	payload []byte
}

func (rt *blockOnceRT) RoundTrip(_ *http.Request) (*http.Response, error) {
	if rt.calls.Add(1) == 1 {
		<-rt.release
	}
	return jsonResp(rt.payload), nil
}

// waitInflightMCP blocks until the MCP single-flight marker is published
// (leader is in-flight) or the deadline elapses. Returns true if observed.
func waitInflightMCP(c *Client, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		c.listCache.mu.Lock()
		inflight := c.listCache.mcpInflight != nil
		c.listCache.mu.Unlock()
		if inflight {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return false
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

// TestCachedListMCPServers_WaiterRespectsContextCancel — a waiter parked
// behind an in-flight leader must return promptly with ctx.Err() when its
// own context is canceled, instead of blocking on the (possibly doomed)
// leader (#51). The leader here is held open on a blocking httptest
// server for the duration of the test.
func TestCachedListMCPServers_WaiterRespectsContextCancel(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]MCPServerEntry{{ServerID: "x", ServerName: "y"}})
	}))
	// Defer order matters (LIFO): close(release) MUST run before srv.Close().
	// srv.Close() blocks until the in-flight leader request completes, and
	// that request is parked on <-release — so release has to close first.
	defer srv.Close()
	defer close(release)
	c := NewClient(srv.URL, "sk-test", logr.Discard(), WithListCacheTTL(time.Second))

	// Leader: occupies the in-flight slot, blocked on the server.
	go func() { _, _ = c.CachedListMCPServers(context.Background()) }()
	if !waitInflightMCP(c, 2*time.Second) {
		t.Fatal("leader never became in-flight")
	}

	// Waiter: cancelable ctx; should unblock via ctx.Done(), not the leader.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := c.CachedListMCPServers(ctx)
		done <- err
	}()
	time.Sleep(50 * time.Millisecond) // let the waiter park on the in-flight channel
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("waiter returned nil err after ctx cancel; want context.Canceled")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter did not return within 2s after ctx cancel — naked <-ch deadlock (#51)")
	}
}

// TestCachedListMCPServers_LeaderPanicUnblocksWaiters — when the leader's
// upstream fetch panics, the deferred cleanup must reset the in-flight
// marker and close the channel so parked waiters wake and fall through to
// a direct fetch, instead of blocking forever (#51).
func TestCachedListMCPServers_LeaderPanicUnblocksWaiters(t *testing.T) {
	payload, _ := json.Marshal([]MCPServerEntry{{ServerID: "x", ServerName: "y"}})
	rt := &blockThenPanicRT{release: make(chan struct{}), payload: payload}
	c := NewClient("http://injected", "sk-test", logr.Discard(), WithListCacheTTL(time.Second))
	c.httpClient.Transport = rt // in-package access to the unexported field

	// Leader: will block in RoundTrip, then panic on release. Recover the
	// panic in its own goroutine so it doesn't fail the test process.
	go func() {
		defer func() { _ = recover() }()
		_, _ = c.CachedListMCPServers(context.Background())
	}()
	if !waitInflightMCP(c, 2*time.Second) {
		t.Fatal("leader never became in-flight")
	}

	// Waiter: parks behind the leader.
	type result struct {
		out []MCPServerEntry
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := c.CachedListMCPServers(context.Background())
		done <- result{out: out, err: err}
	}()
	time.Sleep(50 * time.Millisecond) // let the waiter park

	close(rt.release) // leader unblocks → panics → defer fires → ch closes

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("waiter errored after leader panic: %v", r.err)
		}
		if len(r.out) != 1 || r.out[0].ServerName != "y" {
			t.Fatalf("waiter got unexpected payload after fallback: %+v", r.out)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter stranded after leader panic — defer cleanup missing (#51)")
	}
}

// TestCachedListMCPServers_MidFlightInvalidationNotClobbered — when an
// invalidation lands while the leader's LIST is in flight, the leader must
// NOT cache its pre-mutation snapshot (it would mask a just-created server
// from the vanish-probe and trigger a wrongful delete). After the leader
// returns, the cache must still be empty, so the NEXT call re-fetches (#60).
func TestCachedListMCPServers_MidFlightInvalidationNotClobbered(t *testing.T) {
	payload, _ := json.Marshal([]MCPServerEntry{{ServerID: "x", ServerName: "y"}})
	rt := &blockOnceRT{release: make(chan struct{}), payload: payload}
	c := NewClient("http://injected", "sk-test", logr.Discard(), WithListCacheTTL(time.Second))
	c.httpClient.Transport = rt

	// Leader: blocks in RoundTrip until released.
	leaderDone := make(chan struct{})
	go func() {
		_, _ = c.CachedListMCPServers(context.Background())
		close(leaderDone)
	}()
	if !waitInflightMCP(c, 2*time.Second) {
		t.Fatal("leader never became in-flight")
	}

	// Invalidate WHILE the leader is in-flight (e.g. a concurrent
	// CreateMCPServer succeeded upstream and cleared the cache).
	c.invalidateMCPCache()

	close(rt.release) // leader's LIST returns its now-stale snapshot
	select {
	case <-leaderDone:
	case <-time.After(2 * time.Second):
		t.Fatal("leader did not return")
	}

	// The leader must have SKIPPED its store (epoch advanced mid-flight).
	c.listCache.mu.Lock()
	cached := c.listCache.mcp
	c.listCache.mu.Unlock()
	if cached != nil {
		t.Fatal("leader cached its pre-mutation snapshot despite mid-flight invalidation (#60)")
	}

	// And the next call must re-fetch (RoundTrip call #2), not serve stale.
	before := rt.calls.Load()
	if _, err := c.CachedListMCPServers(context.Background()); err != nil {
		t.Fatalf("post-invalidation refetch: %v", err)
	}
	if got := rt.calls.Load(); got != before+1 {
		t.Fatalf("next call should re-fetch after skipped store; calls went %d -> %d", before, got)
	}
}

// waitInflightAgents mirrors waitInflightMCP for the /v1/agents slot.
func waitInflightAgents(c *Client, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		c.listCache.mu.Lock()
		inflight := c.listCache.agentsInflight != nil
		c.listCache.mu.Unlock()
		if inflight {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return false
}

// TestCachedListAgents_WaiterRespectsContextCancel — Agents mirror of the
// MCP waiter ctx-cancel test (#51, line 138).
func TestCachedListAgents_WaiterRespectsContextCancel(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]AgentEntry{{AgentID: "a1", AgentName: "a"}})
	}))
	// Defer order (LIFO): close(release) before srv.Close() — see the MCP
	// variant for why.
	defer srv.Close()
	defer close(release)
	c := NewClient(srv.URL, "sk-test", logr.Discard(), WithListCacheTTL(time.Second))

	go func() { _, _ = c.CachedListAgents(context.Background()) }()
	if !waitInflightAgents(c, 2*time.Second) {
		t.Fatal("agents leader never became in-flight")
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := c.CachedListAgents(ctx)
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("agents waiter returned nil err after ctx cancel")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("agents waiter stranded after ctx cancel (#51, line 138)")
	}
}
