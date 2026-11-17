// SPDX-License-Identifier: MIT

package providers

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestValidateBaseURL(t *testing.T) {
	bad := []string{
		"http://169.254.169.254/latest/meta-data/", // cloud metadata
		"http://[fd00:ec2::254]/latest/meta-data/", // IPv6 metadata
		"http://127.0.0.1:8080",                    // loopback
		"http://[::1]:8080",                        // IPv6 loopback
		"http://169.254.0.5",                       // link-local
		"ftp://example.com",                        // scheme
		"http://user:pass@example.com",             // userinfo
		"http://example.com/?x=1",                  // query
		"http://example.com/#frag",                 // fragment
		"not a url",                                // unparseable / no scheme
		"",                                         // empty
	}
	for _, u := range bad {
		if err := ValidateBaseURL(u); err == nil {
			t.Errorf("expected rejection: %q", u)
		}
	}

	// Accepted by design: public hosts, private RFC1918, and *.svc — the
	// denylist must NOT break the KubeAI in-cluster use case.
	good := []string{
		"https://api.openai.com/v1",
		"http://kubeai.kubeai.svc/openai/v1",
		"http://10.0.0.5:8000/v1", // private RFC1918 — allowed by design
	}
	for _, u := range good {
		if err := ValidateBaseURL(u); err != nil {
			t.Errorf("expected accept %q: %v", u, err)
		}
	}
}

func TestValidateBaseURL_DeniesLocalhostNames(t *testing.T) {
	for _, u := range []string{
		"http://localhost:8080",
		"http://LOCALHOST/v1",
		"http://localhost./v1",
		"http://metrics.localhost/v1",
	} {
		if err := ValidateBaseURL(u); err == nil {
			t.Errorf("ValidateBaseURL(%q) = nil, want a loopback denial", u)
		}
	}
}

// A redirect may not reach a host the baseUrl denylist rejects.
func TestCheckRedirect_DeniedHostNotFollowed(t *testing.T) {
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer target.Close()
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer src.Close()

	resp, err := (&http.Client{CheckRedirect: CheckRedirect}).Get(src.URL)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("redirect to a loopback host was followed")
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("redirect target received %d requests, want 0", n)
	}
}
