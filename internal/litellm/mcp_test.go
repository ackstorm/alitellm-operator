// SPDX-License-Identifier: Apache-2.0

package litellm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// pathV1MCPServer is the LiteLLM 1.83.10 MCP server collection endpoint
// asserted across this test file's CreateMCPServer / UpdateMCPServer cases.
const pathV1MCPServer = "/v1/mcp/server"

// TestDeleteMCPServer_EmptyIDGuard — M-SEC3: empty server_id must error
// without issuing a request (would otherwise hit the collection route).
func TestDeleteMCPServer_EmptyIDGuard(t *testing.T) {
	c := newTestClient(t, "http://unused")
	if err := c.DeleteMCPServer(context.Background(), ""); err == nil {
		t.Fatal("expected empty-id error, no request issued")
	}
}

// TestDeleteMCPServer_EscapesID — M-SEC3: a `/` or `?` in the ID must reach
// the server as a single escaped path segment.
func TestDeleteMCPServer_EscapesID(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
	}))
	defer srv.Close()
	c := newTestClient(t, srv.URL)
	_ = c.DeleteMCPServer(context.Background(), "a/b?c")
	if gotPath != "/v1/mcp/server/a%2Fb%3Fc" {
		t.Fatalf("id not escaped: %q", gotPath)
	}
}

// TestCreateMCPServerBodyShape1_83_10 — CR-10 / D-7.1-10 regression test.
//
// Asserts that CreateMCPServer produces a body whose top-level field names
// match the locked-down LiteLLM 1.83.10 schema from spec/litellm_api.json
// POST /v1/mcp/server.
//
// Diagnostic-first diff finding (Option A synthetic capture, 2026-05-19):
// - Working Probe 10c body (HTTP 201): {server_name, alias, transport, url, extra_headers}
// - Failing MCPServer/exa-mcp body: {server_name, url, transport, description} (no alias)
//
// Root cause: sending MCPServer create requests without "alias" triggers HTTP 400
// in LiteLLM 1.83.10 (alias required even though spec marks it optional).
// Fix: set Alias = ServerName in the MCPServerRequest when Alias is empty.
// This mirrors the 1.83.10-verified Probe 10c body shape.
//
// All fields are valid per spec/litellm_api.json NewMCPServerRequest schema.
func TestCreateMCPServerBodyShape1_83_10(t *testing.T) {
	var captured []capturedRequest
	srv := httptest.NewServer(captureMock(t, &captured, func(i int, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"server_id":"mcp-uuid","server_name":"exa-mcp","transport":"sse"}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	// Simulate what the controller builds for MCPServer/exa-mcp.
	// Alias is set to ServerName per the 1.83.10 fix (Probe 10c verified shape).
	req := &MCPServerRequest{
		ServerName:  "exa-mcp",
		Alias:       "exa-mcp", // NEW: alias = server_name per 1.83.10 fix
		URL:         "https://exa.ai/mcp/sse",
		Transport:   "sse",
		Description: "Exa search MCP",
	}
	_, err := c.CreateMCPServer(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateMCPServer: %v", err)
	}

	if len(captured) != 1 {
		t.Fatalf("captured: want 1 request, got %d", len(captured))
	}
	got := captured[0]
	if got.Method != http.MethodPost || got.Path != pathV1MCPServer {
		t.Errorf("CreateMCPServer: want POST /v1/mcp/server, got %s %s", got.Method, got.Path)
	}

	var bodyMap map[string]any
	if err := json.Unmarshal(got.Body, &bodyMap); err != nil {
		t.Fatalf("body unmarshal: %v", err)
	}

	// Verify expected keys per spec/litellm_api.json NewMCPServerRequest.
	for _, key := range []string{"server_name", "alias", "url", "transport", "description"} {
		if _, ok := bodyMap[key]; !ok {
			t.Errorf("body missing expected key %q", key)
		}
	}

	// Verify alias matches server_name (the 1.83.10 required pattern).
	if alias, _ := bodyMap["alias"].(string); alias != "exa-mcp" {
		t.Errorf("alias: want exa-mcp, got %v", bodyMap["alias"])
	}
	if name, _ := bodyMap["server_name"].(string); name != "exa-mcp" {
		t.Errorf("server_name: want exa-mcp, got %v", bodyMap["server_name"])
	}

	// Verify transport is a valid 1.83.10 enum value.
	if transport, _ := bodyMap["transport"].(string); transport != "sse" {
		t.Errorf("transport: want sse, got %v", bodyMap["transport"])
	}
}

// TestCreateMCPServerPathIsAdminImmediate — POST /v1/mcp/server. The
// admin-immediate path; the user-submission path is intentionally
// NOT used by this operator.
func TestCreateMCPServerPathIsAdminImmediate(t *testing.T) {
	var captured []capturedRequest
	srv := httptest.NewServer(captureMock(t, &captured, func(i int, w http.ResponseWriter) {
		w.WriteHeader(202) // LiteLLM 1.83.10 returns 202 on POST /v1/mcp/server
		_, _ = w.Write([]byte(`{"server_id":"mcp-1","transport":"sse"}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	_, err := c.CreateMCPServer(context.Background(), &MCPServerRequest{ServerName: "test"})
	if err != nil {
		t.Fatalf("CreateMCPServer: %v", err)
	}
	if len(captured) != 1 || captured[0].Method != http.MethodPost || captured[0].Path != pathV1MCPServer {
		t.Errorf("CreateMCPServer: want POST /v1/mcp/server (admin-immediate), got %+v", captured)
	}
}

// TestUpdateMCPServerUsesPUT — PUT /v1/mcp/server (§5.1 wholesale-replace).
func TestUpdateMCPServerUsesPUT(t *testing.T) {
	var captured []capturedRequest
	srv := httptest.NewServer(captureMock(t, &captured, func(i int, w http.ResponseWriter) {
		w.WriteHeader(202)
		_, _ = w.Write([]byte(`{"server_id":"mcp-1","transport":"sse"}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	_, err := c.UpdateMCPServer(context.Background(), &MCPServerUpdateRequest{ServerID: "mcp-1"})
	if err != nil {
		t.Fatalf("UpdateMCPServer: %v", err)
	}
	if len(captured) != 1 || captured[0].Method != http.MethodPut || captured[0].Path != pathV1MCPServer {
		t.Errorf("UpdateMCPServer: want PUT /v1/mcp/server, got %+v", captured)
	}
}

// TestDeleteMCPServerPath — DELETE /v1/mcp/server/{id}.
func TestDeleteMCPServerPath(t *testing.T) {
	var captured []capturedRequest
	srv := httptest.NewServer(captureMock(t, &captured, func(i int, w http.ResponseWriter) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	if err := c.DeleteMCPServer(context.Background(), "mcp-xyz"); err != nil {
		t.Fatalf("DeleteMCPServer: %v", err)
	}
	if len(captured) != 1 || captured[0].Method != http.MethodDelete || captured[0].Path != "/v1/mcp/server/mcp-xyz" {
		t.Errorf("DeleteMCPServer: want DELETE /v1/mcp/server/mcp-xyz, got %+v", captured)
	}
}

// TestListMCPServersLengthCheck — REL-05 on the bare-array list shape.
// LiteLLM returns a bare JSON array; the helper wraps into
// MCPServerListResponse{Data: .} for the length check.
func TestListMCPServersLengthCheck(t *testing.T) {
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
			got, err := c.ListMCPServers(context.Background())
			if !errors.Is(err, ErrNotFound) {
				t.Errorf("err: want ErrNotFound, got %v", err)
			}
			if got != nil {
				t.Errorf("result: want nil on empty, got %+v", got)
			}
		})
	}
}

// TestListMCPServersOK — happy path: non-empty array returns entries.
func TestListMCPServersOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`[{"server_id":"a","transport":"sse"},{"server_id":"b","transport":"sse"}]`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	got, err := c.ListMCPServers(context.Background())
	if err != nil {
		t.Fatalf("ListMCPServers: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("want 2 entries, got %d", len(got))
	}
}

// TestMCPHelpers401Propagation — REL-06 propagation through MCP helpers.
func TestMCPHelpers401Propagation(t *testing.T) {
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

	_, err := c.CreateMCPServer(context.Background(), &MCPServerRequest{ServerName: "x"})
	check("CreateMCPServer", err)
	_, err = c.UpdateMCPServer(context.Background(), &MCPServerUpdateRequest{ServerID: "x"})
	check("UpdateMCPServer", err)
	err = c.DeleteMCPServer(context.Background(), "x")
	check("DeleteMCPServer", err)
	_, err = c.ListMCPServers(context.Background())
	check("ListMCPServers", err)
}

// TestMCPServerRequest_FullJSONShape — FIX5 H-1 contract lock.
// Snapshot the JSON body shape with every modeled field populated. Locks
// the LiteLLM 1.83.10 wire contract so a struct-field rename or json tag
// drift surfaces here, not as a silent prod regression.
func TestMCPServerRequest_FullJSONShape(t *testing.T) {
	tr := true
	fa := false
	req := &MCPServerRequest{
		ServerName:                "srv",
		Alias:                     "srv",
		URL:                       "https://x",
		Transport:                 "http",
		Description:               "d",
		AuthType:                  "api_key",
		Credentials:               map[string]any{"api_key": "x"},
		MCPInfo:                   map[string]any{"env": "prod"},
		MCPAccessGroups:           []string{"g1"},
		AllowedTools:              []string{"t1"},
		ToolNameToDisplayName:     map[string]any{"t1": "T1"},
		ToolNameToDescription:     map[string]any{"t1": "doc"},
		ExtraHeaders:              []any{"x-litellm-api-key"},
		StaticHeaders:             map[string]any{"x-foo": "bar"},
		Command:                   "npx",
		Args:                      []string{"-y", "pkg"},
		Env:                       map[string]any{"TOKEN": "x"},
		AuthorizationURL:          "https://auth/authorize",
		TokenURL:                  "https://auth/token",
		RegistrationURL:           "https://auth/register",
		OAuth2Flow:                "authorization_code",
		AllowAllKeys:              &tr,
		AvailableOnPublicInternet: &fa,
		OAuthPassthrough:          &tr,
	}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(b)
	for _, want := range []string{
		`"server_name":"srv"`,
		`"transport":"http"`,
		`"auth_type":"api_key"`,
		`"credentials":{"api_key":"x"}`,
		`"mcp_info":{"env":"prod"}`,
		`"mcp_access_groups":["g1"]`,
		`"allowed_tools":["t1"]`,
		`"tool_name_to_display_name":{"t1":"T1"}`,
		`"tool_name_to_description":{"t1":"doc"}`,
		`"extra_headers":["x-litellm-api-key"]`,
		`"static_headers":{"x-foo":"bar"}`,
		`"command":"npx"`,
		`"args":["-y","pkg"]`,
		`"env":{"TOKEN":"x"}`,
		`"authorization_url":"https://auth/authorize"`,
		`"token_url":"https://auth/token"`,
		`"registration_url":"https://auth/register"`,
		`"oauth2_flow":"authorization_code"`,
		`"allow_all_keys":true`,
		`"available_on_public_internet":false`,
		`"oauth_passthrough":true`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("body missing %s\nfull body: %s", want, got)
		}
	}
}

// A nil OAuthPassthrough must not reach the wire at all. Sending an
// explicit false would clear the flag on every server the operator
// reconciles, so `omitempty` is load-bearing here.
func TestMCPServerRequest_OAuthPassthroughOmittedWhenNil(t *testing.T) {
	b, err := json.Marshal(MCPServerRequest{ServerName: "srv", Transport: "http"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), "oauth_passthrough") {
		t.Errorf("nil OAuthPassthrough leaked onto the wire: %s", b)
	}
}
