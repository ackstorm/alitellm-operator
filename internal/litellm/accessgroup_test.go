// SPDX-License-Identifier: Apache-2.0

package litellm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/ackstorm/alitellm-operator/internal/litellm/mock"
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

// putAccessGroupPartial issues a raw PUT with the given JSON body against
// the mock, bypassing litellm.Client.UpdateAccessGroup entirely.
//
// This is needed because AccessGroupUpdateRequest's three managed lists
// carry no `omitempty` (types.go) and UpdateAccessGroup unconditionally
// coerces nil -> []string{} via nonNilAccessGroupLists before marshaling
// (by design: "the reconciler is their sole writer and always sends the
// full computed set"). That means the typed client can NEVER omit one of
// these fields — every call it makes sends all three keys. Proving the
// mock's measured "an omitted field KEEPS the stored value" contract
// therefore requires a partial body the typed client structurally cannot
// produce.
func putAccessGroupPartial(t *testing.T, baseURL, id, body string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, baseURL+"/v1/access_group/"+id, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build partial PUT request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("partial PUT: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("partial PUT status = %d, want 200", resp.StatusCode)
	}
}

// TestAccessGroupMock_UpdateSemantics locks the mock to the semantics MEASURED
// against stock LiteLLM 1.93.0: omitted = KEEP, sent = REPLACE, [] = CLEAR.
// Every access-group envtest depends on the mock being faithful here.
//
// The KEEP/CLEAR-of-one-field cases are driven through a raw partial PUT
// (see putAccessGroupPartial) rather than through UpdateAccessGroup, because
// the typed client always sends all three managed lists (see that helper's
// doc comment) — this test also locks THAT client behavior explicitly in
// its final section, so the "the client can't actually omit" fact doesn't
// silently bit-rot.
func TestAccessGroupMock_UpdateSemantics(t *testing.T) {
	srv := mock.NewServer(t)
	c := newTestClient(t, srv.URL())

	ctx := context.Background()
	created, err := c.CreateAccessGroup(ctx, &AccessGroupCreateRequest{
		AccessGroupName:    "ag-semantics",
		AccessModelNames:   []string{"m1", "m2"},
		AccessMCPServerIDs: []string{"s1"},
		AccessAgentIDs:     []string{"a1"},
	})
	if err != nil {
		t.Fatalf("CreateAccessGroup: %v", err)
	}
	if created.AccessGroupID == "" {
		t.Fatal("CreateAccessGroup returned an empty access_group_id")
	}

	// REPLACE: send models only. MCP + agents must survive untouched (KEEP).
	putAccessGroupPartial(t, srv.URL(), created.AccessGroupID, `{"access_model_names":["m3"]}`)
	got, err := c.GetAccessGroupByName(ctx, "ag-semantics")
	if err != nil || got == nil {
		t.Fatalf("GetAccessGroupByName: got=%v err=%v", got, err)
	}
	if !slices.Equal(got.AccessModelNames, []string{"m3"}) {
		t.Errorf("AccessModelNames = %v, want [m3] (sent list must REPLACE)", got.AccessModelNames)
	}
	if !slices.Equal(got.AccessMCPServerIDs, []string{"s1"}) {
		t.Errorf("AccessMCPServerIDs = %v, want [s1] (omitted field must KEEP)", got.AccessMCPServerIDs)
	}
	if !slices.Equal(got.AccessAgentIDs, []string{"a1"}) {
		t.Errorf("AccessAgentIDs = %v, want [a1] (omitted field must KEEP)", got.AccessAgentIDs)
	}

	// CLEAR: an explicit empty slice must empty the list, not be dropped —
	// MCP stays omitted, so it must still KEEP.
	putAccessGroupPartial(t, srv.URL(), created.AccessGroupID, `{"access_model_names":[]}`)
	got, err = c.GetAccessGroupByName(ctx, "ag-semantics")
	if err != nil || got == nil {
		t.Fatalf("GetAccessGroupByName after clear: got=%v err=%v", got, err)
	}
	if len(got.AccessModelNames) != 0 {
		t.Errorf("AccessModelNames = %v, want empty ([] must CLEAR, not be omitted)", got.AccessModelNames)
	}
	if !slices.Equal(got.AccessMCPServerIDs, []string{"s1"}) {
		t.Errorf("AccessMCPServerIDs = %v, want [s1] still kept after a models-only clear", got.AccessMCPServerIDs)
	}

	// Lock what the REAL client actually produces: UpdateAccessGroup always
	// sends all three managed lists (nonNilAccessGroupLists coerces every nil
	// field to []string{} on every call, never omits), so a request literal
	// that only sets AccessModelNames still WIPES the other two — there is
	// no partial-update path through this client for these three fields by
	// design.
	if _, err := c.UpdateAccessGroup(ctx, created.AccessGroupID, &AccessGroupUpdateRequest{
		AccessModelNames: []string{"m4"},
	}); err != nil {
		t.Fatalf("UpdateAccessGroup: %v", err)
	}
	got, err = c.GetAccessGroupByName(ctx, "ag-semantics")
	if err != nil || got == nil {
		t.Fatalf("GetAccessGroupByName after client update: got=%v err=%v", got, err)
	}
	if !slices.Equal(got.AccessModelNames, []string{"m4"}) {
		t.Errorf("AccessModelNames = %v, want [m4]", got.AccessModelNames)
	}
	if len(got.AccessMCPServerIDs) != 0 {
		t.Errorf("AccessMCPServerIDs = %v, want empty — UpdateAccessGroup sends the full computed set "+
			"on every call and cannot omit a managed list", got.AccessMCPServerIDs)
	}
	if len(got.AccessAgentIDs) != 0 {
		t.Errorf("AccessAgentIDs = %v, want empty — UpdateAccessGroup sends the full computed set "+
			"on every call and cannot omit a managed list", got.AccessAgentIDs)
	}
}

// TestAccessGroupMock_DuplicateNameConflict locks the 409 the CREATE arm's
// name-adoption path depends on.
func TestAccessGroupMock_DuplicateNameConflict(t *testing.T) {
	srv := mock.NewServer(t)
	c := newTestClient(t, srv.URL())

	ctx := context.Background()
	req := &AccessGroupCreateRequest{AccessGroupName: "ag-dup"}
	if _, err := c.CreateAccessGroup(ctx, req); err != nil {
		t.Fatalf("first CreateAccessGroup: %v", err)
	}
	_, err := c.CreateAccessGroup(ctx, req)
	if err == nil {
		t.Fatal("second CreateAccessGroup with a duplicate name: got nil error, want 409")
	}
	var rej *RejectedError
	if !errors.As(err, &rej) {
		t.Fatalf("err = %v, want *RejectedError", err)
	}
	if rej.Status != http.StatusConflict {
		t.Errorf("duplicate-name status = %d, want 409 (%v)", rej.Status, err)
	}
}
