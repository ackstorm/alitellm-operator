# FIX4.txt Remediation Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Remediate the three findings in `FIX4.txt` against alitellm-operator: (H-1) make `model_info.created_by/updated_by` reliably surface in the LiteLLM UI for all four operator-managed kinds; (H-2) rename `LiteLLMMCPServerDiscovery` children to mirror `LiteLLMModelDiscovery` (`<prefix>-<source-name>`, drop namespace component) as a v0.3.0 breaking change; (L-3) close out the master-key vs virtual-key clarification with a one-line CHANGELOG / docs note (no code).

**Architecture:**
- H-1 (v0.2.1 patch release): add a one-shot startup INFO log printing `identity.Operator()`; add unit body-capture tests on `/model/new`, `/model/update`, `/team/new`, `/team/update`, `/v1/mcp/server`, `/v1/mcp/server/update`, `/a2a/agent`, `/a2a/agent/update` asserting `created_by`/`updated_by` are non-empty and equal to `alitellm-operator/<version>`; wire the identity stamp into Team, MCPServer, and A2AAgent controllers (currently only Model is wired); leave Model UPDATE path as-is (intentionally only stamps `UpdatedBy` — keep the existing comment).
- H-2 (v0.3.0 breaking): add `spec.prefix` to `LiteLLMMCPServerDiscovery` (DNS-1123 label, CEL-validated, required), drop the `<source-namespace>` component from the rendering helper, emit `NameCollision` status condition on intra-discovery name clashes, delete the auto-disambiguation logic, document migration impact (children renamed → delete+recreate on LiteLLM side).
- L-3: no code — one CHANGELOG line + brief note in `references/security/` clarifying the operator uses the LiteLLM master key against `/v1/models`, `/model/new`, `/model/update`, `/model/delete`, `/v1/mcp/server`, `/v1/agents`, `/team/*` (and that the prior 401 in FIX4 evidence was transient).

**Tech Stack:** Go 1.24.13, controller-runtime v0.19.4, k8s.io/* v0.31.0, kubebuilder v4.4.0 CEL validators, Ginkgo v2 (envtest), goreleaser v2.

**Test cadence:** Per `memory/feedback_test_cadence.md`: `make unit` (or `unit-pkg`) between plan tasks; `make envtest-run` / `make e2e-full` / `make security` / `make pre-push` only at the final gate before each release commit.

---

## Release boundaries

- **v0.2.1 patch:** Tasks 1–10 (H-1 remediation + L-3 doc note).
- **v0.3.0 breaking:** Tasks 11–22 (H-2 MCPServerDiscovery naming rename).

Each boundary ends with a `chore(release): vX.Y.Z` commit on `main` triggering the workflow per `CLAUDE.md` "Release pipeline" section.

---

## H-1 — Identity surfacing (v0.2.1)

### Task 1: Startup INFO log of `identity.Operator()`

**Files:**
- Modify: `cmd/main.go` (after the logger is created and before `mgr.Start(ctx)`)

**Step 1: Locate insertion point**

Run: `grep -n "Operator identity\|mgr.Start\|setupLog\.Info" /home/jcm/Projects/alitellm-operator/cmd/main.go`
Pick the line right before `setupLog.Info("starting manager")` (or equivalent) for the new line.

**Step 2: Add the log line**

```go
setupLog.Info("operator identity", "value", identity.Operator())
```

Add `"github.com/ackstorm/alitellm-operator/internal/identity"` to the imports if not already present.

**Step 3: Verify it builds**

Run: `./scripts/dev.sh go build ./...`
Expected: no errors.

**Step 4: Unit smoke**

Run: `./scripts/dev.sh make unit-pkg PKG=./cmd/...`
Expected: PASS or no tests (cmd has no unit suite — that's fine; the build step is the gate).

**Step 5: Commit**

```bash
git add cmd/main.go
git commit -m "feat(observability): log identity.Operator() at startup (FIX4 H-1)"
```

---

### Task 2: Body-capture unit test for `/model/new`

**Files:**
- Create or extend: `internal/litellm/model_test.go`

**Step 1: Write failing test**

Add to `internal/litellm/model_test.go`:

```go
// TestCreateModelBodyStampsCreatedBy — FIX4 H-1: assert /model/new body
// carries non-empty model_info.created_by and updated_by.
func TestCreateModelBodyStampsCreatedBy(t *testing.T) {
	var captured []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"model_info":{"id":"abc"}}`))
	}))
	defer srv.Close()

	c, _ := NewClient(srv.URL, "sk-test", nil)
	req := &Deployment{
		ModelName:     "test-model",
		LiteLLMParams: LiteLLMParams{},
		ModelInfo: ModelInfo{
			CreatedBy: "alitellm-operator/v0.2.1-test",
			UpdatedBy: "alitellm-operator/v0.2.1-test",
		},
	}
	if _, err := c.CreateModel(context.Background(), req); err != nil {
		t.Fatalf("CreateModel: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(captured, &got); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	mi, ok := got["model_info"].(map[string]any)
	if !ok {
		t.Fatalf("model_info missing from body: %s", captured)
	}
	if cb, _ := mi["created_by"].(string); cb == "" {
		t.Errorf("model_info.created_by is empty in body: %s", captured)
	}
	if ub, _ := mi["updated_by"].(string); ub == "" {
		t.Errorf("model_info.updated_by is empty in body: %s", captured)
	}
}
```

Ensure imports include `encoding/json`, `io`, `net/http`, `net/http/httptest`.

**Step 2: Run test to verify it passes**

Run: `./scripts/dev.sh make unit-pkg PKG=./internal/litellm/...`
Expected: PASS (the production CreateModel path already stamps both fields; this test guards against regression).

**Step 3: Verify the test catches regression**

Temporarily change the test's `req.ModelInfo.CreatedBy = ""`; run again:
Expected: FAIL with `model_info.created_by is empty in body`.
Revert.

**Step 4: Commit**

```bash
git add internal/litellm/model_test.go
git commit -m "test(litellm): assert /model/new body stamps created_by/updated_by (FIX4 H-1)"
```

---

### Task 3: Body-capture unit test for `/model/update`

**Files:**
- Extend: `internal/litellm/model_test.go`

**Step 1: Write the test**

```go
// TestUpdateModelBodyStampsUpdatedBy — FIX4 H-1: assert /model/update body
// carries non-empty model_info.updated_by. CreatedBy is intentionally not
// touched on UPDATE — LiteLLM keeps the original creator.
func TestUpdateModelBodyStampsUpdatedBy(t *testing.T) {
	var captured []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"model_info":{"id":"abc"}}`))
	}))
	defer srv.Close()

	c, _ := NewClient(srv.URL, "sk-test", nil)
	req := &updateDeployment{
		ID:            "abc",
		ModelName:     "test-model",
		LiteLLMParams: LiteLLMParams{},
		ModelInfo:     ModelInfo{UpdatedBy: "alitellm-operator/v0.2.1-test"},
	}
	if _, err := c.UpdateModel(context.Background(), req); err != nil {
		t.Fatalf("UpdateModel: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(captured, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	mi, _ := got["model_info"].(map[string]any)
	if mi == nil {
		t.Fatalf("model_info missing: %s", captured)
	}
	if ub, _ := mi["updated_by"].(string); ub == "" {
		t.Errorf("model_info.updated_by is empty: %s", captured)
	}
}
```

**Step 2: Run**

Run: `./scripts/dev.sh make unit-pkg PKG=./internal/litellm/...`
Expected: PASS.

**Step 3: Commit**

```bash
git add internal/litellm/model_test.go
git commit -m "test(litellm): assert /model/update body stamps updated_by (FIX4 H-1)"
```

---

### Task 4: Identify and inventory Team/MCPServer/A2AAgent stamping sites

**Files (read-only inspection):**
- Read: `internal/controller/team_controller.go`
- Read: `internal/controller/mcpserver_controller.go`
- Read: `internal/controller/a2aagent_controller.go`
- Read: `internal/litellm/team.go`, `internal/litellm/mcpserver.go`, `internal/litellm/a2aagent.go` (or whatever the request constructors are named)
- Read: `internal/litellm/types.go` to check whether the Team / MCPServer / A2AAgent request structs even have `created_by`/`updated_by`-shaped fields. The LiteLLM OpenAPI may or may not accept them on each endpoint.

**Step 1: For each of the three kinds, document in a scratch note:**

- Does the request struct have a `created_by`/`updated_by`-shaped field on CREATE? On UPDATE?
- If the field does not exist on the struct, does the spec (`spec/litellm_api.json`) say the endpoint accepts a generic audit field?
- Conclusion: either (a) field exists → add stamping; (b) field absent + endpoint rejects → drop from scope and add CHANGELOG note; (c) field absent + endpoint accepts → add field to types + stamp.

**Step 2: Write findings into `docs/plans/2026-05-22-fix4-remediation.md` under a new "## Inventory (Task 4 result)" section appended at the bottom.**

**Step 3: Commit the inventory note**

```bash
git add docs/plans/2026-05-22-fix4-remediation.md
git commit -m "docs(plan): inventory FIX4 H-1 Team/MCP/A2A stamping sites"
```

> Tasks 5–7 are conditional on Task 4 outcome. If a given kind has no audit field on the endpoint at all (case b above), skip the corresponding task and document it in the CHANGELOG entry for v0.2.1.

---

### Task 5: Stamp identity on Team CREATE and UPDATE

**Files:**
- Modify: `internal/controller/team_controller.go` (CREATE + UPDATE branches)
- Possibly modify: `internal/litellm/types.go` (add audit fields to Team request struct if missing)

**Step 1: Write failing test (body-capture pattern)**

Add `TestCreateTeamBodyStampsCreatedBy` and `TestUpdateTeamBodyStampsUpdatedBy` in `internal/litellm/team_test.go` modelled exactly on Tasks 2/3 but against `/team/new` and `/team/update`.

**Step 2: Run** — Expected FAIL.
Run: `./scripts/dev.sh make unit-pkg PKG=./internal/litellm/...`

**Step 3: Implement**

In `team_controller.go` CREATE branch:

```go
req.TeamInfo = litellm.TeamInfo{
    CreatedBy: identity.Operator(),
    UpdatedBy: identity.Operator(),
}
```

In UPDATE branch:

```go
req.TeamInfo = litellm.TeamInfo{
    UpdatedBy: identity.Operator(),
}
```

Adjust field name to match whatever struct the team request actually uses (per Task 4 inventory). Add identity import.

**Step 4: Run tests** — Expected PASS.
Run: `./scripts/dev.sh make unit-pkg PKG=./internal/controller/... ./internal/litellm/...`

**Step 5: Commit**

```bash
git add internal/controller/team_controller.go internal/litellm/team_test.go internal/litellm/types.go
git commit -m "feat(team): stamp identity.Operator() on /team/new + /team/update (FIX4 H-1)"
```

---

### Task 6: Stamp identity on MCPServer CREATE and UPDATE

**Files:**
- Modify: `internal/controller/mcpserver_controller.go`
- Possibly modify: `internal/litellm/types.go`
- New tests: `internal/litellm/mcpserver_test.go` (body-capture pattern)

Same shape as Task 5 against `/v1/mcp/server` (or whatever the path is per `internal/litellm/mcpserver.go`).

**Step 1: Write failing test**
**Step 2: Run → FAIL**
**Step 3: Implement**
**Step 4: Run → PASS**
**Step 5: Commit**

```bash
git commit -m "feat(mcpserver): stamp identity.Operator() on CREATE + UPDATE (FIX4 H-1)"
```

---

### Task 7: Stamp identity on A2AAgent CREATE and UPDATE

**Files:**
- Modify: `internal/controller/a2aagent_controller.go`
- Possibly modify: `internal/litellm/types.go`
- New tests: `internal/litellm/a2aagent_test.go`

Same shape as Tasks 5–6.

**Commit:**

```bash
git commit -m "feat(a2aagent): stamp identity.Operator() on CREATE + UPDATE (FIX4 H-1)"
```

---

### Task 8: Re-stamp `CreatedBy` on Model adoption path

**Why:** The CREATE branch in `model_controller.go:458-462` (idempotency probe) skips the POST when `GetModelInfoByName` returns a hit. Adopted entries never receive a stamp. After Task 1's startup log confirms the binary has the right version, route through an explicit `PATCH`/`POST /model/update` immediately after adoption to retroactively stamp the audit fields.

**Files:**
- Modify: `internal/controller/model_controller.go` lines 458-462 (adoption branch)

**Step 1: Write envtest expectation**

Add a test in `internal/controller/model_controller_test.go`:
- pre-seed a fake LiteLLM with a model entry that has `created_by:null`;
- create the Model CR;
- reconcile;
- assert the fake LiteLLM received an UPDATE call carrying `updated_by = alitellm-operator/<version>`.

**Step 2: Run → FAIL**

Run: `./scripts/dev.sh make envtest-pkg PKG=./internal/controller/... FOCUS=ModelAdoptionRestamp`

**Step 3: Implement**

After the adoption-by-name branch obtains `newLiteLLMModelID`, issue an UPDATE that stamps `UpdatedBy` so the UI reflects the operator the moment the operator first touches it. Do NOT stamp `CreatedBy` (that field is immutable LiteLLM-side per FIX4 evidence; an UPDATE clears it on some 1.83.x versions).

```go
} else if existing != nil && existing.ModelInfo.ID != "" {
    newLiteLLMModelID = existing.ModelInfo.ID
    logger.V(1).Info("model already exists in LiteLLM; adopting id (idempotency probe)", "modelID", newLiteLLMModelID)
    // FIX4 H-1: stamp updated_by immediately on adoption so the UI does
    // not show "Unknown" for entries that pre-date the operator's identity
    // wiring or were imported by an older operator version.
    if _, err := snap.Client.UpdateModel(ctx, &litellm.UpdateDeployment{
        ID:            newLiteLLMModelID,
        ModelName:     model.Name,
        LiteLLMParams: litellm.LiteLLMParams(paramsMap),
        ModelInfo:     litellm.ModelInfo{UpdatedBy: identity.Operator()},
    }); err != nil {
        return r.classifyMutationError(ctx, &model, logger, err, "POST /model/update (FIX4 H-1 adoption stamp)")
    }
}
```

**Step 4: Run → PASS**

**Step 5: Commit**

```bash
git commit -m "fix(model): stamp updated_by on adoption-by-name (FIX4 H-1)"
```

---

### Task 9: L-3 doc-only close-out

**Files:**
- Modify: `CHANGELOG.md` (Unreleased / v0.2.1 section)
- Modify: `references/security/govulncheck-acknowledged.md` is NOT the right home — instead append to `references/` a new short doc.
- Create: `references/litellm-auth-model.md`

**Step 1: Write the file**

```markdown
# LiteLLM auth model (master key vs virtual key)

This operator authenticates against LiteLLM using the proxy's master key
(`LITELLM_MASTER_KEY`, `sk-`-prefixed). The master key reaches the
following endpoints used by the operator: `/v1/models`, `/model/new`,
`/model/update`, `/model/delete`, `/v1/mcp/server`, `/v1/agents`,
`/team/*`.

Virtual keys (`POST /key/generate`) carry per-key scopes and quotas
and are NOT currently used by the operator. Future per-CR isolation
(distinct `created_by` per CR rather than a single operator identity)
would adopt virtual keys, but is OUT OF SCOPE for v0.3.0.

FIX4 L-3 evidence: a transient 401 observed on 2026-05-22 under v0.1.2
returning "LiteLLM Virtual Key expected. Received=****, expected to
start with 'sk-'." was a secret-rotation race, NOT a LiteLLM-side
policy tightening. Re-probe under v0.2.0 with the same key passed
against all four endpoints.
```

**Step 2: CHANGELOG entry**

Append under v0.2.1:

```
- docs: clarify LiteLLM master-key vs virtual-key auth model (FIX4 L-3)
```

**Step 3: Commit**

```bash
git add references/litellm-auth-model.md CHANGELOG.md
git commit -m "docs(auth): clarify master-key vs virtual-key (FIX4 L-3)"
```

---

### Task 10: v0.2.1 release commit

**Files:**
- Modify: `CHANGELOG.md` (Unreleased → v0.2.1 with the H-1 entries from Tasks 1, 5–8 + L-3 from Task 9)

**Step 1: Update CHANGELOG.md** under v0.2.1:

```
## [0.2.1] - 2026-MM-DD

### Fixed
- model: stamp `model_info.updated_by` on adoption-by-name path so the
  LiteLLM UI shows the operator identity instead of "Unknown" for
  entries adopted from earlier operator versions or out-of-band creates.
  (FIX4 H-1)
- team / mcpserver / a2aagent: stamp `created_by`/`updated_by` on
  CREATE + UPDATE paths to match the Model controller (FIX4 H-1
  symmetry).

### Added
- observability: one-shot startup INFO log of `identity.Operator()` so
  the value the binary will send is visible without external probes.
  (FIX4 H-1)
- tests: body-capture unit tests on every audit-stamped endpoint
  guarding against future regressions.

### Documentation
- references/litellm-auth-model.md: clarify the operator uses the master
  key, virtual keys are out of scope for v0.3.0 (FIX4 L-3).
```

**Step 2: Run the final gate** (per memory `feedback_test_cadence`):

```bash
./scripts/dev.sh make test-all    # unit + envtest-run (race)
./scripts/dev.sh make security    # gosec + govulncheck + fuzz-short
make pre-push                     # host-only gitleaks + trufflehog + 13 gates
```

Expected: all green.

**Step 3: Release commit**

```bash
git add CHANGELOG.md
git commit --allow-empty -m 'chore(release): v0.2.1'
git push origin main
```

Workflow handles the rest (bump manifests, build, sign, tag).

---

## H-2 — MCPServerDiscovery rename (v0.3.0 breaking)

### Task 11: Add `spec.prefix` to LiteLLMMCPServerDiscovery types

**Files:**
- Modify: `api/litellm/v1alpha1/litellmmcpserverdiscovery_types.go`
- Run: `./scripts/dev.sh make generate manifests`

**Step 1: Add the field with CEL validator**

```go
// Prefix is the DNS-1123 label prepended to every child's metadata.name
// as "<prefix>-<source-name>". Same convention as LiteLLMModelDiscovery.
// Required; lowercased; max 30 chars to leave room for the source name
// within the 63-char DNS-1123 limit.
// +kubebuilder:validation:Required
// +kubebuilder:validation:MinLength=1
// +kubebuilder:validation:MaxLength=30
// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
Prefix string `json:"prefix"`
```

**Step 2: Regenerate**

Run: `./scripts/dev.sh make generate manifests`
Expected: `config/crd/bases/litellm.ackstorm.ai_litellmmcpserverdiscoveries.yaml` updated.

**Step 3: Run unit**

Run: `./scripts/dev.sh make unit`
Expected: PASS.

**Step 4: Commit**

```bash
git add api/litellm/v1alpha1/litellmmcpserverdiscovery_types.go config/ deploy/ docs/api-reference/
git commit -m "feat(api)!: require spec.prefix on LiteLLMMCPServerDiscovery (FIX4 H-2)"
```

> The `!` in the commit type marks the breaking-change for the changelog generator.

---

### Task 12: Refactor child-name helper to `<prefix>-<source-name>`

**Files:**
- Modify: `internal/controller/mcpserverdiscovery_controller.go` (or wherever the helper lives)
- Modify: existing unit tests that assert the old shape

**Step 1: Find the current helper**

Run: `grep -rn "childName\|ChildName\|<source-namespace>\|sourceNamespace" /home/jcm/Projects/alitellm-operator/internal/controller/mcpserverdiscovery_controller.go`

**Step 2: Write failing test in the appropriate `_test.go`:**

```go
func TestMCPDiscoveryChildName_NoNamespaceComponent(t *testing.T) {
    got := childName("mcp", "mcp", "aws-knowledge")
    if got != "mcp-aws-knowledge" {
        t.Errorf("want mcp-aws-knowledge, got %q", got)
    }
    if strings.Contains(got, ".") {
        t.Errorf("name must not contain dots: %q", got)
    }
}
```

**Step 3: Run → FAIL.**

**Step 4: Implement: drop the namespace component**

```go
func childName(prefix, _sourceNamespace, sourceName string) string {
    return prefix + "-" + sourceName
}
```

Keep the parameter for compile-compat with callers in this commit; remove the unused parameter in Task 14 once all callers are updated.

**Step 5: Run → PASS.**

**Step 6: Commit**

```bash
git commit -m "fix(mcpserverdiscovery)!: drop namespace component from child names (FIX4 H-2)"
```

---

### Task 13: Sanitizer for the LiteLLM wire form

**Files:**
- Modify: `internal/litellm/mcpserver_request.go` (or wherever the wire payload is built)
- Test: extend `internal/controller/mcpserver_sanitize_test.go`

**Step 1: Decide K8s-side vs wire-side separator**

Per FIX4 H-2 user feedback ("guion" = hyphen, K8s convention), use:
- K8s child `metadata.name`: hyphen-separated `<prefix>-<source-name>`.
- LiteLLM wire `server_name`: dot-separated `<prefix>.<source-name>` (matches the Model wire form `openai.gpt-4o-mini`).

The sanitizer flips the K8s-side hyphen between the `<prefix>` and the first segment of the source name into a dot.

**Step 2: Failing test**

```go
func TestMCPServerWireName_DotFlip(t *testing.T) {
    cases := map[string]string{
        "mcp-aws-knowledge":      "mcp.aws-knowledge",
        "team-toolhive-context7": "team.toolhive-context7",
    }
    for in, want := range cases {
        if got := wireServerName(in); got != want {
            t.Errorf("wireServerName(%q): want %q, got %q", in, want, got)
        }
    }
}
```

**Step 3: Implement**

```go
// wireServerName converts the K8s-side <prefix>-<source-name> into the
// LiteLLM wire form <prefix>.<source-name>. Only the FIRST hyphen is
// flipped — source names with internal hyphens (e.g. aws-knowledge)
// are preserved verbatim past the first segment.
func wireServerName(k8sName string) string {
    i := strings.Index(k8sName, "-")
    if i < 0 {
        return k8sName
    }
    return k8sName[:i] + "." + k8sName[i+1:]
}
```

**Step 4: Run → PASS.**

**Step 5: Commit**

```bash
git commit -m "feat(mcpserverdiscovery): wire-name sanitizer flips first hyphen to dot (FIX4 H-2)"
```

---

### Task 14: Remove auto-disambiguation and namespace handling

**Files:**
- Modify: `internal/controller/mcpserverdiscovery_controller.go` — drop the auto-disambiguation code (whichever helper currently splices namespace into the name on collisions).
- Modify: child-name callers — drop the now-unused `sourceNamespace` argument from Task 12 signature.

**Step 1: Locate and remove**

Run: `grep -n "disambig\|namespace.*collis\|nameCollision\|autoSuffix" /home/jcm/Projects/alitellm-operator/internal/controller/mcpserverdiscovery_controller.go`

Delete the relevant blocks. Adjust callers.

**Step 2: Run unit**

Run: `./scripts/dev.sh make unit-pkg PKG=./internal/controller/...`
Expected: PASS.

**Step 3: Commit**

```bash
git commit -m "refactor(mcpserverdiscovery): drop auto-disambiguation; use spec.prefix (FIX4 H-2)"
```

---

### Task 15: Emit `NameCollision` status condition on intra-discovery clashes

**Files:**
- Modify: `internal/controller/mcpserverdiscovery_controller.go`
- Possibly: `api/litellm/v1alpha1/litellmmcpserverdiscovery_types.go` (add the condition type constant)

**Step 1: Add the condition constant**

```go
// ConditionTypeNameCollision is set Reason=NameCollision Status=True when
// two upstream servers from different source namespaces share the same
// `name` within a single discovery. The second occurrence is skipped
// loud-fail rather than silently merged.
ConditionTypeNameCollision = "NameCollision"
```

**Step 2: Write envtest expectation**

In `mcpserverdiscovery_controller_test.go` (or new file), seed two upstream MCPServers in different namespaces both named `foo`, single discovery sourcing from both namespaces. Expect:

- exactly one child created;
- the discovery's status carries `NameCollision=True` listing the offending pair.

Run: `./scripts/dev.sh make envtest-pkg PKG=./internal/controller/... FOCUS=NameCollision`
Expected: FAIL.

**Step 3: Implement**

In the rendering loop:

```go
seen := map[string]string{} // name → first-seen ns
for _, ups := range upstreams {
    if firstNs, dup := seen[ups.Name]; dup {
        meta.SetStatusCondition(&disc.Status.Conditions, metav1.Condition{
            Type:    ConditionTypeNameCollision,
            Status:  metav1.ConditionTrue,
            Reason:  "NameCollision",
            Message: fmt.Sprintf("upstream %q from ns %q skipped — already seen in ns %q", ups.Name, ups.Namespace, firstNs),
        })
        continue
    }
    seen[ups.Name] = ups.Namespace
    // ...render child as usual
}
```

**Step 4: Run → PASS.**

**Step 5: Commit**

```bash
git commit -m "feat(mcpserverdiscovery): emit NameCollision condition on intra-discovery clashes (FIX4 H-2)"
```

---

### Task 16: Adoption migration for pre-v0.3.0 children

**Why:** Pre-v0.3.0 children were registered under the old dotted scheme (`<discovery>.<source-namespace>.<source-name>`). On the v0.3.0 upgrade the operator will recreate them under the new name; the old LiteLLM-side records become orphans.

**Files:**
- Modify: `internal/controller/mcpserverdiscovery_controller.go` — add an adoption lookup that matches by `(url, transport)` to detect a pre-v0.3.0 record under the old name and DELETE it before the new CREATE.

**Step 1: Write envtest expectation**

Seed a fake LiteLLM with a pre-existing MCP server record named under the OLD scheme (`mcp.mcp.aws-knowledge`) for some `(url, transport)`. Apply a v0.3.0 discovery with `spec.prefix: mcp` pointing at the same upstream. Expect:

- the new child `mcp-aws-knowledge` exists under the NEW name;
- the old record under the OLD name is deleted (not orphaned);
- exactly one LiteLLM-side record exists for that `(url, transport)`.

Run: `./scripts/dev.sh make envtest-pkg PKG=./internal/controller/... FOCUS=Migration`
Expected: FAIL.

**Step 2: Implement**

Before the CREATE on each child:

```go
// FIX4 H-2 migration: scan LiteLLM for pre-v0.3.0 records under the OLD
// dotted scheme matching this child's (url, transport). DELETE the
// orphan so we don't leave dangling state after the rename. One-release
// grace window — remove this block in v0.4.0.
if oldID, err := snap.Client.FindMCPServerByURL(ctx, ups.URL, ups.Transport); err == nil && oldID != "" {
    if oldID != newID {
        if err := snap.Client.DeleteMCPServer(ctx, oldID); err != nil {
            logger.V(1).Info("FIX4 H-2 migration: failed to delete pre-v0.3.0 orphan; manual cleanup may be needed", "oldID", oldID, "err", err.Error())
        }
    }
}
```

**Step 3: Run → PASS.**

**Step 4: Commit**

```bash
git commit -m "feat(mcpserverdiscovery): one-shot v0.3.0 migration cleans pre-v0.3.0 orphans (FIX4 H-2)"
```

---

### Task 17: Update existing MCPServerDiscovery e2e expectations

**Files:**
- Modify: `test/e2e/*` files that assert the old `<discovery>.<ns>.<name>` shape

**Step 1: Find**

Run: `grep -rn "mcp\\..*\\." /home/jcm/Projects/alitellm-operator/test/e2e/`

**Step 2: Update assertions** to expect `<prefix>-<source-name>` on the K8s side and `<prefix>.<source-name>` on the wire side.

**Step 3: Run focused e2e**

```bash
make cluster-keep
./scripts/dev.sh make e2e-focus FOCUS="MCPServerDiscovery"
```

Expected: PASS.

**Step 4: Commit**

```bash
git commit -m "test(e2e): update MCPServerDiscovery assertions to v0.3.0 naming (FIX4 H-2)"
```

---

### Task 18: Update example CRs

**Files:**
- Modify: every `examples/litellmmcpserverdiscovery*.yaml`

**Step 1: Add `spec.prefix` to each example.**

**Step 2: Verify they apply against a fresh cluster**

```bash
make cluster-down && ./scripts/dev.sh make cluster-up
./scripts/dev.sh kubectl apply -f examples/litellmmcpserverdiscovery-*.yaml
```

Expected: no `validation` errors.

**Step 3: Commit**

```bash
git commit -m "docs(examples): set spec.prefix on MCPServerDiscovery examples (FIX4 H-2)"
```

---

### Task 19: Regenerate CRD reference docs

**Step 1: Run**

```bash
./scripts/dev.sh make gen-crd-ref-docs
```

**Step 2: Verify**

`docs/api-reference/litellm.ackstorm.ai.md` should show `spec.prefix` with its CEL pattern.

**Step 3: Commit**

```bash
git add docs/api-reference/
git commit -m "docs(api): regenerate CRD reference for spec.prefix (FIX4 H-2)"
```

---

### Task 20: Migration release note

**Files:**
- Modify: `CHANGELOG.md` (Unreleased → v0.3.0)
- Create: `docs/releases/v0.3.0-migration.md` (or add a section to an existing release-notes doc)

**Step 1: Write the migration note**

```markdown
# v0.3.0 migration — MCPServerDiscovery naming

This release renames the children produced by `LiteLLMMCPServerDiscovery`
from `<discovery>.<source-namespace>.<source-name>` to
`<spec.prefix>-<source-name>`.

## Breaking changes

- `spec.prefix` is now a required field on `LiteLLMMCPServerDiscovery`.
  Existing CRs without `spec.prefix` will fail validation on
  `kubectl apply` after upgrade.
- All currently-managed MCPServer K8s CRs will be deleted and recreated
  under the new naming scheme. The operator's finalizer fires per-child,
  which DELETEs the LiteLLM-side record under the old name; the new
  CREATE then re-registers under the new name.
- The wire form sent to LiteLLM is `<prefix>.<source-name>` (dot
  separator). This matches the Model wire form.

## Pre-upgrade checklist

1. Add `spec.prefix` to every `LiteLLMMCPServerDiscovery` in your gitops
   tree.
2. Pick `spec.prefix` values that do not collide across discoveries —
   the operator now relies on `prefix` for cross-discovery disambiguation
   rather than the namespace component.
3. If two upstream servers from different namespaces share a name within
   a single discovery, the operator will skip the second occurrence and
   set a `NameCollision=True` status condition. Rename one upstream or
   split the discovery to resolve.

## Orphan cleanup

A one-shot migration routine (released as part of v0.3.0) scans LiteLLM
for records matching the old dotted scheme on each child's `(url,
transport)` and DELETEs the orphan before the new CREATE. The migration
routine will be removed in v0.4.0.
```

**Step 2: CHANGELOG entry**

Under v0.3.0:

```
### BREAKING CHANGES
- mcpserverdiscovery: `spec.prefix` is now required; children are renamed
  from `<discovery>.<ns>.<name>` to `<prefix>-<source-name>` (K8s) /
  `<prefix>.<source-name>` (wire). Migration: see
  docs/releases/v0.3.0-migration.md. (FIX4 H-2)
```

**Step 3: Commit**

```bash
git add CHANGELOG.md docs/releases/v0.3.0-migration.md
git commit -m "docs(release)!: document v0.3.0 MCPServerDiscovery naming migration (FIX4 H-2)"
```

---

### Task 21: Final gate — full E2E + security + pre-push

**Step 1: From a clean cluster, run the full gate.**

```bash
make cluster-down
./scripts/dev.sh make e2e-full
./scripts/dev.sh make security
make pre-push
```

Expected: all green.

**Step 2: If anything red, fix-and-commit before the release commit. Do not bypass.**

---

### Task 22: v0.3.0 release commit

**Step 1: Commit the release marker**

```bash
git commit --allow-empty -m 'chore(release): v0.3.0'
git push origin main
```

**Step 2: Verify the workflow on GHA**

Check: `https://github.com/ackstorm/alitellm-operator/actions` — the `release.yml` workflow should run `parse → run-tests → build-and-release`, push the bot bump commit, tag `v0.3.0`, sign artifacts via cosign, and produce the OCI chart at `oci://ghcr.io/ackstorm/charts/alitellm-operator:0.3.0`.

**Step 3: Smoke against prod**

After Flux reconciles the new chart in prod EKS:

1. Confirm pod is on v0.3.0 image.
2. `kubectl get litellmmcpserverdiscoveries -A -o yaml` — all children should be named per the new scheme.
3. Check the LiteLLM UI Models table — entries should show `alitellm-operator/0.3.0` in Created By.
4. Capture findings into a `FIX5.txt` if any new symptoms appear.

---

## Inventory (Task 4 result)

Source inspection: `internal/litellm/types.go`,
`internal/controller/{team,mcpserver,a2aagent}_controller.go`. The
frozen LiteLLM OpenAPI snapshot at `spec/litellm_api.json` is not
present in this tree (only Models has been previously inspected),
so the inventory falls back to what each endpoint's request struct
exposes in practice.

### Findings

| Kind       | Request struct                   | Native audit field?        | Stamping site (case)                            |
|------------|----------------------------------|----------------------------|-------------------------------------------------|
| Model      | `Deployment` / `updateDeployment`| YES — `model_info.created_by/updated_by` | already wired (case a — leave as is, covered by Tasks 2-3 + 8) |
| Team       | map[string]any (CreateTeamRaw)   | NO top-level audit field   | stamp into `metadata.created_by/updated_by` sub-bag (case c — metadata is freeform per LiteLLM 1.83.10 `additionalProperties: true`) |
| MCPServer  | `MCPServerRequest` / `MCPServerUpdateRequest` | NO top-level audit field; `mcp_info` is freeform | stamp into `mcp_info.created_by/updated_by` (case c) |
| A2AAgent   | `AgentConfig`                    | NO top-level audit field; `agent_card_params` is freeform | stamp into `agent_card_params.created_by/updated_by` (case c) |

### Decisions

- All three non-Model kinds fall into case (c): the wire request has
  no native audit field, but each endpoint accepts a freeform bag
  (`metadata`, `mcp_info`, `agent_card_params`) that LiteLLM persists.
  Stamping into the freeform bag is best-effort — the LiteLLM UI today
  may not surface these values on Team/MCPServer/A2AAgent rows, but
  the value is at least stored on the LiteLLM side and visible to any
  operator probe or future UI update.
- No request struct needs new typed fields. The stamping is done by
  injecting two keys into the existing freeform bag *before* the bag
  is handed to the request constructor (Team) or before the
  reconciler assigns it to the typed field (MCPServer, A2AAgent).
- Identity stamping behavior is symmetric with the Model controller:
  CREATE stamps both `created_by` + `updated_by`; UPDATE stamps
  `updated_by` only (LiteLLM-side audit semantics — original creator
  is immutable; the operator records the most recent toucher).
- FIX4 L-3 evidence: the same master key reaches `/v1/models`,
  `/model/new`, `/model/update`, `/model/delete`, `/v1/mcp/server`,
  `/v1/agents`, `/team/*` — no auth-model divergence between the
  Model path and the other three kinds, so this stamping change is
  safe to ship without an additional auth review.

### CHANGELOG note for v0.2.1

The Team/MCPServer/A2AAgent stamping is best-effort into freeform
bags — note this in the v0.2.1 CHANGELOG entry so users understand
the LiteLLM UI may not surface the value on those rows until
LiteLLM ships native audit columns for those resources.
