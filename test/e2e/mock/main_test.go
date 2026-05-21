// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestModelsAccept(t *testing.T) {
	s := &server{provider: "openai"}
	s.authMode.Set("accept")
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	s.recordAndAuth(s.handleModels)(rr, req)
	if rr.Code != 200 {
		t.Fatalf("want 200, got %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "gpt-4o-mini") {
		t.Fatalf("body missing gpt-4o-mini: %s", rr.Body.String())
	}
}

func TestModelsReject401(t *testing.T) {
	s := &server{provider: "openai"}
	s.authMode.Set("reject-401")
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	s.recordAndAuth(s.handleModels)(rr, req)
	if rr.Code != 401 {
		t.Fatalf("want 401, got %d", rr.Code)
	}
}

func TestModelsKubeai(t *testing.T) {
	s := &server{provider: "kubeai"}
	s.authMode.Set("accept")
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	s.recordAndAuth(s.handleModels)(rr, req)
	if rr.Code != 200 {
		t.Fatalf("want 200, got %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "qwen-2.5-3b") {
		t.Fatalf("body missing qwen-2.5-3b: %s", rr.Body.String())
	}
}

func TestCallsLog(t *testing.T) {
	s := &server{provider: "openai"}
	s.authMode.Set("accept")

	// Generate one /v1/models call.
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("X-Trace", "abc")
	s.recordAndAuth(s.handleModels)(rr, req)

	// Read call log via admin handler.
	rr2 := httptest.NewRecorder()
	s.handleCalls(rr2, httptest.NewRequest(http.MethodGet, "/__mock/calls", nil))
	if rr2.Code != 200 {
		t.Fatalf("want 200, got %d", rr2.Code)
	}
	var got []recordedCall
	if err := json.Unmarshal(rr2.Body.Bytes(), &got); err != nil {
		t.Fatalf("bad json: %v body=%s", err, rr2.Body.String())
	}
	if len(got) != 1 || got[0].Path != "/v1/models" || got[0].Method != http.MethodGet {
		t.Fatalf("unexpected callLog: %+v", got)
	}
	if v := got[0].Header["X-Trace"]; len(v) == 0 || v[0] != "abc" {
		t.Fatalf("X-Trace header not recorded: %+v", got[0].Header)
	}
}

func TestReset(t *testing.T) {
	s := &server{provider: "openai"}
	s.authMode.Set("accept")
	s.recordAndAuth(s.handleModels)(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/models", nil))

	rr := httptest.NewRecorder()
	s.handleReset(rr, httptest.NewRequest(http.MethodPost, "/__mock/reset", nil))
	if rr.Code != 204 {
		t.Fatalf("want 204, got %d", rr.Code)
	}
	if len(s.callLog) != 0 {
		t.Fatalf("callLog not cleared: %+v", s.callLog)
	}
}

func TestAuthModeFlip(t *testing.T) {
	s := &server{provider: "openai"}
	s.authMode.Set("accept")

	// Flip to reject-401 via admin.
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/__mock/auth-mode", strings.NewReader(`{"mode":"reject-401"}`))
	s.handleAuthMode(rr, req)
	if rr.Code != 204 {
		t.Fatalf("flip: want 204, got %d body=%s", rr.Code, rr.Body.String())
	}
	if got := s.authMode.Get(); got != "reject-401" {
		t.Fatalf("authMode not flipped: %s", got)
	}

	// Confirm subsequent call is rejected.
	rr2 := httptest.NewRecorder()
	s.recordAndAuth(s.handleModels)(rr2, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rr2.Code != 401 {
		t.Fatalf("post-flip: want 401, got %d", rr2.Code)
	}

	// Flip back via admin.
	rr3 := httptest.NewRecorder()
	s.handleAuthMode(rr3, httptest.NewRequest(http.MethodPost, "/__mock/auth-mode", strings.NewReader(`{"mode":"accept"}`)))
	if rr3.Code != 204 {
		t.Fatalf("flip back: want 204, got %d", rr3.Code)
	}
}

func TestAuthModeReject(t *testing.T) {
	s := &server{provider: "openai"}
	s.authMode.Set("accept")
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/__mock/auth-mode", strings.NewReader(`{"mode":"garbage"}`))
	s.handleAuthMode(rr, req)
	if rr.Code != 400 {
		t.Fatalf("want 400 for invalid mode, got %d", rr.Code)
	}
}
