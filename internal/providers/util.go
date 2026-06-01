// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"io"
	"strings"
	"sync"
)

// drainAndClose discards remaining bytes from the HTTP response body
// and closes it. This is the REL-04 contract mirrored from
// internal/litellm/client.go:117 — every code path that holds an
// *http.Response MUST defer this immediately after http.Client.Do
// returns success. Draining before closing enables HTTP keepalive reuse
// and prevents FD/goroutine leaks.
//
// Both Copy and Close errors are intentionally ignored: drain is
// best-effort (a slow upstream should never block the reconciler), and
// a double-close on Close is a no-op on net/http's response body.
func drainAndClose(body io.ReadCloser) {
	if body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, body)
	_ = body.Close()
}

// defaultBaseURLs holds the hardcoded production base URLs per
// provider type. Used by baseURLFor when neither cfg.BaseURL nor a
// test-only override is set.
//
// - anthropic: spec.baseUrl is CEL-forbidden — production always uses
// this default; tests inject via SetTestBaseURL.
// - gemini: same as anthropic.
// - openai: default; users may override via spec.baseUrl for
// OpenAI-compatible providers (Together, vLLM, Groq, OpenRouter).
//
// kubeai has no production default — spec.baseUrl is CEL-required.
// bedrock uses aws-sdk-go-v2's own endpoint resolution; it does not
// flow through this map.
var defaultBaseURLs = map[string]string{
	"anthropic": "https://api.anthropic.com/v1",
	"gemini":    "https://generativelanguage.googleapis.com/v1beta",
	"openai":    "https://api.openai.com/v1",
}

// testBaseURLOverrides is populated ONLY through SetTestBaseURL in
// _test.go compilation units. The Go toolchain excludes _test.go files
// from production binaries, so production code can never write to this
// map (only read it via baseURLFor). The package-private RWMutex
// allows concurrent reads from parallel reconciles.
var (
	testBaseURLOverridesMu sync.RWMutex
	testBaseURLOverrides   = map[string]string{}
)

// baseURLFor returns the effective base URL for a provider type using
// the three-tier priority:
//
// 1. cfgBaseURL — user-supplied via spec.baseUrl (highest priority;
// non-empty means override the default).
// 2. test override — set via SetTestBaseURL in _test.go; the envtest
// in uses this to point anthropic/gemini at
// httptest.NewServer despite those providers having CEL-forbidden
// spec.baseUrl.
// 3. defaultBaseURLs[providerType] — hardcoded production URL.
//
// Returns empty string if no value at any tier (kubeai with empty
// cfg.BaseURL — the caller's constructor MUST validate baseURL before
// this is called; an empty string would cause a malformed request URL).
func baseURLFor(providerType, cfgBaseURL string) string {
	var raw string
	switch {
	case cfgBaseURL != "":
		raw = cfgBaseURL
	default:
		testBaseURLOverridesMu.RLock()
		v, ok := testBaseURLOverrides[providerType]
		testBaseURLOverridesMu.RUnlock()
		if ok {
			raw = v
		} else {
			raw = defaultBaseURLs[providerType]
		}
	}
	// Strip trailing slashes so callers' "<base>/models" never yields
	// "<base>//models" when a user-supplied spec.baseUrl ends in "/".
	return strings.TrimRight(raw, "/")
}
