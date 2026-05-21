// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// canaryOpenAIKey is the synthetic OpenAI API key the canary asserts is
// never logged or surfaced. AC-S1 / MDISC-15.
const canaryOpenAIKey = "sk-canary-XYZ-FAKE-openai"

// TestOpenAI_HappyPath_DefaultBaseURL exercises the production path
// where cfg.BaseURL is "" (default https://api.openai.com/v1 — but
// here SetTestBaseURL redirects to httptest). Returns 2 candidates.
func TestOpenAI_HappyPath_DefaultBaseURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-4o-mini","object":"model"},{"id":"gpt-4-turbo","object":"model"}]}`))
	}))
	defer srv.Close()
	SetTestBaseURL(t, "openai", srv.URL)

	p, err := newOpenAI(context.Background(), ProviderConfig{
		Type: "openai", APIKey: canaryOpenAIKey, HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("newOpenAI err: %v", err)
	}
	if p.Type() != "openai" { //nolint:goconst // wire-level provider discriminator asserted literally across openai_test cases
		t.Fatalf("Type() = %q; want openai", p.Type())
	}
	cands, err := p.List(context.Background())
	if err != nil {
		t.Fatalf("List err: %v", err)
	}
	if len(cands) != 2 {
		t.Fatalf("got %d candidates; want 2", len(cands))
	}
	wantIDs := []string{"gpt-4o-mini", "gpt-4-turbo"}
	for i, c := range cands {
		if c.ID != wantIDs[i] {
			t.Errorf("cands[%d].ID = %q; want %q", i, c.ID, wantIDs[i])
		}
	}
}

// TestOpenAI_OpenAICompatBaseURL exercises the user-supplied baseUrl
// path — the same code path serves Together.ai / vLLM / Groq /
// OpenRouter (CONTEXT.md <specifics> line 279). cfg.BaseURL=srv.URL
// wins over the test override AND the default (three-tier priority,
// see TestBaseURLFor_Priority).
func TestOpenAI_OpenAICompatBaseURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"meta-llama/Llama-3-8b-chat-hf"}]}`))
	}))
	defer srv.Close()
	// Do NOT call SetTestBaseURL — cfg.BaseURL wins over override.

	p, err := newOpenAI(context.Background(), ProviderConfig{
		Type: "openai", APIKey: canaryOpenAIKey, BaseURL: srv.URL, HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("newOpenAI err: %v", err)
	}
	cands, err := p.List(context.Background())
	if err != nil {
		t.Fatalf("List err: %v", err)
	}
	if len(cands) != 1 || cands[0].ID != "meta-llama/Llama-3-8b-chat-hf" {
		t.Fatalf("OpenAI-compat (Together-style) path: got %+v", cands)
	}
}

// TestOpenAI_BearerHeader_IsSet asserts Authorization: Bearer <key>
// is set on the outgoing request.
func TestOpenAI_BearerHeader_IsSet(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()
	SetTestBaseURL(t, "openai", srv.URL)

	p, _ := newOpenAI(context.Background(), ProviderConfig{
		Type: "openai", APIKey: canaryOpenAIKey, HTTPClient: srv.Client(),
	})
	if _, err := p.List(context.Background()); err != nil {
		t.Fatalf("List err: %v", err)
	}
	wantPrefix := "Bearer " + canaryOpenAIKey
	if gotAuth != wantPrefix {
		t.Errorf("Authorization header = %q; want %q", gotAuth, wantPrefix)
	}
}

// TestOpenAI_MissingAPIKey_ReturnsConstructorError verifies newOpenAI
// rejects empty APIKey synchronously. (Note: although List will
// later be reused by KubeAI with empty APIKey, the OPENAI constructor
// still requires the key — see openai.go.)
func TestOpenAI_MissingAPIKey_ReturnsConstructorError(t *testing.T) {
	_, err := newOpenAI(context.Background(), ProviderConfig{
		Type: "openai", APIKey: "", HTTPClient: http.DefaultClient,
	})
	if err == nil {
		t.Fatal("empty APIKey: want err")
	}
}

// TestOpenAI_NilHTTPClient_ReturnsConstructorError verifies HTTPClient required.
func TestOpenAI_NilHTTPClient_ReturnsConstructorError(t *testing.T) {
	_, err := newOpenAI(context.Background(), ProviderConfig{
		Type: "openai", APIKey: canaryOpenAIKey, HTTPClient: nil,
	})
	if err == nil {
		t.Fatal("nil HTTPClient: want err")
	}
}

// TestOpenAI_401_ReturnsProviderAuthError verifies the 401 path.
func TestOpenAI_401_ReturnsProviderAuthError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error":{"message":"Incorrect API key provided","type":"invalid_request_error"}}`))
	}))
	defer srv.Close()
	SetTestBaseURL(t, "openai", srv.URL)

	p, _ := newOpenAI(context.Background(), ProviderConfig{
		Type: "openai", APIKey: canaryOpenAIKey, HTTPClient: srv.Client(),
	})
	_, listErr := p.List(context.Background())
	var target *ProviderAuthError
	if !errors.As(listErr, &target) {
		t.Fatalf("401: want *ProviderAuthError; got %T %v", listErr, listErr)
	}
	if target.Provider != "openai" {
		t.Errorf("target.Provider = %q; want openai", target.Provider)
	}
}

// TestOpenAI_403_ReturnsProviderAuthError covers 403 too.
func TestOpenAI_403_ReturnsProviderAuthError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
	}))
	defer srv.Close()
	SetTestBaseURL(t, "openai", srv.URL)

	p, _ := newOpenAI(context.Background(), ProviderConfig{
		Type: "openai", APIKey: canaryOpenAIKey, HTTPClient: srv.Client(),
	})
	_, listErr := p.List(context.Background())
	var target *ProviderAuthError
	if !errors.As(listErr, &target) {
		t.Fatalf("403: want *ProviderAuthError; got %T", listErr)
	}
}

// TestOpenAI_5xx_ReturnsPlainError verifies 503 → plain (Unreachable).
func TestOpenAI_5xx_ReturnsPlainError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
	}))
	defer srv.Close()
	SetTestBaseURL(t, "openai", srv.URL)

	p, _ := newOpenAI(context.Background(), ProviderConfig{
		Type: "openai", APIKey: canaryOpenAIKey, HTTPClient: srv.Client(),
	})
	_, listErr := p.List(context.Background())
	if listErr == nil {
		t.Fatal("5xx: want err")
	}
	var target *ProviderAuthError
	if errors.As(listErr, &target) {
		t.Fatalf("5xx: must NOT be *ProviderAuthError")
	}
}

// TestOpenAI_CredentialCanary is the MDISC-15 / AC-S1 enforcer.
func TestOpenAI_CredentialCanary(t *testing.T) {
	buf, _ := newBufferSinkLogger(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-4o"}]}`))
	}))
	defer srv.Close()
	SetTestBaseURL(t, "openai", srv.URL)

	p, _ := newOpenAI(context.Background(), ProviderConfig{
		Type: "openai", APIKey: canaryOpenAIKey, HTTPClient: srv.Client(),
	})
	cands, err := p.List(context.Background())
	if err != nil {
		t.Fatalf("List err: %v", err)
	}
	for i, c := range cands {
		if strings.Contains(c.ID, canaryOpenAIKey) || strings.Contains(c.DisplayName, canaryOpenAIKey) {
			t.Errorf("cands[%d] leaks canary: %+v", i, c)
		}
	}
	if strings.Contains(buf.String(), canaryOpenAIKey) {
		t.Fatalf("canaryOpenAIKey leaked into log output: %s", buf.String())
	}
	// 401 error path with key echoed by upstream.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error":{"message":"Invalid API key: ` + canaryOpenAIKey + `"}}`))
	}))
	defer srv2.Close()
	SetTestBaseURL(t, "openai", srv2.URL)
	_, e2 := p.List(context.Background())
	if e2 != nil && strings.Contains(e2.Error(), canaryOpenAIKey) {
		t.Fatalf("error string leaked canary: %s", e2.Error())
	}
}

// TestOpenAI_TypeLabel_DefaultsToOpenAI asserts the deviation contract:
// newOpenAI produces an *openaiProvider whose typeLabel="openai", so
// Type returns "openai". This is the property will
// exploit — its newKubeAI will construct an *openaiProvider with
// typeLabel="kubeai" so Type returns "kubeai" (memory:
// kubeai-openai-consolidation reshape).
func TestOpenAI_TypeLabel_DefaultsToOpenAI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()
	SetTestBaseURL(t, "openai", srv.URL)

	p, err := newOpenAI(context.Background(), ProviderConfig{
		Type: "openai", APIKey: canaryOpenAIKey, HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("newOpenAI err: %v", err)
	}
	// White-box: cast to *openaiProvider and assert typeLabel field.
	concrete, ok := p.(*openaiProvider)
	if !ok {
		t.Fatalf("newOpenAI returned %T; want *openaiProvider", p)
	}
	if concrete.typeLabel != "openai" {
		t.Errorf("typeLabel = %q; want openai", concrete.typeLabel)
	}
	// Behavioral: Type returns the same label.
	if got := p.Type(); got != "openai" {
		t.Errorf("Type() = %q; want openai", got)
	}
}

// TestOpenAI_AuthorizationHeader_ConditionalOnAPIKey verifies the
// deviation contract for the request-building code: the Authorization
// header is set ONLY when apiKey != "". This lets the same List
// be reusable from newKubeAI (which will instantiate
// *openaiProvider with typeLabel="kubeai" + empty apiKey for unauth
// in-cluster KubeAI). newOpenAI itself still requires apiKey at
// constructor level — so we test the conditional by mutating the
// struct field directly via the white-box path.
func TestOpenAI_AuthorizationHeader_ConditionalOnAPIKey(t *testing.T) {
	var gotAuth string
	var gotAuthCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if vals := r.Header.Values("Authorization"); len(vals) > 0 {
			gotAuthCount = len(vals)
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()
	// Override for BOTH typeLabels — the second sub-block re-uses srv.
	SetTestBaseURL(t, "openai", srv.URL)
	SetTestBaseURL(t, "kubeai", srv.URL)

	// Build a provider directly (bypassing newOpenAI's apiKey
	// validation) to exercise the empty-apiKey path that	// newKubeAI will use.
	p := &openaiProvider{
		apiKey:     "",
		baseURL:    "", // resolves via SetTestBaseURL override for "kubeai".
		typeLabel:  "kubeai",
		httpClient: srv.Client(),
	}
	if _, err := p.List(context.Background()); err != nil {
		t.Fatalf("List err: %v", err)
	}
	if gotAuth != "" || gotAuthCount > 0 {
		t.Fatalf("Authorization should be unset when apiKey=\"\"; got %q (count=%d)", gotAuth, gotAuthCount)
	}
	// Type returns the typeLabel verbatim.
	if got := p.Type(); got != "kubeai" {
		t.Errorf("Type() with typeLabel=kubeai = %q; want kubeai", got)
	}

	// Now non-empty apiKey: Authorization MUST be set.
	gotAuth = ""
	p2 := &openaiProvider{
		apiKey:     canaryOpenAIKey,
		baseURL:    "",
		typeLabel:  "openai",
		httpClient: srv.Client(),
	}
	if _, err := p2.List(context.Background()); err != nil {
		t.Fatalf("List err: %v", err)
	}
	wantAuth := "Bearer " + canaryOpenAIKey
	if gotAuth != wantAuth {
		t.Errorf("with apiKey set: Authorization = %q; want %q", gotAuth, wantAuth)
	}
}
