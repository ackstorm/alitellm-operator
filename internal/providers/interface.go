// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"context"
	"fmt"
	"net/http"

	awsv2 "github.com/aws/aws-sdk-go-v2/aws"
)

// Candidate is the provider-returned model identity. ID is the raw,
// unnormalized provider string — Discovery uses ID to build
// spec.params.model = "<litellm-provider>/<ID>" verbatim per MDISC-10.
// DisplayName is informational (provider-side human label, e.g.
// "Claude 3.5 Sonnet"). Provider-specific extras (Gemini token limits,
// Bedrock modalities) flow through spec.info propagation at the
// Discovery CR level, NOT this struct.
type Candidate struct {
	ID          string
	DisplayName string
}

// Provider is the uniform contract for one upstream model source.
// Implementations live in this package (one file each); the reconciler
// constructs them via the registry keyed on spec.type and never
// type-switches on the concrete provider.
type Provider interface {
	// Type returns the spec.type enum literal:
	// "anthropic"|"bedrock"|"elevenlabs"|"gemini"|"kubeai"|"openai".
	// The reconciler uses this only for metrics labels — branching on
	// it is the D-01 anti-pattern this package exists to prevent.
	Type() string

	// List performs one upstream call and returns the model inventory
	// as a slice of Candidate values. Implementations MUST:
	// - drain+close any *http.Response body (REL-04).
	// - cap response reads with io.LimitReader (4MB, PATTERNS.md L277).
	// - return *ProviderAuthError for 401/403 / AWS auth failures.
	// - return plain wrapped error for 5xx / network / decode errors.
	// - NEVER include request headers, response body, or credential
	// material in returned error strings (§9.1 / MDISC-15).
	List(ctx context.Context) ([]Candidate, error)
}

// ProviderConfig is the resolved input to a constructor. The Discovery
// reconciler builds this from the ModelDiscovery CR
// (BaseURL, Region) + the credentials Secret (APIKey, AWSCreds) + the
// manager-owned HTTPClient (D-02).
//
// Constructor responsibilities (per provider):
// - anthropic: requires APIKey, HTTPClient. BaseURL CEL-forbidden in
// production (test-only override via SetTestBaseURL).
// - gemini: requires APIKey, HTTPClient. BaseURL CEL-forbidden.
// - elevenlabs: requires APIKey, HTTPClient. BaseURL CEL-forbidden in
// production (test-only override via SetTestBaseURL / cfg.BaseURL).
// - openai: requires APIKey, HTTPClient. BaseURL optional (used by
// OpenAI-compatible providers — Together, vLLM, Groq, OpenRouter).
// - kubeai: requires HTTPClient + BaseURL (CEL-required). APIKey
// optional. (Filled by.)
// - bedrock: requires Region. AWSCreds optional — nil falls through
// to default credential chain. HTTPClient is unused (aws-sdk-go-v2
// constructs its own internal transport). (Filled by.)
type ProviderConfig struct {
	// Type is the spec.type enum value. The Registry uses this to
	// dispatch; constructors read it only for metrics / error context.
	Type string

	// BaseURL is the user-supplied upstream URL. Empty for
	// anthropic/gemini/bedrock (CEL-forbidden); optional for openai;
	// required for kubeai.
	BaseURL string

	// Region is the AWS region for bedrock (CEL-required for that
	// type; CEL-forbidden elsewhere). Single region per CR per
	// MDISC-16 / PROJECT.md Key Decision.
	Region string

	// APIKey is the resolved string from spec.credentialsSecretRef.
	// Required for anthropic/gemini/openai; optional for kubeai;
	// unused by bedrock.
	APIKey string

	// AWSCreds is the resolved static AWS credentials for bedrock
	// (nil → fall through to default chain — IRSA / env / EC2
	// instance profile / EKS Pod Identity). Per D-05.
	AWSCreds *awsv2.Credentials

	// HTTPClient is the manager-owned shared *http.Client (per D-02:
	// 10s total-request Timeout, 30s Transport.IdleConnTimeout — see
	// cmd/main.go). Used by all HTTP providers (anthropic, gemini,
	// openai, kubeai). NOT used by bedrock.
	HTTPClient *http.Client
}

// ProviderAuthError is returned when the upstream rejects credentials
// (HTTP 401/403, or AWS AccessDenied/InvalidSignature/ExpiredToken).
// The reconciler maps this to: SourceReachable=False, reason=AuthFailed
// (MDISC-19 / spec §6.3 lines 830-835). Distinct from a generic error
// (which maps to reason=Unreachable).
//
// Implementations MUST sanitize the underlying error of credential
// material before wrapping (§9.1 — AWS error strings can carry
// signature fragments; HTTP 401/403 response bodies can echo the key
// in their error.message field). The Cause is rendered via fmt.Errorf
// "%v" verbatim, so the provider's sanitation is load-bearing.
type ProviderAuthError struct {
	// Provider is the spec.type value of the failing provider. The
	// reconciler labels this onto AuthFailed-classified metrics.
	Provider string

	// Cause is the sanitized underlying error. errors.Unwrap returns
	// this verbatim so callers can chain errors.As / errors.Is.
	Cause error
}

// Error returns the templated format
//
//	"providers: <provider> auth failed: <cause>"
//
// The reconciler surfaces this into status.conditions[].message; the
// format MUST NOT include credential material (the provider sanitizes
// Cause before wrapping per §9.1).
func (e *ProviderAuthError) Error() string {
	return fmt.Sprintf("providers: %s auth failed: %v", e.Provider, e.Cause)
}

// Unwrap returns the sanitized Cause so callers can chain errors.Is /
// errors.As against sentinel error values.
func (e *ProviderAuthError) Unwrap() error { return e.Cause }
