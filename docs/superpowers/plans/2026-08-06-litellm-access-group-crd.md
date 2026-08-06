# LiteLLMAccessGroup CRD Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a `LiteLLMAccessGroup` CRD that reconciles a named LiteLLM access group — a reusable bundle of model names, MCP servers, and A2A agents — against `/v1/access_group`, plus a `spec.permission.accessGroups` field on `LiteLLMTeam` that attaches groups to a team.

**Architecture:** The new reconciler owns ONLY the three resource dimensions of the access group (`access_model_names`, `access_mcp_server_ids`, `access_agent_ids`). It NEVER writes `assigned_team_ids` or `assigned_key_ids`. Team attachment is written from the opposite side, by the existing Team reconciler, onto `team.access_group_ids` via `POST /team/update`. This one-writer-per-surface split is what lets us skip the two-PUT delta-repair machinery that `../ach` needs (see Global Constraints).

**Tech Stack:** Go 1.26.4, controller-runtime v0.19.4, k8s.io/* v0.31.0, kubebuilder v4.4.0, Ginkgo v2 (e2e), envtest (controller tests).

## Global Constraints

- **LiteLLM floor: 1.93.0.** Already the repo's floor (`LiteLLMMCPToolset`), so this adds no new constraint. `/v1/access_group` verified present on stock `ghcr.io/berriai/litellm-database:v1.93.0`.
- **Every new `*.go` file starts with `// SPDX-License-Identifier: Apache-2.0`.** Pre-push gate 15 enforces this.
- **All output in English**: code, comments, docs, commit messages.
- **Never bare `kubectl`** against the e2e cluster — use `./scripts/dev.sh kubectl` or `$(E2E_KUBECTL)`. Host kubectl may silently address a production cluster.
- **Run `make` targets bare** (`make test-unit`), never prefixed with `./scripts/dev.sh` — they self-route into the devtools container.
- **Absolute paths for all Edit/Write operations.** Sibling repos with similar layouts live next to this one.
- **Docs in the SAME commit as the code.** No "docs follow-up PR later" (CLAUDE.md documentation-hygiene rule).
- **Measured wire semantics (2026-08-06, stock v1.93.0) — these drive the struct tags:**
  - `PUT /v1/access_group/{id}`: omitted field = KEEP, non-empty list = REPLACE, `[]` = CLEAR, `null` = CLEAR.
  - `POST /team/update`: identical — omitted = KEEP, non-empty = REPLACE, `[]` = CLEAR.
  - `POST /v1/access_group` returns **201**, not 200.
  - `GET /v1/access_group` returns a **bare JSON array**, not `{access_groups: [...]}`.
  - `access_group_id` is **server-minted**; a caller-supplied one is ignored. Adopt by name.
  - Writing `team.access_group_ids` does **NOT** propagate to `access_group.assigned_team_ids` (the group keeps reading `[]`). Do not assert on the group side to verify attachment.
  - Writing `assigned_team_ids` group-side writes the team mirror **only on an ENTER/LEAVE delta**; an idempotent re-PUT does not repair a broken mirror. **This is why we never write that field.**
  - An access group **BYPASSES** this repo's `models: ["__deny_all__"]` deny-by-default sentinel. A group grant overrides `team.models`. This must be documented, not "fixed".
- **Assert authorization by ERROR TYPE, never status code.** An authz denial echoes the sentinel string; an upstream error (`OpenAIException`, `429 No deployments available`) means authz already PASSED. On the e2e mock a correctly-authorized call can never return 200.

---

## File Structure

**New files:**

| Path | Responsibility |
|---|---|
| `internal/litellm/accessgroup.go` | HTTP client: create/list-by-name/update/delete against `/v1/access_group`. Pure transport, no reconcile logic. |
| `internal/litellm/accessgroup_test.go` | httptest-backed unit tests for the client, incl. the 201 and bare-array shapes. |
| `api/litellm/v1alpha1/accessgroup_types.go` | CRD Go types: `LiteLLMAccessGroup`, spec, status, list. |
| `internal/controller/accessgroup_controller.go` | Reconciler: resolve names→IDs, render, hash, create/adopt/update, delete. |
| `internal/controller/accessgroup_controller_test.go` | envtest coverage of the reconciler. |
| `docs/user-guide/access-group.md` | User-facing docs, incl. the deny-all bypass warning. |
| `examples/example-deploy/14-accessgroup.yaml` | Runnable sample CR. |
| `test/e2e/accessgroup_test.go` | Ginkgo e2e specs. |

**Modified files:**

| Path | Change |
|---|---|
| `internal/litellm/types.go` | Add the three access-group wire types. |
| `internal/litellm/mock/mock.go` | Add `/v1/access_group` routes + in-memory store. |
| `api/litellm/v1alpha1/team_types.go` | Add `PermissionSpec.AccessGroups`. |
| `internal/controller/team_permission.go` | Resolve group names→IDs; return them for the team body. |
| `internal/controller/team_controller.go` | Emit `access_group_ids` on `/team/update`; park on `AccessGroupNotFound`. |
| `internal/controller/litellmconnection_controller.go` | Add `reasonAccessGroupNotFound`. |
| `internal/controller/bootsweep.go` | Register `LiteLLMAccessGroupList`. |
| `internal/controller/connection_fanin.go` | Add `connectionToAccessGroups`. |
| `internal/controller/accessgroup_controller.go` | Add `ListAccessGroupRequests` (Task 5 appends it to the Task 4 file — the per-kind convention). |
| `internal/controller/suite_test.go` | Register the reconciler + a **gated** relist runnable. |
| `cmd/main.go` | Wire reconciler + `SafetyRelistRunnable`. |
| `config/crd/kustomization.yaml`, `config/rbac/role.yaml`, `deploy/helm/.../install.yaml`, `deploy/kustomize/manager-rbac.yaml` | Generated/regenerated manifests. |
| `docs/user-guide/index.md`, `docs/user-guide/team.md`, `docs/api-reference/litellm.ackstorm.ai.md` | Docs. |
| `CLAUDE.md` | New failure-mode entries + MANDATORY-read row. |
| `examples/example-deploy/kustomization.yaml` | Register the sample. |

---

## Task 1: LiteLLM access-group HTTP client

**Files:**
- Create: `internal/litellm/accessgroup.go`
- Create: `internal/litellm/accessgroup_test.go`
- Modify: `internal/litellm/types.go` (append at end of file)

**Interfaces:**
- Consumes: `Client.makeRequest(ctx, method, path, body)` from `internal/litellm/client.go`; `IsNotFound(err)` from `internal/litellm/errors.go`.
- Produces:
  - `type AccessGroupCreateRequest struct{ AccessGroupName string; Description string; AccessModelNames, AccessMCPServerIDs, AccessAgentIDs []string }`
  - `type AccessGroupUpdateRequest struct{ AccessGroupName *string; Description *string; AccessModelNames, AccessMCPServerIDs, AccessAgentIDs []string }`
  - `type AccessGroupEntry struct{ AccessGroupID, AccessGroupName, Description string; AccessModelNames, AccessMCPServerIDs, AccessAgentIDs, AssignedTeamIDs, AssignedKeyIDs []string }`
  - `func (c *Client) CreateAccessGroup(ctx, *AccessGroupCreateRequest) (*AccessGroupEntry, error)`
  - `func (c *Client) ListAccessGroups(ctx) ([]AccessGroupEntry, error)`
  - `func (c *Client) GetAccessGroupByName(ctx, name string) (*AccessGroupEntry, error)` — returns `(nil, nil)` when absent
  - `func (c *Client) UpdateAccessGroup(ctx, id string, *AccessGroupUpdateRequest) (*AccessGroupEntry, error)`
  - `func (c *Client) DeleteAccessGroup(ctx, id string) error`

- [ ] **Step 1: Add the wire types**

Append to `/home/coder/workspace/local/alitellm-operator/internal/litellm/types.go`:

```go
// ── Access group wire types (LiteLLM 1.93.0) ─────────────────────────────
//
// Endpoint shapes, all VERIFIED 2026-08-06 against stock
// ghcr.io/berriai/litellm-database:v1.93.0:
//
//	POST   /v1/access_group        body AccessGroupCreateRequest → 201 (NOT 200)
//	GET    /v1/access_group        → BARE array of AccessGroupEntry
//	PUT    /v1/access_group/{id}   body AccessGroupUpdateRequest
//	DELETE /v1/access_group/{id}
//
// /v1/unified_access_group is an exact alias (identical handlers and schemas);
// we use the shorter path. This family is DISJOINT from the legacy
// /access_group/{list,new} family, which is the per-resource model TAG
// namespace that model_controller.go writes via model_info.access_groups —
// a unified group does NOT appear in /access_group/list.
//
// UPDATE SEMANTICS (measured): an OMITTED field keeps the stored value, a
// non-empty list REPLACES wholesale, and `[]` CLEARS. `null` also clears.
// The three lists the operator manages therefore carry NO `omitempty`: the
// reconciler is their sole writer and always sends the full computed set, so
// every PUT is authoritative. `omitempty` would drop a zero-length slice and
// silently turn a "clear" into a "keep", wedging convergence. Callers MUST
// pass a non-nil `[]string{}` for the empty case.
//
// AssignedTeamIDs / AssignedKeyIDs are DELIBERATELY ABSENT from both request
// structs. The operator never writes them: team attachment is written from
// the team side (team.access_group_ids), which is the face LiteLLM actually
// enforces on. Writing the group side would make the operator a second writer
// of that relation and would drag in the delta-repair problem — LiteLLM only
// rewrites the team mirror on an ENTER/LEAVE delta, so an idempotent re-PUT
// cannot repair a broken mirror. Omitting the fields means KEEP, so a
// human-managed assignment survives our updates untouched.

// AccessGroupCreateRequest is the POST /v1/access_group body. Only
// access_group_name is required; the endpoint accepts an all-empty group.
type AccessGroupCreateRequest struct {
	AccessGroupName    string   `json:"access_group_name"`
	Description        string   `json:"description,omitempty"`
	AccessModelNames   []string `json:"access_model_names"`
	AccessMCPServerIDs []string `json:"access_mcp_server_ids"`
	AccessAgentIDs     []string `json:"access_agent_ids"`
}

// AccessGroupUpdateRequest is the PUT /v1/access_group/{id} body. Name and
// Description are pointers so an unset value is genuinely omitted (keep);
// the three managed lists are always serialized (see the note above).
type AccessGroupUpdateRequest struct {
	AccessGroupName    *string  `json:"access_group_name,omitempty"`
	Description        *string  `json:"description,omitempty"`
	AccessModelNames   []string `json:"access_model_names"`
	AccessMCPServerIDs []string `json:"access_mcp_server_ids"`
	AccessAgentIDs     []string `json:"access_agent_ids"`
}

// AccessGroupEntry is one row of GET /v1/access_group and the body returned
// by POST/PUT. AssignedTeamIDs is read-only for the operator and is NOT a
// reliable read of who is attached: a team-side write to team.access_group_ids
// does not propagate here (measured — the group keeps reading []).
type AccessGroupEntry struct {
	AccessGroupID      string   `json:"access_group_id"`
	AccessGroupName    string   `json:"access_group_name"`
	Description        string   `json:"description,omitempty"`
	AccessModelNames   []string `json:"access_model_names"`
	AccessMCPServerIDs []string `json:"access_mcp_server_ids"`
	AccessAgentIDs     []string `json:"access_agent_ids"`
	AssignedTeamIDs    []string `json:"assigned_team_ids"`
	AssignedKeyIDs     []string `json:"assigned_key_ids"`
}
```

- [ ] **Step 2: Write the failing client test**

Create `/home/coder/workspace/local/alitellm-operator/internal/litellm/accessgroup_test.go`:

```go
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
```

> `newTestClient(t, url)` is the existing helper used by the sibling client tests. Open `/home/coder/workspace/local/alitellm-operator/internal/litellm/toolset_test.go` and reuse whatever constructor it uses verbatim; if it builds the client inline rather than via a helper, copy that construction into a local `newTestClient` in this file.

- [ ] **Step 3: Run the test to verify it fails**

Run: `make test-unit-pkg PKG=./internal/litellm/...`
Expected: FAIL — `undefined: (*Client).CreateAccessGroup` (and the other three methods).

- [ ] **Step 4: Write the client**

Create `/home/coder/workspace/local/alitellm-operator/internal/litellm/accessgroup.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package litellm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
)

// CreateAccessGroup issues POST /v1/access_group and returns the created
// entry. LiteLLM MINTS access_group_id — a caller-supplied one is ignored
// (verified 1.93.0), so the caller must read it from the response, exactly
// like MCP toolset_id and A2A agent_id. The endpoint answers 201.
//
// The reconciler is expected to call GetAccessGroupByName first and only POST
// on a nil result: "already exists" semantics live at the controller layer.
func (c *Client) CreateAccessGroup(ctx context.Context, req *AccessGroupCreateRequest) (*AccessGroupEntry, error) {
	if req.AccessGroupName == "" {
		return nil, fmt.Errorf("litellm: CreateAccessGroup: empty access_group_name")
	}
	nonNilAccessGroupLists(&req.AccessModelNames, &req.AccessMCPServerIDs, &req.AccessAgentIDs)
	raw, err := c.makeRequest(ctx, "POST", "/v1/access_group", req)
	if err != nil {
		return nil, err
	}
	var out AccessGroupEntry
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("litellm: decode POST /v1/access_group: %w", err)
	}
	return &out, nil
}

// ListAccessGroups issues GET /v1/access_group. The response is a BARE array
// (the legacy /access_group/list is the one that wraps in an object).
func (c *Client) ListAccessGroups(ctx context.Context) ([]AccessGroupEntry, error) {
	raw, err := c.makeRequest(ctx, "GET", "/v1/access_group", nil)
	if err != nil {
		return nil, err
	}
	var out []AccessGroupEntry
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("litellm: decode GET /v1/access_group: %w", err)
	}
	return out, nil
}

// GetAccessGroupByName returns the entry whose access_group_name matches, or
// (nil, nil) when absent. Absence is NOT an error: the reconciler branches on
// nil to take the CREATE arm. access_group_id is server-minted and not
// derivable from metadata.name, so name lookup is the only adoption path.
func (c *Client) GetAccessGroupByName(ctx context.Context, name string) (*AccessGroupEntry, error) {
	if name == "" {
		return nil, fmt.Errorf("litellm: GetAccessGroupByName: empty name")
	}
	list, err := c.ListAccessGroups(ctx)
	if err != nil {
		if IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	for i := range list {
		if list[i].AccessGroupName == name {
			out := list[i]
			return &out, nil
		}
	}
	return nil, nil
}

// UpdateAccessGroup issues PUT /v1/access_group/{id}. The three managed lists
// are always sent (see the AccessGroupUpdateRequest doc): omitted means KEEP,
// so a nil slice would silently preserve a stale grant.
func (c *Client) UpdateAccessGroup(ctx context.Context, id string, req *AccessGroupUpdateRequest) (*AccessGroupEntry, error) {
	if id == "" {
		return nil, fmt.Errorf("litellm: UpdateAccessGroup: empty access_group_id")
	}
	nonNilAccessGroupLists(&req.AccessModelNames, &req.AccessMCPServerIDs, &req.AccessAgentIDs)
	raw, err := c.makeRequest(ctx, "PUT", "/v1/access_group/"+url.PathEscape(id), req)
	if err != nil {
		return nil, err
	}
	var out AccessGroupEntry
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("litellm: decode PUT /v1/access_group/%s: %w", id, err)
	}
	return &out, nil
}

// DeleteAccessGroup issues DELETE /v1/access_group/{id}. M-SEC3: guard the
// empty id (it would collapse to the collection route) and PathEscape so a
// `/`, `?`, or `#` cannot alter the path — mirrors DeleteMCPToolset.
func (c *Client) DeleteAccessGroup(ctx context.Context, id string) error {
	if id == "" {
		return fmt.Errorf("litellm: DeleteAccessGroup: empty access_group_id")
	}
	_, err := c.makeRequest(ctx, "DELETE", "/v1/access_group/"+url.PathEscape(id), nil)
	return err
}

// nonNilAccessGroupLists coerces nil slices to empty ones so encoding/json
// renders `[]` (an explicit LiteLLM clear) rather than `null`. Both clear on
// this endpoint, but `[]` is the shape we measured and assert on.
func nonNilAccessGroupLists(lists ...*[]string) {
	for _, l := range lists {
		if *l == nil {
			*l = []string{}
		}
	}
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `make test-unit-pkg PKG=./internal/litellm/...`
Expected: PASS, including the three new tests.

- [ ] **Step 6: Lint and commit**

```bash
make qa-lint-changed
cd /home/coder/workspace/local/alitellm-operator
git add internal/litellm/accessgroup.go internal/litellm/accessgroup_test.go internal/litellm/types.go
git commit -m "feat(litellm): add /v1/access_group HTTP client"
```

---

## Task 2: Mock server `/v1/access_group` routes

**Files:**
- Modify: `internal/litellm/mock/mock.go`

**Interfaces:**
- Consumes: nothing from Task 1 — see the note below.
- Produces: `func (m *MockServer) ResetAccessGroups()`; `func (m *MockServer) AccessGroups() []AccessGroupSnapshot`; `func (m *MockServer) SeedAccessGroup(name string) string`.

**IMPORTANT — the mock does NOT import `internal/litellm`.** Check the import
block at `internal/litellm/mock/mock.go:5-17`: it is stdlib-only. Do NOT reach
for `litellm.AccessGroupEntry` here; the mock declares its own private entry
types (see `toolsetEntry`) and marshals responses itself. Keep it that way.

**IMPORTANT — the mock has NO `http.ServeMux`.** All routing lives in two
places:
1. `func (m *MockServer) handle(w http.ResponseWriter, r *http.Request)`
   (line ~1025) — does accounting, then calls a series of
   `write*(w, r, mode) bool` helpers that need to set a NON-200 status code,
   then falls through to a per-mode switch that writes `200` + `statefulBody(r)`.
2. `func (m *MockServer) statefulBody(r *http.Request) []byte` (line ~1183) —
   one long if-chain keyed on `method` + `path`, returning only a BODY. The
   status code is already written by the time it runs, so **any route needing
   a code other than 200 must be a `write*` helper in `handle()`, not a
   `statefulBody` branch.** The toolset routes are split exactly this way
   (`writeDuplicateToolsetConflict` at line 960, `writeAbsentToolsetDelete500`
   at line 1001, called at lines 1116 and 1119).

Access groups need both halves because `POST` answers **201**, not 200.

Model the store on the existing toolset store (`toolsets` / `toolsetByName` /
`toolsetSeq` at `internal/litellm/mock/mock.go:240-248`) — same
server-minted-id + unique-name shape.

- [ ] **Step 1: Add the route constant, entry type, and store fields**

In `/home/coder/workspace/local/alitellm-operator/internal/litellm/mock/mock.go`,
beside `pathMCPToolset` (line ~81):

```go
// pathAccessGroup is the access-group COLLECTION route. Item operations
// (PUT/DELETE) append /{access_group_id}. LiteLLM answers POST with 201 and
// GET with a BARE array.
const pathAccessGroup = "/v1/access_group"
```

Beside `toolsetEntry` (grep for `type toolsetEntry`), add the mock-local entry
type. It deliberately mirrors the wire shape of `litellm.AccessGroupEntry`
without importing it:

```go
// accessGroupEntry is the mock's in-memory access group. Field names and
// JSON tags mirror LiteLLM 1.93.0's wire shape so the operator's client can
// decode this verbatim. Deliberately mock-local: the mock package is
// stdlib-only and must not import internal/litellm.
type accessGroupEntry struct {
	AccessGroupID      string   `json:"access_group_id"`
	AccessGroupName    string   `json:"access_group_name"`
	Description        string   `json:"description"`
	AccessModelNames   []string `json:"access_model_names"`
	AccessMCPServerIDs []string `json:"access_mcp_server_ids"`
	AccessAgentIDs     []string `json:"access_agent_ids"`
	AssignedTeamIDs    []string `json:"assigned_team_ids"`
	AssignedKeyIDs     []string `json:"assigned_key_ids"`
}
```

Beside the toolset store fields (line ~246):

```go
// Access group state. Mirrors the toolset shape: the server MINTS
// access_group_id (a caller-supplied one is ignored) and adoption goes
// through the unique access_group_name.
accessGroups      map[string]*accessGroupEntry // keyed by AccessGroupID
accessGroupByName map[string]string            // AccessGroupName → AccessGroupID
accessGroupSeq    atomic.Int64
```

In the constructor beside `toolsets: make(...)` (line ~326):

```go
accessGroups:      make(map[string]*accessGroupEntry),
accessGroupByName: make(map[string]string),
```

- [ ] **Step 2: Add the two status-code writers**

These handle every access-group response that is NOT 200. Add them beside
`writeAbsentToolsetDelete500` (line ~1001):

```go
// writeAccessGroupCreate serves POST /v1/access_group. It lives here rather
// than in statefulBody because the happy path writes StatusOK before
// statefulBody runs, and this endpoint answers 201 — VERIFIED against stock
// LiteLLM 1.93.0 on 2026-08-06. A duplicate access_group_name answers 409,
// which is how the operator's CREATE arm learns to adopt by name.
//
// Gated on happy mode so the fault modes (401 / transient 5xx / slow) still
// win, exactly like writeDuplicateToolsetConflict.
//
// Returns whether it wrote the response.
func (m *MockServer) writeAccessGroupCreate(w http.ResponseWriter, r *http.Request, mode string) bool {
	if mode != ModeHappy || r.Method != http.MethodPost || r.URL.Path != pathAccessGroup {
		return false
	}

	body, _ := io.ReadAll(r.Body)
	var req struct {
		AccessGroupName    string   `json:"access_group_name"`
		Description        string   `json:"description"`
		AccessModelNames   []string `json:"access_model_names"`
		AccessMCPServerIDs []string `json:"access_mcp_server_ids"`
		AccessAgentIDs     []string `json:"access_agent_ids"`
	}
	_ = json.Unmarshal(body, &req)

	m.mu.Lock()
	if _, dup := m.accessGroupByName[req.AccessGroupName]; dup {
		m.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(fmt.Sprintf(
			`{"detail":{"error":"An access group named '%s' already exists."}}`,
			req.AccessGroupName)))
		return true
	}

	// The server MINTS the id; a caller-supplied access_group_id is ignored.
	id := fmt.Sprintf("mock-access-group-id-%d", m.accessGroupSeq.Add(1))
	e := &accessGroupEntry{
		AccessGroupID:      id,
		AccessGroupName:    req.AccessGroupName,
		Description:        req.Description,
		AccessModelNames:   nonNilStrings(req.AccessModelNames),
		AccessMCPServerIDs: nonNilStrings(req.AccessMCPServerIDs),
		AccessAgentIDs:     nonNilStrings(req.AccessAgentIDs),
		AssignedTeamIDs:    []string{},
		AssignedKeyIDs:     []string{},
	}
	m.accessGroups[id] = e
	m.accessGroupByName[req.AccessGroupName] = id
	out, _ := json.Marshal(e)
	m.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write(out)
	return true
}

// writeAccessGroupDelete404 answers DELETE /v1/access_group/<id> with 404 when
// the group is already gone. Unlike the MCP toolset endpoint (which 500s on an
// absent row — see writeAbsentToolsetDelete500), the access-group endpoint
// behaves correctly, so the operator's confirmed-absent drain sees a clean 404.
//
// Returns whether it wrote the response.
func (m *MockServer) writeAccessGroupDelete404(w http.ResponseWriter, r *http.Request, mode string) bool {
	if mode != ModeHappy || r.Method != http.MethodDelete ||
		!strings.HasPrefix(r.URL.Path, pathAccessGroup+"/") {
		return false
	}
	id := strings.TrimPrefix(r.URL.Path, pathAccessGroup+"/")
	m.mu.Lock()
	_, present := m.accessGroups[id]
	m.mu.Unlock()
	if present {
		return false
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte(fmt.Sprintf(
		`{"detail":{"error":"Access group %s not found."}}`, id)))
	return true
}

// nonNilStrings coerces a nil slice to empty so the mock's JSON renders [],
// never null — matching what LiteLLM returns for these fields.
func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
```

Wire both into `handle()` immediately after the existing toolset writers
(lines 1116-1121), so the block reads:

```go
	if m.writeDuplicateToolsetConflict(w, r, mode) {
		return
	}
	if m.writeAbsentToolsetDelete500(w, r, mode) {
		return
	}
	if m.writeAccessGroupCreate(w, r, mode) {
		return
	}
	if m.writeAccessGroupDelete404(w, r, mode) {
		return
	}
```

- [ ] **Step 3: Add the GET / PUT / DELETE bodies to `statefulBody`**

These three all answer 200, so they belong in the if-chain. Add them beside the
toolset blocks (the toolset DELETE is at line ~1733, GET at ~1799). Note the
locking convention in that function: it takes `m.mu` inside each block and
releases it before returning, rather than holding it across the whole function.

```go
	// ── /v1/access_group ────────────────────────────────────────────────
	// Semantics MEASURED against stock LiteLLM 1.93.0 on 2026-08-06:
	//   - PUT  → an OMITTED field KEEPS the stored value, a sent list
	//            REPLACES wholesale, and [] (or null) CLEARS.
	//   - GET  → a BARE array, not {"access_groups": [...]}.
	// POST (201) and DELETE-of-an-absent-id (404) are handled by the
	// write* helpers in handle(), which can set a status code.
	//
	// assigned_team_ids / assigned_key_ids are stored but NEVER written by
	// these routes: the operator does not manage that face (a team-side
	// write does not propagate here), so they must survive a PUT untouched.

	// PUT /v1/access_group/<id> — id from the path.
	if method == http.MethodPut && strings.HasPrefix(path, pathAccessGroup+"/") {
		id := strings.TrimPrefix(path, pathAccessGroup+"/")
		// Decode into a raw map first so "field absent" (KEEP) is
		// distinguishable from "field sent as []" (CLEAR). That distinction
		// IS this endpoint's contract and is what the operator's struct tags
		// depend on — an omitempty on a managed list would silently turn a
		// clear into a keep.
		body, _ := io.ReadAll(r.Body)
		var raw map[string]json.RawMessage
		_ = json.Unmarshal(body, &raw)

		m.mu.Lock()
		e, ok := m.accessGroups[id]
		if !ok {
			m.mu.Unlock()
			return []byte(`{"detail":{"error":"Access group not found."}}`)
		}
		assign := func(key string, dst *[]string) {
			blob, present := raw[key]
			if !present {
				return // absent = KEEP
			}
			var v []string
			if err := json.Unmarshal(blob, &v); err != nil {
				return
			}
			*dst = nonNilStrings(v) // sent = REPLACE; [] and null both CLEAR
		}
		assign("access_model_names", &e.AccessModelNames)
		assign("access_mcp_server_ids", &e.AccessMCPServerIDs)
		assign("access_agent_ids", &e.AccessAgentIDs)
		if blob, present := raw["description"]; present {
			var d string
			if json.Unmarshal(blob, &d) == nil {
				e.Description = d
			}
		}
		if blob, present := raw["access_group_name"]; present {
			var name string
			if json.Unmarshal(blob, &name) == nil && name != "" && name != e.AccessGroupName {
				delete(m.accessGroupByName, e.AccessGroupName)
				e.AccessGroupName = name
				m.accessGroupByName[name] = id
			}
		}
		out, _ := json.Marshal(e)
		m.mu.Unlock()
		return out
	}

	// DELETE /v1/access_group/<id> — only reached when the group EXISTS
	// (writeAccessGroupDelete404 intercepts the absent case).
	if method == http.MethodDelete && strings.HasPrefix(path, pathAccessGroup+"/") {
		id := strings.TrimPrefix(path, pathAccessGroup+"/")
		m.mu.Lock()
		if e, ok := m.accessGroups[id]; ok {
			delete(m.accessGroupByName, e.AccessGroupName)
		}
		delete(m.accessGroups, id)
		m.mu.Unlock()
		return []byte(fmt.Sprintf(`{"access_group_id":%q,"deleted":true}`, id))
	}

	// GET /v1/access_group — BARE array of all groups.
	if method == http.MethodGet && strings.HasPrefix(path, pathAccessGroup) {
		m.mu.Lock()
		out := make([]*accessGroupEntry, 0, len(m.accessGroups))
		for _, e := range m.accessGroups {
			out = append(out, e)
		}
		m.mu.Unlock()
		sort.Slice(out, func(i, j int) bool {
			return out[i].AccessGroupName < out[j].AccessGroupName
		})
		blob, err := json.Marshal(out)
		if err != nil {
			return []byte(`[]`)
		}
		return blob
	}
```

**Ordering matters:** the `GET` block uses `strings.HasPrefix`, so it also
matches `GET /v1/access_group/<id>`. That is intentional and harmless (the
operator only ever GETs the collection), but it means the PUT and DELETE blocks
must come BEFORE it, as written above.

`sort` is NOT currently in the mock's import block — add it.

- [ ] **Step 4: Add the test helpers**

Beside `ResetToolsets` (line ~515):

```go
// ── Access group helpers ──────────────────────────────

// AccessGroupSnapshot is a read-only copy of one stored access group, handed
// to tests. Exported (unlike accessGroupEntry) because envtests assert on it.
type AccessGroupSnapshot struct {
	AccessGroupID      string
	AccessGroupName    string
	Description        string
	AccessModelNames   []string
	AccessMCPServerIDs []string
	AccessAgentIDs     []string
	AssignedTeamIDs    []string
}

// ResetAccessGroups clears the in-memory access-group store. Call between
// tests that need a clean slate for GET /v1/access_group responses.
func (m *MockServer) ResetAccessGroups() {
	m.mu.Lock()
	m.accessGroups = make(map[string]*accessGroupEntry)
	m.accessGroupByName = make(map[string]string)
	m.mu.Unlock()
}

// AccessGroups returns a name-sorted snapshot of the stored access groups.
func (m *MockServer) AccessGroups() []AccessGroupSnapshot {
	m.mu.Lock()
	out := make([]AccessGroupSnapshot, 0, len(m.accessGroups))
	for _, e := range m.accessGroups {
		out = append(out, AccessGroupSnapshot{
			AccessGroupID:      e.AccessGroupID,
			AccessGroupName:    e.AccessGroupName,
			Description:        e.Description,
			AccessModelNames:   append([]string{}, e.AccessModelNames...),
			AccessMCPServerIDs: append([]string{}, e.AccessMCPServerIDs...),
			AccessAgentIDs:     append([]string{}, e.AccessAgentIDs...),
			AssignedTeamIDs:    append([]string{}, e.AssignedTeamIDs...),
		})
	}
	m.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		return out[i].AccessGroupName < out[j].AccessGroupName
	})
	return out
}

// SeedAccessGroup inserts an access group into the mock's store as if it were
// created out-of-band (NOT via POST /v1/access_group through the operator).
// Returns the minted access_group_id. Used by the envtest that exercises the
// 409-adoption path, where a group with the CR's name already exists.
func (m *MockServer) SeedAccessGroup(name string) string {
	id := fmt.Sprintf("mock-access-group-id-%d", m.accessGroupSeq.Add(1))
	m.mu.Lock()
	m.accessGroups[id] = &accessGroupEntry{
		AccessGroupID:      id,
		AccessGroupName:    name,
		AccessModelNames:   []string{},
		AccessMCPServerIDs: []string{},
		AccessAgentIDs:     []string{},
		AssignedTeamIDs:    []string{},
		AssignedKeyIDs:     []string{},
	}
	m.accessGroupByName[name] = id
	m.mu.Unlock()
	return id
}

// DeleteAccessGroupOutOfBand removes an access group from the mock's store
// WITHOUT going through the HTTP handler. Simulates an out-of-band DELETE in
// LiteLLM, which the operator's vanish probe should detect.
func (m *MockServer) DeleteAccessGroupOutOfBand(id string) {
	m.mu.Lock()
	if e, ok := m.accessGroups[id]; ok {
		delete(m.accessGroupByName, e.AccessGroupName)
	}
	delete(m.accessGroups, id)
	m.mu.Unlock()
}
```

- [ ] **Step 5: Write a round-trip test proving the KEEP/REPLACE/CLEAR contract**

This is the whole reason the mock exists — if it gets these three cases wrong,
every downstream envtest is testing a fiction. Append to
`/home/coder/workspace/local/alitellm-operator/internal/litellm/accessgroup_test.go`
(created in Task 1), using the Task 1 client against a live mock server:

```go
// TestAccessGroupMock_UpdateSemantics locks the mock to the semantics MEASURED
// against stock LiteLLM 1.93.0: omitted = KEEP, sent = REPLACE, [] = CLEAR.
// Every access-group envtest depends on the mock being faithful here.
func TestAccessGroupMock_UpdateSemantics(t *testing.T) {
	srv := mock.NewMockServer(t)
	defer srv.Close()
	c := newTestClient(t, srv.URL())

	ctx := context.Background()
	created, err := c.CreateAccessGroup(ctx, &litellm.AccessGroupCreateRequest{
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
	if _, err := c.UpdateAccessGroup(ctx, created.AccessGroupID, &litellm.AccessGroupUpdateRequest{
		AccessModelNames: []string{"m3"},
	}); err != nil {
		t.Fatalf("UpdateAccessGroup (replace): %v", err)
	}
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

	// CLEAR: an explicit empty slice must empty the list, not be dropped.
	if _, err := c.UpdateAccessGroup(ctx, created.AccessGroupID, &litellm.AccessGroupUpdateRequest{
		AccessModelNames: []string{},
	}); err != nil {
		t.Fatalf("UpdateAccessGroup (clear): %v", err)
	}
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
}

// TestAccessGroupMock_DuplicateNameConflict locks the 409 the CREATE arm's
// name-adoption path depends on.
func TestAccessGroupMock_DuplicateNameConflict(t *testing.T) {
	srv := mock.NewMockServer(t)
	defer srv.Close()
	c := newTestClient(t, srv.URL())

	ctx := context.Background()
	req := &litellm.AccessGroupCreateRequest{AccessGroupName: "ag-dup"}
	if _, err := c.CreateAccessGroup(ctx, req); err != nil {
		t.Fatalf("first CreateAccessGroup: %v", err)
	}
	_, err := c.CreateAccessGroup(ctx, req)
	if err == nil {
		t.Fatal("second CreateAccessGroup with a duplicate name: got nil error, want 409")
	}
	if st := litellm.RejectedStatusOf(err); st != http.StatusConflict {
		t.Errorf("duplicate-name status = %d, want 409 (%v)", st, err)
	}
}
```

> `newTestClient` and `mock.NewMockServer` are whatever the sibling client
> tests already use — open
> `/home/coder/workspace/local/alitellm-operator/internal/litellm/toolset_test.go`
> and match its constructor calls exactly. If it builds the mock or client
> inline rather than via a helper, copy that construction.
>
> `litellm.RejectedStatusOf` is a placeholder for however this package already
> extracts an HTTP status from a returned error — grep for `RejectedError` in
> `internal/litellm/errors.go` and use the existing accessor or a direct
> `errors.As` on `*litellm.RejectedError`, matching what `toolset_test.go`
> does for its own 409 assertion.

- [ ] **Step 6: Run the tests**

Run: `make test-unit-pkg PKG=./internal/litellm/...`
Expected: PASS, including the two new tests.

- [ ] **Step 7: Commit**

```bash
cd /home/coder/workspace/local/alitellm-operator
git add internal/litellm/mock/mock.go internal/litellm/accessgroup_test.go
git commit -m "feat(mock): serve /v1/access_group with measured KEEP/REPLACE/CLEAR semantics"
```

---

## Task 3: `LiteLLMAccessGroup` CRD types

**Files:**
- Create: `api/litellm/v1alpha1/accessgroup_types.go`
- Modify: `api/litellm/v1alpha1/zz_generated.deepcopy.go` (generated)
- Modify: `config/crd/kustomization.yaml` (generated list)

**Interfaces:**
- Produces: `LiteLLMAccessGroup`, `LiteLLMAccessGroupList`, `AccessGroupSpec{Description string; Models, MCPServers, Agents []string; DeletionPolicy string}`, `AccessGroupStatus{ObservedGeneration int64; Conditions []metav1.Condition; LastRendered AccessGroupLastRenderedStatus}`, `AccessGroupLastRenderedStatus{Hash, AccessGroupID string; At *metav1.Time}`.

- [ ] **Step 1: Write the types**

Create `/home/coder/workspace/local/alitellm-operator/api/litellm/v1alpha1/accessgroup_types.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// AccessGroupSpec defines the desired state of a LiteLLM access group — a
// reusable bundle of models, MCP servers, and A2A agents that a team reaches
// through LiteLLMTeam.spec.permission.accessGroups.
//
// SCOPE: this CRD owns the three RESOURCE dimensions only. It never writes
// assigned_team_ids or assigned_key_ids. Team attachment is written from the
// team side (team.access_group_ids), which is the face LiteLLM enforces on;
// keeping a single writer per surface is what lets the operator skip
// delta-repair machinery entirely.
//
// SECURITY: access groups only ADD. A group grant OVERRIDES a team's
// deny-by-default sentinel (models: ["__deny_all__"]) — verified 2026-08-06 on
// LiteLLM 1.93.0. Granting a model here makes it reachable by every team that
// attaches this group, regardless of that team's own spec.permission.models.
type AccessGroupSpec struct {
	// Description is free text forwarded to LiteLLM's `description` field.
	//
	// +optional
	Description string `json:"description,omitempty"`

	// Models is the list of LiteLLM model NAMES this group grants. Forwarded
	// verbatim to access_model_names — LiteLLM matches on model_name, so no
	// resolution step is needed and no CR reference is required.
	//
	// +optional
	Models []string `json:"models,omitempty"`

	// MCPServers is the list of MCP server NAMES this group grants. Each name
	// is resolved to a server_id via GET /v1/mcp/server before projection,
	// because access_mcp_server_ids matches on ids and silently ignores names.
	// An unresolved name parks the CR Ready=False reason=MCPServerNotFound and
	// requeues (ordering dependency with LiteLLMMCPServer CRs — it self-heals
	// once the server exists).
	//
	// +optional
	MCPServers []string `json:"mcpServers,omitempty"`

	// Agents is the list of A2A agent NAMES this group grants. Each name is
	// resolved to an agent_id via GET /v1/agents, same reason and same
	// parking behaviour as MCPServers (reason=AgentNotFound).
	//
	// +optional
	Agents []string `json:"agents,omitempty"`

	// DeletionPolicy controls finalizer behavior when the LiteLLM-side DELETE
	// cannot be confirmed. Defaults to "Orphan" per REL-06 anti-storm.
	//
	// +kubebuilder:validation:Enum=Orphan;Delete
	// +kubebuilder:default=Orphan
	// +optional
	DeletionPolicy string `json:"deletionPolicy,omitempty"`
}

// AccessGroupStatus defines the observed state of a LiteLLMAccessGroup.
type AccessGroupStatus struct {
	// ObservedGeneration is the metadata.generation most recently processed.
	//
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions carries the standard metav1.Condition list. The single type
	// is `Ready`, with reasons:
	//   - Synced             — rendered group matches LiteLLM.
	//   - LiteLLMUnavailable — LiteLLMConnection/default not usable.
	//   - LiteLLMRejected    — LiteLLM returned a 4xx (non-401) on mutation.
	//   - MCPServerNotFound  — a spec.mcpServers name does not resolve yet.
	//   - AgentNotFound      — a spec.agents name does not resolve yet.
	//   - RecreateThrottled  — created-but-not-listed storm breaker tripped.
	//
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// LastRendered is the operator-side drift source of truth.
	//
	// +optional
	LastRendered AccessGroupLastRenderedStatus `json:"lastRendered,omitempty"`
}

// AccessGroupLastRenderedStatus records the rendered state last applied.
//
// AccessGroupID is server-minted: LiteLLM 1.93.0 IGNORES a caller-supplied
// access_group_id and mints a UUID (verified). Same posture as MCP toolset_id
// and A2A agent_id, unlike team_id / MCP server_id which the operator pins to
// metadata.name. Adoption of a pre-existing group therefore goes through the
// unique `access_group_name`, which is metadata.name.
type AccessGroupLastRenderedStatus struct {
	// Hash is the SHA-256 hex of the RFC 8785–canonicalized rendered body.
	//
	// +optional
	Hash string `json:"hash,omitempty"`

	// AccessGroupID is the LiteLLM-assigned UUID, read from the POST response
	// or re-resolved by name via GET /v1/access_group.
	//
	// +optional
	AccessGroupID string `json:"accessGroupID,omitempty"`

	// At is the timestamp of the last SUCCESSFUL render.
	//
	// +optional
	At *metav1.Time `json:"at,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=llag,categories=litellm
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=".status.conditions[?(@.type=='Ready')].reason"
// +kubebuilder:printcolumn:name="GroupID",type=string,JSONPath=".status.lastRendered.accessGroupID"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// LiteLLMAccessGroup is the Schema for the litellmaccessgroups API.
//
// metadata.name IS the LiteLLM `access_group_name` (unique server-side — a
// duplicate create returns 409), which is how the operator adopts a
// pre-existing group after a restart.
type LiteLLMAccessGroup struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AccessGroupSpec   `json:"spec,omitempty"`
	Status AccessGroupStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// LiteLLMAccessGroupList contains a list of LiteLLMAccessGroup.
type LiteLLMAccessGroupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []LiteLLMAccessGroup `json:"items"`
}

func init() {
	SchemeBuilder.Register(&LiteLLMAccessGroup{}, &LiteLLMAccessGroupList{})
}
```

- [ ] **Step 2: Generate deepcopy + CRD manifests**

Run:
```bash
cd /home/coder/workspace/local/alitellm-operator
make gen-code gen-manifests
```
Expected: `config/crd/bases/litellm.ackstorm.ai_litellmaccessgroups.yaml` created; `zz_generated.deepcopy.go` gains `LiteLLMAccessGroup` methods.

- [ ] **Step 3: Register the CRD in kustomize + Helm**

Add the generated filename to the `resources:` list in `/home/coder/workspace/local/alitellm-operator/config/crd/kustomization.yaml`, then sync the chart:

```bash
cd /home/coder/workspace/local/alitellm-operator
make helm-sync
```
Expected: `deploy/helm/alitellm-operator/crd-sources/litellm.ackstorm.ai_litellmaccessgroups.yaml` present and referenced from `templates/install.yaml`.

- [ ] **Step 4: Verify the tree is clean apart from intended files**

Run: `git status --short`
Expected: only the new CRD yaml, the kustomization edit, the chart files, `zz_generated.deepcopy.go`, and the new types file. **`config/manager/kustomization.yaml` must NOT be dirty** — if it is, a build recipe bypassed the `kustomize_pin_image` macro; revert that file and investigate before continuing.

- [ ] **Step 5: Regenerate the API reference**

The documentation-hygiene rule in `CLAUDE.md` requires docs to land in the SAME
commit as the contract change. A new CRD is a contract change, so regenerate
now — do not defer it to Task 7.

Run: `make gen-crd-ref-docs`
Expected: `docs/api-reference/litellm.ackstorm.ai.md` gains a `LiteLLMAccessGroup`
section. Confirm with `git diff --stat docs/api-reference/`.

- [ ] **Step 6: Commit**

```bash
cd /home/coder/workspace/local/alitellm-operator
git add api/ config/ deploy/helm/ docs/api-reference/
git commit -m "feat(api): add LiteLLMAccessGroup CRD types"
```

---

## Task 4: AccessGroup reconciler

**Files:**
- Create: `internal/controller/accessgroup_controller.go`
- Create: `internal/controller/accessgroup_controller_test.go`

**Interfaces:**
- Consumes: Task 1's client methods; Task 3's types; from `internal/controller/shared_helpers.go` — `newAckMissingFn`, `classifyMutationError`, `writeStatusWithRetry`, `SafetyRelistRunnable`; from `internal/connection` — `ConnectionSnapshot.Usable()`.
- Produces: `type AccessGroupReconciler struct{...}` with `SetupWithManager(mgr ctrl.Manager, relistCh <-chan reconcile.Request) error`; `func ListAccessGroupRequests(ctx context.Context, c client.Client, ns string) ([]reconcile.Request, error)`.

**Structural template:** `/home/coder/workspace/local/alitellm-operator/internal/controller/mcptoolset_controller.go` is the closest sibling — server-minted id, name-based adoption, vanish probe, churn breaker, no secrets, no free-form params. Follow its reconcile-step ORDER (Step 1/2a/2b/3/11/12), substituting `LiteLLMMCPToolset`→`LiteLLMAccessGroup`, `ToolsetID`→`AccessGroupID`, `mcpToolsetFinalizer`→`accessGroupFinalizer`, and the four client calls. The steps below give the parts that genuinely differ.

**Do NOT copy logic that already lives in `internal/controller/shared_helpers.go`.** This repo consolidated the cross-controller behavior there specifically to stop six copy-pasted versions from drifting (`CLAUDE.md`, "Cross-controller shared helpers"). Call the existing helpers rather than re-implementing them:

| Behavior | Use this, do not re-derive |
|---|---|
| Typed 4xx classification | `is4xxStatus` / `rejectedStatus` |
| Deletion-path ack-missing gate | `newAckMissingFn` |
| LiteLLM-mutation error classification | `classifyMutationError` (bind your CR with a thin `writeStatus` closure, as the siblings do) |
| Optimistic-locked status write | `writeStatusWithRetry[T]` |
| Periodic drift relist | `SafetyRelistRunnable` |

What legitimately gets written fresh per kind: the reconcile step order itself, the field-selector index constant, `Index*SecretRefs` (n/a here — no secrets), the render/hash functions, and the domain-specific client calls. If while implementing you find yourself copying a block of five or more lines from `mcptoolset_controller.go` that is NOT one of those, stop and check whether the block belongs in `shared_helpers.go` instead; report it in your task report rather than duplicating it silently.

- [ ] **Step 1: Write the failing render + resolve test**

Create `/home/coder/workspace/local/alitellm-operator/internal/controller/accessgroup_controller_test.go` with the pure-logic half first:

```go
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"testing"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
)

// TestRenderAccessGroup_ResolvesNamesAndKeepsModelsVerbatim pins the three
// dimensions' differing treatment: models pass through, MCP servers and agents
// are resolved name→id because LiteLLM matches those on ids and silently
// ignores names.
func TestRenderAccessGroup_ResolvesNamesAndKeepsModelsVerbatim(t *testing.T) {
	spec := litellmv1alpha1.AccessGroupSpec{
		Models:     []string{"gpt-4", "claude-opus"},
		MCPServers: []string{"slack"},
		Agents:     []string{"finops"},
	}
	serverIDs := map[string]string{"slack": "srv-1"}
	agentIDs := map[string]string{"finops": "agt-1"}

	got, missing := renderAccessGroup(spec, serverIDs, agentIDs)
	if len(missing.MCPServers)+len(missing.Agents) != 0 {
		t.Fatalf("unexpected missing: %+v", missing)
	}
	if len(got.Models) != 2 || got.Models[0] != "gpt-4" {
		t.Errorf("Models = %v, want verbatim passthrough", got.Models)
	}
	if len(got.MCPServerIDs) != 1 || got.MCPServerIDs[0] != "srv-1" {
		t.Errorf("MCPServerIDs = %v, want [srv-1]", got.MCPServerIDs)
	}
	if len(got.AgentIDs) != 1 || got.AgentIDs[0] != "agt-1" {
		t.Errorf("AgentIDs = %v, want [agt-1]", got.AgentIDs)
	}
}

// TestRenderAccessGroup_EmptySpecRendersNonNilLists guards the CLEAR contract:
// nil slices would serialize as null/absent and KEEP a stale grant upstream.
func TestRenderAccessGroup_EmptySpecRendersNonNilLists(t *testing.T) {
	got, _ := renderAccessGroup(litellmv1alpha1.AccessGroupSpec{}, nil, nil)
	if got.Models == nil || got.MCPServerIDs == nil || got.AgentIDs == nil {
		t.Fatalf("nil slice in %+v — an omitted list KEEPS the stale value upstream", got)
	}
}

// TestRenderAccessGroup_ReportsUnresolvedNames drives the parking path: an
// unresolved name must be reported, never silently dropped (a dropped name is
// a silent authorization gap).
func TestRenderAccessGroup_ReportsUnresolvedNames(t *testing.T) {
	spec := litellmv1alpha1.AccessGroupSpec{
		MCPServers: []string{"slack", "ghost"},
		Agents:     []string{"nobody"},
	}
	_, missing := renderAccessGroup(spec, map[string]string{"slack": "srv-1"}, nil)
	if len(missing.MCPServers) != 1 || missing.MCPServers[0] != "ghost" {
		t.Errorf("missing.MCPServers = %v, want [ghost]", missing.MCPServers)
	}
	if len(missing.Agents) != 1 || missing.Agents[0] != "nobody" {
		t.Errorf("missing.Agents = %v, want [nobody]", missing.Agents)
	}
}

// TestAccessGroupHash_StableAcrossDeclarationOrder guards the steady-state
// short-circuit: a reordered spec must not look like drift and trigger a PUT.
func TestAccessGroupHash_StableAcrossDeclarationOrder(t *testing.T) {
	a, _ := renderAccessGroup(litellmv1alpha1.AccessGroupSpec{
		Models: []string{"m1", "m2"}}, nil, nil)
	b, _ := renderAccessGroup(litellmv1alpha1.AccessGroupSpec{
		Models: []string{"m2", "m1"}}, nil, nil)
	if accessGroupHash(a) != accessGroupHash(b) {
		t.Error("hash differs on declaration order — every reconcile would PUT")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `make test-unit-pkg PKG=./internal/controller/...`
Expected: FAIL — `undefined: renderAccessGroup`, `undefined: accessGroupHash`.

- [ ] **Step 3: Write the render + hash helpers**

Create `/home/coder/workspace/local/alitellm-operator/internal/controller/accessgroup_controller.go` starting with the pure half:

```go
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
)

// accessGroupFinalizer is the finalizer managed by the LiteLLMAccessGroup
// reconciler. DELETE /v1/access_group/<id> removes the group before the CR
// leaves etcd.
const accessGroupFinalizer = "accessgroups.litellm.ackstorm.ai/finalizer"

// accessGroupKind is the metric label for LiteLLMAccessGroup CRs.
const accessGroupKind = "LiteLLMAccessGroup"

// renderedAccessGroup is the resolved, order-normalized projection of an
// AccessGroupSpec. All three slices are guaranteed NON-NIL: they serialize as
// `[]` (an explicit LiteLLM clear), never `null`/absent, which upstream reads
// as "keep the stale value".
type renderedAccessGroup struct {
	Models       []string `json:"access_model_names"`
	MCPServerIDs []string `json:"access_mcp_server_ids"`
	AgentIDs     []string `json:"access_agent_ids"`
}

// missingAccessGroupRefs collects spec names with no upstream id.
type missingAccessGroupRefs struct {
	MCPServers []string
	Agents     []string
}

// renderAccessGroup resolves a spec into the LiteLLM projection.
//
// Models pass through VERBATIM — LiteLLM matches access_model_names on
// model_name. MCP servers and agents are resolved name→id because those two
// dimensions match on ids and SILENTLY IGNORE names (same trap as team
// object_permission.agents). An unresolved name is reported, never dropped:
// dropping it would silently narrow a permission object with no signal.
//
// Every slice is sorted so the hash is order-independent and a reordered spec
// does not read as drift.
func renderAccessGroup(
	spec litellmv1alpha1.AccessGroupSpec,
	serverIDs, agentIDs map[string]string,
) (renderedAccessGroup, missingAccessGroupRefs) {
	var missing missingAccessGroupRefs

	models := append([]string{}, spec.Models...)
	sort.Strings(models)

	servers := make([]string, 0, len(spec.MCPServers))
	for _, name := range spec.MCPServers {
		id, ok := serverIDs[name]
		if !ok {
			missing.MCPServers = append(missing.MCPServers, name)
			continue
		}
		servers = append(servers, id)
	}
	sort.Strings(servers)

	agents := make([]string, 0, len(spec.Agents))
	for _, name := range spec.Agents {
		id, ok := agentIDs[name]
		if !ok {
			missing.Agents = append(missing.Agents, name)
			continue
		}
		agents = append(agents, id)
	}
	sort.Strings(agents)

	return renderedAccessGroup{
		Models:       models,
		MCPServerIDs: servers,
		AgentIDs:     agents,
	}, missing
}

// accessGroupHash is the SHA-256 hex of the rendered projection. Feeds
// status.lastRendered.hash and the steady-state short-circuit.
func accessGroupHash(r renderedAccessGroup) string {
	blob, err := json.Marshal(r)
	if err != nil {
		// Marshaling three []string cannot fail; fall back to a value that
		// never compares equal so a bug forces a re-render rather than a
		// spurious steady state.
		return fmt.Sprintf("unmarshalable-%v", err)
	}
	sum := sha256.Sum256(blob)
	return fmt.Sprintf("%x", sum)
}
```

- [ ] **Step 4: Run to verify the pure tests pass**

Run: `make test-unit-pkg PKG=./internal/controller/...`
Expected: PASS for the four new tests.

- [ ] **Step 5: Commit the pure half**

```bash
cd /home/coder/workspace/local/alitellm-operator
git add internal/controller/accessgroup_controller.go internal/controller/accessgroup_controller_test.go
git commit -m "feat(controller): add access-group render + hash helpers"
```

- [ ] **Step 6: Add the reconciler struct and Reconcile**

Append to `accessgroup_controller.go`. Copy the imports and the Step 1/2a/2b/3/11/12 skeleton from `mcptoolset_controller.go` with the substitutions listed above, then use these for the parts that differ:

```go
// +kubebuilder:rbac:groups=litellm.ackstorm.ai,resources=litellmaccessgroups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=litellm.ackstorm.ai,resources=litellmaccessgroups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=litellm.ackstorm.ai,resources=litellmaccessgroups/finalizers,verbs=update

// Events RBAC marker inheritance: the package-wide events marker lives on
// internal/controller/mcpserver_controller.go. kubebuilder marker scope is
// per-package — this reconciler INHERITS the grant and MUST NOT duplicate it.
```

Reconcile ordering (documented at the top of the method, mirroring the sibling):

```
Step 1:  Fetch the CR (NotFound → nil).
Step 2a: DeletionTimestamp → DELETE /v1/access_group/<id> (name-resolve
         fallback when AccessGroupID is empty) → RemoveFinalizer.
Step 2b: Finalizer absent → AddFinalizer → return.
Step 3:  Gate on snap.Usable() — NOT snap.Ready (issue #74: a Ready snapshot
         with a nil Client is reachable and nil-derefs).
Step 4:  Resolve spec.mcpServers → server_id via ListMCPServers, and
         spec.agents → agent_id via ListA2AAgents.
Step 5:  renderAccessGroup. Non-empty `missing` → park Ready=False with
         reason=MCPServerNotFound / AgentNotFound and return WITHOUT requeue
         (the SafetyRelistRunnable re-drives it; see Task 5).
Step 6:  accessGroupHash over the render.
Step 7:  Existence probe by NAME against GET /v1/access_group (vanish
         detection). Resolve by PRESENCE of the tracked id in the full set,
         not by the first row.
Step 8:  Steady-state short-circuit — INCLUDING the Ready-condition heal
         (issue #102; mandatory in every steady-state short-circuit).
Step 9:  Branch CREATE (POST, id read from the RESPONSE, 409 → adopt by name)
         vs UPDATE (PUT with the id in the PATH).
Step 10: Classify mutation errors per §7.7.
Step 11: Update status (LastRendered + Ready=Synced).
```

The steady-state block MUST carry the heal — a bare `return` here is the #102 bug that leaves a CR pinned `Ready=False` forever:

```go
// Step 8: steady state. The Ready-condition heal is NOT optional: the Step 3
// connection gate stamps observedGeneration alongside Ready=False and leaves
// lastRendered intact, so once the connection recovers all three predicates
// below hold and the reconciler would never re-enter the branch where the
// terminal Ready=True write lives. Self-perpetuating without this (#102).
if persistedID != "" && ag.Status.LastRendered.Hash == currentHash &&
	ag.Status.ObservedGeneration == ag.Generation {
	if ready := apimeta.FindStatusCondition(ag.Status.Conditions, conditionTypeReady); ready == nil ||
		ready.Status != metav1.ConditionTrue || ready.Reason != reasonSynced {
		if err := r.writeStatus(ctx, &ag, metav1.ConditionTrue, reasonSynced, "access group registered"); err != nil {
			if apierrors.IsConflict(err) {
				return ctrl.Result{}, nil
			}
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}
```

The CREATE arm's 409 adoption:

```go
// Step 9 CREATE arm. access_group_id is SERVER-MINTED (a supplied one is
// ignored), so the id comes from the response. A duplicate access_group_name
// returns 409 — adopt the existing group by name rather than parking, which
// is how the operator re-attaches after a restart that lost status.
created, err := snap.Client.CreateAccessGroup(ctx, &litellm.AccessGroupCreateRequest{
	AccessGroupName:    ag.Name,
	Description:        ag.Spec.Description,
	AccessModelNames:   rendered.Models,
	AccessMCPServerIDs: rendered.MCPServerIDs,
	AccessAgentIDs:     rendered.AgentIDs,
})
if err != nil {
	if rejectedStatus(err) == http.StatusConflict {
		existing, rErr := snap.Client.GetAccessGroupByName(ctx, ag.Name)
		if rErr != nil {
			return r.classifyMutationError(ctx, &ag, rErr)
		}
		if existing != nil {
			newID = existing.AccessGroupID
			goto adopted
		}
	}
	return r.classifyMutationError(ctx, &ag, err)
}
newID = created.AccessGroupID
```

> Replace the `goto adopted` sketch with the sibling's actual control flow — `mcptoolset_controller.go`'s CREATE arm already implements exactly this 409-adopt shape; mirror its structure rather than introducing a label.

- [ ] **Step 7: Write the envtest coverage**

Append to `accessgroup_controller_test.go`. First write these local helpers, mirroring the toolset ones in `internal/controller/mcptoolset_controller_test.go` — `toolsetSampleCR` (line 28), `ensureNoToolset` (line 40), `pollToolsetCondition` (line 64), `resetMockToolset` (line 81), `setupReadyConnectionToolset` (line 89). Open that file and copy their bodies, substituting the access-group kind and client calls:

```go
func resetMockAccessGroup()                                    { mockServer.ResetAccessGroups() }
func ensureNoAccessGroup(t *testing.T, ctx context.Context, name string) { /* delete the CR if present, wait for absence */ }
func pollAccessGroupCondition(t *testing.T, ctx context.Context, name, reason string) *litellmv1alpha1.LiteLLMAccessGroup
func accessGroupSampleCR(name string, spec litellmv1alpha1.AccessGroupSpec) *litellmv1alpha1.LiteLLMAccessGroup
```

Then this fully-worked first test — it fixes the harness pattern the other six follow:

```go
// TestAccessGroup_CreateOnFirstReconcile asserts the CREATE arm: a fresh CR
// produces one upstream group named after the CR, and the SERVER-MINTED id
// lands in status (never derived from metadata.name).
func TestAccessGroup_CreateOnFirstReconcile(t *testing.T) {
	ctx := context.Background()
	name := "ag-create-test"
	resetMockAccessGroup()
	ensureNoAccessGroup(t, ctx, name)
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionAccessGroup(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		setConnCacheReady()
		ensureNoAccessGroup(t, context.Background(), name)
	})

	cr := accessGroupSampleCR(name, litellmv1alpha1.AccessGroupSpec{
		Models: []string{"gpt-3.5-turbo"},
	})
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create access group CR: %v", err)
	}

	got := pollAccessGroupCondition(t, ctx, name, reasonSynced)
	c := apimeta.FindStatusCondition(got.Status.Conditions, conditionTypeReady)
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != reasonSynced {
		t.Fatalf("Ready condition = %+v, want True/Synced", c)
	}
	id := got.Status.LastRendered.AccessGroupID
	if id == "" {
		t.Fatal("status.lastRendered.accessGroupID is empty; it must carry the server-minted id")
	}
	if id == name {
		t.Errorf("accessGroupID = %q == metadata.name; the id is SERVER-MINTED and must "+
			"be read from the POST response, not derived from the name", id)
	}

	groups := mockServer.AccessGroups()
	if len(groups) != 1 {
		t.Fatalf("mock has %d groups, want exactly 1 (a duplicate means adoption failed)", len(groups))
	}
	if groups[0].AccessGroupName != name {
		t.Errorf("access_group_name = %q, want %q", groups[0].AccessGroupName, name)
	}
	if len(groups[0].AccessModelNames) != 1 || groups[0].AccessModelNames[0] != "gpt-3.5-turbo" {
		t.Errorf("access_model_names = %v, want [gpt-3.5-turbo]", groups[0].AccessModelNames)
	}
	// The operator must never write this face — a team-side write does not
	// propagate here, and writing it would make us a second mirror writer.
	if len(groups[0].AssignedTeamIDs) != 0 {
		t.Errorf("assigned_team_ids = %v, want [] — the operator must not write it",
			groups[0].AssignedTeamIDs)
	}
}
```

> `setupReadyConnectionAccessGroup` is the per-kind connection fixture; copy `setupReadyConnectionToolset` from `mcptoolset_controller_test.go` verbatim, changing only the CR kind it creates.

Then the remaining six, each following that shape:

2. `TestAccessGroup_UpdateOnSpecChange` — add a model → mock group's `access_model_names` contains it; assert the drift-correction SHAPE (`>=1` PUT), **never an exact mutation count** (the reconcile loop is at-least-once; exact counts flake).
3. `TestAccessGroup_ShrinkToEmptyClears` — spec goes from `models: [a]` to `models: []` → mock group's `access_model_names` is `[]`. This is the regression test for the `omitempty` trap.
4. `TestAccessGroup_ParksOnUnresolvedServer` — `mcpServers: [ghost]` → `Ready=False`, `reason=MCPServerNotFound`, and NO group created in the mock.
5. `TestAccessGroup_AdoptsExistingByName` — pre-seed the mock with a group of the same name → reconcile adopts it (id matches the seeded one, no duplicate row).
6. `TestAccessGroup_DeleteRemovesGroup` — delete the CR → group gone from the mock, finalizer drained.
7. `TestAccessGroup_HealsStaleReadyFalse` — force `Ready=False` with a matching hash and observedGeneration → next reconcile rewrites `Ready=True/Synced` (the #102 guard).

Each test must call `mockServer.ResetAccessGroups()` in its setup.

- [ ] **Step 8: Run the envtest suite**

Run: `make test-envtest-pkg PKG=./internal/controller/... FOCUS=TestAccessGroup`
Expected: PASS, 7 tests.

- [ ] **Step 9: Commit**

```bash
cd /home/coder/workspace/local/alitellm-operator
git add internal/controller/accessgroup_controller.go internal/controller/accessgroup_controller_test.go
git commit -m "feat(controller): reconcile LiteLLMAccessGroup against /v1/access_group"
```

---

## Task 5: Wire the reconciler into the manager

**Files:**
- Modify: `internal/controller/bootsweep.go:57,75,101`
- Modify: `internal/controller/connection_fanin.go` (append)
- Modify: `internal/controller/accessgroup_controller.go` (append `ListAccessGroupRequests`)
- Modify: `internal/controller/suite_test.go`
- Modify: `cmd/main.go` (beside the MCPToolset block at lines 405-432)
- Modify: `config/rbac/role.yaml`, `deploy/kustomize/manager-rbac.yaml` (generated)

**Interfaces:**
- Consumes: `AccessGroupReconciler` (Task 4).
- Produces: `ListAccessGroupRequests`; `BootSweep.AccessGroupEvents chan event.GenericEvent`; `(*AccessGroupReconciler).connectionToAccessGroups`.

- [ ] **Step 1: Register in bootsweep**

In `/home/coder/workspace/local/alitellm-operator/internal/controller/bootsweep.go`, add beside `MCPToolsetEvents` (line 57):

```go
AccessGroupEvents chan event.GenericEvent
```

beside line 75:

```go
AccessGroupEvents: mkChan(),
```

and beside line 101:

```go
{&litellmv1alpha1.LiteLLMAccessGroupList{}, b.AccessGroupEvents},
```

- [ ] **Step 2: Add the connection fan-in**

Append to `/home/coder/workspace/local/alitellm-operator/internal/controller/connection_fanin.go`:

```go
func (r *AccessGroupReconciler) connectionToAccessGroups(ctx context.Context, obj client.Object) []reconcile.Request {
	return connectionFanIn(ctx, r.Client, obj, &litellmv1alpha1.LiteLLMAccessGroupList{}, r.Namespace, "LiteLLMAccessGroup")
}
```

- [ ] **Step 3: Add the relist lister**

Append to `/home/coder/workspace/local/alitellm-operator/internal/controller/accessgroup_controller.go`, mirroring `ListMCPToolsetRequests`.

**Placement note:** every `List<Kind>Requests` func lives in its OWN controller file, NOT in `safety_relist.go` — verified: `ListModelRequests` is at `model_controller.go:1189`, `ListMCPToolsetRequests` at `mcptoolset_controller.go:589`, `ListTeamRequests` at `team_controller.go:1438`, and so on for a2aagent, mcpserver, and guardrail. `safety_relist.go` holds only the generic `SafetyRelistRunnable`. Follow the convention:

```go
// ListAccessGroupRequests enumerates every LiteLLMAccessGroup in scope as a
// reconcile.Request. Feeds the SafetyRelistRunnable — this is the ONLY
// periodic drift path. Do NOT reach for RequeueAfter: a RequeueAfter only
// fires from the return site carrying it, so any early return (notably the
// MCPServerNotFound / AgentNotFound parking arms) silently loses the CR's
// periodic tick. That is the #102 failure family.
func ListAccessGroupRequests(ctx context.Context, c client.Client, ns string) ([]reconcile.Request, error) {
	var list litellmv1alpha1.LiteLLMAccessGroupList
	if err := c.List(ctx, &list, client.InNamespace(ns)); err != nil {
		return nil, err
	}
	out := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
	}
	return out, nil
}
```

> Match the EXACT signature of the existing `List*Requests` funcs before writing this — open `internal/controller/mcptoolset_controller.go:589` and copy its parameter list and return types.

- [ ] **Step 4: Wire in `cmd/main.go`**

Beside the MCPToolset block (lines 405-432) in `/home/coder/workspace/local/alitellm-operator/cmd/main.go`:

```go
// AccessGroupReconciler: per-CR resolve/render/apply against
// /v1/access_group. No Secret indexer — the CRD has no spec.secrets.
accessGroupSafetyRelistCh := make(chan reconcile.Request, 256)
if err := mgr.Add(&controller.SafetyRelistRunnable{
	Client:       mgr.GetClient(),
	Namespace:    watchNS,
	Interval:     relistInterval,
	Log:          ctrl.Log.WithName("accessgroup-safety-relist"),
	RequeueCh:    accessGroupSafetyRelistCh,
	ListRequests: controller.ListAccessGroupRequests,
	LogLabel:     "accessgroups",
}); err != nil {
	setupLog.Error(err, "unable to add accessgroup SafetyRelistRunnable")
	os.Exit(1)
}
if err := (&controller.AccessGroupReconciler{
	Client:     mgr.GetClient(),
	Scheme:     mgr.GetScheme(),
	Recorder:   mgr.GetEventRecorderFor("accessgroup-controller"),
	Log:        ctrl.Log.WithName("controller").WithName("AccessGroup"),
	BootEvents: bootSweep.AccessGroupEvents,
}).SetupWithManager(mgr, accessGroupSafetyRelistCh); err != nil {
	setupLog.Error(err, "unable to set up AccessGroup reconciler")
	os.Exit(1)
}
```

> Copy the remaining struct fields (Cache, Namespace, DeletionPolicy defaults, churn breaker) from the adjacent `MCPToolsetReconciler` literal — the field set must match whatever Task 4's struct declares.

- [ ] **Step 5: Register in the envtest suite with a GATED runnable**

In `/home/coder/workspace/local/alitellm-operator/internal/controller/suite_test.go`, register `AccessGroupReconciler` beside the other reconcilers, and add its `SafetyRelistRunnable` with the gate:

```go
Gate: suiteRelistEnabled.Load,
```

> **This gate is mandatory.** An ungated suite runnable ticks at 100ms for the whole package run and is the #74 contention flake. Tests that need relist opt in with `enableSuiteRelist(t)`.

- [ ] **Step 6: Regenerate RBAC and verify**

```bash
cd /home/coder/workspace/local/alitellm-operator
make gen-manifests
git diff --stat config/rbac/role.yaml
```
Expected: `role.yaml` gains `litellmaccessgroups`, `.../status`, `.../finalizers` rules.

- [ ] **Step 7: Run the full controller suite**

Run: `make test-envtest`
Expected: PASS. If new flakes appear in UNRELATED tests, suspect the gate from Step 5 was omitted.

- [ ] **Step 8: Commit**

```bash
cd /home/coder/workspace/local/alitellm-operator
git add internal/controller/ cmd/main.go config/ deploy/
git commit -m "feat(controller): wire AccessGroup reconciler, bootsweep, fan-in and safety relist"
```

---

## Task 6: Attach access groups from the Team CR

**Files:**
- Modify: `api/litellm/v1alpha1/team_types.go` (`PermissionSpec`)
- Modify: `internal/controller/team_permission.go`
- Modify: `internal/controller/team_controller.go`
- Modify: `internal/controller/litellmconnection_controller.go` (reason constant)
- Modify: `internal/controller/team_permission_test.go`

**Interfaces:**
- Consumes: `Client.ListAccessGroups` (Task 1).
- Produces: `PermissionSpec.AccessGroups []string`; `projectPermission` gains a returned `accessGroupIDs []string`.

**Critical distinction:** `PermissionSpec.ModelGroups` already exists and projects into the top-level `models` list — that is the LEGACY model-TAG namespace. The new field is different: it carries UNIFIED access-group names and projects onto the team's top-level `access_group_ids`. Do not merge the two.

- [ ] **Step 1: Add the spec field**

In `/home/coder/workspace/local/alitellm-operator/api/litellm/v1alpha1/team_types.go`, inside `PermissionSpec` after `ModelGroups`:

```go
	// AccessGroups is the list of LiteLLMAccessGroup NAMES this team is
	// attached to. Each name is resolved to an access_group_id via
	// GET /v1/access_group and projected onto the team's TOP-LEVEL
	// `access_group_ids` — NOT onto object_permission.
	//
	// Distinct from ModelGroups: that field carries legacy model-TAG names
	// and merges into `models`. This one carries unified access-group names
	// from the /v1/access_group object family. The two namespaces are
	// disjoint (a unified group does not appear in /access_group/list).
	//
	// SECURITY: an attached group only ADDS. A group granting a model
	// OVERRIDES this team's deny-by-default sentinel — verified 2026-08-06
	// on LiteLLM 1.93.0: a team with models:["__deny_all__"] plus an
	// attached group granting a model stops being denied. Treat every
	// attached group as a widening of this team's ceiling.
	//
	// An unresolved name parks the Team Ready=False
	// reason=AccessGroupNotFound and requeues (ordering dependency with
	// LiteLLMAccessGroup CRs, same shape as AgentNotFound).
	//
	// +optional
	AccessGroups []string `json:"accessGroups,omitempty"`
```

- [ ] **Step 2: Write the failing projection test**

Append to `/home/coder/workspace/local/alitellm-operator/internal/controller/team_permission_test.go`:

```go
// TestProjectPermission_ResolvesAccessGroupNames pins the name→id resolution
// and the unresolved-name reporting.
func TestProjectPermission_ResolvesAccessGroupNames(t *testing.T) {
	perm := &litellmv1alpha1.PermissionSpec{AccessGroups: []string{"shared", "ghost"}}
	groupNameToID := map[string]string{"shared": "ag-1"}

	_, _, groupIDs, missing := projectPermission(perm, nil, nil, groupNameToID)
	if len(groupIDs) != 1 || groupIDs[0] != "ag-1" {
		t.Errorf("groupIDs = %v, want [ag-1]", groupIDs)
	}
	if len(missing.AccessGroups) != 1 || missing.AccessGroups[0] != "ghost" {
		t.Errorf("missing.AccessGroups = %v, want [ghost]", missing.AccessGroups)
	}
}

// TestProjectPermission_EmptyAccessGroupsClears is the revocation regression:
// dropping the last group must send [] (a clear), never omit the field. An
// omitted access_group_ids KEEPS the stale grant — measured on 1.93.0 — which
// would be a silent authorization leak, the same class as the v0.7.25 bug.
func TestProjectPermission_EmptyAccessGroupsClears(t *testing.T) {
	perm := &litellmv1alpha1.PermissionSpec{}
	_, _, groupIDs, _ := projectPermission(perm, nil, nil, nil)
	if groupIDs == nil {
		t.Fatal("groupIDs is nil — a nil slice omits the field and KEEPS the stale grant")
	}
	if len(groupIDs) != 0 {
		t.Errorf("groupIDs = %v, want []", groupIDs)
	}
}

// TestProjectPermission_AccessGroupsGetNoDenyAllSentinel documents why this
// field differs from models/agents: an empty access_group_ids grants nothing
// (there is no group to widen through), so it is already fail-CLOSED. A
// sentinel here would inject a bogus id into a correct filter — same reasoning
// as mcp_toolsets.
func TestProjectPermission_AccessGroupsGetNoDenyAllSentinel(t *testing.T) {
	perm := &litellmv1alpha1.PermissionSpec{}
	_, _, groupIDs, _ := projectPermission(perm, nil, nil, nil)
	for _, id := range groupIDs {
		if id == modelDenyAllSentinel || id == agentDenyAllSentinel {
			t.Errorf("sentinel %q leaked into access_group_ids", id)
		}
	}
}
```

> **Signature change — this is the widest edit in the task.** `projectPermission` is currently:
>
> ```go
> func projectPermission(
> 	perm *litellmv1alpha1.PermissionSpec,
> 	agentNameToID map[string]string,
> 	toolsetNameToID map[string]string,
> ) (models []string, objectPermission map[string]any, missing missingRefs)
> ```
>
> It becomes a FOURTH parameter (`accessGroupNameToID map[string]string`) and a FOURTH return value, inserted before `missing`:
>
> ```go
> ) (models []string, objectPermission map[string]any, accessGroupIDs []string, missing missingRefs)
> ```
>
> There are **14 call sites**: 13 in `internal/controller/team_permission_test.go` and 1 in `internal/controller/team_controller.go`. Every one must be updated in this same edit or the package will not compile. Find them with:
>
> ```bash
> grep -rn "projectPermission(" internal/controller/
> ```
>
> Also extend the `missingRefs` struct at `internal/controller/team_permission.go:31` — it currently holds `Agents` and `Toolsets` only:
>
> ```go
> type missingRefs struct {
> 	Agents       []string
> 	Toolsets     []string
> 	AccessGroups []string
> }
> ```

- [ ] **Step 3: Run to verify it fails**

Run: `make test-unit-pkg PKG=./internal/controller/...`
Expected: FAIL — wrong number of arguments / return values.

- [ ] **Step 4: Implement the projection**

In `/home/coder/workspace/local/alitellm-operator/internal/controller/team_permission.go`, extend the signature and add, beside the `resolvedToolsets` block:

```go
	// Unified access-group names → access_group_id UUIDs. NO deny-by-default
	// sentinel: an empty access_group_ids grants nothing (there is no group to
	// widen through), so the field is already fail-CLOSED — same reasoning as
	// mcp_toolsets. A sentinel here would inject a bogus id into a filter that
	// is already correct.
	resolvedAccessGroups := make([]string, 0, len(perm.AccessGroups))
	for _, name := range perm.AccessGroups {
		id, ok := accessGroupNameToID[name]
		if !ok {
			missing.AccessGroups = append(missing.AccessGroups, name)
			continue
		}
		resolvedAccessGroups = append(resolvedAccessGroups, id)
	}
```

Add `AccessGroups []string` to the `missing` struct, and return `resolvedAccessGroups` as the new third value.

- [ ] **Step 5: Emit the field from the Team reconciler**

In `/home/coder/workspace/local/alitellm-operator/internal/controller/team_controller.go`, at the Step 7b body assignment, alongside the existing unconditional `object_permission` emission:

```go
	// ALWAYS-EMIT: access_group_ids is sent unconditionally whenever a
	// spec.permission block is present, as [] when empty. POST /team/update
	// merges per field — an OMITTED field keeps its stale value (measured on
	// 1.93.0), so a shrink-to-empty would silently fail to revoke. This is the
	// same trap as the v0.7.25 object_permission leak.
	body["access_group_ids"] = emptyIfNil(accessGroupIDs)
```

Resolve the name→id map before that point by calling `snap.Client.ListAccessGroups(ctx)` and indexing by `AccessGroupName`, and park on `missing.AccessGroups` with the new reason, mirroring the existing `AgentNotFound` / `ToolsetNotFound` arms.

Add to `/home/coder/workspace/local/alitellm-operator/internal/controller/litellmconnection_controller.go` beside `reasonToolsetNotFound`:

```go
	// reasonAccessGroupNotFound — a spec.permission.accessGroups entry names a
	// group GET /v1/access_group does not (yet) list. Non-terminal: the Team is
	// requeued (ordering dependency with LiteLLMAccessGroup CRs).
	reasonAccessGroupNotFound = "AccessGroupNotFound"
```

- [ ] **Step 6: Run the tests**

Run: `make test-unit-pkg PKG=./internal/controller/... && make test-envtest-pkg PKG=./internal/controller/... FOCUS=TestTeam`
Expected: PASS.

- [ ] **Step 7: Document the field in the team user guide**

The documentation-hygiene rule in `CLAUDE.md` requires docs to land in the SAME
commit as the contract change. `spec.permission.accessGroups` is a new
user-facing CRD field, so document it here — do not defer it to Task 7.

Append to the `spec.permission` section of
`/home/coder/workspace/local/alitellm-operator/docs/user-guide/team.md`
(open the file first and match its existing heading level and table style for
the sibling fields `mcpServers` / `agents` / `mcpToolsets`):

```markdown
### `accessGroups`

Names of `LiteLLMAccessGroup` resources to attach to this team. The operator
resolves each name to its server-minted `access_group_id` via
`GET /v1/access_group` and writes the resolved list to `team.access_group_ids`.

```yaml
spec:
  permission:
    accessGroups: [anthropic-tier, internal-mcp]
```

A name that does not resolve parks the team `Ready=False`,
`reason=AccessGroupNotFound` and requeues — create the `LiteLLMAccessGroup`
first, and the team self-heals. This is the same ordering dependency that
`agents` and `mcpToolsets` have.

!!! warning "Access groups BYPASS deny-by-default"

    A `spec.permission` block with an empty `models` list emits the
    `__deny_all__` sentinel, which denies every model (see
    [deny-by-default](#deny-by-default)). An attached access group **overrides**
    that: LiteLLM treats group grants as additive, so a group granting
    `gpt-4o` makes `gpt-4o` reachable by this team's keys even though
    `models` is `["__deny_all__"]`. Measured on LiteLLM 1.93.0.
    Treat `accessGroups` as a grant, never as a filter.
```

- [ ] **Step 8: Regenerate manifests and commit**

```bash
cd /home/coder/workspace/local/alitellm-operator
make gen-manifests helm-sync gen-crd-ref-docs
git add api/ internal/controller/ config/ deploy/ docs/api-reference/ docs/user-guide/team.md
git commit -m "feat(team): attach unified access groups via spec.permission.accessGroups"
```

---

## Task 7: E2E, examples, and documentation

**Files:**
- Create: `test/e2e/accessgroup_test.go`
- Create: `examples/example-deploy/14-accessgroup.yaml`
- Create: `docs/user-guide/access-group.md`
- Modify: `examples/example-deploy/kustomization.yaml`, `docs/user-guide/index.md`, `docs/user-guide/team.md`, `CLAUDE.md`

- [ ] **Step 1: Write the example CR**

Create `/home/coder/workspace/local/alitellm-operator/examples/example-deploy/14-accessgroup.yaml`:

```yaml
# SPDX-License-Identifier: Apache-2.0
apiVersion: litellm.ackstorm.ai/v1alpha1
kind: LiteLLMAccessGroup
metadata:
  name: shared-tooling
  namespace: default
spec:
  description: Models and tools shared across product teams
  models:
    - gpt-3.5-turbo
  mcpServers:
    - example-mcp
  agents: []
  deletionPolicy: Orphan
---
# Attaching the group WIDENS every team that references it. A group grant
# overrides the team's own deny-by-default sentinel.
apiVersion: litellm.ackstorm.ai/v1alpha1
kind: LiteLLMTeam
metadata:
  name: example-team-with-group
  namespace: default
spec:
  permission:
    models: []
    accessGroups:
      - shared-tooling
```

Register `14-accessgroup.yaml` in `examples/example-deploy/kustomization.yaml`.

- [ ] **Step 2: Write the e2e specs**

Create `/home/coder/workspace/local/alitellm-operator/test/e2e/accessgroup_test.go`. Model the structure on `test/e2e/team_test.go`. Cover:

1. **AG-01 CRUD** — apply the CR → group appears in `GET /v1/access_group` with the rendered lists; patch `spec.models` → the change lands; delete → the group is gone.
2. **AG-02 revocation** — shrink `spec.models` from one entry to `[]` → the upstream group's `access_model_names` is `[]`. The regression guard for the omit-vs-clear trap.
3. **AG-03 team attachment** — attach via `spec.permission.accessGroups` → `GET /team/info?team_id=<id>` reports the group in `access_group_ids`.
   **Read the mirror, never the group side**: a team-side write does NOT propagate to `access_group.assigned_team_ids` (measured). Asserting on the group side will fail.
4. **AG-04 bypass is real** — a team with an empty `spec.permission` (deny-all sentinel) is denied inference; attach a group granting the model; the same key is no longer denied.
   **Assert by ERROR TYPE, never status code.** The denial echoes `__deny_all__`; a pass shows an upstream error (`OpenAIException` / `429 No deployments available`). On the mock a correctly-authorized call cannot return 200 — a `== 200` assertion inverts the verdict.
5. **AG-05 ordering** — a Team referencing a not-yet-created group parks `Ready=False reason=AccessGroupNotFound`, then self-heals once the `LiteLLMAccessGroup` CR is applied.

Use the retrying `curlPodJSON` / `curlPodBody` helpers from `test/e2e/curl_helpers_test.go` for any response body the spec parses — a one-shot `kubectl run --rm -i` can return EMPTY stdout with exit 0.

- [ ] **Step 3: Run the e2e specs against the standing cluster**

```bash
cd /home/coder/workspace/local/alitellm-operator
make operator-redeploy
make e2e-focus FOCUS="AG-0"
```
Expected: 5 specs PASS.

- [ ] **Step 4: Write the user guide**

Create `/home/coder/workspace/local/alitellm-operator/docs/user-guide/access-group.md` covering: what an access group is; the three dimensions and which are resolved by name; that `metadata.name` is the `access_group_name` and ids are server-minted; attaching via `spec.permission.accessGroups`; and a prominent **warning** that groups only ADD and override the deny-by-default sentinel. Add a "Two access-group namespaces" section distinguishing this CRD from `model_info.access_groups` / `DEFAULT_ACCESS_GROUP` and from `spec.permission.modelGroups`.

Add the page to the nav in `docs/user-guide/index.md`, and cross-link from `docs/user-guide/team.md` under the permission section.

- [ ] **Step 5: Regenerate the API reference**

```bash
cd /home/coder/workspace/local/alitellm-operator
make gen-crd-ref-docs
```
Expected: `docs/api-reference/litellm.ackstorm.ai.md` gains the `LiteLLMAccessGroup` types.

- [ ] **Step 6: Update CLAUDE.md**

Add to the owned-CRDs list; add a MANDATORY Reading Table row (`Access groups / team attachment` → `docs/user-guide/access-group.md`); and add these `### ❌ ... ✅ ...` entries under "Common failure modes":

1. **Confusing the two access-group namespaces** — `/access_group/*` (legacy model tags, what `DEFAULT_ACCESS_GROUP` writes) vs `/v1/access_group` (the unified object). A unified group never appears in `/access_group/list`.
2. **Expecting an access group to RESTRICT** — groups only ADD; a group grant overrides `models: ["__deny_all__"]`. Verified 2026-08-06 on 1.93.0.
3. **Asserting team attachment from the group side** — `assigned_team_ids` stays `[]` after a team-side write. Read `GET /team/info`.
4. **Putting `omitempty` on a managed access-group list** — omitted = KEEP upstream, so a shrink-to-empty silently fails to revoke.
5. **Writing `assigned_team_ids` from the operator** — makes it a second writer of a relation LiteLLM only syncs on an ENTER/LEAVE delta; an idempotent re-PUT cannot repair a broken mirror.

- [ ] **Step 7: Full gate**

```bash
cd /home/coder/workspace/local/alitellm-operator
make cluster-reset
make e2e-full
make verify
```
Expected: e2e suite green; `verify` (lint + unit + security + pre-push) green.

- [ ] **Step 8: Commit**

```bash
cd /home/coder/workspace/local/alitellm-operator
git add test/e2e/ examples/ docs/ CLAUDE.md
git commit -m "test(e2e): cover access-group CRUD, revocation, attachment and bypass"
```

---

## Self-Review Notes

**Deferred deliberately (YAGNI):**
- `assigned_key_ids` — never exposed. ach measured an agent-permission collapse when a KEY has both a team and a group with differing agent lists (effective set becomes every agent on the proxy). Add only with a measured reason.
- Group-side `assigned_team_ids` management and the two-PUT delta repair — unnecessary because we write the enforcing team mirror directly. Revisit only if a use case needs attachment without a Team CR.
- Validation that a `spec.models` entry names a live model — LiteLLM does not validate, and the repo already chose non-validation for MCPToolset. An inert grant is the documented failure mode.
- Access-group name prefixing (ach uses `ach-env-`) — this operator's `metadata.name` is already the user's chosen identifier, and there is no second writer to disambiguate from.

**Known gap:** the agent-permission collapse bug is inherited from ach's prod measurement and was NOT re-verified on the e2e cluster. It only bites via `assigned_key_ids`, which this plan never writes, so it is out of the blast radius — but the CLAUDE.md entry should say "measured by ach in prod, not re-verified here".
