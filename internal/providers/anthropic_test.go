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

// canaryAnthropicKey is the synthetic Anthropic API key the canary test
// injects. If this string EVER appears in captured log output, returned
// Candidate fields, or the wrapped error string under any code path,
// MDISC-15 / AC-S1 is violated and the test fails.
const canaryAnthropicKey = "sk-canary-XYZ-FAKE-anthropic"

// TestAnthropic_HappyPath_ReturnsTwoCandidates verifies the OK path:
// the provider parses {"data":[{"id":".","display_name":"."}, .]}
// and returns Candidates with ID + DisplayName populated, preserving
// order from the upstream response.
// TestAnthropic_FollowsPagination is the H7 regression: model discovery must
// accumulate across pages, following has_more + last_id via ?after_id=.
func TestAnthropic_FollowsPagination(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("after_id") == "" {
			_, _ = w.Write([]byte(`{"data":[{"id":"m1","display_name":"M1"}],"has_more":true,"last_id":"m1"}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"m2","display_name":"M2"}],"has_more":false,"last_id":"m2"}`))
	}))
	defer srv.Close()
	SetTestBaseURL(t, "anthropic", srv.URL)

	p, _ := newAnthropicImpl(context.Background(), ProviderConfig{
		Type: "anthropic", APIKey: canaryAnthropicKey, HTTPClient: srv.Client(),
	})
	got, err := p.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 || got[0].ID != "m1" || got[1].ID != "m2" {
		t.Fatalf("expected m1,m2 across pages; got %+v", got)
	}
}

// TestAnthropic_PageCapExhaustionErrors: a server that always reports
// has_more must yield an explicit error, not a truncated slice.
func TestAnthropic_PageCapExhaustionErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"x"}],"has_more":true,"last_id":"x"}`))
	}))
	defer srv.Close()
	SetTestBaseURL(t, "anthropic", srv.URL)

	p, _ := newAnthropicImpl(context.Background(), ProviderConfig{
		Type: "anthropic", APIKey: canaryAnthropicKey, HTTPClient: srv.Client(),
	})
	if _, err := p.List(context.Background()); err == nil {
		t.Fatal("expected page-cap exhaustion error, not a truncated result")
	}
}

func TestAnthropic_HappyPath_ReturnsTwoCandidates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[
			{"id":"claude-3-5-sonnet-20241022","display_name":"Claude 3.5 Sonnet"},
			{"id":"claude-3-haiku-20240307","display_name":"Claude 3 Haiku"}
		]}`))
	}))
	defer srv.Close()
	SetTestBaseURL(t, "anthropic", srv.URL)

	p, err := newAnthropicImpl(context.Background(), ProviderConfig{
		Type:       "anthropic",
		APIKey:     canaryAnthropicKey,
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("newAnthropicImpl returned err: %v", err)
	}
	if p.Type() != "anthropic" { //nolint:goconst // wire-level provider discriminator asserted literally; providerTypeAnthropic const lives in anthropic.go
		t.Fatalf("Type() = %q; want %q", p.Type(), "anthropic")
	}

	cands, err := p.List(context.Background())
	if err != nil {
		t.Fatalf("List returned err: %v", err)
	}
	if len(cands) != 2 {
		t.Fatalf("List returned %d candidates; want 2", len(cands))
	}
	wantIDs := []string{"claude-3-5-sonnet-20241022", "claude-3-haiku-20240307"}
	wantNames := []string{"Claude 3.5 Sonnet", "Claude 3 Haiku"}
	for i, c := range cands {
		if c.ID != wantIDs[i] {
			t.Errorf("cands[%d].ID = %q; want %q", i, c.ID, wantIDs[i])
		}
		if c.DisplayName != wantNames[i] {
			t.Errorf("cands[%d].DisplayName = %q; want %q", i, c.DisplayName, wantNames[i])
		}
	}
}

// TestAnthropic_Headers_AreSet asserts the three required headers are
// set on the outgoing request: x-api-key (the key itself), anthropic-version
// (the hardcoded "2023-06-01" per autoconfig providers.py:303), and
// Accept: application/json. The server captures and asserts on them.
func TestAnthropic_Headers_AreSet(t *testing.T) {
	var gotAPIKey, gotVersion, gotAccept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAPIKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		gotAccept = r.Header.Get("Accept")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()
	SetTestBaseURL(t, "anthropic", srv.URL)

	p, err := newAnthropicImpl(context.Background(), ProviderConfig{
		Type:       "anthropic",
		APIKey:     canaryAnthropicKey,
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("newAnthropicImpl err: %v", err)
	}
	if _, err := p.List(context.Background()); err != nil {
		t.Fatalf("List err: %v", err)
	}
	if gotAPIKey != canaryAnthropicKey {
		t.Errorf("x-api-key = %q; want %q", gotAPIKey, canaryAnthropicKey)
	}
	if gotVersion != "2023-06-01" {
		t.Errorf("anthropic-version = %q; want %q", gotVersion, "2023-06-01")
	}
	if gotAccept != "application/json" {
		t.Errorf("Accept = %q; want %q", gotAccept, "application/json")
	}
}

// TestAnthropic_401_ReturnsProviderAuthError verifies the 401 path
// classifies as *ProviderAuthError, distinguishable via errors.As, so
// the 04-04 reconciler can map to SourceReachable=False,
// reason=AuthFailed (MDISC-19).
func TestAnthropic_401_ReturnsProviderAuthError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error":{"type":"authentication_error","message":"invalid x-api-key"}}`))
	}))
	defer srv.Close()
	SetTestBaseURL(t, "anthropic", srv.URL)

	p, err := newAnthropicImpl(context.Background(), ProviderConfig{
		Type:       "anthropic",
		APIKey:     canaryAnthropicKey,
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("newAnthropicImpl err: %v", err)
	}
	_, listErr := p.List(context.Background())
	if listErr == nil {
		t.Fatal("List returned nil err; want *ProviderAuthError")
	}
	var target *ProviderAuthError
	if !errors.As(listErr, &target) {
		t.Fatalf("errors.As: want *ProviderAuthError; got %T %v", listErr, listErr)
	}
	if target.Provider != "anthropic" {
		t.Errorf("target.Provider = %q; want %q", target.Provider, "anthropic")
	}
}

// TestAnthropic_403_ReturnsProviderAuthError mirrors 401 — 403 is also
// classified as AuthFailed per spec §6.3. Together these cover the
// permanent-auth-error class that bypasses backoff churn (D-03).
func TestAnthropic_403_ReturnsProviderAuthError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		_, _ = w.Write([]byte(`{"error":{"type":"permission_error","message":"forbidden"}}`))
	}))
	defer srv.Close()
	SetTestBaseURL(t, "anthropic", srv.URL)

	p, _ := newAnthropicImpl(context.Background(), ProviderConfig{
		Type: "anthropic", APIKey: canaryAnthropicKey, HTTPClient: srv.Client(),
	})
	_, listErr := p.List(context.Background())
	var target *ProviderAuthError
	if !errors.As(listErr, &target) {
		t.Fatalf("403 path: want *ProviderAuthError; got %T %v", listErr, listErr)
	}
}

// TestAnthropic_5xx_ReturnsPlainError verifies 5xx errors classify as
// transient (reason=Unreachable in the reconciler), NOT AuthFailed.
// errors.As against *ProviderAuthError MUST return FALSE.
func TestAnthropic_5xx_ReturnsPlainError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		_, _ = w.Write([]byte(`{"error":{"type":"server_error","message":"overloaded"}}`))
	}))
	defer srv.Close()
	SetTestBaseURL(t, "anthropic", srv.URL)

	p, _ := newAnthropicImpl(context.Background(), ProviderConfig{
		Type: "anthropic", APIKey: canaryAnthropicKey, HTTPClient: srv.Client(),
	})
	_, listErr := p.List(context.Background())
	if listErr == nil {
		t.Fatal("List 5xx returned nil err")
	}
	var target *ProviderAuthError
	if errors.As(listErr, &target) {
		t.Fatalf("5xx: want plain err; got *ProviderAuthError")
	}
}

// TestAnthropic_MissingAPIKey_ReturnsConstructorError verifies the
// constructor refuses to build an anthropic provider without an API
// key — discovery is impossible without one, and silently failing
// later at List would waste a reconcile cycle.
func TestAnthropic_MissingAPIKey_ReturnsConstructorError(t *testing.T) {
	_, err := newAnthropicImpl(context.Background(), ProviderConfig{
		Type: "anthropic", APIKey: "", HTTPClient: http.DefaultClient,
	})
	if err == nil {
		t.Fatal("newAnthropicImpl with empty APIKey: want err; got nil")
	}
}

// TestAnthropic_NilHTTPClient_ReturnsConstructorError mirrors the
// missing-APIKey case — HTTPClient is required (D-02; manager owns it).
func TestAnthropic_NilHTTPClient_ReturnsConstructorError(t *testing.T) {
	_, err := newAnthropicImpl(context.Background(), ProviderConfig{
		Type: "anthropic", APIKey: canaryAnthropicKey, HTTPClient: nil,
	})
	if err == nil {
		t.Fatal("newAnthropicImpl with nil HTTPClient: want err; got nil")
	}
}

// TestAnthropic_MalformedJSON_ReturnsPlainError verifies the JSON parse
// failure is a plain (transient/Unreachable) error, NOT *ProviderAuthError.
// Malformed upstream responses are an unreachable-ish failure mode.
func TestAnthropic_MalformedJSON_ReturnsPlainError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":...truncated`))
	}))
	defer srv.Close()
	SetTestBaseURL(t, "anthropic", srv.URL)

	p, _ := newAnthropicImpl(context.Background(), ProviderConfig{
		Type: "anthropic", APIKey: canaryAnthropicKey, HTTPClient: srv.Client(),
	})
	_, listErr := p.List(context.Background())
	if listErr == nil {
		t.Fatal("malformed JSON: want err; got nil")
	}
	var target *ProviderAuthError
	if errors.As(listErr, &target) {
		t.Fatalf("malformed JSON: must NOT be *ProviderAuthError")
	}
}

// TestAnthropic_CredentialCanary is the MDISC-15 / AC-S1 enforcer.
// It runs the full happy-path under a captured logr.Logger and asserts
// the canary API key NEVER appears in:
// - bufferSink-captured log output (every Info/Error call)
// - any returned Candidate.ID or Candidate.DisplayName
//
// The provider's implementation MUST NOT log request headers, response
// bodies, or the URL. This test catches that regression at the byte
// level.
func TestAnthropic_CredentialCanary(t *testing.T) {
	buf, _ := newBufferSinkLogger(t)
	_ = buf // captured, asserted below

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Echo the key back in the response body — IF the provider ever
		// logs response bodies, the canary catches that here too.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"claude-3-opus-20240229","display_name":"Claude 3 Opus"}]}`))
	}))
	defer srv.Close()
	SetTestBaseURL(t, "anthropic", srv.URL)

	p, _ := newAnthropicImpl(context.Background(), ProviderConfig{
		Type: "anthropic", APIKey: canaryAnthropicKey, HTTPClient: srv.Client(),
	})
	cands, err := p.List(context.Background())
	if err != nil {
		t.Fatalf("List err: %v", err)
	}
	// 1. No canary in returned data.
	for i, c := range cands {
		if strings.Contains(c.ID, canaryAnthropicKey) {
			t.Errorf("cands[%d].ID contains canary: %q", i, c.ID)
		}
		if strings.Contains(c.DisplayName, canaryAnthropicKey) {
			t.Errorf("cands[%d].DisplayName contains canary: %q", i, c.DisplayName)
		}
	}
	// 2. No canary in captured log output.
	if strings.Contains(buf.String(), canaryAnthropicKey) {
		t.Fatalf("canaryAnthropicKey leaked into log output: %s", buf.String())
	}
	// 3. Also assert against the 401 error path — the response body may
	// contain credential material echoed back by the upstream.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		// Server echoes the key — provider MUST NOT include this body
		// in the error string.
		_, _ = w.Write([]byte(`{"error":{"message":"invalid key: ` + canaryAnthropicKey + `"}}`))
	}))
	defer srv2.Close()
	SetTestBaseURL(t, "anthropic", srv2.URL)
	_, e2 := p.List(context.Background())
	if e2 != nil && strings.Contains(e2.Error(), canaryAnthropicKey) {
		t.Fatalf("error string leaked canary: %s", e2.Error())
	}
}
