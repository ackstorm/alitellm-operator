// SPDX-License-Identifier: Apache-2.0

package litellm

import (
	"strings"
	"testing"
)

func TestValidateEndpoint(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		in      string
		wantErr string // empty = expect nil
	}{
		// ── valid ──────────────────────────────────────────────────
		{"http host port", "http://litellm.default.svc.cluster.local:4000", ""},
		{"https host", "https://litellm.example.com", ""},
		{"https with path prefix", "https://gw.example.com/litellm", ""},
		{"https with path prefix trailing slash", "https://gw.example.com/litellm/", ""},
		{"ipv6 literal", "https://[::1]:4000", ""},
		{"punycode host", "https://xn--bcher-kva.example", ""},
		{"http no port", "http://litellm", ""},

		// ── invalid ────────────────────────────────────────────────
		{"empty", "", "empty endpoint"},
		{"whitespace only", "   ", "empty endpoint"},
		{"slash only", "/", "empty endpoint"},
		{"missing scheme", "litellm:4000", "scheme must be http or https"},
		{"non-http scheme", "ftp://litellm:4000", "scheme must be http or https"},
		{"wss scheme", "wss://litellm:4000", "scheme must be http or https"},
		{"opaque uri", "http:opaque", "opaque endpoint"},
		{"userinfo", "http://user:pass@litellm:4000", "userinfo not allowed"},
		{"userinfo no pass", "http://user@litellm:4000", "userinfo not allowed"},
		{"query string", "http://litellm:4000?debug=1", "query string not allowed"},
		{"fragment", "http://litellm:4000#frag", "fragment not allowed"},
		{"empty host", "http:///path", "host required"},
		{"port too high", "http://litellm:99999", "invalid port"},
		{"port zero", "http://litellm:0", "invalid port"},
		{"trailing newline", "http://litellm:4000\n", "whitespace or control character"},
		{"embedded tab", "http://lite\tllm:4000", "whitespace or control character"},
		{"unicode host", "https://bücher.example", "non-ASCII host"},
		{"control char", "http://litellm:4000\x01", "whitespace or control character"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateEndpoint(tc.in)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("want nil, got %v", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("want error containing %q, got nil", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}
