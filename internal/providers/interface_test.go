// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"errors"
	"strings"
	"testing"
)

// TestRegistry_HasSixProviders asserts the Registry map exposes
// constructors for ALL SIX spec.type values (anthropic, bedrock,
// elevenlabs, gemini, kubeai, openai). D-01 forbids any switch on
// spec.type outside this map — so the integrity of the map keys IS the
// per-type dispatch contract.
func TestRegistry_HasSixProviders(t *testing.T) {
	if got := len(Registry); got != 6 {
		t.Fatalf("Registry: expected 6 entries; got %d", got)
	}
	for _, k := range []string{"anthropic", "bedrock", "elevenlabs", "gemini", "kubeai", "openai"} {
		if _, ok := Registry[k]; !ok {
			t.Errorf("Registry: missing key %q", k)
		}
	}
}

// TestProviderAuthError_ErrorString asserts the templated format
// "providers: <provider> auth failed: <cause>". The reconciler in 04-04
// surfaces the .Error string into status.conditions[].message; the
// format MUST NOT include credential material (the cause is sanitized by
// the provider before wrapping, per §9.1).
func TestProviderAuthError_ErrorString(t *testing.T) {
	cause := errors.New("401 unauthorized")
	err := &ProviderAuthError{Provider: "anthropic", Cause: cause}
	want := "providers: anthropic auth failed: 401 unauthorized"
	if got := err.Error(); got != want {
		t.Fatalf("Error() = %q; want %q", got, want)
	}
	var target *ProviderAuthError
	if !errors.As(err, &target) {
		t.Fatalf("errors.As: target should resolve to *ProviderAuthError")
	}
	if target.Provider != "anthropic" {
		t.Fatalf("target.Provider = %q; want %q", target.Provider, "anthropic")
	}
}

// TestProviderAuthError_Unwrap asserts errors.Unwrap returns the wrapped
// Cause verbatim, so callers can use errors.Is against sentinel errors.
func TestProviderAuthError_Unwrap(t *testing.T) {
	cause := errors.New("401 unauthorized")
	err := &ProviderAuthError{Provider: "anthropic", Cause: cause}
	if got := errors.Unwrap(err); got != cause {
		t.Fatalf("Unwrap() = %v; want %v", got, cause)
	}
}

// TestBaseURLFor_Priority exercises the three-tier priority:
// 1. cfg.BaseURL (highest) → user-supplied via spec.baseUrl
// 2. test override → set via SetTestBaseURL
// 3. defaultBaseURLs[providerType] → hardcoded production URL
//
// Used by all five providers to resolve their per-call endpoint.
func TestBaseURLFor_Priority(t *testing.T) {
	t.Run("cfgBaseURL non-empty wins", func(t *testing.T) {
		SetTestBaseURL(t, "openai", "http://test-override")
		if got := baseURLFor("openai", "http://cfg-base"); got != "http://cfg-base" {
			t.Fatalf("baseURLFor returned %q; want cfg-base", got)
		}
	})
	t.Run("test override beats default", func(t *testing.T) {
		SetTestBaseURL(t, "openai", "http://test-override")
		if got := baseURLFor("openai", ""); got != "http://test-override" {
			t.Fatalf("baseURLFor returned %q; want test-override", got)
		}
	})
	t.Run("falls through to default", func(t *testing.T) {
		// No SetTestBaseURL on this subtest — only the parent's override
		// (which was cleaned up after that parent subtest returned).
		if got := baseURLFor("openai", ""); got != "https://api.openai.com/v1" {
			t.Fatalf("baseURLFor returned %q; want production default", got)
		}
	})
}

// TestSetTestBaseURL_RestoresOnCleanup verifies the seam's cleanup
// behavior — after a subtest finishes, the override is removed and
// baseURLFor returns the hardcoded default again. Without this, parallel
// tests would observe each other's seam state.
func TestSetTestBaseURL_RestoresOnCleanup(t *testing.T) {
	// Sanity: starting state has no override for "anthropic".
	if got := baseURLFor("anthropic", ""); got != "https://api.anthropic.com/v1" {
		t.Fatalf("pre-test baseline: got %q; want production default", got)
	}
	t.Run("inner subtest sets override", func(t *testing.T) {
		SetTestBaseURL(t, "anthropic", "http://fake-anthropic")
		if got := baseURLFor("anthropic", ""); got != "http://fake-anthropic" {
			t.Fatalf("inside subtest: got %q; want override", got)
		}
	})
	// After the subtest returned, the override MUST be restored.
	if got := baseURLFor("anthropic", ""); got != "https://api.anthropic.com/v1" {
		t.Fatalf("after subtest cleanup: got %q; want production default", got)
	}
}

// TestSetTestBaseURL_PanicsOnNilT verifies the nil-t guard. The seam is
// exported (capital S) for cross-package use; the panic prevents the
// helper from being accidentally invoked outside a test context (where
// t.Cleanup would silently no-op).
func TestSetTestBaseURL_PanicsOnNilT(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("SetTestBaseURL(nil, ...) did not panic")
		}
		msg, ok := r.(string)
		if !ok || !strings.Contains(msg, "nil") {
			t.Fatalf("panic value %v: expected message containing 'nil'", r)
		}
	}()
	SetTestBaseURL(nil, "anthropic", "http://nope")
}
