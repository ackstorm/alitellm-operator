// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"fmt"
	"time"
)

// EnvSafetyRelistInterval is the operator-pod env var that overrides the
// default safety-relist cadence (10 minutes — DefaultSafetyRelistInterval)
// shared by the MCPServer, Model, Team, and A2AAgent reconcilers AND the
// three relist Runnables (Model, Team, GuardRail). Accepts any
// time.ParseDuration string ("10s", "1m", "30m", "1h"). Empty / unset →
// DefaultSafetyRelistInterval.
//
// Floor: 5s. Values below the floor are rejected at parse time (returns
// error from ParseSafetyRelistInterval). Rationale: the safety-relist is
// the safety net, NOT the primary recovery channel; sub-5s cadence
// risks reconcile-storm regressions like the pre-v0.4.4 loop. The 5s
// minimum still permits the e2e Tier 2 AC-M3 conformance gate (10s
// configured) to compress safety-relist into the CI wall-clock budget;
// production installs should stay at the 10m default or raise to 30m.
const EnvSafetyRelistInterval = "LITELLM_OPERATOR_SAFETY_RELIST_INTERVAL"

// SafetyRelistFloor is the minimum permitted safety-relist cadence.
// Smaller intervals are storm-risk per the v0.4.4 root-cause analysis.
const SafetyRelistFloor = 5 * time.Second

// DefaultSafetyRelistInterval is the canonical safety-relist cadence used
// when LITELLM_OPERATOR_SAFETY_RELIST_INTERVAL is unset. Shared by the four
// reconciler RequeueAfter paths AND the three relist Runnables (Model,
// Team, GuardRail) so one env knob yields one cadence everywhere. Matches
// the *_controller.go package-var defaults.
const DefaultSafetyRelistInterval = 10 * time.Minute

// ResolvedSafetyRelistInterval parses the env override, applying the 5s
// floor, and falls back to DefaultSafetyRelistInterval when unset. Use the
// single returned value for BOTH SetSafetyRelistIntervals and every
// Runnable.Interval — there must be exactly one parse of this env var
// (H5: cmd/main.go previously parsed it a second time, with a different
// floor and a different invalid-input fallback, and two of the three
// runnables ignored it entirely and hardcoded 30m).
func ResolvedSafetyRelistInterval(raw string) (time.Duration, error) {
	d, err := ParseSafetyRelistInterval(raw)
	if err != nil {
		return 0, err
	}
	if d == 0 {
		return DefaultSafetyRelistInterval, nil
	}
	return d, nil
}

// ParseSafetyRelistInterval parses a duration string from the env var
// shape used by EnvSafetyRelistInterval. Returns (default, nil) on
// empty input (caller keeps the package defaults). Returns an error on
// malformed input OR sub-floor values.
func ParseSafetyRelistInterval(raw string) (time.Duration, error) {
	if raw == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid %s=%q: %w", EnvSafetyRelistInterval, raw, err)
	}
	if d < SafetyRelistFloor {
		return 0, fmt.Errorf("%s=%q below floor %s", EnvSafetyRelistInterval, raw, SafetyRelistFloor)
	}
	return d, nil
}

// SetSafetyRelistIntervals overrides the package-level safety-relist
// vars for MCPServer / Model / Team / A2AAgent reconcilers. Call ONCE
// at startup, BEFORE any reconciler begins running. Concurrent or
// post-start calls are not race-safe (the package vars are read without
// locks from hot paths to keep the Reconcile loop allocation-free).
//
// d <= 0 is a no-op (callers should pass the parsed env value;
// ParseSafetyRelistInterval returns 0 on unset to signal "keep defaults").
//
// NOTE: GuardRail is intentionally NOT covered here. The guardrail
// reconciler has no RequeueAfter safety-relist path (it relies on the
// BootSweeper relist + watch events), so there is no guardrail interval
// var to set. Do not add one without also adding the reconciler-side
// requeue.
func SetSafetyRelistIntervals(d time.Duration) {
	if d <= 0 {
		return
	}
	mcpSafetyRelistInterval = d
	modelSafetyRelistInterval = d
	teamSafetyRelistInterval = d
	a2aAgentSafetyRelistInterval = d
}
