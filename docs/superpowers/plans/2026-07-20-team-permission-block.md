# LiteLLMTeam Typed `spec.permission` Block Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace raw `spec.params.object_permission` / `spec.params.models` passthrough on `LiteLLMTeam` with a typed, operator-MANAGED, reconciled `spec.permission` block that projects onto LiteLLM's top-level `team.models` and nested `object_permission`, resolving A2A agent names to UUIDs.

**Architecture:** A new `PermissionSpec` sub-block on `TeamSpec` (pointer, so absent-block is distinguishable from empty). A pure `projectPermission` function renders the block into the two LiteLLM body fields it maps to. The Team reconciler builds an agent-name→UUID map via `GET /v1/agents`, calls the projection, requeues on unresolved agent names (mirroring the SecretNotFound pattern), and overlays the result onto the existing `map[string]any` body BEFORE the Step 8 hash — so drift detection and CREATE/UPDATE routing pick it up for free.

**Tech Stack:** Go, controller-runtime v0.19.4, k8s.io/* v0.31.0, kubebuilder v4.4.0. Host has NO Go — every toolchain command self-routes into the devtools container. Run bare `make <target>`.

## Global Constraints

- Every `*.go` file starts with `// SPDX-License-Identifier: Apache-2.0` (pre-push gate 15 enforces).
- Body MUST stay `map[string]any` — never a typed struct with `omitempty` (spec §6.7 requires explicit JSON `null` on absent budget keys; a struct would drop them).
- Reason/event constant literals live ONCE in `internal/controller/litellmconnection_controller.go` (goconst gate). Add new ones there.
- No `RequeueAfter` for periodic/safety-relist paths — the `SafetyRelistRunnable` owns those (issue #102). `RequeueAfter` IS allowed for deterministic "referenced object not ready yet" requeues (the SecretNotFound pattern uses `snap.NormalizedRequeueOnRejectedAfter()`).
- Docs hygiene: doc/CLAUDE.md/example changes ship in the SAME commit as the code that changes behavior. CRD reference + CRD manifests are GENERATED (`make gen-manifests`, `make gen-code`, `make gen-crd-ref-docs`) — never hand-edit `config/crd/**`, `deploy/helm/**/crds/**`, `docs/api-reference/**`, or `zz_generated.deepcopy.go`.
- Team reconcile builds the body in `internal/controller/team_controller.go`; the LiteLLM HTTP surface is `internal/litellm/team.go` (`CreateTeamRaw` / `UpdateTeamRaw` / `ListTeamsByAlias`) and `internal/litellm/agents.go` (`ListAgents`).

## Design decisions (locked)

**Empty vs absent semantics (spec gotcha #5):**
- `spec.permission` **absent** (`nil`): operator does NOT manage `models` / `object_permission`. Whatever the user placed in `spec.params` passes through unchanged (backward-compat + migration path). This is the ONLY mode that preserves the pre-feature behavior.
- `spec.permission` **present**: operator OWNS `body["models"]` and `body["object_permission"]`. Any `models` / `object_permission` key in `spec.params` is deleted and a `ProjectionOverride` Warning Event is emitted (`permission` wins).
- Within a present block, a **nil or empty sublist contributes nothing** — it is omitted from the projected output. Rationale: an empty allowed list means "allow all" in LiteLLM (`get_allowed_agents`), so sending `[]` is indistinguishable from omitting the key; omitting avoids any lock-to-zero misread. Consequence (documented ceiling): you cannot CLEAR a stale out-of-band `object_permission` by supplying an empty block — remove the field in LiteLLM once, or supply a non-empty block.

**Projection mapping (verified empirically on a live proxy):**
- `models` + `modelGroups` → merged into ONE `body["models"]` list (LiteLLM's top-level `team.models` accepts both specific names and access-group names).
- `mcpServers` → `object_permission.mcp_servers` (LiteLLM resolves names→ids itself).
- `mcpGroups` → `object_permission.mcp_access_groups`.
- `agents` → `object_permission.agents` — **operator resolves NAME→agent_id UUID** via `GET /v1/agents` (names are silently ignored by LiteLLM).
- `agentGroups` → `object_permission.agent_access_groups` — **DEAD field in LiteLLM 1.83.10** (no API tags an agent into a group). Projected for forward-compat + a `AgentGroupsNoOp` Warning Event fires when non-empty.

**Agent name→UUID failure mode (gotcha #4):** an agent name absent from `GET /v1/agents` (A2A CR not registered yet) does NOT hard-fail — `Ready=False, reason=AgentNotFound` with the missing names, requeued via `snap.NormalizedRequeueOnRejectedAfter()` (mirrors SecretNotFound).

---

## File Structure

- `api/litellm/v1alpha1/team_types.go` — add `PermissionSpec` type + `Permission *PermissionSpec` field on `TeamSpec`; reverse the TEAM-03/TEAM-04 doc comments.
- `api/litellm/v1alpha1/zz_generated.deepcopy.go` — GENERATED (`make gen-code`).
- `internal/controller/team_permission.go` — NEW: pure `projectPermission` function.
- `internal/controller/team_permission_test.go` — NEW: unit tests for the pure function.
- `internal/controller/team_controller.go` — hook the projection into Steps 6–7; add agent resolution + `AgentNotFound` requeue + `AgentGroupsNoOp` event.
- `internal/controller/litellmconnection_controller.go` — add `reasonAgentNotFound` + `eventReasonAgentGroupsNoOp` constants.
- `internal/controller/team_controller_test.go` — envtest coverage mirroring existing team tests.
- `docs/user-guide/team.md`, `examples/example-deploy/07-team.yaml`, `CLAUDE.md` — docs + example + failure-mode note.
- Generated: `config/crd/**`, `deploy/helm/alitellm-operator/crds/**`, `docs/api-reference/**`.

---

### Task 1: `PermissionSpec` API type + `TeamSpec.Permission` field

**Files:**
- Modify: `api/litellm/v1alpha1/team_types.go`
- Generated: `api/litellm/v1alpha1/zz_generated.deepcopy.go`, `config/crd/**`, `deploy/helm/alitellm-operator/crds/**`

**Interfaces:**
- Produces: `PermissionSpec` struct with exported string-slice fields `Models`, `ModelGroups`, `McpServers`, `McpGroups`, `Agents`, `AgentGroups`; and `TeamSpec.Permission *PermissionSpec`. Later tasks consume `team.Spec.Permission`.

- [ ] **Step 1: Add the `PermissionSpec` type**

Insert this type in `api/litellm/v1alpha1/team_types.go` immediately BEFORE the `TeamSpec` struct definition (after `RateLimitsSpec`):

```go
// PermissionSpec is the optional typed resource-permission sub-block on
// TeamSpec. Unlike the pre-existing `spec.params` passthrough, this block is
// operator-MANAGED and reconciled: the operator OWNS the projected LiteLLM
// `team.models` and `object_permission` fields whenever this block is
// present, so out-of-band UI edits to those fields do NOT survive
// reconciliation (see the TEAM-03/TEAM-04 note on TeamSpec).
//
// Modeled as a pointer at the TeamSpec level (`Permission *PermissionSpec`)
// so whole-block absence (nil) is distinguishable from a present-but-empty
// block. Absent block → the operator manages nothing here and the raw
// `spec.params.models` / `spec.params.object_permission` (if any) pass
// through unchanged (migration path). Present block → the operator projects
// every non-empty sublist and deletes any colliding `spec.params` key
// (emitting a ProjectionOverride Event).
//
// Empty-vs-absent per sublist: a nil or empty sublist contributes NOTHING to
// the projection (it is omitted, not sent as `[]`). An empty allowed list
// means "allow all" in LiteLLM (get_allowed_agents), so omitting is both
// equivalent on the wire and safe against a lock-to-zero misread.
//
// Projection to LiteLLM (verified empirically against LiteLLM 1.83.10):
//   - Models + ModelGroups → merged into the top-level `models` list (LiteLLM
//     accepts specific model names AND model-access-group names mixed there).
//   - McpServers → object_permission.mcp_servers (LiteLLM resolves name→id).
//   - McpGroups  → object_permission.mcp_access_groups.
//   - Agents     → object_permission.agents. LiteLLM enforces on agent_id
//     UUIDs and SILENTLY IGNORES names, so the operator resolves each name to
//     its agent_id via GET /v1/agents before projecting. An unresolved name
//     (A2A agent not registered yet) requeues the Team with
//     reason=AgentNotFound rather than hard-failing.
//   - AgentGroups → object_permission.agent_access_groups. DEAD FIELD in
//     LiteLLM 1.83.10 (no API tags an agent into a group), retained for
//     forward-compat; the reconciler emits a Warning/AgentGroupsNoOp Event
//     when this sublist is non-empty.
type PermissionSpec struct {
	// Models is the list of specific LiteLLM model NAMES this team may use.
	// Merged with ModelGroups into the single top-level `models` list.
	//
	// +optional
	Models []string `json:"models,omitempty"`

	// ModelGroups is the list of model ACCESS-GROUP names this team may use.
	// Merged with Models into the single top-level `models` list.
	//
	// +optional
	ModelGroups []string `json:"modelGroups,omitempty"`

	// McpServers is the list of specific MCP server NAMES (aliases) this team
	// may use. Projected onto object_permission.mcp_servers; LiteLLM resolves
	// names to server ids automatically.
	//
	// +optional
	McpServers []string `json:"mcpServers,omitempty"`

	// McpGroups is the list of MCP access-group names this team may use.
	// Projected onto object_permission.mcp_access_groups.
	//
	// +optional
	McpGroups []string `json:"mcpGroups,omitempty"`

	// Agents is the list of A2A agent NAMES (human-friendly) this team may
	// use. The operator resolves each name to its agent_id UUID via
	// GET /v1/agents before projecting onto object_permission.agents — LiteLLM
	// enforces on UUIDs and ignores names. An unresolved name requeues the
	// Team (reason=AgentNotFound).
	//
	// +optional
	Agents []string `json:"agents,omitempty"`

	// AgentGroups is the list of A2A agent access-group names. Projected onto
	// object_permission.agent_access_groups for forward-compat, but this is a
	// NO-OP in LiteLLM 1.83.10 (the API never tags an agent into a group). The
	// reconciler emits a Warning/AgentGroupsNoOp Event when this is non-empty.
	//
	// +optional
	AgentGroups []string `json:"agentGroups,omitempty"`
}
```

- [ ] **Step 2: Add the `Permission` field to `TeamSpec`**

In `api/litellm/v1alpha1/team_types.go`, add this field to the `TeamSpec` struct, immediately AFTER the `RateLimits *RateLimitsSpec` field and BEFORE the `Params` field:

```go
	// Permission is the optional typed, operator-MANAGED resource-permission
	// sub-block (see PermissionSpec). When present, the operator OWNS the
	// projected LiteLLM `models` and `object_permission` fields and deletes any
	// colliding `spec.params.models` / `spec.params.object_permission` key
	// (emitting a ProjectionOverride Event). When absent, those raw params keys
	// pass through unchanged (migration path). Modeled as a pointer so
	// whole-block absence is distinguishable from an empty block.
	//
	// +optional
	Permission *PermissionSpec `json:"permission,omitempty"`
```

- [ ] **Step 3: Regenerate deepcopy + CRD manifests**

Run: `make gen-code gen-manifests`
Expected: no errors; `git status` shows modifications to `api/litellm/v1alpha1/zz_generated.deepcopy.go` (new `PermissionSpec` DeepCopy funcs), `config/crd/**`, and `deploy/helm/alitellm-operator/crds/**` (new `permission` schema with the six string-array properties).

- [ ] **Step 4: Verify it compiles**

Run: `./scripts/dev.sh go build ./...`
Expected: exit 0, no output.

- [ ] **Step 5: Commit**

```bash
git add api/litellm/v1alpha1/team_types.go api/litellm/v1alpha1/zz_generated.deepcopy.go config/crd deploy/helm/alitellm-operator/crds
git commit -m "feat(team): add typed spec.permission block to TeamSpec API"
```

---

### Task 2: Pure `projectPermission` function + unit tests

**Files:**
- Create: `internal/controller/team_permission.go`
- Test: `internal/controller/team_permission_test.go`

**Interfaces:**
- Consumes: `litellmv1alpha1.PermissionSpec` (Task 1).
- Produces: `func projectPermission(perm *litellmv1alpha1.PermissionSpec, agentNameToID map[string]string) (models []string, objectPermission map[string]any, missingAgents []string)`. Task 3 consumes this exact signature.

- [ ] **Step 1: Write the failing test**

Create `internal/controller/team_permission_test.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"reflect"
	"testing"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
)

func TestProjectPermission_MergesModelsAndGroups(t *testing.T) {
	perm := &litellmv1alpha1.PermissionSpec{
		Models:      []string{"gpt-4o", "claude-opus"},
		ModelGroups: []string{"anthropic"},
	}
	models, op, missing := projectPermission(perm, nil)
	if want := []string{"gpt-4o", "claude-opus", "anthropic"}; !reflect.DeepEqual(models, want) {
		t.Errorf("models: want %v, got %v", want, models)
	}
	if len(op) != 0 {
		t.Errorf("objectPermission: want empty, got %v", op)
	}
	if len(missing) != 0 {
		t.Errorf("missingAgents: want none, got %v", missing)
	}
}

func TestProjectPermission_McpAndGroups(t *testing.T) {
	perm := &litellmv1alpha1.PermissionSpec{
		McpServers: []string{"hindsight"},
		McpGroups:  []string{"team-a"},
	}
	_, op, _ := projectPermission(perm, nil)
	if !reflect.DeepEqual(op["mcp_servers"], []string{"hindsight"}) {
		t.Errorf("mcp_servers: got %v", op["mcp_servers"])
	}
	if !reflect.DeepEqual(op["mcp_access_groups"], []string{"team-a"}) {
		t.Errorf("mcp_access_groups: got %v", op["mcp_access_groups"])
	}
}

func TestProjectPermission_AgentsResolvedToUUIDs(t *testing.T) {
	perm := &litellmv1alpha1.PermissionSpec{Agents: []string{"planner", "coder"}}
	m := map[string]string{"planner": "uuid-1", "coder": "uuid-2"}
	_, op, missing := projectPermission(perm, m)
	if len(missing) != 0 {
		t.Fatalf("missingAgents: want none, got %v", missing)
	}
	if !reflect.DeepEqual(op["agents"], []string{"uuid-1", "uuid-2"}) {
		t.Errorf("agents: want resolved UUIDs, got %v", op["agents"])
	}
}

func TestProjectPermission_UnresolvedAgentReported(t *testing.T) {
	perm := &litellmv1alpha1.PermissionSpec{Agents: []string{"planner", "ghost"}}
	m := map[string]string{"planner": "uuid-1"}
	_, op, missing := projectPermission(perm, m)
	if !reflect.DeepEqual(missing, []string{"ghost"}) {
		t.Errorf("missingAgents: want [ghost], got %v", missing)
	}
	// Resolved agents still projected (caller decides to requeue on missing).
	if !reflect.DeepEqual(op["agents"], []string{"uuid-1"}) {
		t.Errorf("agents: want [uuid-1], got %v", op["agents"])
	}
}

func TestProjectPermission_AgentGroupsProjectedVerbatim(t *testing.T) {
	perm := &litellmv1alpha1.PermissionSpec{AgentGroups: []string{"grp-a"}}
	_, op, _ := projectPermission(perm, nil)
	if !reflect.DeepEqual(op["agent_access_groups"], []string{"grp-a"}) {
		t.Errorf("agent_access_groups: got %v", op["agent_access_groups"])
	}
}

func TestProjectPermission_EmptySublistsOmitted(t *testing.T) {
	perm := &litellmv1alpha1.PermissionSpec{
		Models:     []string{},
		McpServers: []string{},
		Agents:     []string{},
	}
	models, op, missing := projectPermission(perm, nil)
	if len(models) != 0 {
		t.Errorf("models: want empty, got %v", models)
	}
	if len(op) != 0 {
		t.Errorf("objectPermission: want empty (no [] keys), got %v", op)
	}
	if len(missing) != 0 {
		t.Errorf("missingAgents: want none, got %v", missing)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `make test-envtest-pkg PKG=./internal/controller/... FOCUS=TestProjectPermission`
(Or faster, since this is pure logic with no envtest: `./scripts/dev.sh go test ./internal/controller/ -run TestProjectPermission -count=1`)
Expected: FAIL — `undefined: projectPermission`.

- [ ] **Step 3: Write the minimal implementation**

Create `internal/controller/team_permission.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
)

// projectPermission renders a typed spec.permission block into the two
// LiteLLM team body fields it maps to: the top-level `models` list (merged
// specific model names + model access-group names) and the nested
// `object_permission` object (mcp_servers, mcp_access_groups, agents,
// agent_access_groups).
//
// agentNameToID resolves human-friendly A2A agent NAMES to the agent_id
// UUIDs LiteLLM enforces on object_permission.agents (names are silently
// ignored by LiteLLM). Names absent from the map are returned in
// missingAgents so the caller can requeue (reason=AgentNotFound) instead of
// silently dropping agents; the successfully-resolved agents are still
// projected.
//
// Semantics (spec gotcha #5): the caller must not invoke this with a nil
// block. Within a present block, a nil or empty sublist contributes NOTHING
// — an empty allowed list means "allow all" in LiteLLM, so we omit the key
// rather than send [] and risk a lock-to-zero misread.
func projectPermission(
	perm *litellmv1alpha1.PermissionSpec,
	agentNameToID map[string]string,
) (models []string, objectPermission map[string]any, missingAgents []string) {
	// models = specific model names + model access-group names, merged.
	models = append(models, perm.Models...)
	models = append(models, perm.ModelGroups...)

	objectPermission = map[string]any{}
	if len(perm.McpServers) > 0 {
		objectPermission["mcp_servers"] = perm.McpServers
	}
	if len(perm.McpGroups) > 0 {
		objectPermission["mcp_access_groups"] = perm.McpGroups
	}
	if len(perm.Agents) > 0 {
		resolved := make([]string, 0, len(perm.Agents))
		for _, name := range perm.Agents {
			id, ok := agentNameToID[name]
			if !ok {
				missingAgents = append(missingAgents, name)
				continue
			}
			resolved = append(resolved, id)
		}
		if len(resolved) > 0 {
			objectPermission["agents"] = resolved
		}
	}
	if len(perm.AgentGroups) > 0 {
		objectPermission["agent_access_groups"] = perm.AgentGroups
	}
	return models, objectPermission, missingAgents
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `./scripts/dev.sh go test ./internal/controller/ -run TestProjectPermission -count=1`
Expected: PASS (all six sub-tests).

- [ ] **Step 5: Lint the touched package**

Run: `make qa-lint-changed`
Expected: no findings.

- [ ] **Step 6: Commit**

```bash
git add internal/controller/team_permission.go internal/controller/team_permission_test.go
git commit -m "feat(team): add pure projectPermission function"
```

---

### Task 3: Wire projection into the Team reconciler

**Files:**
- Modify: `internal/controller/team_controller.go`
- Modify: `internal/controller/litellmconnection_controller.go` (reason + event constants)

**Interfaces:**
- Consumes: `projectPermission(...)` (Task 2); `snap.Client.ListAgents(ctx) ([]litellm.AgentEntry, error)` (returns `litellm.ErrNotFound` on zero agents); `AgentEntry.AgentName` / `AgentEntry.AgentID`; `r.writeStatus(ctx, &team, status, reason, message)`; `snap.NormalizedRequeueOnRejectedAfter()`; `r.classifyMutationError(...)`.
- Produces: no new exported symbol; behavior change in `Reconcile`.

- [ ] **Step 1: Add the reason + event constants**

In `internal/controller/litellmconnection_controller.go`, add to the Reason constant block (the `const (` group containing `reasonSynced` at line ~49, after `reasonInsecureEndpoint`):

```go
	// reasonAgentNotFound — a spec.permission.agents entry names an A2A
	// agent that GET /v1/agents does not (yet) list. Non-terminal: the
	// Team is requeued (ordering dependency with LiteLLMA2AAgent CRs),
	// mirroring reasonSecretNotFound.
	reasonAgentNotFound = "AgentNotFound"
```

And add to the Event reason constant block (the `const (` group containing `eventReasonProjectionOverride` at line ~89):

```go
	// eventReasonAgentGroupsNoOp — spec.permission.agentGroups is non-empty.
	// It projects to object_permission.agent_access_groups for forward-compat
	// but is a NO-OP in LiteLLM 1.83.10 (no API tags an agent into a group).
	eventReasonAgentGroupsNoOp = "AgentGroupsNoOp"
```

- [ ] **Step 2: Add the `strings` import**

In `internal/controller/team_controller.go`, add `"strings"` to the stdlib import group (alphabetically after `"sort"`):

```go
	"sort"
	"strings"
	"sync"
```

- [ ] **Step 3: Add permission-vs-params precedence (Step 6 region)**

In `internal/controller/team_controller.go`, immediately AFTER the seventh (`tpm_limit_type`) ProjectionOverride block and BEFORE the `// ─── Step 7: Build merged body` comment, insert:

```go
	// ─── Step 6b: spec.permission owns models + object_permission ─────────
	//
	// When the typed permission block is present the operator OWNS the LiteLLM
	// `models` and `object_permission` fields (TEAM-03/TEAM-04 reversal). Any
	// same-named key in spec.params is dropped and a ProjectionOverride Event
	// fires — the typed block always wins. Absent block → params passthrough
	// is untouched (migration path).
	if team.Spec.Permission != nil {
		for _, key := range []string{"models", "object_permission"} {
			if _, collides := paramsMap[key]; collides {
				r.Recorder.Eventf(&team, corev1.EventTypeWarning, eventReasonProjectionOverride,
					"key %q overridden by typed-field projection (operator overlays spec.permission)",
					key)
				delete(paramsMap, key)
			}
		}
	}
```

- [ ] **Step 4: Apply the projection to the body (after Step 7, before Step 8 hash)**

In `internal/controller/team_controller.go`, immediately AFTER the `blocked` drop block (the `if v, ok := body["blocked"]; ok { ... }` that closes just before `// ─── Step 8: Compute currentRenderedHash`) and BEFORE the Step 8 comment, insert:

```go
	// ─── Step 7b: Project spec.permission onto the body ───────────────────
	//
	// Applied BEFORE the Step 8 hash so the projected models + object_permission
	// participate in drift detection and CREATE/UPDATE routing automatically.
	// Resolves A2A agent names → agent_id UUIDs (LiteLLM ignores names).
	if perm := team.Spec.Permission; perm != nil {
		// Build the name→UUID map only when agents are actually referenced.
		// ponytail: GET /v1/agents runs per reconcile when agents are set;
		// cache it if it ever shows up in a profile (Teams reconcile rarely).
		var agentNameToID map[string]string
		if len(perm.Agents) > 0 {
			agents, aerr := snap.Client.ListAgents(ctx)
			if aerr != nil && !errors.Is(aerr, litellm.ErrNotFound) {
				return r.classifyMutationError(ctx, &team, logger, aerr, "GET /v1/agents")
			}
			// ErrNotFound → zero agents registered → empty map → all names
			// missing → AgentNotFound requeue below.
			agentNameToID = make(map[string]string, len(agents))
			for _, a := range agents {
				agentNameToID[a.AgentName] = a.AgentID
			}
		}

		models, objectPermission, missing := projectPermission(perm, agentNameToID)
		if len(missing) > 0 {
			msg := fmt.Sprintf("spec.permission.agents not yet registered in LiteLLM: %s",
				strings.Join(missing, ", "))
			if werr := r.writeStatus(ctx, &team, metav1.ConditionFalse, reasonAgentNotFound, msg); werr != nil {
				logStatusUpdateErr(logger, werr, "reason", reasonAgentNotFound)
			}
			metrics.ReconcileTotal.WithLabelValues(teamKind, "success").Inc()
			// Ordering dependency with LiteLLMA2AAgent CRs — requeue like
			// SecretNotFound rather than hard-fail.
			return ctrl.Result{RequeueAfter: snap.NormalizedRequeueOnRejectedAfter()}, nil
		}

		if len(models) > 0 {
			body["models"] = models
		}
		if len(objectPermission) > 0 {
			body["object_permission"] = objectPermission
		}

		if len(perm.AgentGroups) > 0 {
			r.Recorder.Eventf(&team, corev1.EventTypeWarning, eventReasonAgentGroupsNoOp,
				"spec.permission.agentGroups projects to object_permission.agent_access_groups, "+
					"but LiteLLM 1.83.10 never writes that field (no API tags an agent into a group) — no-op")
		}
	}
```

- [ ] **Step 5: Verify it compiles**

Run: `./scripts/dev.sh go build ./...`
Expected: exit 0.

- [ ] **Step 6: Run the pure unit tests still pass**

Run: `./scripts/dev.sh go test ./internal/controller/ -run TestProjectPermission -count=1`
Expected: PASS.

- [ ] **Step 7: Lint**

Run: `make qa-lint-changed`
Expected: no findings.

- [ ] **Step 8: Commit**

```bash
git add internal/controller/team_controller.go internal/controller/litellmconnection_controller.go
git commit -m "feat(team): project spec.permission onto LiteLLM body with agent name resolution"
```

---

### Task 4: envtest coverage for the reconciler wiring

**Files:**
- Modify: `internal/controller/team_controller_test.go`

**Interfaces:**
- Consumes: existing test helpers `mockServer` (`*mock.MockServer`), `setupReadyConnectionTeam(t, ctx)`, `teamSampleCR(name)`, `pollTeamCondition(t, ctx, name, reason, timeout)`, `ensureNoTeam(t, ctx, name)`, `teamReconciler.ResetImplicitDefaultCache()`, `resetConnCacheSnapshot()`; mock helpers `mockServer.LastTeamBody(alias)`, `mockServer.MutationsByTeamAlias(alias)`, `mockServer.AddHandManagedAgent(name) string`, `mockServer.GetAgentID(name) string`, `mockServer.ResetAgents()`, `mockServer.ResetTeams()`, `mockServer.ResetCounters()`, `mockServer.ResetRecorded()`, `mockServer.SetMode(mock.ModeHappy)`.

Before writing, confirm helper names/signatures against the current file (they may have drifted): `grep -n 'func setupReadyConnectionTeam\|func teamSampleCR\|func pollTeamCondition\|func ensureNoTeam' internal/controller/*_test.go` and `grep -n 'func (m \*MockServer) AddHandManagedAgent\|func (m \*MockServer) GetAgentID\|func (m \*MockServer) LastTeamBody' internal/litellm/mock/mock.go`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/controller/team_controller_test.go`:

```go
// ──────────────────────────────────────────────────────────────────────────
// spec.permission projection tests
// ──────────────────────────────────────────────────────────────────────────

// TestTeamPermission_ProjectsModelsAndMcp — a present permission block with
// models/modelGroups/mcpServers/mcpGroups projects onto the top-level `models`
// list and `object_permission` on the POST /team/new body.
func TestTeamPermission_ProjectsModelsAndMcp(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetTeams()
	mockServer.ResetAgents()
	teamReconciler.ResetImplicitDefaultCache()
	ensureNoTeam(t, ctx, "team-perm-models")
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionTeam(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoTeam(t, context.Background(), "team-perm-models")
	})

	cr := teamSampleCR("team-perm-models")
	cr.Spec.Permission = &litellmv1alpha1.PermissionSpec{
		Models:      []string{"gpt-4o"},
		ModelGroups: []string{"anthropic"},
		McpServers:  []string{"hindsight"},
		McpGroups:   []string{"team-a"},
	}
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create Team: %v", err)
	}

	pollTeamCondition(t, ctx, "team-perm-models", reasonSynced, 30*time.Second)
	body := mockServer.LastTeamBody("team-perm-models")
	if body == nil {
		t.Fatalf("LastTeamBody nil")
	}
	models, _ := body["models"].([]any)
	if len(models) != 2 {
		t.Errorf("body.models: want [gpt-4o anthropic], got %v", body["models"])
	}
	op, ok := body["object_permission"].(map[string]any)
	if !ok {
		t.Fatalf("body.object_permission: want map, got %T (%v)", body["object_permission"], body["object_permission"])
	}
	if mcp, _ := op["mcp_servers"].([]any); len(mcp) != 1 || mcp[0] != "hindsight" {
		t.Errorf("object_permission.mcp_servers: got %v", op["mcp_servers"])
	}
	if grp, _ := op["mcp_access_groups"].([]any); len(grp) != 1 || grp[0] != "team-a" {
		t.Errorf("object_permission.mcp_access_groups: got %v", op["mcp_access_groups"])
	}
}

// TestTeamPermission_ResolvesAgentNamesToUUIDs — agent NAMES in the block are
// resolved to agent_id UUIDs (via GET /v1/agents) on the projected body.
func TestTeamPermission_ResolvesAgentNamesToUUIDs(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetTeams()
	mockServer.ResetAgents()
	teamReconciler.ResetImplicitDefaultCache()
	ensureNoTeam(t, ctx, "team-perm-agents")
	resetConnCacheSnapshot()

	// Register the agent in the mock so GET /v1/agents resolves it.
	agentID := mockServer.AddHandManagedAgent("planner")

	cleanupConn := setupReadyConnectionTeam(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoTeam(t, context.Background(), "team-perm-agents")
	})

	cr := teamSampleCR("team-perm-agents")
	cr.Spec.Permission = &litellmv1alpha1.PermissionSpec{Agents: []string{"planner"}}
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create Team: %v", err)
	}

	pollTeamCondition(t, ctx, "team-perm-agents", reasonSynced, 30*time.Second)
	body := mockServer.LastTeamBody("team-perm-agents")
	op, ok := body["object_permission"].(map[string]any)
	if !ok {
		t.Fatalf("object_permission: want map, got %T", body["object_permission"])
	}
	agents, _ := op["agents"].([]any)
	if len(agents) != 1 || agents[0] != agentID {
		t.Errorf("object_permission.agents: want [%s] (resolved UUID), got %v", agentID, op["agents"])
	}
}

// TestTeamPermission_AgentNotFoundRequeues — an agent name absent from
// GET /v1/agents parks the Team Ready=False/AgentNotFound (no /team/new).
func TestTeamPermission_AgentNotFoundRequeues(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetTeams()
	mockServer.ResetAgents()
	teamReconciler.ResetImplicitDefaultCache()
	ensureNoTeam(t, ctx, "team-perm-ghost")
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionTeam(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoTeam(t, context.Background(), "team-perm-ghost")
	})

	cr := teamSampleCR("team-perm-ghost")
	cr.Spec.Permission = &litellmv1alpha1.PermissionSpec{Agents: []string{"ghost"}}
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create Team: %v", err)
	}

	tm := pollTeamCondition(t, ctx, "team-perm-ghost", reasonAgentNotFound, 30*time.Second)
	c := apimeta.FindStatusCondition(tm.Status.Conditions, conditionTypeReady)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != reasonAgentNotFound {
		t.Fatalf("Ready condition: want False/AgentNotFound, got %+v", c)
	}
	if got := mockServer.MutationsByTeamAlias("team-perm-ghost"); got != 0 {
		t.Errorf("MutationsByTeamAlias: want 0 (parked before /team/new), got %d", got)
	}
}

// TestTeamPermission_OverridesParamsModels — a permission block deletes a
// colliding spec.params.models key (permission wins).
func TestTeamPermission_OverridesParamsModels(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetTeams()
	mockServer.ResetAgents()
	teamReconciler.ResetImplicitDefaultCache()
	ensureNoTeam(t, ctx, "team-perm-override")
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionTeam(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoTeam(t, context.Background(), "team-perm-override")
	})

	cr := teamSampleCR("team-perm-override")
	cr.Spec.Params = runtime.RawExtension{Raw: []byte(`{"models":["stale-from-params"]}`)}
	cr.Spec.Permission = &litellmv1alpha1.PermissionSpec{Models: []string{"gpt-4o"}}
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create Team: %v", err)
	}

	pollTeamCondition(t, ctx, "team-perm-override", reasonSynced, 30*time.Second)
	body := mockServer.LastTeamBody("team-perm-override")
	models, _ := body["models"].([]any)
	if len(models) != 1 || models[0] != "gpt-4o" {
		t.Errorf("body.models: want [gpt-4o] (permission wins over params), got %v", body["models"])
	}
}

// TestTeamPermission_AbsentBlockPassesParamsThrough — with NO permission
// block, spec.params.models still passes through unchanged (migration path).
func TestTeamPermission_AbsentBlockPassesParamsThrough(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetTeams()
	mockServer.ResetAgents()
	teamReconciler.ResetImplicitDefaultCache()
	ensureNoTeam(t, ctx, "team-perm-absent")
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionTeam(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoTeam(t, context.Background(), "team-perm-absent")
	})

	cr := teamSampleCR("team-perm-absent")
	cr.Spec.Params = runtime.RawExtension{Raw: []byte(`{"models":["passthrough-model"]}`)}
	// No cr.Spec.Permission.
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create Team: %v", err)
	}

	pollTeamCondition(t, ctx, "team-perm-absent", reasonSynced, 30*time.Second)
	body := mockServer.LastTeamBody("team-perm-absent")
	models, _ := body["models"].([]any)
	if len(models) != 1 || models[0] != "passthrough-model" {
		t.Errorf("body.models: want passthrough [passthrough-model], got %v", body["models"])
	}
}
```

- [ ] **Step 2: Confirm the tests reference existing symbols**

The tests use `litellmv1alpha1.PermissionSpec` — verify `litellmv1alpha1` is already imported in the test file: `grep -n 'litellmv1alpha1' internal/controller/team_controller_test.go | head -1`. If not aliased that way, match the file's existing import alias.

- [ ] **Step 3: Run the new tests**

Run: `make test-envtest-pkg PKG=./internal/controller/... FOCUS=TestTeamPermission`
Expected: PASS (all five). If envtest fails at suite SETUP with `WaitForCacheSync: did not sync within 30s` or mass ~30s poll-timeouts, that is host starvation, NOT a code failure — re-run or rely on CI's Envtest job.

- [ ] **Step 4: Run the full team test group to check for regressions**

Run: `make test-envtest-pkg PKG=./internal/controller/... FOCUS=TestTeam`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/controller/team_controller_test.go
git commit -m "test(team): envtest coverage for spec.permission projection"
```

---

### Task 5: Documentation, example, and TEAM-03/TEAM-04 reversal

**Files:**
- Modify: `api/litellm/v1alpha1/team_types.go` (TEAM-03/TEAM-04 doc comments)
- Modify: `docs/user-guide/team.md`
- Modify: `examples/example-deploy/07-team.yaml`
- Modify: `CLAUDE.md`
- Generated: `docs/api-reference/**`

- [ ] **Step 1: Reverse the TEAM-03/TEAM-04 doc comments in team_types.go**

In `api/litellm/v1alpha1/team_types.go`, the `TeamSpec` doc-comment block currently lists `models` / `object_permission` as UNMANAGED under TEAM-03 and TEAM-04. Replace the TEAM-03 bullet that reads:

```
// - any resource-allowlist field projecting to LiteLLM `models` /
// `object_permission.*`: runtime resource gating is delegated to an
// external system at the per-Environment level, NOT on the Team. Spec §6.7.
```

with:

```
// - resource gating projecting to LiteLLM `models` and
// `object_permission.*` is now MANAGED via the typed `spec.permission`
// sub-block (see PermissionSpec). This REVERSES the original _FINALv3
// delegation: when `spec.permission` is present the operator OWNS those
// LiteLLM fields, so out-of-band UI edits to `models` / `object_permission`
// do NOT survive reconciliation. When the block is ABSENT the original
// delegated/passthrough behavior is preserved (raw `spec.params.models` /
// `spec.params.object_permission` forward unchanged).
```

And in the TEAM-04 paragraph, replace the sentence:

```
// Unmanaged top-level fields
// (`members_with_roles`, `models`, `object_permission`) are still
// unmanaged if the user puts them inside `params`; LiteLLM accepts them on
// create, the operator does not enumerate or revert them on subsequent
// reconciles.
```

with:

```
// `members_with_roles` remains unmanaged if placed inside `params`. `models`
// and `object_permission` inside `params` are ALSO passthrough-unmanaged
// ONLY when `spec.permission` is absent; when `spec.permission` is present
// the operator deletes those params keys (ProjectionOverride Event) and owns
// the projected values (see PermissionSpec).
```

- [ ] **Step 2: Add a `spec.permission` section to team.md**

In `docs/user-guide/team.md`, after the `## Reserved overlay keys — `ProjectionOverride` warning` section (line ~130) and before `## `Team/default` carve-out`, insert:

````markdown
## Resource permissions — `spec.permission`

`spec.permission` is a typed, operator-MANAGED block that controls which
models, MCP servers, and A2A agents a team may use. When present, the
operator OWNS the projected LiteLLM `models` and `object_permission` fields —
out-of-band UI edits to them do NOT survive reconciliation.

```yaml
spec:
  permission:
    models:      ["gpt-4o"]        # specific model names
    modelGroups: ["anthropic"]     # model access-group names (merged into models)
    mcpServers:  ["hindsight"]     # specific MCP server names/aliases
    mcpGroups:   ["team-a"]        # MCP access-group names
    agents:      ["planner"]       # A2A agent NAMES (resolved to UUIDs by the operator)
    agentGroups: ["grp-a"]         # A2A agent access-group names (see no-op note)
```

Projection to LiteLLM:

| CR field       | LiteLLM target                              |
|----------------|---------------------------------------------|
| `models` + `modelGroups` | top-level `team.models` (one merged list) |
| `mcpServers`   | `object_permission.mcp_servers`             |
| `mcpGroups`    | `object_permission.mcp_access_groups`       |
| `agents`       | `object_permission.agents` (name→UUID resolved) |
| `agentGroups`  | `object_permission.agent_access_groups` (**no-op**, see below) |

**Agent name resolution.** LiteLLM enforces `object_permission.agents` on
agent `agent_id` UUIDs and silently ignores names. The operator resolves each
name via `GET /v1/agents`. If a referenced agent is not yet registered (the
`LiteLLMA2AAgent` CR has not reconciled), the team is parked
`Ready=False, reason=AgentNotFound` (listing the missing names) and requeued —
it recovers automatically once the agent appears.

**`agentGroups` is a no-op.** LiteLLM 1.83.10 has no API to tag an agent into
an access group, so `object_permission.agent_access_groups` is never enforced.
The field is retained for forward-compat; the operator emits a Warning
`AgentGroupsNoOp` Event when it is non-empty.

**Empty vs absent.** An absent `spec.permission` block leaves any raw
`spec.params.models` / `spec.params.object_permission` untouched (passthrough).
A present block with an empty sublist (`agents: []`) omits that key entirely —
an empty allowed list means "allow all" in LiteLLM, so it is never sent as
`[]`. Because of this, an empty block cannot CLEAR a stale out-of-band
`object_permission`; remove it in LiteLLM once, or supply a non-empty block.

**Precedence.** With `spec.permission` present, any `models` or
`object_permission` key inside `spec.params` is dropped and a
`ProjectionOverride` Warning Event fires — the typed block always wins.

### Migration from `spec.params.object_permission`

Teams currently using `spec.params.object_permission` (or
`spec.params.models`) continue to work unchanged as long as `spec.permission`
is absent. To adopt the typed block, move each value:

```yaml
# before
spec:
  params:
    models: ["gpt-4o"]
    object_permission:
      mcp_servers: ["hindsight"]
# after
spec:
  permission:
    models:     ["gpt-4o"]
    mcpServers: ["hindsight"]
```

Note that `object_permission.agents` in the old form required raw UUIDs; the
new `spec.permission.agents` takes human-friendly NAMES instead.
````

- [ ] **Step 3: Update the team.md intro line**

In `docs/user-guide/team.md` line ~11, replace:

```
The operator does NOT manage team membership, models allow-list, or
object permissions. Those delegated external identity / user-
```

with:

```
The operator does NOT manage team membership (delegated to external identity).
Models allow-list and object permissions ARE managed via the typed
`spec.permission` block (see below); when that block is absent they remain
raw `spec.params` passthrough.
```

(Adjust the following wrapped line accordingly so the paragraph reads cleanly.)

- [ ] **Step 4: Add a permission example to 07-team.yaml**

In `examples/example-deploy/07-team.yaml`, under the `LiteLLMTeam` `spec:`, add a commented `permission:` block near `params: {}`:

```yaml
  # Typed, operator-MANAGED resource permissions. When present the operator
  # OWNS the projected LiteLLM `models` + `object_permission` fields (out-of-band
  # UI edits do not survive). agents take human-friendly NAMES (resolved to
  # agent_id UUIDs via GET /v1/agents); agentGroups is a forward-compat no-op.
  # permission:
  #   models:      ["gpt-4o"]
  #   modelGroups: ["anthropic"]
  #   mcpServers:  ["hindsight"]
  #   mcpGroups:   ["team-a"]
  #   agents:      ["planner"]
```

- [ ] **Step 5: Add a CLAUDE.md failure-mode note**

In `CLAUDE.md`, under "Common failure modes", add:

````markdown
### ❌ Team `spec.permission.agents` uses UUIDs, or team stuck `AgentNotFound`
```yaml
spec:
  permission:
    agents: ["3f9c-uuid-..."]   # WRONG — pass the human NAME, not the UUID
```
✅ `spec.permission.agents` takes A2A agent NAMES; the operator resolves them
to `agent_id` UUIDs via `GET /v1/agents` (LiteLLM enforces `object_permission.agents`
on UUIDs and silently ignores names). If a name isn't registered yet the team
parks `Ready=False, reason=AgentNotFound` and requeues — create the
`LiteLLMA2AAgent` CR first (ordering dependency), it self-heals. `spec.permission.agentGroups`
projects to `object_permission.agent_access_groups` but is a NO-OP in LiteLLM
1.83.10 (no API tags an agent into a group) — a `AgentGroupsNoOp` Warning Event
is emitted. WHY: `object_permission.agents` matches on `agent_id`; a name yields
ZERO agents. Absent `spec.permission` block → raw `spec.params.{models,object_permission}`
passthrough is preserved (migration path).
````

- [ ] **Step 6: Regenerate the CRD reference docs**

Run: `make gen-crd-ref-docs`
Expected: `docs/api-reference/**` updated with the new `permission` field. Do NOT hand-edit these files.

- [ ] **Step 7: Commit**

```bash
git add api/litellm/v1alpha1/team_types.go docs/user-guide/team.md examples/example-deploy/07-team.yaml CLAUDE.md docs/api-reference
git commit -m "docs(team): document spec.permission block, reverse TEAM-03/04 unmanaged note"
```

---

### Task 6: Full verification gate + release

**Files:** none (verification + release only).

- [ ] **Step 1: Regenerate everything and confirm no drift**

Run: `make gen-code gen-manifests gen-crd-ref-docs`
Then: `git status --porcelain`
Expected: empty (all generated artifacts already committed in Tasks 1 & 5). If anything shows up, `git add` + amend the relevant commit.

- [ ] **Step 2: Unit + lint sweep**

Run: `make test-unit && make qa-lint`
Expected: PASS, no lint findings.

- [ ] **Step 3: Full envtest (controller changes)**

Run: `make test-envtest`
Expected: PASS. (Host starvation may cause suite-SETUP timeouts — if so, trust CI's Envtest job.)

- [ ] **Step 4: E2E focused check (mandatory for controller changes)**

Per CLAUDE.md, changes to `internal/controller/` MUST confirm E2E green before push.

```bash
make e2e-full                              # cluster-up + suite; cluster KEPT
make e2e-focus FOCUS="Team"                # focused re-run if iterating
```
Expected: green. Teardown is explicit (`make cluster-down`).

- [ ] **Step 5: Push (pre-push hook runs the full gate)**

Ensure the hook is installed once: `make hooks`. Then:

```bash
git push origin main
```
The installed pre-push hook runs `make pre-push` automatically (secret scanners, SPDX, govulncheck ack-list, full lint + unit). Do NOT run `make pre-push` manually first (double-spends the gate) and NEVER use `--no-verify`. If a gate fails, fix the root cause and re-push.

- [ ] **Step 6: Cut a patch release**

```bash
make release-cut VERSION=0.7.24
```
This runs the release preconditions (on `main`, clean tree, in sync with `origin/main`), creates the empty `chore(release): v0.7.24` commit, runs the pre-push gate, and pushes to `main`. The `release.yml` workflow then runs `make test-full`, bumps manifests, builds + signs artifacts, and creates the tag as the LAST step (orphan-tag-safe). Confirm `VERSION` is the next patch above the current `v0.7.23` at cut time — re-check `git tag --list 'v*' | sort -V | tail -1` first.

---

## Self-Review

**Spec coverage:**
- Go API type additions + kubebuilder markers → Task 1 (`+optional` on each field; no `MaxItems` — see open questions).
- CRD schema changes → Task 1 Step 3 (generated).
- Projection function + reconcile hook → Tasks 2 & 3.
- `GET /v1/agents` name→id resolution (caching, failure/requeue) → Task 3 Step 4 (map built per reconcile only when agents referenced; `AgentNotFound` requeue).
- Precedence + `ProjectionOverride` events → Task 3 Step 3.
- Status Conditions → Task 3 (`reasonAgentNotFound`) + steady-state `Synced` via existing writeStatus.
- TEAM-03/04 doc update → Task 5 Step 1.
- `agentGroups` no-op handling → Task 3 Step 4 (`AgentGroupsNoOp` event) + Task 5 docs.
- Test plan (unit + envtest) → Tasks 2 & 4.
- Migration note → Task 5 Step 2 (team.md § Migration).

**Body-assembly composition (gotcha #1):** projection runs after the 7 structural overlays and the `blocked` drop, BEFORE the Step 8 `canonicalJSON` hash — so `models` / `object_permission` participate in drift detection and CREATE/UPDATE routing with no separate hash field. Confirmed against `team_controller.go` Step 7/8 structure.

**Type consistency:** `projectPermission(perm *litellmv1alpha1.PermissionSpec, agentNameToID map[string]string) (models []string, objectPermission map[string]any, missingAgents []string)` — identical signature in Tasks 2 (def) and 3 (call). `AgentEntry.AgentName` / `AgentEntry.AgentID` (json `agent_name` / `agent_id`) confirmed in `internal/litellm/types.go`. `ListAgents` returns `litellm.ErrNotFound` on empty — handled.

## Open questions (flag before/at execution)

1. **`/team/update` wholesale-replace of `object_permission` — UNVERIFIED.** The empirical checks confirmed CREATE projection. Whether `POST /team/update` clears a previously-set `object_permission` when the new body omits it (or shrinks a sublist) was not verified. The Step 6b precedence + Step 7b projection assume the body sent is authoritative. **Confirm on the live proxy / e2e (Task 6 Step 4): update a team from `mcpServers:[a,b]` to `mcpServers:[a]` and check the removed server is dropped.** If update does NOT shrink, a delete-and-recreate path (like the model `infoHash` case) may be needed — out of scope for this plan; raise as a follow-up.
2. **`MaxItems` validation** on the six string slices — skipped (YAGNI). Add if a real bound emerges.
3. **Absent-block passthrough** is the chosen migration semantic (params.models/object_permission survive when `spec.permission` is nil). Confirm this is desired vs. a hard deprecation of the params keys. The plan assumes passthrough (non-breaking).
4. **Model/MCP group name validation** — the plan trusts LiteLLM to resolve `modelGroups` / `mcpGroups` / `mcpServers` names and does not pre-validate their existence (unlike agents, which MUST resolve to UUIDs). If a group name is wrong, LiteLLM silently drops it. Pre-validation is deferred; agents are the only name→id resolution because LiteLLM structurally requires it.
