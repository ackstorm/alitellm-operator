// SPDX-License-Identifier: Apache-2.0

package providers

import "testing"

func TestValidateBaseURL(t *testing.T) {
	bad := []string{
		"http://169.254.169.254/latest/meta-data/", // cloud metadata
		"http://[fd00:ec2::254]/latest/meta-data/",  // IPv6 metadata
		"http://127.0.0.1:8080",                      // loopback
		"http://[::1]:8080",                          // IPv6 loopback
		"http://169.254.0.5",                         // link-local
		"ftp://example.com",                          // scheme
		"http://user:pass@example.com",               // userinfo
		"http://example.com/?x=1",                    // query
		"http://example.com/#frag",                   // fragment
		"not a url",                                  // unparseable / no scheme
		"",                                           // empty
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
