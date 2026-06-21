// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// providerTypeElevenLabs is the wire-level discriminator for the
// ElevenLabs provider — used in Type(), endpoint resolution, and
// *ProviderAuthError.Provider. Extracted as a const so goconst stays
// quiet across the in-file occurrences.
const providerTypeElevenLabs = "elevenlabs"

// elevenlabsProvider holds the resolved per-CR state for one
// ModelDiscovery instance pointing at ElevenLabs. Same lifecycle as
// anthropicProvider / geminiProvider — built fresh by the reconciler
// each refresh; the manager-owned *http.Client owns connection reuse
// (D-02).
//
// ElevenLabs is a hosted SaaS with a single public endpoint, so
// spec.baseUrl is CEL-forbidden for this type and baseURL is "" in
// production (List resolves via defaultBaseURLs["elevenlabs"]); tests
// inject via cfg.BaseURL or SetTestBaseURL.
type elevenlabsProvider struct {
	apiKey     string
	httpClient *http.Client
	baseURL    string
}

// newElevenLabsImpl is the real constructor. ElevenLabs requires an API
// key (CEL requires spec.credentialsSecretRef) and the shared HTTP
// client. Validation order matches the other HTTP providers.
func newElevenLabsImpl(ctx context.Context, cfg ProviderConfig) (Provider, error) {
	_ = ctx
	if cfg.APIKey == "" {
		return nil, errMissingAPIKey
	}
	if cfg.HTTPClient == nil {
		return nil, errNilHTTPClient
	}
	return &elevenlabsProvider{
		apiKey:     cfg.APIKey,
		httpClient: cfg.HTTPClient,
		baseURL:    cfg.BaseURL,
	}, nil
}

// Type returns the discriminator literal "elevenlabs". Used by the
// reconciler for metrics labels only.
func (p *elevenlabsProvider) Type() string { return providerTypeElevenLabs }

// List issues GET <baseURL>/models with the key in the xi-api-key header
// (NOT Authorization, NOT the URL query — keeps the key out of every URL
// string per the gemini H1 posture). ElevenLabs returns a BARE JSON
// array (no {"data":[...]} envelope, no pagination) of model objects;
// we project model_id → Candidate.ID and name → Candidate.DisplayName.
//
// Error classification mirrors the other HTTP providers (MDISC-19):
//   - 401/403 → *ProviderAuthError (reason=AuthFailed; permanent). Cause
//     is synthetic — the response body is NEVER included (§9.1; the
//     upstream may echo the key in its error detail).
//   - other 4xx + 5xx → plain wrapped error with status code (NO body).
//   - decode err → plain wrapped error (NOT *ProviderAuthError).
//
// 4MB body cap (PATTERNS.md L277); REL-04 drain+close deferred.
func (p *elevenlabsProvider) List(ctx context.Context) ([]Candidate, error) {
	endpoint := baseURLFor(providerTypeElevenLabs, p.baseURL) + "/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("elevenlabs: build request: %w", err)
	}
	req.Header.Set("xi-api-key", p.apiKey)
	req.Header.Set("Accept", "application/json")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		// %w wrap only — net/http err strings may carry the URL (no key in it).
		return nil, fmt.Errorf("elevenlabs: transport error: %w", err)
	}
	defer drainAndClose(resp.Body) // REL-04

	switch {
	case resp.StatusCode == http.StatusUnauthorized,
		resp.StatusCode == http.StatusForbidden:
		return nil, &ProviderAuthError{
			Provider: providerTypeElevenLabs,
			Cause:    fmt.Errorf("status %d", resp.StatusCode),
		}
	case resp.StatusCode >= 400:
		return nil, fmt.Errorf("elevenlabs: list models: status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("elevenlabs: read response: %w", err)
	}

	// ElevenLabs /v1/models returns a bare array, NOT an OpenAI-style
	// {"data":[...]} envelope. Each element carries model_id (routable ID)
	// and name (human label). Capability flags (can_do_text_to_speech,
	// etc.) are intentionally ignored — narrowing is the user's job via
	// spec.filters, consistent with every other provider.
	var decoded []struct {
		ModelID string `json:"model_id"`
		Name    string `json:"name"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, fmt.Errorf("elevenlabs: decode response: %w", err)
	}

	candidates := make([]Candidate, 0, len(decoded))
	for _, m := range decoded {
		candidates = append(candidates, Candidate{
			ID:          m.ModelID,
			DisplayName: m.Name,
		})
	}
	return candidates, nil
}
