// SPDX-License-Identifier: Apache-2.0

package litellm

import "testing"

func TestSanitizeMCPServerName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		in        string
		separator string
		want      string
	}{
		// separator="." — ackstorm prod config; '.' forbidden, '-' allowed.
		{"dot-sep / three-part discovery", "test-toolhive-discovery.mcp.context7", ".", "test-toolhive-discovery-mcp-context7"},
		{"dot-sep / no dots in name", "context7", ".", "context7"},
		{"dot-sep / single dot", "a.b", ".", "a-b"},
		{"dot-sep / trailing dot", "abc.", ".", "abc-"},
		{"dot-sep / empty", "", ".", ""},

		// separator="-" — LiteLLM default; '-' forbidden, '.' allowed.
		{"dash-sep / hyphenated name", "test-toolhive-discovery", "-", "test.toolhive.discovery"},
		{"dash-sep / no dashes", "context7", "-", "context7"},
		{"dash-sep / dotted name passes through", "a.b.c", "-", "a.b.c"},

		// separator="" — defaults to "-" semantics per LiteLLM upstream.
		{"empty-sep defaults to dash", "a-b", "", "a.b"},

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
