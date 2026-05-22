// SPDX-License-Identifier: Apache-2.0

package litellm

import "testing"

func TestSanitizeMCPServerName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"no dots", "abc", "abc"},
		{"single dot", "a.b", "a-b"},
		{"three-part discovery", "test-toolhive-discovery.mcp.context7", "test-toolhive-discovery-mcp-context7"},
		{"trailing dot", "abc.", "abc-"},
		{"only dots", "...", "---"},
		{"empty", "", ""},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := SanitizeMCPServerName(tc.in); got != tc.want {
				t.Fatalf("SanitizeMCPServerName(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
