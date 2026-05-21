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
