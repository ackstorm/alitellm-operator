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

// canaryElevenLabsKey is the synthetic key the canary asserts is never
// logged or surfaced (MDISC-15 / §9.1).
const canaryElevenLabsKey = "xi-canary-XYZ-FAKE-elevenlabs"

// TestElevenLabs_HappyPath parses the bare-array /v1/models response and
// asserts the xi-api-key header carries the key (NOT Authorization).
func TestElevenLabs_HappyPath(t *testing.T) {
	var gotKey, gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("xi-api-key")
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"model_id":"eleven_multilingual_v2","name":"Eleven Multilingual v2"},{"model_id":"scribe_v1","name":"Scribe v1"}]`))
	}))
	defer srv.Close()

	p, err := newElevenLabs(context.Background(), ProviderConfig{
		Type: "elevenlabs", APIKey: canaryElevenLabsKey, BaseURL: srv.URL, HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("newElevenLabs err: %v", err)
	}
	if got := p.Type(); got != "elevenlabs" { //nolint:goconst // wire-level provider discriminator asserted literally across elevenlabs_test cases
		t.Fatalf("Type() = %q; want elevenlabs", got)
	}
	cands, err := p.List(context.Background())
	if err != nil {
		t.Fatalf("List err: %v", err)
	}
	if len(cands) != 2 {
		t.Fatalf("got %d candidates; want 2", len(cands))
	}
	if cands[0].ID != "eleven_multilingual_v2" || cands[0].DisplayName != "Eleven Multilingual v2" {
		t.Errorf("cands[0] = %+v; want {eleven_multilingual_v2, Eleven Multilingual v2}", cands[0])
	}
	if cands[1].ID != "scribe_v1" {
		t.Errorf("cands[1].ID = %q; want scribe_v1", cands[1].ID)
	}
	if gotKey != canaryElevenLabsKey {
		t.Errorf("xi-api-key = %q; want %q", gotKey, canaryElevenLabsKey)
	}
	if gotAuth != "" {
		t.Errorf("Authorization header must be unset; got %q", gotAuth)
	}
	if gotPath != "/v1/models" && gotPath != "/models" {
		// srv.URL has no path, so baseURLFor(srv.URL)+"/models" → "/models".
		// Production base ".../v1" makes it "/v1/models". Accept both shapes.
		t.Errorf("request path = %q; want .../models", gotPath)
	}
}

// TestElevenLabs_MissingAPIKey_ReturnsConstructorError verifies the
// constructor synchronously rejects an empty APIKey (CEL requires
// credentialsSecretRef; this is the in-process backstop).
func TestElevenLabs_MissingAPIKey_ReturnsConstructorError(t *testing.T) {
	_, err := newElevenLabs(context.Background(), ProviderConfig{
		Type: "elevenlabs", APIKey: "", HTTPClient: http.DefaultClient,
	})
	if err == nil {
		t.Fatal("empty APIKey: want err; got nil")
	}
}

// TestElevenLabs_NilHTTPClient_ReturnsConstructorError verifies the
// universal HTTPClient gate.
func TestElevenLabs_NilHTTPClient_ReturnsConstructorError(t *testing.T) {
	_, err := newElevenLabs(context.Background(), ProviderConfig{
		Type: "elevenlabs", APIKey: canaryElevenLabsKey, HTTPClient: nil,
	})
	if err == nil {
		t.Fatal("nil HTTPClient: want err; got nil")
	}
}

// TestElevenLabs_401_ReturnsProviderAuthError verifies 401 → AuthFailed.
func TestElevenLabs_401_ReturnsProviderAuthError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	p, err := newElevenLabs(context.Background(), ProviderConfig{
		Type: "elevenlabs", APIKey: canaryElevenLabsKey, BaseURL: srv.URL, HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("newElevenLabs err: %v", err)
	}
	_, listErr := p.List(context.Background())
	var target *ProviderAuthError
	if !errors.As(listErr, &target) {
		t.Fatalf("401: want *ProviderAuthError; got %T %v", listErr, listErr)
	}
	if target.Provider != "elevenlabs" {
		t.Errorf("target.Provider = %q; want elevenlabs", target.Provider)
	}
}

// TestElevenLabs_5xx_ReturnsPlainError verifies 5xx → Unreachable (NOT AuthFailed).
func TestElevenLabs_5xx_ReturnsPlainError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	p, err := newElevenLabs(context.Background(), ProviderConfig{
		Type: "elevenlabs", APIKey: canaryElevenLabsKey, BaseURL: srv.URL, HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("newElevenLabs err: %v", err)
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

// TestElevenLabs_CredentialCanary enforces MDISC-15 / §9.1: even when the
// upstream echoes the key in a 401 body, it must not appear in the error.
func TestElevenLabs_CredentialCanary(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"detail":{"message":"Invalid API key: ` + canaryElevenLabsKey + `"}}`))
	}))
	defer srv.Close()

	p, err := newElevenLabs(context.Background(), ProviderConfig{
		Type: "elevenlabs", APIKey: canaryElevenLabsKey, BaseURL: srv.URL, HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("newElevenLabs err: %v", err)
	}
	_, listErr := p.List(context.Background())
	if listErr == nil {
		t.Fatal("401: want err; got nil")
	}
	if strings.Contains(listErr.Error(), canaryElevenLabsKey) {
		t.Fatalf("error string leaked canary: %s", listErr.Error())
	}
}

// TestElevenLabs_Registry_Routes verifies the registry entry resolves to
// the real constructor.
func TestElevenLabs_Registry_Routes(t *testing.T) {
	ctor, ok := Registry["elevenlabs"]
	if !ok {
		t.Fatal("Registry has no elevenlabs entry")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	p, err := ctor(context.Background(), ProviderConfig{
		Type: "elevenlabs", APIKey: canaryElevenLabsKey, BaseURL: srv.URL, HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("Registry[elevenlabs] err: %v", err)
	}
	if p.Type() != "elevenlabs" {
		t.Errorf("Type() = %q; want elevenlabs", p.Type())
	}
}
