// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"strconv"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"
)

// EnvRecreateLimitPerMin caps how many CREATE (recreate) calls the Model
// reconciler issues against LiteLLM for a single CR within a 60s sliding
// window before it trips the circuit breaker and parks the CR in
// Ready=False / reason=RecreateThrottled.
//
// Guards the reconcile-storm class where LiteLLM accepts POST /model/new
// (HTTP 200, returns a fresh model_id) but the entry never becomes visible
// to the operator's existence probe (GET /model/info) — "created-but-not-
// listed". Each reconcile then clears the stored ModelID and re-POSTs at
// reconcile speed. Observed in prod with use_in_pass_through models on an
// incompatible LiteLLM build. (Router pseudo-models hit the same invisible-
// to-/model/info trait but are handled earlier by isRouterModel, which skips
// the existence probe entirely, so they never reach this breaker.)
const EnvRecreateLimitPerMin = "LITELLM_OPERATOR_RECREATE_LIMIT_PER_MIN"

// DefaultRecreateLimitPerMin is the per-CR recreate ceiling used when
// EnvRecreateLimitPerMin is unset, non-numeric, or <= 0.
const DefaultRecreateLimitPerMin = 10

// churnWindow is the sliding window over which recreates are counted.
const churnWindow = time.Minute

// ResolveRecreateLimitPerMin parses the env override. Empty / non-numeric /
// <= 0 → DefaultRecreateLimitPerMin. 0 does NOT disable the breaker: a
// disabled breaker reintroduces the storm, so the floor is the default.
func ResolveRecreateLimitPerMin(raw string) int {
	if raw == "" {
		return DefaultRecreateLimitPerMin
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return DefaultRecreateLimitPerMin
	}
	return n
}

// churnGuard is a per-CR sliding-window recreate counter. Thread-safe.
// Keyed by namespaced name; the clock is injectable for tests.
//
// nil-safe: every method tolerates a nil receiver and behaves as a disabled
// breaker (Count → 0, never trips). Tests that don't wire a guard get the
// no-throttle path for free; production wires newChurnGuard() in
// SetupWithManager.
type churnGuard struct {
	mu     sync.Mutex
	events map[types.NamespacedName][]time.Time
	now    func() time.Time
}

func newChurnGuard() *churnGuard {
	return &churnGuard{
		events: make(map[types.NamespacedName][]time.Time),
		now:    time.Now,
	}
}

// pruneLocked drops events for key at or before cutoff and returns the
// surviving count. Caller holds g.mu.
func (g *churnGuard) pruneLocked(key types.NamespacedName, cutoff time.Time) int {
	ev := g.events[key]
	keep := ev[:0]
	for _, t := range ev {
		if t.After(cutoff) {
			keep = append(keep, t)
		}
	}
	if len(keep) == 0 {
		delete(g.events, key)
		return 0
	}
	g.events[key] = keep
	return len(keep)
}

// Count returns the recreates recorded for key within the last churnWindow,
// pruning expired entries as a side effect.
func (g *churnGuard) Count(key types.NamespacedName) int {
	if g == nil {
		return 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.pruneLocked(key, g.now().Add(-churnWindow))
}

// Record appends a recreate event for key at the current time and returns
// the post-insert count within the window.
func (g *churnGuard) Record(key types.NamespacedName) int {
	if g == nil {
		return 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	g.pruneLocked(key, now.Add(-churnWindow))
	g.events[key] = append(g.events[key], now)
	return len(g.events[key])
}

// Forget drops all recorded events for key. Called when a CR reaches steady
// state (so a recovered model is not permanently throttled) and on delete.
func (g *churnGuard) Forget(key types.NamespacedName) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.events, key)
}
