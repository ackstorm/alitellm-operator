// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// anthropicProvider holds the resolved per-CR state for one
// ModelDiscovery instance pointing at Anthropic. The struct is built
// fresh by the reconciler each refresh cycle — there is no per-call
// caching here (the manager-owned *http.Client owns connection reuse
// per D-02).
// providerTypeAnthropic is the wire-level discriminator for the
// Anthropic provider — used in Type(), endpoint resolution, and
// *ProviderAuthError.Provider. Extracted as a const so goconst stays
// quiet across the three in-file occurrences.
const providerTypeAnthropic = "anthropic"

type anthropicProvider struct {
	apiKey     string
	httpClient *http.Client
	// baseURL is cfg.BaseURL captured at constructor time. In
	// production this is always "" for anthropic (spec.baseUrl is
	// CEL-forbidden for this type), so List resolves via the
	// test override map → defaultBaseURLs["anthropic"]. Tests set
	// the override via SetTestBaseURL(t, "anthropic", srv.URL).
	baseURL string
}

// errMissingAPIKey / errNilHTTPClient are constructor-side sentinel
// errors. The reconciler in 04-04 wraps these with the CR
// namespace/name context before surfacing to status.conditions[].message.
var (
	errMissingAPIKey = errors.New("providers: APIKey is required")
	errNilHTTPClient = errors.New("providers: HTTPClient is required")
)

// newAnthropicImpl is the real constructor — it replaces the Task 1
// stub in registry.go. Validates required cfg fields up front so
// failures surface synchronously instead of at List time.
func newAnthropicImpl(ctx context.Context, cfg ProviderConfig) (Provider, error) {
	_ = ctx
	if cfg.APIKey == "" {
		return nil, errMissingAPIKey
	}
	if cfg.HTTPClient == nil {
		return nil, errNilHTTPClient
	}
	return &anthropicProvider{
		apiKey:     cfg.APIKey,
		httpClient: cfg.HTTPClient,
		baseURL:    cfg.BaseURL,
	}, nil
}

// Type returns the discriminator literal "anthropic". Used by the
// reconciler for metrics labels only — branching on Type outside
// registry.go is the D-01 anti-pattern.
func (p *anthropicProvider) Type() string { return providerTypeAnthropic }

// List performs GET <baseURL>/models with the three required Anthropic
// headers and parses the {"data":[{"id","display_name"}]} response.
//
// Implementation contract:
// - body cap: 4MB (PATTERNS.md L277 — scaled up from LiteLLM's 1MB to
// handle AWS Bedrock control-plane responses; the same cap is used
// uniformly across all HTTP providers).
// - drain+close: deferred immediately after http.Client.Do per REL-04.
// - 401/403 → *ProviderAuthError; the response body is NOT included
// in the wrapped error (§9.1 — the upstream may echo the key).
// - other 4xx + 5xx → plain wrapped error with the status code; body
// is NOT included.
// - decode err → plain wrapped error (NOT *ProviderAuthError).
func (p *anthropicProvider) List(ctx context.Context) ([]Candidate, error) {
	endpoint := baseURLFor(providerTypeAnthropic, p.baseURL) + "/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("anthropic: build request: %w", err)
	}
	req.Header.Set("x-api-key", p.apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("Accept", "application/json")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		// Note: we deliberately wrap with %w but do NOT include err
		// in a separate %v slot — net/http error strings can carry
		// the URL with embedded credential fragments in rare DNS
		// failure paths. The %w wrapper keeps errors.As / errors.Is
		// working without leaking text via Error.
		return nil, fmt.Errorf("anthropic: transport error: %w", err)
	}
	defer drainAndClose(resp.Body) // REL-04: every code path.

	switch {
	case resp.StatusCode == http.StatusUnauthorized,
		resp.StatusCode == http.StatusForbidden:
		// MDISC-19 / spec §6.3 lines 830-835: 401/403 → AuthFailed
		// classification. Cause is a synthetic sentinel — we do NOT
		// include resp body (the upstream may echo the key).
		return nil, &ProviderAuthError{
			Provider: providerTypeAnthropic,
			Cause:    fmt.Errorf("status %d", resp.StatusCode),
		}
	case resp.StatusCode >= 400:
		// Non-auth 4xx + 5xx → transient/Unreachable. Status code
		// included for diagnostics; body is NOT (it may carry
		// credential echoes per §9.1).
		return nil, fmt.Errorf("anthropic: list models: status %d", resp.StatusCode)
	}

	// 2xx — read with 4MB cap. (4 << 20 = 4 * 1024 * 1024 bytes.)
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("anthropic: read response: %w", err)
	}

	var decoded struct {
		Data []struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		// NOT *ProviderAuthError — malformed JSON is unreachable-ish.
		// %w is safe here (json error messages don't carry credentials).
		return nil, fmt.Errorf("anthropic: decode response: %w", err)
	}

	candidates := make([]Candidate, 0, len(decoded.Data))
	for _, m := range decoded.Data {
		candidates = append(candidates, Candidate{
			ID:          m.ID,
			DisplayName: m.DisplayName,
		})
	}
	return candidates, nil
}
