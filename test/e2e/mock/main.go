// SPDX-License-Identifier: Apache-2.0

// Package main is the Tier 2 mock provider (openai / kubeai compatible).
//
// Binary contract: see spec/tier2-test-env.md §7.
package main

import (
	"embed"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"sync"
)

//go:embed fixtures/*.json
var embeddedFixtures embed.FS

type server struct {
	provider    string
	authMode    atomicString
	fixturePath string
	mu          sync.Mutex
	callLog     []recordedCall
}

type recordedCall struct {
	Method string              `json:"method"`
	Path   string              `json:"path"`
	Header map[string][]string `json:"header"`
}

type atomicString struct {
	mu sync.RWMutex
	v  string
}

func (a *atomicString) Get() string  { a.mu.RLock(); defer a.mu.RUnlock(); return a.v }
func (a *atomicString) Set(s string) { a.mu.Lock(); defer a.mu.Unlock(); a.v = s }

func main() {
	provider := getenv("MOCK_PROVIDER", "openai")
	authMode := getenv("MOCK_AUTH_MODE", "accept")
	listen := getenv("MOCK_LISTEN_ADDR", ":8080")
	fixture := getenv("MOCK_FIXTURE_PATH", "")

	s := &server{provider: provider, fixturePath: fixture}
	s.authMode.Set(authMode)

	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/v1/models", s.recordAndAuth(s.handleModels))
	mux.HandleFunc("/v1/chat/completions", s.recordAndAuth(s.handleChatCompletions))
	mux.HandleFunc("/__mock/calls", s.handleCalls)
	mux.HandleFunc("/__mock/reset", s.handleReset)
	mux.HandleFunc("/__mock/auth-mode", s.handleAuthMode)

	log.Printf("[litellm-mock] provider=%s authMode=%s listen=%s", provider, authMode, listen)
	if err := http.ListenAndServe(listen, mux); err != nil {
		log.Fatal(err)
	}
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func (s *server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}

func (s *server) recordAndAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.callLog = append(s.callLog, recordedCall{Method: r.Method, Path: r.URL.Path, Header: r.Header})
		s.mu.Unlock()
		if s.authMode.Get() == "reject-401" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"mock 401"}`))
			return
		}
		next(w, r)
	}
}

func (s *server) handleModels(w http.ResponseWriter, _ *http.Request) {
	var blob []byte
	var err error
	if s.fixturePath != "" {
		blob, err = os.ReadFile(s.fixturePath)
	} else {
		blob, err = embeddedFixtures.ReadFile("fixtures/" + s.provider + "-models.json")
	}
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(blob)
}

// handleChatCompletions serves a canned OpenAI-shaped chat completion.
//
// Exists so the suite can assert the POSITIVE authorization path: that a
// team-scoped key granted a model can actually run inference through it.
// TEAM-05 covers only the denial path, which LiteLLM answers at its own auth
// layer without ever calling a provider — so without this handler nothing
// proves a grant WORKS, only that a non-grant fails.
//
// The reply echoes a fixed marker rather than anything from the request: the
// assertion under test is authorization, not model behaviour. Honors
// MOCK_AUTH_MODE via recordAndAuth like every other route.
func (s *server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model string `json:"model"`
	}
	if body, err := io.ReadAll(r.Body); err == nil {
		_ = json.Unmarshal(body, &req)
	}
	if req.Model == "" {
		req.Model = "mock-model"
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id":      "chatcmpl-mock",
		"object":  "chat.completion",
		"created": 0,
		"model":   req.Model,
		"choices": []map[string]any{{
			"index":         0,
			"finish_reason": "stop",
			"message": map[string]any{
				"role":    "assistant",
				"content": "E2E-MOCK-COMPLETION-OK",
			},
		}},
		"usage": map[string]any{
			"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2,
		},
	})
}

func (s *server) handleCalls(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.callLog)
}

func (s *server) handleReset(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	s.callLog = nil
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleAuthMode(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Mode string `json:"mode"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<10)).Decode(&body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	if body.Mode != "accept" && body.Mode != "reject-401" {
		http.Error(w, "mode must be accept|reject-401", http.StatusBadRequest)
		return
	}
	s.authMode.Set(body.Mode)
	w.WriteHeader(http.StatusNoContent)
}
