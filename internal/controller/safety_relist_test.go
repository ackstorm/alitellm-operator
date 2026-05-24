// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"testing"
	"time"
)

// TestParseSafetyRelistInterval_EmptyReturnsZero — empty input keeps
// the package defaults (zero signals "no override").
func TestParseSafetyRelistInterval_EmptyReturnsZero(t *testing.T) {
	got, err := ParseSafetyRelistInterval("")
	if err != nil {
		t.Fatalf("empty: unexpected err: %v", err)
	}
	if got != 0 {
		t.Errorf("empty: want 0, got %v", got)
	}
}

// TestParseSafetyRelistInterval_Valid — duration strings within bounds
// parse and round-trip.
func TestParseSafetyRelistInterval_Valid(t *testing.T) {
	for _, raw := range []string{"5s", "10s", "30s", "1m", "10m", "1h", "24h"} {
		got, err := ParseSafetyRelistInterval(raw)
		if err != nil {
			t.Errorf("%q: unexpected err: %v", raw, err)
			continue
		}
		want, _ := time.ParseDuration(raw)
		if got != want {
			t.Errorf("%q: got %v, want %v", raw, got, want)
		}
	}
}

// TestParseSafetyRelistInterval_BelowFloor — sub-5s rejected.
func TestParseSafetyRelistInterval_BelowFloor(t *testing.T) {
	for _, raw := range []string{"1s", "4s", "1ms"} {
		if _, err := ParseSafetyRelistInterval(raw); err == nil {
			t.Errorf("%q: expected floor rejection, got nil err", raw)
		}
	}
}

// TestParseSafetyRelistInterval_Malformed — non-duration strings error.
func TestParseSafetyRelistInterval_Malformed(t *testing.T) {
	for _, raw := range []string{"abc", "10", "30 seconds", "-5m"} {
		if _, err := ParseSafetyRelistInterval(raw); err == nil {
			t.Errorf("%q: expected parse error, got nil err", raw)
		}
	}
}

// TestSetSafetyRelistIntervals_SetsAllFour — single setter updates the
// four package-level vars. Restore defaults after to avoid bleed-through
// to other tests in the package.
func TestSetSafetyRelistIntervals_SetsAllFour(t *testing.T) {
	origMCP := mcpSafetyRelistInterval
	origModel := modelSafetyRelistInterval
	origTeam := teamSafetyRelistInterval
	origA2A := a2aAgentSafetyRelistInterval
	t.Cleanup(func() {
		mcpSafetyRelistInterval = origMCP
		modelSafetyRelistInterval = origModel
		teamSafetyRelistInterval = origTeam
		a2aAgentSafetyRelistInterval = origA2A
	})

	SetSafetyRelistIntervals(7 * time.Minute)

	for name, got := range map[string]time.Duration{
		"mcp":   mcpSafetyRelistInterval,
		"model": modelSafetyRelistInterval,
		"team":  teamSafetyRelistInterval,
		"a2a":   a2aAgentSafetyRelistInterval,
	} {
		if got != 7*time.Minute {
			t.Errorf("%s: want 7m, got %v", name, got)
		}
	}
}

// TestSetSafetyRelistIntervals_ZeroIsNoop — zero / negative keep
// existing values (signals "no override" path from ParseSafetyRelistInterval).
func TestSetSafetyRelistIntervals_ZeroIsNoop(t *testing.T) {
	origMCP := mcpSafetyRelistInterval
	t.Cleanup(func() { mcpSafetyRelistInterval = origMCP })

	mcpSafetyRelistInterval = 13 * time.Minute
	SetSafetyRelistIntervals(0)
	if mcpSafetyRelistInterval != 13*time.Minute {
		t.Errorf("zero-noop: want 13m preserved, got %v", mcpSafetyRelistInterval)
	}
	SetSafetyRelistIntervals(-1 * time.Second)
	if mcpSafetyRelistInterval != 13*time.Minute {
		t.Errorf("negative-noop: want 13m preserved, got %v", mcpSafetyRelistInterval)
	}
}
