// SPDX-License-Identifier: Apache-2.0

package litellm

import "testing"

// TestSanitizeMCPServerName pins both the FIX.txt HIGH-1 rewrite behavior
// for inputs that contain the forbidden char AND the FIX2.txt HIGH-9
// no-op-on-safe-input contract that restores upgrade-stability for names
// like "test-exa-mcp".
func TestSanitizeMCPServerName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		in        string
		separator string
		want      string
	}{
		// ── separator="." (NEW default; "." is the forbidden char) ─────────
		// Dotted discovery child: rewrite "." → "-".
		{"dot-sep / three-part discovery", "test-toolhive-discovery.mcp.context7", ".", "test-toolhive-discovery-mcp-context7"},
		{"dot-sep / single dot", "a.b", ".", "a-b"},
		{"dot-sep / trailing dot", "abc.", ".", "abc-"},
		// HIGH-9 no-op-on-safe: hyphen-name has no ".", stays as-is.
		{"dot-sep / hyphen-name unchanged", "test-exa-mcp", ".", "test-exa-mcp"},
		{"dot-sep / no dots in name", "context7", ".", "context7"},
		{"dot-sep / empty", "", ".", ""},

		// ── separator="-" (legacy; "-" is the forbidden char) ──────────────
		// Hyphenated name under non-stock LiteLLM that rejects "-".
		{"dash-sep / hyphenated name", "test-toolhive-discovery", "-", "test.toolhive.discovery"},
		// HIGH-9 no-op-on-safe: dotted name has no "-", stays as-is.
		{"dash-sep / dotted name unchanged", "a.b.c", "-", "a.b.c"},
		{"dash-sep / no dashes", "context7", "-", "context7"},

		// ── separator="" (defaults to "." — the NEW default) ───────────────
		// Empty sep + dotted input → rewrite.
		{"empty-sep defaults to dot / rewrite", "a.b", "", "a-b"},
		// Empty sep + hyphen-name has no ".", stays unchanged (HIGH-9 path).
		{"empty-sep defaults to dot / hyphen-name unchanged", "test-exa-mcp", "", "test-exa-mcp"},

		// Defensive: unrecognized separator → passthrough (CEL enforces the
		// enum at admission, so this branch is never reached in prod).
		{"unrecognized sep / passthrough", "a.b-c", "_", "a.b-c"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := SanitizeMCPServerName(tc.in, tc.separator); got != tc.want {
				t.Fatalf("SanitizeMCPServerName(%q, %q) = %q, want %q",
					tc.in, tc.separator, got, tc.want)
			}
		})
	}
}
