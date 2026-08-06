// SPDX-License-Identifier: Apache-2.0

package litellm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestCreateAccessGroup_Accepts201AndSendsEmptyLists pins the two shapes that
// bit us elsewhere: LiteLLM answers POST with 201 (not 200), and the three
// managed lists must serialize as [] rather than being dropped.
func TestCreateAccessGroup_Accepts201AndSendsEmptyLists(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/access_group" || r.Method != http.MethodPost {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"access_group_id":"ag-1","access_group_name":"grp",
			"access_model_names":[],"access_mcp_server_ids":[],"access_agent_ids":[],
			"assigned_team_ids":[],"assigned_key_ids":[]}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	got, err := c.CreateAccessGroup(context.Background(), &AccessGroupCreateRequest{
		AccessGroupName:    "grp",
		AccessModelNames:   []string{},
		AccessMCPServerIDs: []string{},
		AccessAgentIDs:     []string{},
	})
	if err != nil {
		t.Fatalf("CreateAccessGroup: %v", err)
	}
	if got.AccessGroupID != "ag-1" {
		t.Errorf("access_group_id = %q, want ag-1", got.AccessGroupID)
	}
	for _, k := range []string{"access_model_names", "access_mcp_server_ids", "access_agent_ids"} {
		v, present := gotBody[k]
		if !present {
			t.Errorf("%s missing from body — omitempty would turn a clear into a keep", k)
			continue
		}
		if arr, ok := v.([]any); !ok || len(arr) != 0 {
			t.Errorf("%s = %#v, want []", k, v)
		}
	}
	if _, present := gotBody["assigned_team_ids"]; present {
		t.Error("assigned_team_ids must NOT be sent — the operator never writes that face")
	}
}

// TestGetAccessGroupByName_BareArrayAndMiss pins the bare-array list shape and
// the (nil, nil) miss contract the reconciler branches on.
func TestGetAccessGroupByName_BareArrayAndMiss(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"access_group_id":"ag-7","access_group_name":"wanted",
			"access_model_names":["m1"],"access_mcp_server_ids":[],"access_agent_ids":[],
			"assigned_team_ids":[],"assigned_key_ids":[]}]`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	hit, err := c.GetAccessGroupByName(context.Background(), "wanted")
	if err != nil {
		t.Fatalf("GetAccessGroupByName(hit): %v", err)
	}
	if hit == nil || hit.AccessGroupID != "ag-7" {
		t.Fatalf("hit = %#v, want ag-7", hit)
	}
	miss, err := c.GetAccessGroupByName(context.Background(), "absent")
	if err != nil {
		t.Fatalf("GetAccessGroupByName(miss): %v", err)
	}
	if miss != nil {
		t.Errorf("miss = %#v, want nil (the reconciler branches on nil to CREATE)", miss)
	}
}

// TestDeleteAccessGroup_RejectsEmptyID guards the collection-route collapse
// (M-SEC3, same posture as DeleteMCPToolset).
func TestDeleteAccessGroup_RejectsEmptyID(t *testing.T) {
	c := newTestClient(t, "http://127.0.0.1:1")
	if err := c.DeleteAccessGroup(context.Background(), ""); err == nil {
		t.Fatal("DeleteAccessGroup(\"\") = nil, want error — empty id collapses to the collection route")
	}
}
