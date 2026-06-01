// SPDX-License-Identifier: Apache-2.0

package litellm

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
)

const testMasterKey = "sk-test-abc-DEF-123"

// 401 body literal lifted verbatim from 01-01-SUMMARY.md.
const litellmAuth401Body = `{"error":{"message":"Authentication Error, Invalid proxy server token passed. Received API Key = sk-test-abc-DEF-123, Key Hash (Token) =61def7928d739903cc1d300521e6ac878bf50e70720607e03ff077cd6c5cb57d. Unable to find token in cache or ` + "`LiteLLM_VerificationTokenTable`" + `","type":"token_not_found_in_db","param":"key","code":"401"}}`

func newTestClient(t *testing.T, url string) *Client {
	t.Helper()
	return NewClient(url, testMasterKey, logr.Discard())
}

// TestMakeRequest_LargeSuccessBodyNotCappedAt1MB is the H4 regression at
// the makeRequest layer: a 2xx LIST body larger than the 1 MB error-envelope
// cap (but under the 16 MB list-body cap) must be returned in full, not
// truncated into an invalid-JSON decode loop. The exact-cap and
// over-cap-errors mechanics are covered by readbody_test.go.
func TestMakeRequest_LargeSuccessBodyNotCappedAt1MB(t *testing.T) {
	const size = 2 << 20 // 2 MB — over errEnvelopeCap, under listBodyCap
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write(make([]byte, size))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	body, err := c.makeRequest(context.Background(), "GET", "/v2/model/info", nil)
	if err != nil {
		t.Fatalf("2 MB success body must not be capped: %v", err)
	}
	if len(body) != size {
		t.Fatalf("want %d bytes, got %d", size, len(body))
	}
}

// Test401IsTypedError — REL-06. Mock returns 401 with the literal
// LiteLLM 1.83.10 body shape recorded// returned error satisfies errors.As(*Auth401Error).
func Test401IsTypedError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(401)
		_, _ = w.Write([]byte(litellmAuth401Body))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	_, err := c.ProbeConnection(context.Background())
	if err == nil {
		t.Fatalf("expected error on 401, got nil")
	}

	var auth401 *Auth401Error
	if !errors.As(err, &auth401) {
		t.Fatalf("expected errors.As to resolve *Auth401Error; got %T: %v", err, err)
	}
	if auth401.Path != "/key/health" {
		t.Errorf("Auth401Error.Path: want /key/health, got %q", auth401.Path)
	}
}

// TestProbeConnectionPathIsKeyHealth — the probe path is POST /key/health
// (swapped from GET /models). The endpoint is auth-gated (so 401 detection
// still works) AND returns the proxy's view of master-key + logging
// callback health, which the connection reconciler surfaces as a
// secondary LoggingHealthy condition.
func TestProbeConnectionPathIsKeyHealth(t *testing.T) {
	var (
		mu        sync.Mutex
		gotPath   string
		gotAuth   string
		gotMethod string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotMethod = r.Method
		mu.Unlock()
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"key":"healthy","logging_callbacks":{"status":"healthy","details":""}}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	res, err := c.ProbeConnection(context.Background())
	if err != nil {
		t.Fatalf("ProbeConnection: %v", err)
	}
	if res.LoggingStatus != "healthy" {
		t.Errorf("ProbeResult.LoggingStatus: want healthy, got %q", res.LoggingStatus)
	}

	mu.Lock()
	defer mu.Unlock()
	if gotPath != "/key/health" {
		t.Fatalf("path: want /key/health, got %q", gotPath)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method: want %s, got %q", http.MethodPost, gotMethod)
	}
	if gotAuth != "Bearer "+testMasterKey {
		t.Errorf("auth header: want %q, got %q", "Bearer "+testMasterKey, gotAuth)
	}
}

// TestAuthHeaderOverrideViaEnv — the LITELLM_OPERATOR_AUTH_HEADER env
// var switches the auth header from Authorization: Bearer to
// x-litellm-api-key. Documents the escape hatch.
func TestAuthHeaderOverrideViaEnv(t *testing.T) {
	t.Setenv(EnvAuthHeader, "x-litellm-api-key")

	var (
		mu             sync.Mutex
		gotAuth        string
		gotXLitellmKey string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotAuth = r.Header.Get("Authorization")
		gotXLitellmKey = r.Header.Get("x-litellm-api-key")
		mu.Unlock()
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	if _, err := c.ProbeConnection(context.Background()); err != nil {
		t.Fatalf("ProbeConnection: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if gotAuth != "" {
		t.Errorf("Authorization header should be empty when override set, got %q", gotAuth)
	}
	if gotXLitellmKey != testMasterKey {
		t.Errorf("x-litellm-api-key: want %q, got %q", testMasterKey, gotXLitellmKey)
	}
}

// TestMakeRequestDefersDrainAndClose — REL-04 reinforcement at the
// Client.makeRequest layer. The proper proxy for "drain+close success
// on every code path" is HTTP keepalive reuse: if the response body is
// drained+closed, net/http parks the underlying TCP connection in the
// idle pool and reuses it for the next request. If the body is NOT
// drained, the connection is abandoned and a fresh TCP handshake is
// required next time.
//
// We count UNIQUE connections opened by the server (StateNew). With
// drain+close working correctly, 1000 sequential requests should reuse
// a tiny pool (typically 1–2 connections). Without drain+close, the
// count grows ~linearly with the request count.
func TestMakeRequestDefersDrainAndClose(t *testing.T) {
	var newConns int64
	var activeNow int64
	var maxConcurrent int64

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		// Non-trivial body (1 KB) — exercises the read+close path.
		_, _ = w.Write([]byte(`{"ok":true,"size":1024,"data":"` + string(make([]byte, 1024)) + `"}`))
	}))
	// Assign ConnState BEFORE Start; httptest.NewServer's serve goroutine
	// reads srv.Config.ConnState as connections arrive, so a post-Start
	// assignment is a data race (detected by `go test -race`).
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		switch s {
		case http.StateNew:
			atomic.AddInt64(&newConns, 1)
			cur := atomic.AddInt64(&activeNow, 1)
			for {
				maxC := atomic.LoadInt64(&maxConcurrent)
				if cur <= maxC || atomic.CompareAndSwapInt64(&maxConcurrent, maxC, cur) {
					break
				}
			}
		case http.StateClosed, http.StateHijacked:
			atomic.AddInt64(&activeNow, -1)
		}
	}
	srv.Start()
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	for i := 0; i < 1000; i++ {
		if _, err := c.ProbeConnection(context.Background()); err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
	}

	// Keepalive reuse: a healthy drain+close path should open vastly
	// fewer than 1000 unique TCP connections — typically 1, sometimes
	// a handful due to idle timeouts. We assert <50 as a generous
	// upper bound that still cleanly distinguishes "drained" from
	// "leaked" (without drain, every request opens a fresh connection).
	got := atomic.LoadInt64(&newConns)
	if got > 50 {
		t.Fatalf("REL-04 leak suspected: 1000 sequential probes opened %d unique TCP connections (expected <50 due to keepalive reuse)", got)
	}

	// Sanity: at the end of the test there should be no more than a
	// few connections still active (keepalive idle pool); give the
	// runtime a brief moment to settle and then check max-concurrent
	// stayed bounded.
	time.Sleep(50 * time.Millisecond)
	if maxC := atomic.LoadInt64(&maxConcurrent); maxC > 20 {
		t.Errorf("max concurrent connections grew unbounded: %d (REL-04 keepalive starvation)", maxC)
	}
}

// TestNon2xxNon401IsGenericError — anything that is not 2xx and not 401
// is mapped to a generic error (NOT *Auth401Error). The error message
// includes the status code AND the parsed LiteLLM error code, but
// NEVER the raw body (§9.1).
func TestNon2xxNon401IsGenericError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(422)
		_, _ = w.Write([]byte(`{"error":{"message":"validation failed: model_name required","type":"validation_error","param":"model_name","code":"422"}}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	_, err := c.makeRequest(context.Background(), http.MethodPost, "/model/new", map[string]any{})
	if err == nil {
		t.Fatalf("expected error on 422")
	}
	var auth401 *Auth401Error
	if errors.As(err, &auth401) {
		t.Fatalf("422 must NOT be classified as *Auth401Error")
	}
	// Body content "validation failed: model_name required" must NOT
	// appear in the error string (§9.1 — bodies never in error
	// surfacing because they bubble into Events / Status conditions).
	if got := err.Error(); contains(got, "model_name required") {
		t.Errorf("error string leaked body content: %q", got)
	}
}

// TestDelete404IsSuccess — §7.7 idempotent delete: DELETE returning
// 404 is treated as success (the resource was already gone).
func TestDelete404IsSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "DELETE" {
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"error":{"message":"not found","type":"x","param":null,"code":"404"}}`))
			return
		}
		w.WriteHeader(500)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	_, err := c.makeRequest(context.Background(), "DELETE", "/v1/agents/zzz", nil)
	if err != nil {
		t.Errorf("DELETE 404 should be success, got: %v", err)
	}
}

// contains is a small substring helper. Avoids pulling strings into the
// test package import list when not otherwise needed.
func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// TestNewClient_PreservesPathPrefix locks the prefix-preservation
// contract referenced by issue #25 acceptance bullet — reverse-proxy
// deployments mount LiteLLM under a path prefix (e.g.
// https://gw/litellm) and the operator must concatenate path segments
// onto the prefix, not strip it. Pins behavior so future refactors of
// strings.TrimRight do not silently regress this.
func TestNewClient_PreservesPathPrefix(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want string
	}{
		{"http://litellm:4000", "http://litellm:4000"},
		{"http://litellm:4000/", "http://litellm:4000"},
		{"http://litellm:4000///", "http://litellm:4000"},
		{"https://gw.example.com/litellm", "https://gw.example.com/litellm"},
		{"https://gw.example.com/litellm/", "https://gw.example.com/litellm"},
		{"https://gw.example.com/litellm/v1", "https://gw.example.com/litellm/v1"},
		{"https://[::1]:4000", "https://[::1]:4000"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			c := NewClient(tc.in, "sk-test", logr.Discard())
			if c.endpoint != tc.want {
				t.Fatalf("endpoint = %q, want %q", c.endpoint, tc.want)
			}
		})
	}
}

// TestRejectedError_TypeIsLiteLLMEnumOrEmpty — v0.7.3 follow-up to
// UAT LOW-02. RejectedError.Type is documented as carrying LiteLLM's
// closed-enum error.type field (auth_error, validation_error, …).
// processLitellmError uses the internal sentinel "unparsed" when the
// envelope cannot be deserialized; that sentinel must NOT leak into
// RejectedError.Type, otherwise the CR status.message reads
// `type=unparsed` which is operator state pretending to be LiteLLM
// state. Verified live in production v0.7.2 (uat-model-invalid case).
func TestRejectedError_TypeIsLiteLLMEnumOrEmpty(t *testing.T) {
	tcs := []struct {
		name     string
		status   int
		body     string
		wantType string
	}{
		{
			name:     "valid envelope with type → propagated",
			status:   422,
			body:     `{"error":{"message":"bad model","type":"validation_error","param":"model","code":"422"}}`,
			wantType: "validation_error",
		},
		{
			name:     "unparseable body → empty (NOT 'unparsed' sentinel)",
			status:   422,
			body:     `<html>500 internal server error</html>`,
			wantType: "",
		},
		{
			name:     "envelope shape with empty code+message → empty (sentinel branch)",
			status:   422,
			body:     `{"error":{"message":"","type":"","param":null,"code":""}}`,
			wantType: "",
		},
		{
			name:     "envelope with code but no type field → empty",
			status:   422,
			body:     `{"error":{"message":"bad","param":"x","code":"422"}}`,
			wantType: "",
		},
	}

	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			c := newTestClient(t, srv.URL)
			_, err := c.makeRequest(context.Background(), http.MethodPost, "/model/new", map[string]any{})
			if err == nil {
				t.Fatalf("expected error on %d", tc.status)
			}
			var rej *RejectedError
			if !errors.As(err, &rej) {
				t.Fatalf("expected *RejectedError, got %T: %v", err, err)
			}
			if rej.Type != tc.wantType {
				t.Fatalf("RejectedError.Type = %q, want %q", rej.Type, tc.wantType)
			}
		})
	}
}
