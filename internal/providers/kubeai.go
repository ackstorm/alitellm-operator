// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"context"
	"fmt"
)

// newKubeAI replaces the 04-03a stub. It constructs the same
// *openaiProvider that handles OpenAI and OpenAI-compatible providers,
// stamped with typeLabel="kubeai" and with empty apiKey allowed.
//
// Validation order matches the other HTTP providers (anthropic, gemini,
// openai): BaseURL first (kubeai-specific gate), then HTTPClient
// (universal gate). APIKey is intentionally not validated — empty is
// valid for unauthenticated in-cluster deployments.
func newKubeAI(ctx context.Context, cfg ProviderConfig) (Provider, error) {
	_ = ctx
	if cfg.BaseURL == "" {
		// kubeai-specific message — distinct from the generic
		// errMissingBaseURL sentinel because the CEL contract on the
		// CRD already names "spec.baseUrl"; surfacing the same phrasing
		// in status.conditions[].message keeps the user-facing error
		// consistent with the validation rule that fired at admission.
		return nil, fmt.Errorf("kubeai: spec.baseUrl is required (no production default)")
	}
	if cfg.HTTPClient == nil {
		return nil, errNilHTTPClient
	}
	// baseURLFor("kubeai", p.baseURL) is the resolved-at-call-time
	// helper invoked from (*openaiProvider).List. Because
	// defaultBaseURLs has no "kubeai" entry, the only sources are
	// cfg.BaseURL (validated non-empty above) and the test override
	// map; production never reaches the empty-string fallback.
	return &openaiProvider{
		apiKey:     cfg.APIKey, // OPTIONAL — "" is acceptable.
		baseURL:    cfg.BaseURL,
		typeLabel:  "kubeai",
		httpClient: cfg.HTTPClient,
	}, nil
}
