// SPDX-License-Identifier: Apache-2.0

package litellm

import (
	"strings"
	"testing"
)

// TestClassifyEndpointTransport — M-SEC2: only plaintext http to a remote
// host is flagged insecure. https, loopback http, and in-cluster (*.svc /
// bare service name) http are all secure-enough.
func TestClassifyEndpointTransport(t *testing.T) {
	t.Parallel()
	secure := []string{
		"https://api.example.com",                       // https
		"https://litellm.litellm-system.svc",            // https svc
		"http://litellm.litellm-system.svc",             // in-cluster svc
		"http://litellm.litellm-system.svc.cluster.local", // fqdn svc
		"http://litellm",                                 // bare service name
		"http://localhost:4000",                          // loopback name
		"http://127.0.0.1:4000",                          // loopback IP
		"http://[::1]:4000",                              // IPv6 loopback
	}
	for _, e := range secure {
		insecure, err := ClassifyEndpointTransport(e)
		if err != nil {
			t.Errorf("ClassifyEndpointTransport(%q) err: %v", e, err)
		}
		if insecure {
			t.Errorf("ClassifyEndpointTransport(%q): want secure, got insecureRemote=true", e)
		}
	}
	insecureRemotes := []string{
		"http://api.example.com",
		"http://api.example.com/litellm/v1",
		"http://203.0.113.10:4000",
	}
	for _, e := range insecureRemotes {
		insecure, err := ClassifyEndpointTransport(e)
		if err != nil {
			t.Errorf("ClassifyEndpointTransport(%q) err: %v", e, err)
		}
		if !insecure {
			t.Errorf("ClassifyEndpointTransport(%q): want insecureRemote=true", e)
		}
	}
	// Invalid endpoints propagate the ValidateEndpoint error.
	if _, err := ClassifyEndpointTransport("ftp://x"); err == nil {
		t.Error("expected error for invalid endpoint")
	}
}

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
