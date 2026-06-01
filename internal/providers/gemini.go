// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// providerTypeGemini is the wire-level discriminator for the
// Gemini provider — used in Type(), endpoint resolution, and
// *ProviderAuthError.Provider.
const providerTypeGemini = "gemini"

// geminiProvider holds the resolved per-CR state for one
// ModelDiscovery instance pointing at Gemini. Same lifecycle as
// anthropicProvider — built fresh by the reconciler each refresh.
type geminiProvider struct {
	apiKey     string
	httpClient *http.Client
	baseURL    string // "" in production (spec.baseUrl CEL-forbidden for gemini); tests inject via SetTestBaseURL.
}

// newGeminiImpl is the real constructor. Validates required cfg fields
// up front. cfg.BaseURL is captured (always "" in production); the
// List resolves the effective URL via baseURLFor each call.
func newGeminiImpl(ctx context.Context, cfg ProviderConfig) (Provider, error) {
	_ = ctx
	if cfg.APIKey == "" {
		return nil, errMissingAPIKey
	}
	if cfg.HTTPClient == nil {
		return nil, errNilHTTPClient
	}
	return &geminiProvider{
		apiKey:     cfg.APIKey,
		httpClient: cfg.HTTPClient,
		baseURL:    cfg.BaseURL,
	}, nil
}

func (p *geminiProvider) Type() string { return providerTypeGemini }

// List issues GET <baseURL>/models with the key in the x-goog-api-key
// header (NOT the URL query). H1: a query-embedded key lands in request
// URLs that (*url.Error).Error() echoes verbatim into CR status and logs
// on a transport error — see internal/controller/modeldiscovery_controller.go
// writeBothConditions. Mirrors the anthropic x-api-key header posture, so
// no provider key ever enters a URL string.
//
// Error classification mirrors anthropic.List:
// - 401/403 → *ProviderAuthError (reason=AuthFailed; permanent).
// - other 4xx + 5xx → plain wrapped error with status code (NO body).
// - decode err → plain wrapped error.
//
// 4MB body cap (PATTERNS.md L277); REL-04 drain+close deferred.
func (p *geminiProvider) List(ctx context.Context) ([]Candidate, error) {
	endpoint := baseURLFor(providerTypeGemini, p.baseURL) + "/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("gemini: build request: %w", err)
	}
	// H1: key travels in the x-goog-api-key header, never the URL query.
	req.Header.Set("x-goog-api-key", p.apiKey)
	req.Header.Set("Accept", "application/json")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		// %w wrap only — net/http err strings may carry URL with key.
		return nil, fmt.Errorf("gemini: transport error: %w", err)
	}
	defer drainAndClose(resp.Body) // REL-04

	switch {
	case resp.StatusCode == http.StatusUnauthorized,
		resp.StatusCode == http.StatusForbidden:
		// Cause is a synthetic — body NEVER included (§9.1; upstream
		// may echo the key in error.message).
		return nil, &ProviderAuthError{
			Provider: providerTypeGemini,
			Cause:    fmt.Errorf("status %d", resp.StatusCode),
		}
	case resp.StatusCode >= 400:
		return nil, fmt.Errorf("gemini: list models: status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("gemini: read response: %w", err)
	}

	var decoded struct {
		Models []struct {
			Name        string `json:"name"`
			DisplayName string `json:"displayName"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, fmt.Errorf("gemini: decode response: %w", err)
	}

	candidates := make([]Candidate, 0, len(decoded.Models))
	for _, m := range decoded.Models {
		// Strip "models/" prefix per CONTEXT.md Claude's Discretion item 4.
		// Candidate.ID is semantically "the routable ID" — Discovery's
		// spec.params.model overlay will then construct
		// "gemini/<id>" verbatim per MDISC-10.
		candidates = append(candidates, Candidate{
			ID:          strings.TrimPrefix(m.Name, "models/"),
			DisplayName: m.DisplayName,
		})
	}
	return candidates, nil
}
