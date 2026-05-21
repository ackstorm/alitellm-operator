// SPDX-License-Identifier: Apache-2.0

package providers

import "context"

// TestingCleanup is the minimal subset of *testing.T that
// `SetTestBaseURL` needs. *testing.T satisfies this interface via its
// own `Helper` + `Cleanup(func)` methods. Defining the interface in
// a non-`_test.go` file lets cross-package envtest scenarios (e.g.
// `internal/controller/modeldiscovery_controller_test.go`) call this
// seam — the original `_test.go` placement of `SetTestBaseURL` could
// only be linked into the providers package's own test binary (#04-04
// plan-time seam mistake; corrected here as a Rule 3 auto-fix).
//
// Production code does NOT instantiate this interface — the only
// non-`*testing.T` implementer would be a hand-rolled struct in a
// build-tag-gated file, which we don't ship. The interface exists in
// production binaries (~200 bytes of type metadata) but is unreachable.
type TestingCleanup interface {
	Helper()
	Cleanup(func())
}

// RegisterTestProvider overrides Registry[providerType] with a
// constructor that returns the supplied Provider verbatim. The override
// is restored on test cleanup via t.Cleanup. Test-only by convention —
// production code never holds a TestingCleanup implementer, so this
// function is dead code in production binaries.
//
// Used by cross-package envtest scenarios (internal/controller's
// modeldiscovery_controller_test.go) that need deterministic provider
// responses WITHOUT spinning up an httptest.Server. The Bedrock NORM1
// scenario in uses this seam because Bedrock's aws-sdk-go-v2
// transport bypasses net/http (SetTestBaseURL is ineffective).
//
// The constructor closure CAPTURES the supplied Provider — callers MUST
// own the Provider's lifetime (typically a struct literal allocated in
// the test function's stack frame). The constructor IGNORES the passed
// ctx + ProviderConfig and returns (p, nil) verbatim.
func RegisterTestProvider(t TestingCleanup, providerType string, p Provider) {
	if t == nil {
		panic("providers.RegisterTestProvider: nil TestingCleanup")
	}
	t.Helper()
	registryMu.Lock()
	prev, existed := Registry[providerType]
	Registry[providerType] = func(_ context.Context, _ ProviderConfig) (Provider, error) { //nolint:unparam // err is always nil here but signature must satisfy the Constructor func type
		return p, nil
	}
	registryMu.Unlock()
	t.Cleanup(func() {
		registryMu.Lock()
		defer registryMu.Unlock()
		if existed {
			Registry[providerType] = prev
		} else {
			delete(Registry, providerType)
		}
	})
}

// SetTestBaseURL overrides the default upstream URL for a provider type
// (anthropic | gemini | openai). The override is restored on test
// cleanup via the t.Cleanup hook. Test-only by convention — production
// code never holds a TestingCleanup implementer, so this function is
// dead code in production binaries.
//
// Cross-package callers (controller envtests) pass `*testing.T`
// directly; the interface conversion is implicit via Go's structural
// typing rules.
//
// The seam covers the case where a provider has CEL-forbidden
// `spec.baseUrl` (anthropic, gemini) — the reconciler builds
// `ProviderConfig.BaseURL = ""` for those types, so the test override
// is the only path to point them at `httptest.NewServer`. For openai
// (and the kubeai variant of *openaiProvider), tests typically use the
// `cfg.BaseURL` path directly; this seam is a fallback.
//
// Priority order (lowest precedence first):
//
//	defaultBaseURLs[type] → test override (here) → cfg.BaseURL
//
// See `baseURLFor` in util.go for the resolution logic.
func SetTestBaseURL(t TestingCleanup, providerType, url string) {
	if t == nil {
		panic("providers.SetTestBaseURL: nil TestingCleanup")
	}
	t.Helper()
	testBaseURLOverridesMu.Lock()
	prev, existed := testBaseURLOverrides[providerType]
	testBaseURLOverrides[providerType] = url
	testBaseURLOverridesMu.Unlock()

	t.Cleanup(func() {
		testBaseURLOverridesMu.Lock()
		if existed {
			testBaseURLOverrides[providerType] = prev
		} else {
			delete(testBaseURLOverrides, providerType)
		}
		testBaseURLOverridesMu.Unlock()
	})
}
