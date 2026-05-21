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

// canaryKubeAIKey is the synthetic KubeAI API key the canary asserts is
// never logged or surfaced. AC-S1 / MDISC-15.
const canaryKubeAIKey = "sk-canary-XYZ-FAKE-kubeai"

// TestKubeAI_HappyPath_NoAuth exercises the in-cluster, no-auth case
// (CONTEXT.md <specifics> line 278 — KubeAI tolerates empty
// Authorization). The provider must omit the Authorization header
// entirely when APIKey == "" — see openai.go's conditional branch.
func TestKubeAI_HappyPath_NoAuth(t *testing.T) {
	var gotAuth string
	var gotAuthCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAuthCount = len(r.Header.Values("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"qwen2-7b","object":"model"},{"id":"llama-3-8b","object":"model"}]}`))
	}))
	defer srv.Close()

	p, err := newKubeAI(context.Background(), ProviderConfig{
		Type: "kubeai", APIKey: "", BaseURL: srv.URL, HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("newKubeAI err: %v", err)
	}
	if got := p.Type(); got != "kubeai" { //nolint:goconst // wire-level provider discriminator asserted literally across kubeai_test cases
		t.Fatalf("Type() = %q; want kubeai", got)
	}
	cands, err := p.List(context.Background())
	if err != nil {
		t.Fatalf("List err: %v", err)
	}
	if len(cands) != 2 {
		t.Fatalf("got %d candidates; want 2", len(cands))
	}
	wantIDs := []string{"qwen2-7b", "llama-3-8b"}
	for i, c := range cands {
		if c.ID != wantIDs[i] {
			t.Errorf("cands[%d].ID = %q; want %q", i, c.ID, wantIDs[i])
		}
	}
	if gotAuth != "" || gotAuthCount > 0 {
		t.Fatalf("Authorization MUST be unset for empty APIKey; got %q (count=%d)", gotAuth, gotAuthCount)
	}
}

// TestKubeAI_HappyPath_WithAuth exercises the case where KubeAI is
// fronted by an auth proxy and the user supplies a Bearer token via
// spec.credentialsSecretRef. The provider must set
// Authorization: Bearer <key>.
func TestKubeAI_HappyPath_WithAuth(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"qwen2-7b"},{"id":"llama-3-8b"}]}`))
	}))
	defer srv.Close()

	p, err := newKubeAI(context.Background(), ProviderConfig{
		Type: "kubeai", APIKey: canaryKubeAIKey, BaseURL: srv.URL, HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("newKubeAI err: %v", err)
	}
	cands, err := p.List(context.Background())
	if err != nil {
		t.Fatalf("List err: %v", err)
	}
	if len(cands) != 2 {
		t.Fatalf("got %d candidates; want 2", len(cands))
	}
	wantAuth := "Bearer " + canaryKubeAIKey
	if gotAuth != wantAuth {
		t.Errorf("Authorization = %q; want %q", gotAuth, wantAuth)
	}
}

// TestKubeAI_MissingBaseURL_ReturnsConstructorError verifies the
// constructor synchronously rejects an empty BaseURL. KubeAI has no
// production default (defaultBaseURLs intentionally omits "kubeai"),
// so spec.baseUrl is required at admission time (MDISC-18 CEL rule).
// The constructor's gate is the in-process backstop for that rule.
func TestKubeAI_MissingBaseURL_ReturnsConstructorError(t *testing.T) {
	_, err := newKubeAI(context.Background(), ProviderConfig{
		Type: "kubeai", APIKey: "", BaseURL: "", HTTPClient: http.DefaultClient,
	})
	if err == nil {
		t.Fatal("empty BaseURL: want err; got nil")
	}
	msg := err.Error()
	// Accept either phrasing — "baseUrl" or "BaseURL" or "spec.baseUrl"
	// (the underlying sentinel from 04-03a is "providers: BaseURL is
	// required"; a wrapped form mentioning "kubeai" is also acceptable).
	lower := strings.ToLower(msg)
	if !strings.Contains(lower, "baseurl") && !strings.Contains(lower, "base url") {
		t.Errorf("error %q does not mention baseUrl", msg)
	}
}

// TestKubeAI_401_ReturnsProviderAuthError verifies the 401 path is
// classified as AuthFailed (MDISC-19 / spec §6.3 lines 830-835). Even
// when KubeAI is deployed without auth, a mid-rotation flip can return
// 401; the reconciler must surface SourceReachable=False
// reason=AuthFailed.
func TestKubeAI_401_ReturnsProviderAuthError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	p, err := newKubeAI(context.Background(), ProviderConfig{
		Type: "kubeai", APIKey: canaryKubeAIKey, BaseURL: srv.URL, HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("newKubeAI err: %v", err)
	}
	_, listErr := p.List(context.Background())
	var target *ProviderAuthError
	if !errors.As(listErr, &target) {
		t.Fatalf("401: want *ProviderAuthError; got %T %v", listErr, listErr)
	}
	if target.Provider != "kubeai" {
		t.Errorf("target.Provider = %q; want kubeai", target.Provider)
	}
}

// TestKubeAI_5xx_ReturnsPlainError verifies 5xx → Unreachable (NOT
// AuthFailed). Required by the spec's uniform-source classification.
func TestKubeAI_5xx_ReturnsPlainError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	p, err := newKubeAI(context.Background(), ProviderConfig{
		Type: "kubeai", APIKey: "", BaseURL: srv.URL, HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("newKubeAI err: %v", err)
	}
	_, listErr := p.List(context.Background())
	if listErr == nil {
		t.Fatal("5xx: want err; got nil")
	}
	var target *ProviderAuthError
	if errors.As(listErr, &target) {
		t.Fatalf("5xx: must NOT be *ProviderAuthError; got %T", listErr)
	}
}

// TestKubeAI_CredentialCanary is the MDISC-15 / AC-S1 enforcer for the
// kubeai constructor. When an APIKey is set, the value must never appear
// in log output, returned Candidate fields, or wrapped error strings —
// even when the upstream's error response echoes the key (a known
// behavior of some OpenAI-compatible reverse proxies).
func TestKubeAI_CredentialCanary(t *testing.T) {
	buf, _ := newBufferSinkLogger(t)

	// Happy path with canary key.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"qwen2-7b"}]}`))
	}))
	defer srv.Close()

	p, err := newKubeAI(context.Background(), ProviderConfig{
		Type: "kubeai", APIKey: canaryKubeAIKey, BaseURL: srv.URL, HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("newKubeAI err: %v", err)
	}
	cands, err := p.List(context.Background())
	if err != nil {
		t.Fatalf("List err: %v", err)
	}
	for i, c := range cands {
		if strings.Contains(c.ID, canaryKubeAIKey) || strings.Contains(c.DisplayName, canaryKubeAIKey) {
			t.Errorf("cands[%d] leaks canary: %+v", i, c)
		}
	}
	if strings.Contains(buf.String(), canaryKubeAIKey) {
		t.Fatalf("canaryKubeAIKey leaked into log output: %s", buf.String())
	}

	// 401 error path with key echoed by the upstream — error string
	// must NOT include the canary (defense: status-only formatting).
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"Invalid API key: ` + canaryKubeAIKey + `"}}`))
	}))
	defer srv2.Close()

	p2, err := newKubeAI(context.Background(), ProviderConfig{
		Type: "kubeai", APIKey: canaryKubeAIKey, BaseURL: srv2.URL, HTTPClient: srv2.Client(),
	})
	if err != nil {
		t.Fatalf("newKubeAI err: %v", err)
	}
	_, listErr := p2.List(context.Background())
	if listErr == nil {
		t.Fatal("401: want err; got nil")
	}
	if strings.Contains(listErr.Error(), canaryKubeAIKey) {
		t.Fatalf("error string leaked canary: %s", listErr.Error())
	}
}

// TestKubeAI_Registry_Routes verifies the registry entry resolves to
// the real constructor (no more "not yet implemented" stub).
func TestKubeAI_Registry_Routes(t *testing.T) {
	ctor, ok := Registry["kubeai"]
	if !ok {
		t.Fatal("Registry has no kubeai entry")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	p, err := ctor(context.Background(), ProviderConfig{
		Type: "kubeai", BaseURL: srv.URL, HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("Registry[kubeai] err: %v", err)
	}
	if p.Type() != "kubeai" {
		t.Errorf("Type() = %q; want kubeai", p.Type())
	}
}
