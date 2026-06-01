// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
// header (NOT the URL query) and follows nextPageToken across all pages.
//
// H1: a query-embedded key lands in request URLs that (*url.Error).Error()
// echoes verbatim into CR status and logs on a transport error — see
// internal/controller/modeldiscovery_controller.go writeBothConditions.
// The x-goog-api-key header (mirroring the anthropic x-api-key posture)
// keeps the key out of every URL string.
//
// H6: models.list defaults to ~50 items/page; reading one page silently
// truncated the discovered Model set while still reporting Ready=Synced.
// The loop follows nextPageToken until empty, with a hard page cap that
// errors rather than returning a truncated set.
//
// Error classification mirrors anthropic.List:
// - 401/403 → *ProviderAuthError (reason=AuthFailed; permanent).
// - other 4xx + 5xx → plain wrapped error with status code (NO body).
// - decode err → plain wrapped error.
//
// 4MB per-page body cap (PATTERNS.md L277); REL-04 drain+close per page.
func (p *geminiProvider) List(ctx context.Context) ([]Candidate, error) {
	const maxPages = 1000
	base := baseURLFor(providerTypeGemini, p.baseURL) + "/models"
	candidates := make([]Candidate, 0, 64)
	pageToken := ""
	for page := 0; page < maxPages; page++ {
		endpoint := base
		if pageToken != "" {
			endpoint = base + "?" + url.Values{"pageToken": {pageToken}}.Encode()
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, fmt.Errorf("gemini: build request: %w", err)
		}
		// H1: key travels in the x-goog-api-key header, never the URL query.
		req.Header.Set("x-goog-api-key", p.apiKey)
		req.Header.Set("Accept", "application/json")

		resp, err := p.httpClient.Do(req)
		if err != nil {
			// %w wrap only — net/http err strings may carry URL (no key now).
			return nil, fmt.Errorf("gemini: transport error: %w", err)
		}
		// Classify + read inside an inline func so defer drainAndClose runs
		// per page (REL-04) regardless of which branch returns.
		next, err := func() (string, error) {
			defer drainAndClose(resp.Body)
			switch {
			case resp.StatusCode == http.StatusUnauthorized,
				resp.StatusCode == http.StatusForbidden:
				// Cause is synthetic — body NEVER included (§9.1; upstream
				// may echo the key in error.message).
				return "", &ProviderAuthError{
					Provider: providerTypeGemini,
					Cause:    fmt.Errorf("status %d", resp.StatusCode),
				}
			case resp.StatusCode >= 400:
				return "", fmt.Errorf("gemini: list models: status %d", resp.StatusCode)
			}
			body, rerr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
			if rerr != nil {
				return "", fmt.Errorf("gemini: read response: %w", rerr)
			}
			var decoded struct {
				Models []struct {
					Name        string `json:"name"`
					DisplayName string `json:"displayName"`
				} `json:"models"`
				NextPageToken string `json:"nextPageToken"`
			}
			if jerr := json.Unmarshal(body, &decoded); jerr != nil {
				return "", fmt.Errorf("gemini: decode response: %w", jerr)
			}
			for _, m := range decoded.Models {
				// Strip "models/" prefix per CONTEXT.md Claude's Discretion
				// item 4. Candidate.ID is semantically "the routable ID" —
				// Discovery's spec.params.model overlay then constructs
				// "gemini/<id>" verbatim per MDISC-10.
				candidates = append(candidates, Candidate{
					ID:          strings.TrimPrefix(m.Name, "models/"),
					DisplayName: m.DisplayName,
				})
			}
			return decoded.NextPageToken, nil
		}()
		if err != nil {
			return nil, err
		}
		if next == "" {
			return candidates, nil
		}
		pageToken = next
	}
	// Cap reached while Gemini still returned a nextPageToken: refuse to
	// return a truncated set (would re-create the H6 silent-truncation bug).
	return nil, fmt.Errorf("gemini: model list exceeded %d pages; refusing truncated result", maxPages)
}
