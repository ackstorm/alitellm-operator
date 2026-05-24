// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"fmt"
	"time"
)

// EnvSafetyRelistInterval is the operator-pod env var that overrides the
// default safety-relist cadence (5 minutes) shared by the MCPServer,
// Model, Team, and A2AAgent reconcilers. Accepts any time.ParseDuration
// string ("10s", "1m", "30m", "1h"). Empty / unset → leave defaults.
//
// Floor: 5s. Values below the floor are rejected at parse time (returns
// error from ParseSafetyRelistInterval). Rationale: the safety-relist is
// the safety net, NOT the primary recovery channel; sub-5s cadence
// risks reconcile-storm regressions like the pre-v0.4.4 loop. The 5s
// minimum still permits the e2e Tier 2 AC-M3 conformance gate (10s
// configured) to compress safety-relist into the CI wall-clock budget;
// production installs should stay at the 5m default or raise to 30m.
const EnvSafetyRelistInterval = "LITELLM_OPERATOR_SAFETY_RELIST_INTERVAL"

// SafetyRelistFloor is the minimum permitted safety-relist cadence.
// Smaller intervals are storm-risk per the v0.4.4 root-cause analysis.
const SafetyRelistFloor = 5 * time.Second

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
func SetSafetyRelistIntervals(d time.Duration) {
	if d <= 0 {
		return
	}
	mcpSafetyRelistInterval = d
	modelSafetyRelistInterval = d
	teamSafetyRelistInterval = d
	a2aAgentSafetyRelistInterval = d
}
