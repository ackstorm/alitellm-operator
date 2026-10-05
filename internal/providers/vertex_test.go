// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const vertexTestProject = "alt06-gemini"

// vertexTestKey returns a service-account JSON key with a fresh RSA key.
func vertexTestKey(t *testing.T) []byte {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return vertexKeyJSON(t, string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})))
}

func vertexKeyJSON(t *testing.T, privateKey string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]string{
		"type":         "service_account",
		"project_id":   vertexTestProject,
		"client_email": "discovery@alt06-gemini.iam.gserviceaccount.com",
		"private_key":  privateKey,
		"token_uri":    "https://attacker.invalid/token", // ignored by design
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// fakeTokenServer answers the JWT-bearer grant with access token "tok-123"
// after checking the assertion's issuer and scope claims.
func fakeTokenServer(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil || r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" {
			http.Error(w, `{"error":"unsupported_grant_type"}`, http.StatusBadRequest)
			return
		}
		parts := strings.Split(r.Form.Get("assertion"), ".")
		claims := map[string]any{}
		if len(parts) == 3 {
			raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
			_ = json.Unmarshal(raw, &claims)
		}
		if claims["iss"] != "discovery@alt06-gemini.iam.gserviceaccount.com" || claims["scope"] != vertexScope {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"tok-123","token_type":"Bearer","expires_in":3600}`))
	}))
	t.Cleanup(srv.Close)
	SetTestBaseURL(t, vertexTokenURLKey, srv.URL)
}

// newTestVertex builds a provider for location "eu".
func newTestVertex(t *testing.T, key []byte) Provider {
	t.Helper()
	p, err := newVertexImpl(context.Background(), ProviderConfig{
		Type: "vertex", Region: "eu", VertexCredentials: key, HTTPClient: http.DefaultClient,
	})
	if err != nil {
		t.Fatalf("newVertexImpl: %v", err)
	}
	return p
}

// TestVertex_List_TokenPaginationProjectHeader: the provider exchanges the
// key for a token, sends it with x-goog-user-project, follows
// nextPageToken, strips the publisher path from IDs, and stamps
// vertex_location / vertex_project on every candidate.
func TestVertex_List_TokenPaginationProjectHeader(t *testing.T) {
	fakeTokenServer(t)
	pages := map[string]string{
		"":   `{"publisherModels":[{"name":"publishers/google/models/gemini-3.8-flash","versionId":"default","launchStage":"GA"}],"nextPageToken":"p2"}`,
		"p2": `{"publisherModels":[{"name":"publishers/google/models/gemini-3.5-flash-lite"},{"name":"publishers/google/models/imagen-4.0-generate-001"},{"name":"publishers/google/models/gemini-embedding-001"}]}`,
	}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1beta1/publishers/google/models" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer tok-123" || r.Header.Get("x-goog-user-project") != vertexTestProject {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(pages[r.URL.Query().Get("pageToken")]))
	}))
	defer api.Close()
	SetTestBaseURL(t, "vertex", api.URL)

	cands, err := newTestVertex(t, vertexTestKey(t)).List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []string{"gemini-3.8-flash", "gemini-3.5-flash-lite"}
	if len(cands) != len(want) {
		t.Fatalf("got %+v; want IDs %v", cands, want)
	}
	for i, c := range cands {
		if c.ID != want[i] || c.Params["vertex_location"] != "eu" || c.Params["vertex_project"] != vertexTestProject {
			t.Errorf("cand[%d] = %+v; want ID=%s vertex_location=eu vertex_project=%s", i, c, want[i], vertexTestProject)
		}
	}
}

func TestVertex_BaseURLPerLocation(t *testing.T) {
	for loc, want := range map[string]string{
		"global":       "https://aiplatform.googleapis.com",
		"eu":           "https://aiplatform.eu.rep.googleapis.com",
		"us":           "https://aiplatform.us.rep.googleapis.com",
		"europe-west1": "https://europe-west1-aiplatform.googleapis.com",
	} {
		if got := vertexBaseURL(loc); got != want {
			t.Errorf("vertexBaseURL(%q) = %q; want %q", loc, got, want)
		}
	}
}

// TestVertex_ListStatusClassification: 401/403 → *ProviderAuthError without
// the body; 5xx → plain error.
func TestVertex_ListStatusClassification(t *testing.T) {
	fakeTokenServer(t)
	for _, tc := range []struct {
		status int
		auth   bool
	}{{401, true}, {403, true}, {500, false}, {503, false}} {
		api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(`{"error":{"message":"BODY-CANARY"}}`))
		}))
		SetTestBaseURL(t, "vertex", api.URL)
		_, err := newTestVertex(t, vertexTestKey(t)).List(context.Background())
		api.Close()
		var authErr *ProviderAuthError
		if err == nil || errors.As(err, &authErr) != tc.auth {
			t.Errorf("status %d: err=%v; want auth=%v", tc.status, err, tc.auth)
		}
		if err != nil && strings.Contains(err.Error(), "BODY-CANARY") {
			t.Errorf("status %d: body leaked into error: %v", tc.status, err)
		}
	}
}

// TestVertex_TokenRejected_AuthErrorNoBody: a token-endpoint rejection is an
// auth failure whose message carries neither the response body nor key bytes.
func TestVertex_TokenRejected_AuthErrorNoBody(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"BODY-CANARY"}`))
	}))
	defer tokenSrv.Close()
	SetTestBaseURL(t, vertexTokenURLKey, tokenSrv.URL)

	_, err := newTestVertex(t, vertexTestKey(t)).List(context.Background())
	var authErr *ProviderAuthError
	if !errors.As(err, &authErr) {
		t.Fatalf("want *ProviderAuthError; got %T %v", err, err)
	}
	if strings.Contains(err.Error(), "BODY-CANARY") {
		t.Errorf("token response body leaked: %v", err)
	}
}

// TestVertex_KeyCanary: malformed keys and an unparseable private_key are
// rejected by the constructor without echoing key material.
func TestVertex_KeyCanary(t *testing.T) {
	const canary = "KEY-CANARY-0123456789"
	bad := "-----BEGIN PRIVATE KEY-----\n" + canary + "\n-----END PRIVATE KEY-----\n"
	for name, key := range map[string][]byte{
		"not json":        []byte(`{"private_key":"` + canary + `"`),
		"missing fields":  []byte(`{"private_key":"` + canary + `"}`),
		"bad private_key": vertexKeyJSON(t, bad),
	} {
		_, err := newVertexImpl(context.Background(), ProviderConfig{
			Region: "eu", VertexCredentials: key, HTTPClient: http.DefaultClient,
		})
		if err == nil || strings.Contains(err.Error(), canary) {
			t.Errorf("%s: err=%v; want error without canary", name, err)
		}
	}
}

// TestVertex_LocationValidated: spec.region is spliced into a hostname, so
// anything but a plain location token is rejected up front.
func TestVertex_LocationValidated(t *testing.T) {
	for _, loc := range []string{"", "evil.example/x#", "EU", "eu.attacker.com"} {
		_, err := newVertexImpl(context.Background(), ProviderConfig{
			Region: loc, VertexCredentials: vertexTestKey(t), HTTPClient: http.DefaultClient,
		})
		if err == nil {
			t.Errorf("location %q: want error", loc)
		}
	}
}

// TestVertex_TokenEndpointDown_NotAuth: a token-endpoint outage is a plain
// (Unreachable) error, not an auth failure blaming the key.
func TestVertex_TokenEndpointDown_NotAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	SetTestBaseURL(t, vertexTokenURLKey, srv.URL)
	srv.Close()
	_, err := newTestVertex(t, vertexTestKey(t)).List(context.Background())
	var authErr *ProviderAuthError
	if err == nil || errors.As(err, &authErr) {
		t.Fatalf("want plain error; got %T %v", err, err)
	}
}
