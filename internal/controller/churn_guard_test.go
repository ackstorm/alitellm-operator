// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"
)

func TestResolveRecreateLimitPerMin(t *testing.T) {
	cases := []struct {
		raw  string
		want int
	}{
		{"", DefaultRecreateLimitPerMin},
		{"5", 5},
		{"25", 25},
		{"1", 1},
		{"abc", DefaultRecreateLimitPerMin}, // non-numeric → default
		{"0", DefaultRecreateLimitPerMin},   // 0 does not disable the breaker
		{"-3", DefaultRecreateLimitPerMin},  // negative → default
		{"  ", DefaultRecreateLimitPerMin},  // whitespace is non-numeric → default
	}
	for _, c := range cases {
		if got := ResolveRecreateLimitPerMin(c.raw); got != c.want {
			t.Errorf("ResolveRecreateLimitPerMin(%q) = %d, want %d", c.raw, got, c.want)
		}
	}
}

// fakeClock returns a settable time so the sliding window is deterministic.
type fakeClock struct{ t time.Time }

func (f *fakeClock) now() time.Time          { return f.t }
func (f *fakeClock) advance(d time.Duration) { f.t = f.t.Add(d) }

func newTestGuard(clk *fakeClock) *churnGuard {
	g := newChurnGuard()
	g.now = clk.now
	return g
}

func TestChurnGuard_RecordAndCountWithinWindow(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	g := newTestGuard(clk)
	key := types.NamespacedName{Namespace: "ns", Name: "m"}

	if got := g.Count(key); got != 0 {
		t.Fatalf("empty Count = %d, want 0", got)
	}
	for i := 1; i <= 3; i++ {
		if got := g.Record(key); got != i {
			t.Fatalf("Record #%d returned %d, want %d", i, got, i)
		}
	}
	if got := g.Count(key); got != 3 {
		t.Fatalf("Count after 3 records = %d, want 3", got)
	}
}

func TestChurnGuard_WindowPruning(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	g := newTestGuard(clk)
	key := types.NamespacedName{Namespace: "ns", Name: "m"}

	g.Record(key) // t0
	clk.advance(30 * time.Second)
	g.Record(key) // t0+30s
	if got := g.Count(key); got != 2 {
		t.Fatalf("Count within window = %d, want 2", got)
	}

	// Advance past the window relative to the first event only.
	clk.advance(31 * time.Second) // now t0+61s: first event (t0) expired, second (t0+30s) lives
	if got := g.Count(key); got != 1 {
		t.Fatalf("Count after partial expiry = %d, want 1", got)
	}

	// Advance past the window for all events.
	clk.advance(60 * time.Second)
	if got := g.Count(key); got != 0 {
		t.Fatalf("Count after full expiry = %d, want 0", got)
	}
}

func TestChurnGuard_PerKeyIsolation(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	g := newTestGuard(clk)
	a := types.NamespacedName{Namespace: "ns", Name: "a"}
	b := types.NamespacedName{Namespace: "ns", Name: "b"}

	g.Record(a)
	g.Record(a)
	g.Record(b)
	if got := g.Count(a); got != 2 {
		t.Errorf("Count(a) = %d, want 2", got)
	}
	if got := g.Count(b); got != 1 {
		t.Errorf("Count(b) = %d, want 1", got)
	}
}

func TestChurnGuard_Forget(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	g := newTestGuard(clk)
	key := types.NamespacedName{Namespace: "ns", Name: "m"}

	g.Record(key)
	g.Record(key)
	g.Forget(key)
	if got := g.Count(key); got != 0 {
		t.Fatalf("Count after Forget = %d, want 0", got)
	}
}

func TestChurnGuard_NilSafe(t *testing.T) {
	var g *churnGuard // nil — disabled breaker
	key := types.NamespacedName{Namespace: "ns", Name: "m"}
	if got := g.Count(key); got != 0 {
		t.Errorf("nil Count = %d, want 0", got)
	}
	if got := g.Record(key); got != 0 {
		t.Errorf("nil Record = %d, want 0", got)
	}
	g.Forget(key) // must not panic
}

func TestIsRouterModel(t *testing.T) {
	cases := []struct {
		name   string
		params map[string]any
		want   bool
	}{
		{"complexity router", map[string]any{"model": "auto_router/complexity_router"}, true},
		{"semantic auto router", map[string]any{"model": "auto_router/auto_router_1"}, true},
		{"plain anthropic", map[string]any{"model": "claude-opus-4-7"}, false},
		{"gemini prefixed", map[string]any{"model": "gemini/gemini-flash-latest"}, false},
		{"missing model", map[string]any{"rpm": 100}, false},
		{"non-string model", map[string]any{"model": 42}, false},
		{"empty", map[string]any{}, false},
		{"substring not prefix", map[string]any{"model": "x-auto_router/y"}, false},
	}
	for _, c := range cases {
		if got := isRouterModel(c.params); got != c.want {
			t.Errorf("%s: isRouterModel = %v, want %v", c.name, got, c.want)
		}
	}
}
