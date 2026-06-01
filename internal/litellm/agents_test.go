// SPDX-License-Identifier: Apache-2.0

package litellm

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestUpdateAgent_EmptyIDGuard — M-SEC3: empty agent_id must error without
// issuing a request (would otherwise hit the collection route).
func TestUpdateAgent_EmptyIDGuard(t *testing.T) {
	c := newTestClient(t, "http://unused")
	if _, err := c.UpdateAgent(context.Background(), "", &AgentConfig{}); err == nil {
		t.Fatal("expected empty-id error, no request issued")
	}
}

// TestDeleteAgent_EmptyIDGuard — M-SEC3.
func TestDeleteAgent_EmptyIDGuard(t *testing.T) {
	c := newTestClient(t, "http://unused")
	if err := c.DeleteAgent(context.Background(), ""); err == nil {
		t.Fatal("expected empty-id error, no request issued")
	}
}

// TestDeleteAgent_EscapesID — M-SEC3: a `/` or `?` in the ID must reach the
// server as a single escaped path segment.
func TestDeleteAgent_EscapesID(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
	}))
	defer srv.Close()
	c := newTestClient(t, srv.URL)
	_ = c.DeleteAgent(context.Background(), "a/b?c")
	if gotPath != "/v1/agents/a%2Fb%3Fc" {
		t.Fatalf("id not escaped: %q", gotPath)
	}
}

// TestCreateAgentPath — POST /v1/agents.
func TestCreateAgentPath(t *testing.T) {
	var captured []capturedRequest
	srv := httptest.NewServer(captureMock(t, &captured, func(i int, w http.ResponseWriter) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"agent_id":"a1","agent_name":"x","agent_card_params":{}}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	_, err := c.CreateAgent(context.Background(), &AgentConfig{
		AgentName:       "x",
		AgentCardParams: map[string]any{"k": "v"},
	})
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	if len(captured) != 1 || captured[0].Method != http.MethodPost || captured[0].Path != "/v1/agents" {
		t.Errorf("CreateAgent: want POST /v1/agents, got %+v", captured)
	}
}

// TestUpdateAgentPath — PUT /v1/agents/{id} (NOT PATCH; §5.1 holds for
// agents empirically verified).
func TestUpdateAgentPath(t *testing.T) {
	var captured []capturedRequest
	srv := httptest.NewServer(captureMock(t, &captured, func(i int, w http.ResponseWriter) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"agent_id":"a1","agent_name":"x","agent_card_params":{}}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	_, err := c.UpdateAgent(context.Background(), "a1", &AgentConfig{
		AgentName:       "x",
		AgentCardParams: map[string]any{"k": "v"},
	})
	if err != nil {
		t.Fatalf("UpdateAgent: %v", err)
	}
	if len(captured) != 1 || captured[0].Method != http.MethodPut || captured[0].Path != "/v1/agents/a1" {
		t.Errorf("UpdateAgent: want PUT /v1/agents/a1, got %+v", captured)
	}
}

// TestDeleteAgentPath — DELETE /v1/agents/{id}.
func TestDeleteAgentPath(t *testing.T) {
	var captured []capturedRequest
	srv := httptest.NewServer(captureMock(t, &captured, func(i int, w http.ResponseWriter) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	if err := c.DeleteAgent(context.Background(), "a-zzz"); err != nil {
		t.Fatalf("DeleteAgent: %v", err)
	}
	if len(captured) != 1 || captured[0].Method != http.MethodDelete || captured[0].Path != "/v1/agents/a-zzz" {
		t.Errorf("DeleteAgent: want DELETE /v1/agents/a-zzz, got %+v", captured)
	}
}

// TestListAgentsLengthCheck — REL-05 on the bare-array list shape.
func TestListAgentsLengthCheck(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"empty_array", `[]`},
		{"null_response", `null`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(200)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			c := newTestClient(t, srv.URL)
			got, err := c.ListAgents(context.Background())
			if !errors.Is(err, ErrNotFound) {
				t.Errorf("err: want ErrNotFound, got %v", err)
			}
			if got != nil {
				t.Errorf("result: want nil on empty, got %+v", got)
			}
		})
	}
}

// TestListAgentsPath — path-string assertion incl. health_check=false.
func TestListAgentsPath(t *testing.T) {
	var captured []capturedRequest
	srv := httptest.NewServer(captureMock(t, &captured, func(i int, w http.ResponseWriter) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`[{"agent_id":"a1","agent_name":"x"}]`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	_, _ = c.ListAgents(context.Background())
	if len(captured) != 1 || captured[0].Method != http.MethodGet {
		t.Fatalf("ListAgents: want GET, got %+v", captured)
	}
	if !strings.HasPrefix(captured[0].Path, "/v1/agents") {
		t.Errorf("path: want prefix /v1/agents, got %q", captured[0].Path)
	}
	if !strings.Contains(captured[0].Path, "health_check=false") {
		t.Errorf("path: want health_check=false, got %q", captured[0].Path)
	}
}

// TestAgentsHelpers401Propagation — REL-06 propagation through agent helpers.
func TestAgentsHelpers401Propagation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		_, _ = w.Write([]byte(litellmAuth401Body))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	check := func(name string, err error) {
		t.Helper()
		var a *Auth401Error
		if !errors.As(err, &a) {
			t.Errorf("%s: want *Auth401Error, got %T: %v", name, err, err)
		}
	}

	_, err := c.CreateAgent(context.Background(), &AgentConfig{AgentName: "x", AgentCardParams: map[string]any{}})
	check("CreateAgent", err)
	_, err = c.UpdateAgent(context.Background(), "x", &AgentConfig{AgentName: "x", AgentCardParams: map[string]any{}})
	check("UpdateAgent", err)
	err = c.DeleteAgent(context.Background(), "x")
	check("DeleteAgent", err)
	_, err = c.ListAgents(context.Background())
	check("ListAgents", err)
}
