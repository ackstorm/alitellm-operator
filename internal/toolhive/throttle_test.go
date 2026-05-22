// SPDX-License-Identifier: Apache-2.0

package toolhive

import (
	"sync"
	"testing"
	"time"
)

// TestDedupLogThrottle_FirstCallLogs asserts the throttle approves the
// initial emission for any key.
func TestDedupLogThrottle_FirstCallLogs(t *testing.T) {
	tr := newDedupLogThrottle()
	key := dedupKey{Kind: "MCPServer", Namespace: "mcp", Name: "context7"}
	if !tr.shouldLog(key, time.Minute) {
		t.Fatalf("first shouldLog(%v) returned false; expected true", key)
	}
}

// TestDedupLogThrottle_RepeatWithinWindowSuppressed asserts that ten
// consecutive calls inside the window produce exactly one approval
// (FIX.txt LOW-7: regression for the 22-server-per-reconcile prod spam).
func TestDedupLogThrottle_RepeatWithinWindowSuppressed(t *testing.T) {
	tr := newDedupLogThrottle()
	key := dedupKey{Kind: "MCPServer", Namespace: "mcp", Name: "context7"}
	approved := 0
	for i := 0; i < 10; i++ {
		if tr.shouldLog(key, time.Minute) {
			approved++
		}
	}
	if approved != 1 {
		t.Fatalf("approved log emissions inside window: got %d, want 1 (FIX.txt LOW-7)", approved)
	}
}

// TestDedupLogThrottle_DistinctKeysIndependent asserts that the throttle
// is per-key — a suppression on key A does not silence key B.
func TestDedupLogThrottle_DistinctKeysIndependent(t *testing.T) {
	tr := newDedupLogThrottle()
	a := dedupKey{Kind: "MCPServer", Namespace: "mcp", Name: "alpha"}
	b := dedupKey{Kind: "MCPServer", Namespace: "mcp", Name: "beta"}
	if !tr.shouldLog(a, time.Minute) {
		t.Fatalf("first log on a was suppressed")
	}
	if !tr.shouldLog(b, time.Minute) {
		t.Fatalf("first log on b was suppressed even though a is independent")
	}
	if tr.shouldLog(a, time.Minute) {
		t.Errorf("repeat a inside window unexpectedly approved")
	}
	if tr.shouldLog(b, time.Minute) {
		t.Errorf("repeat b inside window unexpectedly approved")
	}
}

// TestDedupLogThrottle_AfterWindowReopens asserts a sub-window emission
// re-approves once the window has elapsed.
func TestDedupLogThrottle_AfterWindowReopens(t *testing.T) {
	tr := newDedupLogThrottle()
	key := dedupKey{Kind: "MCPServer", Namespace: "mcp", Name: "context7"}

	if !tr.shouldLog(key, 50*time.Millisecond) {
		t.Fatalf("first emission suppressed")
	}
	if tr.shouldLog(key, 50*time.Millisecond) {
		t.Fatalf("immediate repeat unexpectedly approved")
	}
	time.Sleep(60 * time.Millisecond)
	if !tr.shouldLog(key, 50*time.Millisecond) {
		t.Fatalf("post-window emission was suppressed; throttle stuck")
	}
}

// TestDedupLogThrottle_ConcurrentSafe asserts the throttle is
// goroutine-safe (mu protects lastLoggedAt).
func TestDedupLogThrottle_ConcurrentSafe(t *testing.T) {
	tr := newDedupLogThrottle()
	key := dedupKey{Kind: "MCPServer", Namespace: "mcp", Name: "context7"}

	var wg sync.WaitGroup
	const goroutines = 50
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			tr.shouldLog(key, time.Minute)
		}()
	}
	wg.Wait()
	// No explicit assertion beyond "didn't deadlock or race-fail under -race".
}
