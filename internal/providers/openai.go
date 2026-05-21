// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// openaiProvider holds the resolved per-CR state for one
// ModelDiscovery instance pointing at OpenAI or an OpenAI-compatible
// provider (Together, vLLM, Groq, OpenRouter — via cfg.BaseURL) OR
// KubeAI (via newKubeAI which constructs this with
// typeLabel="kubeai" and empty apiKey).
//
// Per the user-mandated reshape, typeLabel parameterizes the Type
// method so the same struct can serve both the "openai" and "kubeai"
// discriminator values without code duplication. Metrics labels still
// distinguish the two via typeLabel.
type openaiProvider struct {
	apiKey     string
	baseURL    string
	typeLabel  string // "openai" or "kubeai" — drives Type() and metric labels.
	httpClient *http.Client
}

// newOpenAIImpl is the real constructor for the "openai" type. It
// validates that apiKey AND httpClient are present, and sets
// typeLabel="openai". The KubeAI constructor will live
// in a separate file but will reuse *openaiProvider with
// typeLabel="kubeai" and may set apiKey to "".
func newOpenAIImpl(ctx context.Context, cfg ProviderConfig) (Provider, error) {
	_ = ctx
	if cfg.APIKey == "" {
		// OpenAI proper requires an API key. KubeAI's constructor
		// (04-03b) will skip this validation.
		return nil, errMissingAPIKey
	}
	if cfg.HTTPClient == nil {
		return nil, errNilHTTPClient
	}
	return &openaiProvider{
		apiKey:     cfg.APIKey,
		baseURL:    cfg.BaseURL,
		typeLabel:  "openai",
		httpClient: cfg.HTTPClient,
	}, nil
}

// Type returns the typeLabel value set at construction time. For
// instances built via newOpenAI this is always "openai";// newKubeAI will set typeLabel="kubeai" so Type returns "kubeai"
// from a *openaiProvider — the reconciler then labels metrics
// distinctly even though the wire-format code is shared.
func (p *openaiProvider) Type() string { return p.typeLabel }

// List issues GET <baseURL>/models. Authorization: Bearer is set ONLY
// when p.apiKey != "" — the conditional makes the same code path
// reusable from newKubeAI which permits empty apiKey
// (KubeAI runs in-cluster with no auth in the canonical sample).
//
// Error classification mirrors anthropic.go (MDISC-19):
// - 401/403 → *ProviderAuthError (Provider field carries typeLabel
// so metrics can distinguish openai-401 from kubeai-401).
// - other 4xx + 5xx → plain wrapped error.
// - decode err → plain wrapped error.
//
// 4MB body cap (PATTERNS.md L277); REL-04 drain+close deferred.
func (p *openaiProvider) List(ctx context.Context) ([]Candidate, error) {
	endpoint := baseURLFor(p.typeLabel, p.baseURL) + "/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("%s: build request: %w", p.typeLabel, err)
	}
	req.Header.Set("Accept", "application/json")
	// CONDITIONAL Authorization — load-bearing for the KubeAI reuse
	// path. OpenAI proper always passes this branch
	// because newOpenAIImpl requires non-empty apiKey.
	if p.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.apiKey)
	}

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: transport error: %w", p.typeLabel, err)
	}
	defer drainAndClose(resp.Body) // REL-04

	switch {
	case resp.StatusCode == http.StatusUnauthorized,
		resp.StatusCode == http.StatusForbidden:
		// Provider field carries typeLabel — so 04-04 metrics can
		// label this as type="openai" or type="kubeai" distinctly.
		return nil, &ProviderAuthError{
			Provider: p.typeLabel,
			Cause:    fmt.Errorf("status %d", resp.StatusCode),
		}
	case resp.StatusCode >= 400:
		return nil, fmt.Errorf("%s: list models: status %d", p.typeLabel, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("%s: read response: %w", p.typeLabel, err)
	}

	// OpenAI-compat wire shape. The "object":"model" field is present
	// in real OpenAI responses but ignored — we project only ID.
	// DisplayName is NOT in OpenAI's /v1/models shape (unlike
	// Anthropic's data[].display_name), so Candidate.DisplayName is
	// left empty.
	var decoded struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, fmt.Errorf("%s: decode response: %w", p.typeLabel, err)
	}

	candidates := make([]Candidate, 0, len(decoded.Data))
	for _, m := range decoded.Data {
		candidates = append(candidates, Candidate{
			ID: m.ID,
			// DisplayName left empty — OpenAI /v1/models doesn't
			// provide one. Discovery's spec.info propagation handles
			// any user-supplied display labels.
		})
	}
	return candidates, nil
}
