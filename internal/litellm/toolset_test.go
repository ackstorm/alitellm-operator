// SPDX-License-Identifier: Apache-2.0

package litellm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The POST /v1/mcp/toolset body must carry toolset_name always, and tools as
// an explicit array. `description` is omitempty (LiteLLM accepts null).
func TestMCPToolsetRequest_Marshal(t *testing.T) {
	req := &MCPToolsetRequest{
		ToolsetName: "research-tools",
		Description: "curated research subset",
		Tools: []MCPToolsetTool{
			{ServerID: "hindsight", ToolName: "web_search"},
			{ServerID: "hindsight", ToolName: "fetch_page"},
		},
	}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["toolset_name"] != "research-tools" {
		t.Errorf("toolset_name = %v, want research-tools", got["toolset_name"])
	}
	tools, ok := got["tools"].([]any)
	if !ok || len(tools) != 2 {
		t.Fatalf("tools = %v, want 2 entries", got["tools"])
	}
	first, _ := tools[0].(map[string]any)
	if first["server_id"] != "hindsight" || first["tool_name"] != "web_search" {
		t.Errorf("tools[0] = %v", first)
	}
}

// ALWAYS-EMIT: an emptied tools list must serialize as `[]`, never be omitted
// and never `null`. LiteLLM's PUT replaces the field; an omitted field would
// keep the stale tool list (same merge hazard as team object_permission).
func TestMCPToolsetRequest_EmptyToolsSerializesAsArray(t *testing.T) {
	req := &MCPToolsetRequest{ToolsetName: "empty", Tools: []MCPToolsetTool{}}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !json.Valid(b) {
		t.Fatalf("invalid json: %s", b)
	}
	var got map[string]any
	_ = json.Unmarshal(b, &got)
	tools, present := got["tools"]
	if !present {
		t.Fatalf("tools key MUST be present (explicit clear), got %s", b)
	}
	arr, ok := tools.([]any)
	if !ok {
		t.Fatalf("tools must be a JSON array, got %T in %s", tools, b)
	}
	if len(arr) != 0 {
		t.Errorf("tools = %v, want []", arr)
	}
}

// The UPDATE body carries toolset_id in the BODY, not the path.
func TestMCPToolsetUpdateRequest_CarriesIDInBody(t *testing.T) {
	req := &MCPToolsetUpdateRequest{
		ToolsetID:   "6d071d99-39d2-44f9-8182-8917827b7c45",
		ToolsetName: "research-tools",
		Tools:       []MCPToolsetTool{},
	}
	b, _ := json.Marshal(req)
	var got map[string]any
	_ = json.Unmarshal(b, &got)
	if got["toolset_id"] != "6d071d99-39d2-44f9-8182-8917827b7c45" {
		t.Errorf("toolset_id missing from body: %s", b)
	}
}

func TestMCPToolsetEntry_Unmarshal(t *testing.T) {
	// Verbatim capture from LiteLLM 1.93.0 GET /v1/mcp/toolset.
	raw := `[{"toolset_id":"6d071d99-39d2-44f9-8182-8917827b7c45","toolset_name":"ts-mixed","description":null,"tools":[{"server_id":"probe-srv","tool_name":"some_tool"}],"created_at":"2026-07-27T09:43:52.144000Z","created_by":"default_user_id","updated_at":"2026-07-27T09:43:52.144000Z","updated_by":"default_user_id"}]`
	var arr []MCPToolsetEntry
	if err := json.Unmarshal([]byte(raw), &arr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(arr) != 1 {
		t.Fatalf("len = %d, want 1", len(arr))
	}
	if arr[0].ToolsetID != "6d071d99-39d2-44f9-8182-8917827b7c45" {
		t.Errorf("ToolsetID = %q", arr[0].ToolsetID)
	}
	if arr[0].ToolsetName != "ts-mixed" {
		t.Errorf("ToolsetName = %q", arr[0].ToolsetName)
	}
	if len(arr[0].Tools) != 1 || arr[0].Tools[0].ServerID != "probe-srv" {
		t.Errorf("Tools = %v", arr[0].Tools)
	}
}

func TestClient_CreateMCPToolset(t *testing.T) {
	var gotPath, gotMethod string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"toolset_id":"generated-uuid","toolset_name":"ts","tools":[]}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	out, err := c.CreateMCPToolset(context.Background(), &MCPToolsetRequest{
		ToolsetName: "ts",
		Tools:       []MCPToolsetTool{},
	})
	if err != nil {
		t.Fatalf("CreateMCPToolset: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/v1/mcp/toolset" {
		t.Errorf("got %s %s, want POST /v1/mcp/toolset", gotMethod, gotPath)
	}
	if out.ToolsetID != "generated-uuid" {
		t.Errorf("ToolsetID = %q, want generated-uuid", out.ToolsetID)
	}
}

// PUT sends the id in the body and hits the COLLECTION path. A path-style
// update (/v1/mcp/toolset/<id>) is a 405 on LiteLLM 1.93.0.
func TestClient_UpdateMCPToolset_IDInBodyNotPath(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"toolset_id":"the-id","toolset_name":"ts","tools":[]}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	if _, err := c.UpdateMCPToolset(context.Background(), &MCPToolsetUpdateRequest{
		ToolsetID: "the-id",
		Tools:     []MCPToolsetTool{},
	}); err != nil {
		t.Fatalf("UpdateMCPToolset: %v", err)
	}
	if gotPath != "/v1/mcp/toolset" {
		t.Errorf("path = %q, want /v1/mcp/toolset (id goes in the BODY)", gotPath)
	}
	if gotBody["toolset_id"] != "the-id" {
		t.Errorf("body toolset_id = %v, want the-id", gotBody["toolset_id"])
	}
}

func TestClient_UpdateMCPToolset_EmptyIDRejected(t *testing.T) {
	c := newTestClient(t, "http://unused")
	_, err := c.UpdateMCPToolset(context.Background(), &MCPToolsetUpdateRequest{ToolsetID: ""})
	if err == nil {
		t.Fatal("want error on empty toolset_id, got nil")
	}
}

func TestClient_DeleteMCPToolset_EscapesID(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	if err := c.DeleteMCPToolset(context.Background(), "a/b?c"); err != nil {
		t.Fatalf("DeleteMCPToolset: %v", err)
	}
	if gotPath != "/v1/mcp/toolset/a%2Fb%3Fc" {
		t.Errorf("path = %q; id must be PathEscaped so it cannot alter the route", gotPath)
	}
}

func TestClient_DeleteMCPToolset_EmptyIDRejected(t *testing.T) {
	c := newTestClient(t, "http://unused")
	if err := c.DeleteMCPToolset(context.Background(), ""); err == nil {
		t.Fatal("want error on empty toolset_id (would collapse to the collection route)")
	}
}

func TestClient_ListMCPToolsets_EmptyIsNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	_, err := c.ListMCPToolsets(context.Background())
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound on empty array (REL-05)", err)
	}
}
