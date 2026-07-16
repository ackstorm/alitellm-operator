// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// canaryGeminiKey is the synthetic Google API key the Gemini canary
// asserts is never logged or surfaced. Gemini's auth surface (URL
// query parameter) is higher-risk than header-based auth — any URL
// logging anywhere would leak the key. The test enforces non-leak.
const canaryGeminiKey = "AIza-canary-XYZ-FAKE-gemini"

// TestGemini_HappyPath_StripsModelsPrefix asserts the ID transform from
// CONTEXT.md Claude's Discretion item 4 + autoconfig generator.py:427:
// the provider strips "models/" prefix from each model's name so
// Candidate.ID is "the routable ID" semantically.
func TestGemini_HappyPath_StripsModelsPrefix(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[
			{"name":"models/gemini-pro","displayName":"Gemini Pro"},
			{"name":"models/gemini-1.5-flash","displayName":"Gemini 1.5 Flash"}
		]}`))
	}))
	defer srv.Close()
	SetTestBaseURL(t, "gemini", srv.URL)

	p, err := newGeminiImpl(context.Background(), ProviderConfig{
		Type: "gemini", APIKey: canaryGeminiKey, HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("newGeminiImpl err: %v", err)
	}
	if p.Type() != "gemini" {
		t.Fatalf("Type() = %q; want gemini", p.Type())
	}
	cands, err := p.List(context.Background())
	if err != nil {
		t.Fatalf("List err: %v", err)
	}
	if len(cands) != 2 {
		t.Fatalf("got %d candidates; want 2", len(cands))
	}
	wantIDs := []string{"gemini-pro", "gemini-1.5-flash"}
	wantNames := []string{"Gemini Pro", "Gemini 1.5 Flash"}
	for i, c := range cands {
		if c.ID != wantIDs[i] {
			t.Errorf("cands[%d].ID = %q; want %q (must be POST-strip)", i, c.ID, wantIDs[i])
		}
		if c.DisplayName != wantNames[i] {
			t.Errorf("cands[%d].DisplayName = %q; want %q", i, c.DisplayName, wantNames[i])
		}
	}
}

// TestGemini_KeyInHeaderNotQuery verifies H1: the key travels in the
// x-goog-api-key header and never enters the URL query, so a leaked
// request URL cannot carry it.
func TestGemini_KeyInHeaderNotQuery(t *testing.T) {
	var gotHeader, gotRawQuery, gotAuthHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("x-goog-api-key")
		gotRawQuery = r.URL.RawQuery
		gotAuthHeader = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[]}`))
	}))
	defer srv.Close()
	SetTestBaseURL(t, "gemini", srv.URL)

	p, _ := newGeminiImpl(context.Background(), ProviderConfig{
		Type: "gemini", APIKey: canaryGeminiKey, HTTPClient: srv.Client(),
	})
	if _, err := p.List(context.Background()); err != nil {
		t.Fatalf("List err: %v", err)
	}
	if gotHeader != canaryGeminiKey {
		t.Errorf("x-goog-api-key header = %q; want %q", gotHeader, canaryGeminiKey)
	}
	if strings.Contains(gotRawQuery, "key=") || strings.Contains(gotRawQuery, canaryGeminiKey) ||
		strings.Contains(gotRawQuery, url.QueryEscape(canaryGeminiKey)) {
		t.Errorf("key leaked into URL query: %q", gotRawQuery)
	}
	// Authorization header MUST NOT be set (Gemini uses x-goog-api-key, NOT Bearer).
	if gotAuthHeader != "" {
		t.Errorf("Authorization header = %q; Gemini must not send it", gotAuthHeader)
	}
}

// TestGemini_TransportError_NoKeyLeak is the H1 regression: a transport
// error must not echo the key in the error string, because the reconciler
// writes that string into CR status conditions.
func TestGemini_TransportError_NoKeyLeak(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	SetTestBaseURL(t, "gemini", srv.URL)
	srv.Close() // force connection-refused transport error

	p, err := newGeminiImpl(context.Background(), ProviderConfig{
		Type: "gemini", APIKey: canaryGeminiKey, HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	_, listErr := p.List(context.Background())
	if listErr == nil {
		t.Fatal("expected transport error")
	}
	if strings.Contains(listErr.Error(), canaryGeminiKey) ||
		strings.Contains(listErr.Error(), url.QueryEscape(canaryGeminiKey)) {
		t.Errorf("transport error leaked key: %v", listErr)
	}
}

// TestGemini_FollowsNextPageToken is the H6 regression: model discovery
// must accumulate models across all pages, following nextPageToken.
func TestGemini_FollowsNextPageToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("pageToken") == "" {
			_, _ = w.Write([]byte(`{"models":[{"name":"models/a","displayName":"A"}],"nextPageToken":"tok2"}`))
			return
		}
		_, _ = w.Write([]byte(`{"models":[{"name":"models/b","displayName":"B"}]}`))
	}))
	defer srv.Close()
	SetTestBaseURL(t, "gemini", srv.URL)

	p, _ := newGeminiImpl(context.Background(), ProviderConfig{
		Type: "gemini", APIKey: canaryGeminiKey, HTTPClient: srv.Client(),
	})
	got, err := p.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 models across pages; got %d: %+v", len(got), got)
	}
	if got[0].ID != "a" || got[1].ID != "b" {
		t.Errorf("unexpected accumulated IDs: %+v", got)
	}
}

// TestGemini_PageCapExhaustionErrors: a server that always returns a
// nextPageToken must yield an explicit "exceeded" error, not a partial
// slice (no silent truncation).
func TestGemini_PageCapExhaustionErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[{"name":"models/x"}],"nextPageToken":"always-more"}`))
	}))
	defer srv.Close()
	SetTestBaseURL(t, "gemini", srv.URL)

	p, _ := newGeminiImpl(context.Background(), ProviderConfig{
		Type: "gemini", APIKey: canaryGeminiKey, HTTPClient: srv.Client(),
	})
	if _, err := p.List(context.Background()); err == nil {
		t.Fatal("expected page-cap exhaustion error, not a truncated result")
	}
}

// TestGemini_401_ReturnsProviderAuthError verifies the 401 path
// classifies as *ProviderAuthError.
func TestGemini_401_ReturnsProviderAuthError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error":{"code":401,"message":"API key not valid","status":"UNAUTHENTICATED"}}`))
	}))
	defer srv.Close()
	SetTestBaseURL(t, "gemini", srv.URL)

	p, _ := newGeminiImpl(context.Background(), ProviderConfig{
		Type: "gemini", APIKey: canaryGeminiKey, HTTPClient: srv.Client(),
	})
	_, listErr := p.List(context.Background())
	var target *ProviderAuthError
	if !errors.As(listErr, &target) {
		t.Fatalf("401: want *ProviderAuthError; got %T %v", listErr, listErr)
	}
	if target.Provider != "gemini" {
		t.Errorf("target.Provider = %q; want gemini", target.Provider)
	}
}

// TestGemini_403_ReturnsProviderAuthError mirrors 401.
func TestGemini_403_ReturnsProviderAuthError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		_, _ = w.Write([]byte(`{"error":{"code":403}}`))
	}))
	defer srv.Close()
	SetTestBaseURL(t, "gemini", srv.URL)

	p, _ := newGeminiImpl(context.Background(), ProviderConfig{
		Type: "gemini", APIKey: canaryGeminiKey, HTTPClient: srv.Client(),
	})
	_, listErr := p.List(context.Background())
	var target *ProviderAuthError
	if !errors.As(listErr, &target) {
		t.Fatalf("403: want *ProviderAuthError; got %T %v", listErr, listErr)
	}
}

// TestGemini_5xx_ReturnsPlainError verifies 503 → plain (Unreachable),
// NOT *ProviderAuthError.
func TestGemini_5xx_ReturnsPlainError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
	}))
	defer srv.Close()
	SetTestBaseURL(t, "gemini", srv.URL)

	p, _ := newGeminiImpl(context.Background(), ProviderConfig{
		Type: "gemini", APIKey: canaryGeminiKey, HTTPClient: srv.Client(),
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

// TestGemini_MissingAPIKey_ReturnsConstructorError verifies the
// constructor rejects empty APIKey synchronously.
func TestGemini_MissingAPIKey_ReturnsConstructorError(t *testing.T) {
	_, err := newGeminiImpl(context.Background(), ProviderConfig{
		Type: "gemini", APIKey: "", HTTPClient: http.DefaultClient,
	})
	if err == nil {
		t.Fatal("empty APIKey: want err")
	}
}

// TestGemini_NilHTTPClient_ReturnsConstructorError verifies HTTPClient
// is required.
func TestGemini_NilHTTPClient_ReturnsConstructorError(t *testing.T) {
	_, err := newGeminiImpl(context.Background(), ProviderConfig{
		Type: "gemini", APIKey: canaryGeminiKey, HTTPClient: nil,
	})
	if err == nil {
		t.Fatal("nil HTTPClient: want err")
	}
}

// TestGemini_CredentialCanary is the MDISC-15 / AC-S1 enforcer for
// Gemini. Critical because Gemini's URL query parameter is the auth
// channel — any URL-logging anywhere leaks the key. The provider must
// NOT log req.URL.String; the test verifies via bufferSink that the
// canary key (URL-encoded form) is absent from every captured line.
func TestGemini_CredentialCanary(t *testing.T) {
	buf, _ := newBufferSinkLogger(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[{"name":"models/gemini-pro","displayName":"Gemini Pro"}]}`))
	}))
	defer srv.Close()
	SetTestBaseURL(t, "gemini", srv.URL)

	p, _ := newGeminiImpl(context.Background(), ProviderConfig{
		Type: "gemini", APIKey: canaryGeminiKey, HTTPClient: srv.Client(),
	})
	cands, err := p.List(context.Background())
	if err != nil {
		t.Fatalf("List err: %v", err)
	}
	// 1. No canary (or URL-encoded canary) in returned data.
	encoded := url.QueryEscape(canaryGeminiKey)
	for i, c := range cands {
		if strings.Contains(c.ID, canaryGeminiKey) || strings.Contains(c.ID, encoded) {
			t.Errorf("cands[%d].ID leaks canary: %q", i, c.ID)
		}
		if strings.Contains(c.DisplayName, canaryGeminiKey) || strings.Contains(c.DisplayName, encoded) {
			t.Errorf("cands[%d].DisplayName leaks canary: %q", i, c.DisplayName)
		}
	}
	// 2. No canary in captured log output (raw OR URL-encoded form).
	out := buf.String()
	if strings.Contains(out, canaryGeminiKey) || strings.Contains(out, encoded) {
		t.Fatalf("canaryGeminiKey leaked into log output: %s", out)
	}
	// 3. 401 error path with key echoed by upstream.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error":{"message":"key invalid: ` + canaryGeminiKey + `"}}`))
	}))
	defer srv2.Close()
	SetTestBaseURL(t, "gemini", srv2.URL)
	_, e2 := p.List(context.Background())
	if e2 != nil && (strings.Contains(e2.Error(), canaryGeminiKey) || strings.Contains(e2.Error(), encoded)) {
		t.Fatalf("error string leaked canary: %s", e2.Error())
	}
}
