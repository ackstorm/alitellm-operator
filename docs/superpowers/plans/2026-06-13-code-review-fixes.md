# Code-Review Fixes Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fix the 13 verified correctness/observability defects (plus one efficiency cluster) found in the full-codebase max-effort review of alitellm-operator, each behind a regression test.

**Architecture:** Each phase is an independently-executable, self-contained group of TDD tasks targeting one defect (or a tightly-related pair). Phases are ordered by severity (HIGH → LOW). A phase touches one subsystem at a time (model controller, deletion paths, connection cache, modeldiscovery, guardrail, metrics, litellm client/mock). Phase 9 (the large pure-DRY refactor, finding #14) is intentionally deferred to a separate follow-up plan — see its note.

**Tech Stack:** Go 1.26, controller-runtime v0.19.4, k8s.io/* v0.31.0. Tests are pure Go `testing` (NO Ginkgo): unit tests use `httptest` + an in-package fake; controller tests use `envtest` with a shared `TestMain` and an in-process mock LiteLLM server (`internal/litellm/mock`). Host has NO Go — every `make` target self-routes into the devtools container.

---

## Conventions (read once, applies to every task)

**Test commands** (run bare on host; they auto-route into devtools):
- litellm unit tests: `make test-unit-pkg PKG=./internal/litellm/...` (no `FOCUS` var; for one test use `./scripts/dev.sh go test -v -count=1 -run TestName ./internal/litellm/...`)
- connection unit tests: `make test-unit-pkg PKG=./internal/connection/...`
- metrics unit tests: `make test-unit-pkg PKG=./internal/metrics/...`
- controller unit tests (fake client): `make test-unit-pkg PKG=./internal/controller/...`
- controller envtest (one test): `make test-envtest-pkg PKG=./internal/controller/... FOCUS=TestName`
- lint touched packages: `make qa-lint-changed`
- build: `make build-operator`

**Envtest helpers already defined in `internal/controller/*_test.go`** (do NOT redefine):
- `WatchNamespace` = `"default"`; package vars `mockServer`, `connCache`, `suiteLLMClient`.
- `modelSampleCR(name string) *litellmv1alpha1.LiteLLMModel`
- `ensureNoModel(t, ctx, name)`, `pollModelCondition(t, ctx, name, wantReason, timeout) *LiteLLMModel`
- `connDefaultCR()`, `ensureNoConnectionDefault(t, ctx)`, `pollSnapshotReason(timeout, wantReason)`
- `setConnCacheReady()` (rebuilds the shared cache `Ready:true, Client: suiteLLMClient` — NEVER a bare `Ready:true` literal, per #74)
- `updateWithRetry(ctx, key, obj, mutateFn)` — `retry.RetryOnConflict` wrapper.
- Constants: `conditionTypeReady="Ready"`, `reasonSynced="Synced"`, `reasonLiteLLMUnavailable`, `reasonSecretNotFound`, `reasonRecreateThrottled`, `modelKind`, `guardrailKind`.

**Mock helpers (`internal/litellm/mock`)**: `mockServer.SetMode(mock.ModeHappy|Mode401|Mode422|ModeTransient5xx)`, `ResetCounters()`, `ResetRecorded()`, `ResetModels()`, `Recorded() []mock.RecordedCall{Method,Path,When}`, `MutationsByModelName(name)`, `GetModelID(name)`, `HasModel(name)`, `Mutations()`, `Reads()`.

**litellm unit-test helpers (`internal/litellm/*_test.go`)**: `newTestClient(t, url) *Client`, `captureMock(t, &captured, respond)` returning `capturedRequest{Method,Path,Body}`, white-box `processLitellmError(body) (kind, msg, code string)`, `RejectedError{Method,Path,Status,Code,Type,Message}`.

**SPDX header**: every new `*.go` file MUST start with `// SPDX-License-Identifier: Apache-2.0` (pre-push gate enforces).

**No CRD changes**: none of these fixes alter a CRD schema, so `make gen-manifests` is not required unless a task says so.

---

## Phase 0: Branch setup

### Task 0: Create the working branch

**Files:** none

- [ ] **Step 1: Create branch off main**

```bash
cd /home/coder/workspace/local/alitellm-operator
git checkout main && git pull --ff-only
git checkout -b fix/code-review-2026-06-13
```

- [ ] **Step 2: Confirm clean baseline build**

Run: `make build-operator`
Expected: build succeeds, no errors.

---

## Phase 1: Model `spec.info` is dropped on the wire (Finding #1, HIGH)

`spec.info` is documented as "forwarded verbatim to LiteLLM's `model_info`" but the controller builds a typed `litellm.ModelInfo{}` that only carries id/identity/timestamps and never copies the user's `infoMap`. `ModelInfo.Extra` is `json:"-"` (never serialized) and there is no custom marshaler. Fix: make `ModelInfo` inline-merge `Extra` at marshal time, and have the controller put `infoMap` into `Extra` on all four body constructions.

### Task 1.1: `ModelInfo.MarshalJSON` inlines `Extra`

**Files:**
- Modify: `internal/litellm/types.go` (the `ModelInfo` struct block, lines 14-34)
- Test: `internal/litellm/types_test.go` (create if absent; else append)

- [ ] **Step 1: Write the failing test**

Create/append `internal/litellm/types_test.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package litellm

import (
	"encoding/json"
	"testing"
)

func TestModelInfo_MarshalJSON_MergesExtra(t *testing.T) {
	mi := ModelInfo{
		CreatedBy: "alitellm-operator/test",
		Extra: map[string]any{
			"base_model": "gpt-4o-mini",
			"tier":       "paid",
			"custom_key": "v1",
		},
	}
	b, err := json.Marshal(mi)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"created_by", "base_model", "tier", "custom_key"} {
		if _, ok := got[k]; !ok {
			t.Errorf("marshaled model_info missing key %q; got %s", k, b)
		}
	}
}

func TestModelInfo_MarshalJSON_EmptyStaysEmpty(t *testing.T) {
	// CR-16: an empty ModelInfo must NOT serialize "id":"" (omitempty) and
	// must produce a bare object (no spurious keys).
	b, err := json.Marshal(ModelInfo{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(b) != "{}" {
		t.Errorf("empty ModelInfo: want {}, got %s", b)
	}
}

func TestModelInfo_MarshalJSON_TypedFieldWinsOverExtra(t *testing.T) {
	// Operator overlay (typed field) must win over a colliding Extra key.
	mi := ModelInfo{CreatedBy: "operator", Extra: map[string]any{"created_by": "user"}}
	b, _ := json.Marshal(mi)
	var got map[string]any
	_ = json.Unmarshal(b, &got)
	if got["created_by"] != "operator" {
		t.Errorf("created_by: want operator (typed wins), got %v", got["created_by"])
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `./scripts/dev.sh go test -v -count=1 -run TestModelInfo_MarshalJSON ./internal/litellm/...`
Expected: FAIL — `base_model`/`tier`/`custom_key` absent (Extra is `json:"-"`).

- [ ] **Step 3: Add the custom marshaler**

In `internal/litellm/types.go`, immediately after the `ModelInfo` struct definition (after line 34), add:

```go
// MarshalJSON inlines Extra into the model_info object so spec.info
// pass-through keys reach LiteLLM (D-05). Typed fields take precedence on
// a key collision (operator overlay wins, e.g. created_by/id). Empty Extra
// is a no-op, preserving CR-16 omitempty semantics (empty ModelInfo → {}).
func (m ModelInfo) MarshalJSON() ([]byte, error) {
	type alias ModelInfo // shed the custom marshaler to avoid recursion; Extra stays json:"-"
	base, err := json.Marshal(alias(m))
	if err != nil {
		return nil, err
	}
	if len(m.Extra) == 0 {
		return base, nil
	}
	var merged map[string]json.RawMessage
	if err := json.Unmarshal(base, &merged); err != nil {
		return nil, err
	}
	for k, v := range m.Extra {
		if _, exists := merged[k]; exists {
			continue // typed field already set this key — operator overlay wins
		}
		raw, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		merged[k] = raw
	}
	return json.Marshal(merged)
}
```

`encoding/json` is already imported in `types.go` (line 5).

- [ ] **Step 4: Run test to verify it passes**

Run: `./scripts/dev.sh go test -v -count=1 -run TestModelInfo_MarshalJSON ./internal/litellm/...`
Expected: PASS (all three).

- [ ] **Step 5: Commit**

```bash
git add internal/litellm/types.go internal/litellm/types_test.go
git commit -m "fix(litellm): inline ModelInfo.Extra so spec.info reaches the wire"
```

### Task 1.2: Controller forwards `infoMap` via `ModelInfo.Extra` on all body paths

**Files:**
- Modify: `internal/controller/model_controller.go` (4 sites: 656-662 adoption-update, 672-694 create, 735-748 D-02 recreate, 768-778 normal update)

- [ ] **Step 1: Add `Extra: infoMap` to the CREATE body (lines 688-693)**

In `internal/controller/model_controller.go`, the create `req := &litellm.Deployment{...}` block, change the `ModelInfo` literal to include `Extra`:

```go
				ModelInfo: litellm.ModelInfo{
					CreatedBy: identity.Operator(),
					UpdatedBy: identity.Operator(),
					CreatedAt: nowStamp,
					UpdatedAt: nowStamp,
					Extra:     infoMap, // D-05: forward spec.info verbatim
				},
```

- [ ] **Step 2: Add `Extra: infoMap` to the D-02 recreate body (lines 742-748)**

```go
				ModelInfo: litellm.ModelInfo{
					ID:        "",
					CreatedBy: identity.Operator(),
					UpdatedBy: identity.Operator(),
					CreatedAt: nowStamp,
					UpdatedAt: nowStamp,
					Extra:     infoMap, // D-05: forward spec.info verbatim
				},
```

- [ ] **Step 3: Add `Extra: infoMap` to the normal UPDATE body (lines 774-777)**

```go
				ModelInfo: litellm.ModelInfo{
					ID:        model.Status.LastRendered.ModelID,
					UpdatedBy: identity.Operator(),
					Extra:     infoMap, // D-05: forward spec.info verbatim
				},
```

- [ ] **Step 4: Add `Extra: infoMap` to the adoption-update body (lines 659-662)**

```go
					ModelInfo: litellm.ModelInfo{
						ID:        newModelID,
						UpdatedBy: identity.Operator(),
						Extra:     infoMap, // D-05: forward spec.info verbatim
					},
```

- [ ] **Step 5: Verify it builds**

Run: `make build-operator`
Expected: success. (`infoMap` is in scope at all four sites — it is built at line 428 and mutated at line 484.)

- [ ] **Step 6: Commit**

```bash
git add internal/controller/model_controller.go
git commit -m "fix(model): forward spec.info into model_info on every create/update path"
```

### Task 1.3: Mock captures model_info body for assertions

**Files:**
- Modify: `internal/litellm/mock/mock.go` (struct fields ~166-172; `ResetModels` ~319-321; `/model/new` handler ~951-956; `/model/update` handler ~971-974; add accessor near `GetModelID` ~684)

- [ ] **Step 1: Add a capture field to the `MockServer` struct**

In the struct (right after the `models map[string]*modelEntry` field, ~line 172), add:

```go
	// lastModelInfo records the model_info sub-block of the most recent
	// POST /model/new or /model/update for each model_name, so tests can
	// assert spec.info forwarding. Guarded by m.mu.
	lastModelInfo map[string]map[string]any
```

- [ ] **Step 2: Initialise it in `ResetModels` and the constructor**

In `ResetModels()` (~line 319-321), add the reset line:

```go
func (m *MockServer) ResetModels() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.models = make(map[string]*modelEntry)
	m.lastModelInfo = make(map[string]map[string]any) // ADD
}
```

Find where `m.models = make(...)` is first initialised in `NewServer` and add the same `m.lastModelInfo = make(map[string]map[string]any)` line beside it.

- [ ] **Step 3: Capture in the `/model/new` handler**

In the `POST /model/new` block, inside the `m.mu.Lock()` section (after line 953), add:

```go
		m.mu.Lock()
		if !routerCreate {
			m.models[modelName] = &modelEntry{ModelID: modelID, ModelName: modelName}
		}
		if mi, ok := reqBody["model_info"].(map[string]any); ok {
			m.lastModelInfo[modelName] = mi // ADD
		}
		m.perModelMutations[modelName]++
		m.mu.Unlock()
```

- [ ] **Step 4: Capture in the `/model/update` handler**

In the `POST /model/update` block, inside the `m.mu.Lock()` section (after line 972), add:

```go
		m.mu.Lock()
		entry, exists := m.models[modelName]
		if mi, ok := reqBody["model_info"].(map[string]any); ok {
			m.lastModelInfo[modelName] = mi // ADD
		}
		m.perModelMutations[modelName]++
		m.mu.Unlock()
```

- [ ] **Step 5: Add the accessor** (near `GetModelID`, ~line 684)

```go
// LastModelInfoBody returns the model_info sub-block of the most recent
// POST /model/new or /model/update for the given model_name (nil if none).
func (m *MockServer) LastModelInfoBody(name string) map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastModelInfo[name]
}
```

- [ ] **Step 6: Verify it builds**

Run: `./scripts/dev.sh go build ./internal/litellm/mock/...`
Expected: success.

- [ ] **Step 7: Commit**

```bash
git add internal/litellm/mock/mock.go
git commit -m "test(mock): capture model_info body for spec.info forwarding assertions"
```

### Task 1.4: Envtest — controller forwards `spec.info` on create

**Files:**
- Test: `internal/controller/model_controller_test.go` (append)

- [ ] **Step 1: Write the failing test**

Append to `internal/controller/model_controller_test.go`:

```go
func TestModel_SpecInfo_ForwardedToLiteLLM(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetModels()
	ensureNoModel(t, ctx, "specinfo-fwd")
	resetConnCacheSnapshot()

	ensureNoConnectionDefault(t, ctx)
	connCR := connDefaultCR()
	if err := k8sClient.Create(ctx, connCR); err != nil {
		t.Fatalf("create LiteLLMConnection: %v", err)
	}
	t.Cleanup(func() {
		mockServer.SetMode(mock.ModeHappy)
		_ = k8sClient.Delete(context.Background(), connCR, &client.DeleteOptions{})
		ensureNoModel(t, context.Background(), "specinfo-fwd")
		time.Sleep(50 * time.Millisecond)
	})
	if snap := pollSnapshotReason(30*time.Second, reasonSynced); snap.Reason != reasonSynced {
		t.Fatalf("connection not Synced")
	}

	cr := modelSampleCR("specinfo-fwd")
	cr.Spec.Info = runtime.RawExtension{Raw: []byte(`{"base_model":"gpt-4o-mini","tier":"paid"}`)}
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create Model: %v", err)
	}

	m := pollModelCondition(t, ctx, "specinfo-fwd", reasonSynced, 30*time.Second)
	if c := apimeta.FindStatusCondition(m.Status.Conditions, conditionTypeReady); c == nil || c.Reason != reasonSynced {
		t.Fatalf("want Ready=Synced, got %+v", c)
	}

	mi := mockServer.LastModelInfoBody("specinfo-fwd")
	if mi == nil {
		t.Fatal("no model_info body captured for specinfo-fwd")
	}
	if mi["base_model"] != "gpt-4o-mini" {
		t.Errorf("model_info.base_model: want gpt-4o-mini, got %v (full: %+v)", mi["base_model"], mi)
	}
	if mi["tier"] != "paid" {
		t.Errorf("model_info.tier: want paid, got %v", mi["tier"])
	}
}
```

- [ ] **Step 2: Run test to verify it fails on a pre-Task-1.2 build, passes now**

Run: `make test-envtest-pkg PKG=./internal/controller/... FOCUS=TestModel_SpecInfo_ForwardedToLiteLLM`
Expected: PASS (Task 1.2 already applied). If you want to see it fail first, `git stash` the model_controller.go change, run (expect FAIL: `base_model` absent), then `git stash pop`.

- [ ] **Step 3: Commit**

```bash
git add internal/controller/model_controller_test.go
git commit -m "test(model): assert spec.info reaches LiteLLM model_info on create"
```

---

## Phase 2: Terminating-strand fixes (Findings #2 + #3, HIGH)

Two distinct strand bugs: (#2) model/mcp/a2a finalizer deletes return a deterministic non-404 4xx raw → infinite controller-runtime backoff, CR stuck `Terminating`; (#3) guardrail finalizer routes the never-persisted-ID case through `onAckMissing` → stuck `Terminating` under `deletionPolicy: Delete`.

### Task 2.1: Mock mode that returns a deterministic 4xx on delete endpoints

**Files:**
- Modify: `internal/litellm/mock/mock.go` (mode consts ~22-30; the `switch mode` block ~855)

- [ ] **Step 1: Add the mode constant**

Near the other `Mode*` consts (around line 22-30), add:

```go
	// ModeDelete422 returns HTTP 422 for any delete-shaped request
	// (POST .../delete or HTTP DELETE) and serves all other paths happily.
	// Used to exercise the deterministic-4xx finalizer-delete path.
	ModeDelete422 = "delete422"
```

- [ ] **Step 2: Add the case to the `switch mode` block** (after the `Mode422` case, ~line 879)

```go
	case ModeDelete422:
		isDelete := (r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/delete")) ||
			r.Method == http.MethodDelete
		if isDelete {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"error":{"message":"Unprocessable Entity","type":"invalid_request_error","param":null,"code":"422"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(m.statefulBody(r))
		return
```

`strings` is already imported in mock.go (used at line 946).

- [ ] **Step 3: Verify it builds**

Run: `./scripts/dev.sh go build ./internal/litellm/mock/...`
Expected: success.

- [ ] **Step 4: Commit**

```bash
git add internal/litellm/mock/mock.go
git commit -m "test(mock): add ModeDelete422 for deterministic delete-4xx tests"
```

### Task 2.2: Model deletion — route deterministic non-404 4xx through `onAckMissing`

**Files:**
- Modify: `internal/controller/model_controller.go` (direct-ID delete switch ~235-253; name-resolve delete switch ~294-309)
- Test: `internal/controller/model_controller_test.go` (append)

- [ ] **Step 1: Write the failing test** (append)

```go
func TestModel_DeletionPath_Deterministic4xx_OrphanDrains(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetModels()
	ensureNoModel(t, ctx, "del-4xx-orphan")
	resetConnCacheSnapshot()

	ensureNoConnectionDefault(t, ctx)
	connCR := connDefaultCR()
	if err := k8sClient.Create(ctx, connCR); err != nil {
		t.Fatalf("create conn: %v", err)
	}
	t.Cleanup(func() {
		mockServer.SetMode(mock.ModeHappy)
		_ = k8sClient.Delete(context.Background(), connCR, &client.DeleteOptions{})
		ensureNoModel(t, context.Background(), "del-4xx-orphan")
		time.Sleep(50 * time.Millisecond)
	})
	if snap := pollSnapshotReason(30*time.Second, reasonSynced); snap.Reason != reasonSynced {
		t.Fatalf("conn not Synced")
	}

	// Create with deletionPolicy=Orphan so a deterministic 4xx on delete drains.
	cr := modelSampleCR("del-4xx-orphan")
	cr.Spec.DeletionPolicy = string(deletionpolicy.Orphan)
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create model: %v", err)
	}
	_ = pollModelCondition(t, ctx, "del-4xx-orphan", reasonSynced, 30*time.Second)

	// Now make every delete fail with 422, then delete the CR.
	mockServer.SetMode(mock.ModeDelete422)
	var got litellmv1alpha1.LiteLLMModel
	if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: WatchNamespace, Name: "del-4xx-orphan"}, &got); err != nil {
		t.Fatalf("get model: %v", err)
	}
	if err := k8sClient.Delete(ctx, &got); err != nil {
		t.Fatalf("delete model: %v", err)
	}

	// Under Orphan, a deterministic 4xx is "ack-missing" → finalizer drained.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		err := k8sClient.Get(ctx, client.ObjectKey{Namespace: WatchNamespace, Name: "del-4xx-orphan"}, &got)
		if apierrors.IsNotFound(err) {
			return // success — CR drained
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("CR stuck Terminating after deterministic 4xx under Orphan policy")
}
```

(`deletionpolicy` and `apierrors` are already imported in the controller test package; if the editor reports a missing import, add `"github.com/ackstorm/alitellm-operator/internal/controller/deletionpolicy"` and `apierrors "k8s.io/apimachinery/pkg/api/errors"`.)

- [ ] **Step 2: Run to verify it fails**

Run: `make test-envtest-pkg PKG=./internal/controller/... FOCUS=TestModel_DeletionPath_Deterministic4xx_OrphanDrains`
Expected: FAIL — CR stuck (the 422 hits `default: return err`, never drains).

- [ ] **Step 3: Add the `is4xxError` case to the direct-ID delete switch**

In `model_controller.go`, the direct-ID `switch` (currently `case errors.As(...auth401)`, `case litellm.IsNotFound(err)`, `default`), insert a new case **before** `default:` (after line 249):

```go
			case is4xxError(err):
				// Deterministic non-404 4xx (404 already handled above): the
				// delete will never succeed by retrying. Cannot confirm absence,
				// so route through onAckMissing — policy-aware (Delete: block +
				// Event + metric; Orphan: drain). Mirrors the 401 fast-path.
				logger.Info("deletion: deterministic 4xx on DeleteModel; ack-missing", "error", err.Error())
				if aerr := onAckMissing("4xx on DeleteModel: " + err.Error()); aerr != nil {
					return ctrl.Result{}, aerr
				}
```

- [ ] **Step 4: Add the same case to the name-resolve delete switch** (after line 306, before its `default:`)

```go
							case is4xxError(err):
								logger.Info("deletion: deterministic 4xx on DeleteModel post-name-resolve; ack-missing", "error", err.Error())
								if aerr := onAckMissing("4xx on DeleteModel post-name-resolve: " + err.Error()); aerr != nil {
									return ctrl.Result{}, aerr
								}
```

- [ ] **Step 5: Run to verify it passes**

Run: `make test-envtest-pkg PKG=./internal/controller/... FOCUS=TestModel_DeletionPath_Deterministic4xx_OrphanDrains`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/controller/model_controller.go internal/controller/model_controller_test.go
git commit -m "fix(model): route deterministic 4xx on delete through onAckMissing (no infinite strand)"
```

### Task 2.3: MCPServer deletion — same fix

**Files:**
- Modify: `internal/controller/mcpserver_controller.go` (delete error handling ~213-224)

- [ ] **Step 1: Add the `is4xxError` branch**

In the `if err := snap.Client.DeleteMCPServer(...); err != nil {` block (lines 213-224), change the `if errors.As(...)/else` to add a middle branch:

```go
						if err := snap.Client.DeleteMCPServer(ctx, serverID); err != nil {
							var auth401 *litellm.Auth401Error
							switch {
							case errors.As(err, &auth401):
								r.Cache.InvalidateOn401()
								logger.Info("deletion: 401 fast-path; cache invalidated", "path", auth401.Path)
								if gerr := onAckMissing("401 on DeleteMCPServer"); gerr != nil {
									return ctrl.Result{}, gerr
								}
							case is4xxError(err):
								logger.Info("deletion: deterministic 4xx on DeleteMCPServer; ack-missing", "error", err.Error())
								if gerr := onAckMissing("4xx on DeleteMCPServer: " + err.Error()); gerr != nil {
									return ctrl.Result{}, gerr
								}
							default:
								// Transient error — return for backoff. Finalizer stays.
								return ctrl.Result{}, err
							}
						} else {
```

(If `errors` is not yet imported in this file, it is — used at line 215.)

- [ ] **Step 2: Verify it builds**

Run: `make build-operator`
Expected: success.

- [ ] **Step 3: Add a regression test** mirroring `TestModel_DeletionPath_Deterministic4xx_OrphanDrains`, in `internal/controller/mcpserver_controller_test.go`. Open that file, find the existing finalizer-delete test (search for `Delete(ctx` + `IsNotFound`) to copy its CR-creation + connection-gate setup helpers (e.g. an `mcpSampleCR`/`mcpServerSampleCR` constructor — use whichever exists), then adapt: set `cr.Spec.DeletionPolicy = string(deletionpolicy.Orphan)`, reach `Synced`, `mockServer.SetMode(mock.ModeDelete422)`, delete the CR, and assert it reaches `IsNotFound` within 30s.

```go
func TestMCPServer_DeletionPath_Deterministic4xx_OrphanDrains(t *testing.T) {
	// Structure: copy the connection-gate + CR-creation preamble from the
	// existing finalizer-delete test in this file (mcpserver_controller_test.go).
	// Then:
	//   cr.Spec.DeletionPolicy = string(deletionpolicy.Orphan)
	//   ... create, poll to reasonSynced ...
	//   mockServer.SetMode(mock.ModeDelete422)
	//   k8sClient.Delete(ctx, &got)
	//   poll until apierrors.IsNotFound (success) or t.Fatalf after 30s.
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `make test-envtest-pkg PKG=./internal/controller/... FOCUS=TestMCPServer_DeletionPath_Deterministic4xx_OrphanDrains`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/controller/mcpserver_controller.go internal/controller/mcpserver_controller_test.go
git commit -m "fix(mcpserver): route deterministic 4xx on delete through onAckMissing"
```

### Task 2.4: A2AAgent deletion — same fix

**Files:**
- Modify: `internal/controller/a2aagent_controller.go` (delete error handling ~211-222)

- [ ] **Step 1: Add the `is4xxError` branch**

Find the `DeleteAgent` error block (the `if errors.As(...auth401) {...} else { return ctrl.Result{}, err }` around line 211-222) and rewrite as a `switch` mirroring Task 2.3:

```go
						if err := snap.Client.DeleteAgent(ctx, agentID); err != nil {
							var auth401 *litellm.Auth401Error
							switch {
							case errors.As(err, &auth401):
								r.Cache.InvalidateOn401()
								logger.Info("deletion: 401 fast-path; cache invalidated", "path", auth401.Path)
								if gerr := onAckMissing("401 on DeleteAgent"); gerr != nil {
									return ctrl.Result{}, gerr
								}
							case is4xxError(err):
								logger.Info("deletion: deterministic 4xx on DeleteAgent; ack-missing", "error", err.Error())
								if gerr := onAckMissing("4xx on DeleteAgent: " + err.Error()); gerr != nil {
									return ctrl.Result{}, gerr
								}
							default:
								return ctrl.Result{}, err
							}
						} else {
```

Match the exact variable names already in that file (`agentID`, the `onAckMissing` closure name, the success-`else` body). Read the surrounding ~15 lines first to keep the success branch intact.

- [ ] **Step 2: Verify it builds**

Run: `make build-operator`
Expected: success.

- [ ] **Step 3: Add a regression test** `TestA2AAgent_DeletionPath_Deterministic4xx_OrphanDrains` in `internal/controller/a2aagent_controller_test.go`, mirroring Task 2.3 Step 3 against the existing a2a finalizer-delete test's helpers.

- [ ] **Step 4: Run to verify it passes**

Run: `make test-envtest-pkg PKG=./internal/controller/... FOCUS=TestA2AAgent_DeletionPath_Deterministic4xx_OrphanDrains`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/controller/a2aagent_controller.go internal/controller/a2aagent_controller_test.go
git commit -m "fix(a2aagent): route deterministic 4xx on delete through onAckMissing"
```

### Task 2.5: Guardrail deletion — drain on confirmed-absent (never-persisted ID)

**Files:**
- Modify: `internal/controller/litellmguardrail_controller.go` (the `else` branch at lines 207-217)
- Test: `internal/controller/litellmguardrail_controller_test.go` (append)

- [ ] **Step 1: Write the failing test** (append)

```go
func TestGuardRail_DeletionPath_NeverPersisted_DeletePolicyDrains(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.Mode422) // POST /guardrails happy? No — see note.
	// NOTE: Mode422 only 422s /model/new. To keep GuardrailID empty, instead
	// create the CR while the connection is NOT usable so create never runs,
	// OR reject the guardrail create. Simplest reliable path: create the CR,
	// let it persist an ID, then clear status.lastRendered.guardrailID to ""
	// to simulate "create rejected before persist", then delete with policy=Delete.
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetGuardrails()
	resetConnCacheSnapshot()

	ensureNoConnectionDefault(t, ctx)
	connCR := connDefaultCR()
	if err := k8sClient.Create(ctx, connCR); err != nil {
		t.Fatalf("create conn: %v", err)
	}
	t.Cleanup(func() {
		mockServer.SetMode(mock.ModeHappy)
		_ = k8sClient.Delete(context.Background(), connCR, &client.DeleteOptions{})
		time.Sleep(50 * time.Millisecond)
	})
	if snap := pollSnapshotReason(30*time.Second, reasonSynced); snap.Reason != reasonSynced {
		t.Fatalf("conn not Synced")
	}

	// Create a guardrail CR with deletionPolicy=Delete. Use the existing
	// guardrail sample-CR constructor in this test file (search for the
	// helper that builds a *LiteLLMGuardRail). Set DeletionPolicy=Delete.
	cr := guardrailSampleCR("never-persisted")          // use the actual helper name in this file
	cr.Spec.DeletionPolicy = string(deletionpolicy.Delete)
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create guardrail: %v", err)
	}
	// Wait for the finalizer to be added (Synced), then force guardrailID="".
	_ = pollGuardRailCondition(t, ctx, "never-persisted", reasonSynced, 30*time.Second) // use actual poll helper
	if err := updateWithRetry(ctx, client.ObjectKeyFromObject(cr), &litellmv1alpha1.LiteLLMGuardRail{}, func(o client.Object) error {
		gr := o.(*litellmv1alpha1.LiteLLMGuardRail)
		gr.Status.LastRendered.GuardrailID = ""
		return r.Status().Update(ctx, gr) // adapt to the suite's status-update helper
	}); err != nil {
		t.Fatalf("clear guardrailID: %v", err)
	}

	var got litellmv1alpha1.LiteLLMGuardRail
	_ = k8sClient.Get(ctx, client.ObjectKeyFromObject(cr), &got)
	if err := k8sClient.Delete(ctx, &got); err != nil {
		t.Fatalf("delete: %v", err)
	}

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(cr), &got)) {
			return // success
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("guardrail stuck Terminating with empty guardrailID under Delete policy")
}
```

**Authoring note:** the status-clear step depends on the suite's exact status-update mechanics — open `litellmguardrail_controller_test.go` and reuse the helper an existing test uses to mutate guardrail status (the suite exposes the reconciler as a package var; mirror how another test sets `Status.LastRendered`). Replace `guardrailSampleCR` / `pollGuardRailCondition` with the real helper names in that file.

- [ ] **Step 2: Run to verify it fails**

Run: `make test-envtest-pkg PKG=./internal/controller/... FOCUS=TestGuardRail_DeletionPath_NeverPersisted_DeletePolicyDrains`
Expected: FAIL — CR stuck (empty ID → `onAckMissing` → blocked under Delete).

- [ ] **Step 3: Apply the fix**

In `litellmguardrail_controller.go`, replace the `else` block at lines 207-217 with a split that drains on confirmed-absent:

```go
				} else if !snap.Usable() {
					// LiteLLM unavailable — cannot confirm absence; gate on policy.
					if err := onAckMissing("LiteLLM unavailable"); err != nil {
						return ctrl.Result{}, err
					}
				} else {
					// snap.Usable() && guardrailID == "": the operator never
					// persisted an ID, so it never confirmed a create. Treat as
					// confirmed-absent and drain regardless of policy — mirrors the
					// model controller's onConfirmedAbsent fix; routing this through
					// onAckMissing stranded deletionPolicy=Delete CRs in Terminating.
					metrics.DeletionBlocked.Forget(guardrailKind, gr.Namespace, gr.Name)
					logger.Info("finalizer removed; guardrail_id never persisted (confirmed absent)",
						"name", gr.Name)
				}
```

(Control falls through to the existing `RemoveFinalizer` at line 222.)

- [ ] **Step 4: Run to verify it passes**

Run: `make test-envtest-pkg PKG=./internal/controller/... FOCUS=TestGuardRail_DeletionPath_NeverPersisted_DeletePolicyDrains`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/controller/litellmguardrail_controller.go internal/controller/litellmguardrail_controller_test.go
git commit -m "fix(guardrail): drain finalizer on confirmed-absent (never-persisted id) under Delete"
```

---

## Phase 3: Connection 401-recovery (Findings #4 + #7, MEDIUM-HIGH)

### Task 3.1: `InvalidateOn401` must reset `lastReady` so recovery re-fires `emitRebuilt`

**Files:**
- Modify: `internal/connection/cache.go` (`InvalidateOn401`, ~225-257)
- Test: `internal/connection/cache_test.go` (append)

- [ ] **Step 1: Write the failing test** (append)

```go
func TestInvalidateOn401_RecoveryReEmits(t *testing.T) {
	c := NewCache(logr.Discard())
	sub := c.Subscribe()
	c.Rebuild(ConnectionSnapshot{Ready: true, Reason: "Synced"})
	<-sub // drain the initial false→true emit

	// Transient 401 then recovery to Ready=true.
	c.InvalidateOn401()
	c.Rebuild(ConnectionSnapshot{Ready: true, Reason: "Synced"})

	select {
	case <-sub:
		// expected: recovery is a real false→true edge and re-enqueues dependents
	case <-time.After(1 * time.Second):
		t.Fatal("InvalidateOn401 recovery did not re-emit: lastReady was not reset")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `make test-unit-pkg PKG=./internal/connection/...`
Expected: FAIL on `TestInvalidateOn401_RecoveryReEmits` — no event (lastReady stayed true → `Swap(true)` returned true → no emit).

- [ ] **Step 3: Apply the fix**

In `internal/connection/cache.go`, inside `InvalidateOn401`, within the block that stores the placeholder (the `if c.invalidated.CompareAndSwap(false, true)` guard), add a `lastReady` reset **before** `c.snapshot.Store(&placeholder)`:

```go
		c.lastReady.Store(false) // so a subsequent Ready Rebuild is a real false→true edge that re-fires emitRebuilt
		c.snapshot.Store(&placeholder)
```

(Place it inside the same CAS-guarded block so it only runs on the first invalidation in a storm window, matching the existing storm-gate semantics.)

- [ ] **Step 4: Run to verify it passes**

Run: `make test-unit-pkg PKG=./internal/connection/...`
Expected: PASS (and no existing connection test regresses).

- [ ] **Step 5: Commit**

```bash
git add internal/connection/cache.go internal/connection/cache_test.go
git commit -m "fix(connection): reset lastReady in InvalidateOn401 so 401-recovery re-enqueues dependents"
```

### Task 3.2: ModelAlias must call `InvalidateOn401` on a 401

**Files:**
- Modify: `internal/controller/modelalias_controller.go` (the two error sites ~131-138)
- Test: `internal/controller/modelalias_controller_test.go` (append)

- [ ] **Step 1: Write the failing test** (append)

Use the `FakeConnectionCache` (defined in `noop_controller_test.go`, has an `Invalidated atomic.Bool` flipped by its `InvalidateOn401()`) as the reconciler's cache so the call is observable in a unit test:

```go
func TestModelAlias_401_InvalidatesCache(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = litellmv1alpha1.AddToScheme(scheme)
	// A ModelAlias CR so the reconciler has work to do.
	ma := &litellmv1alpha1.LiteLLMModelAlias{
		ObjectMeta: metav1.ObjectMeta{Name: "ma-401", Namespace: WatchNamespace},
		// populate minimally per the existing modelalias unit tests in this file
	}
	cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ma).Build()

	// A 401-returning mock client wired into a snapshot via the fake cache.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"type":"token_not_found_in_db","code":"401","message":"x"}}`))
	}))
	defer srv.Close()
	llm := litellm.NewClient(srv.URL, "sk-test", logr.Discard())

	fc := &FakeConnectionCache{}
	fc.SetSnapshot(connection.ConnectionSnapshot{Ready: true, Reason: reasonSynced, Client: llm}) // use the fake's setter

	r := &ModelAliasReconciler{Client: cli, Scheme: scheme, Cache: fc /* + other required fields per existing tests */}
	_, _ = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(ma)})

	if !fc.Invalidated.Load() {
		t.Fatal("ModelAlias did not call InvalidateOn401 on a 401 from LiteLLM")
	}
}
```

**Authoring note:** open `modelalias_controller_test.go` + `noop_controller_test.go` to copy (a) the exact `ModelAliasReconciler` field set used by existing tests, (b) the real `FakeConnectionCache` snapshot-setter name, and (c) a valid minimal `LiteLLMModelAlias` spec. Keep the assertion (`fc.Invalidated.Load()`).

- [ ] **Step 2: Run to verify it fails**

Run: `make test-envtest-pkg PKG=./internal/controller/... FOCUS=TestModelAlias_401_InvalidatesCache`
Expected: FAIL — `Invalidated` is false.

- [ ] **Step 3: Apply the fix**

In `modelalias_controller.go`, at BOTH error sites (after `GetRouterSettings` ~line 131-134 and after `UpdateRouterSettings` ~line 136-138), call `InvalidateOn401` before `broadcastNotReady` when the error is a 401:

```go
	current, err := cli.GetRouterSettings(ctx)
	if err != nil {
		var auth401 *litellm.Auth401Error
		if errors.As(err, &auth401) {
			r.Cache.InvalidateOn401()
		}
		msg := fmt.Sprintf("GET /get/config/callbacks: %v", err)
		return r.broadcastNotReady(ctx, list.Items, modelAliasErrorReason(err), msg, snap.NormalizedRequeueOnRejectedAfter(), logger)
	}
```

Apply the identical 3-line `var auth401 ...; if errors.As ... { r.Cache.InvalidateOn401() }` guard before the `UpdateRouterSettings` error's `broadcastNotReady`. Ensure `errors` and `litellm` are imported (they are — `modelAliasErrorReason` already uses both).

- [ ] **Step 4: Run to verify it passes**

Run: `make test-envtest-pkg PKG=./internal/controller/... FOCUS=TestModelAlias_401_InvalidatesCache`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/controller/modelalias_controller.go internal/controller/modelalias_controller_test.go
git commit -m "fix(modelalias): invalidate connection cache on 401 (immediate re-probe like siblings)"
```

---

## Phase 4: `DeleteModel` empty-ID guard (Finding #5, MEDIUM)

### Task 4.1: Add the empty-ID guard to `DeleteModel`

**Files:**
- Modify: `internal/litellm/model.go` (`DeleteModel`, ~59-62)
- Test: `internal/litellm/model_test.go` (append)

- [ ] **Step 1: Write the failing test** (append)

```go
func TestDeleteModel_EmptyIDGuard(t *testing.T) {
	c := newTestClient(t, "http://unreachable.invalid")
	err := c.DeleteModel(context.Background(), "")
	if err == nil {
		t.Fatal("expected error on empty model_id; no request should be issued")
	}
	if !strings.Contains(err.Error(), "empty model_id") {
		t.Errorf("error %q does not mention empty model_id", err.Error())
	}
}
```

(`strings` and `context` are already imported by the litellm test files; add them if the editor flags it.)

- [ ] **Step 2: Run to verify it fails**

Run: `./scripts/dev.sh go test -v -count=1 -run TestDeleteModel_EmptyIDGuard ./internal/litellm/...`
Expected: FAIL — currently no guard; the call attempts a request (and either errors with a transport error not mentioning "empty model_id", or unexpectedly succeeds against a stub).

- [ ] **Step 3: Apply the fix**

In `internal/litellm/model.go`, change `DeleteModel` to guard first (mirroring `DeleteMCPServer`/`DeleteAgent`/`DeleteGuardrail`):

```go
func (c *Client) DeleteModel(ctx context.Context, modelID string) error {
	if modelID == "" {
		return fmt.Errorf("litellm: DeleteModel: empty model_id")
	}
	_, err := c.makeRequest(ctx, "POST", "/model/delete", &ModelDeleteRequest{ID: modelID})
	return err
}
```

(Ensure `fmt` is imported in `model.go`; if not, add it.)

- [ ] **Step 4: Run to verify it passes**

Run: `./scripts/dev.sh go test -v -count=1 -run TestDeleteModel_EmptyIDGuard ./internal/litellm/...`
Expected: PASS.

- [ ] **Step 5: Run the whole litellm package to confirm no regression**

Run: `make test-unit-pkg PKG=./internal/litellm/...`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/litellm/model.go internal/litellm/model_test.go
git commit -m "fix(litellm): guard DeleteModel against empty model_id (match sibling delete helpers)"
```

---

## Phase 5: ModelDiscovery secret-error classification + stale condition (Findings #6 + #11, MEDIUM)

### Task 5.1: Distinguish transient secret-read errors from genuinely-missing secrets

**Files:**
- Modify: `internal/controller/modeldiscovery_controller.go` (`resolveStringKey` ~1045-1063, `resolveAWSCredentials` ~1068-1099, the four call sites ~402-433)
- Test: `internal/controller/secret_resolve_test.go` (append; this file already uses `interceptor.Funcs`)

- [ ] **Step 1: Write the failing test** (append to `secret_resolve_test.go`)

```go
func TestResolveStringKey_TransientError_NotMissing(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	boom := errors.New("apiserver throttled")
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
				return boom
			},
		}).Build()

	r := &ModelDiscoveryReconciler{Client: c, Scheme: scheme}
	ref := &litellmv1alpha1.SecretObjectRef{Name: "creds"}
	_, missing, err := r.resolveStringKey(context.Background(), WatchNamespace, ref, "ANTHROPIC_API_KEY")
	if err == nil {
		t.Fatal("expected transient error to be returned")
	}
	if missing {
		t.Error("transient error must NOT be classified as missing (SecretNotFound)")
	}
}

func TestResolveStringKey_NotFound_IsMissing(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).Build() // no secret → IsNotFound
	r := &ModelDiscoveryReconciler{Client: c, Scheme: scheme}
	ref := &litellmv1alpha1.SecretObjectRef{Name: "creds"}
	_, missing, err := r.resolveStringKey(context.Background(), WatchNamespace, ref, "ANTHROPIC_API_KEY")
	if !missing {
		t.Errorf("a genuinely-absent secret must be missing=true (err=%v)", err)
	}
}
```

- [ ] **Step 2: Run to verify it fails (compile error — signature change)**

Run: `make test-unit-pkg PKG=./internal/controller/...`
Expected: FAIL to compile — `resolveStringKey` returns 2 values, test expects 3.

- [ ] **Step 3: Change `resolveStringKey` to return a `missing` bool**

In `modeldiscovery_controller.go`, change the signature and the NotFound branch:

```go
func (r *ModelDiscoveryReconciler) resolveStringKey(
	ctx context.Context, namespace string, ref *litellmv1alpha1.SecretObjectRef, key string,
) (val string, missing bool, err error) {
	var secret corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: ref.Name}, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return "", true, fmt.Errorf("%s/%s:%s not found", namespace, ref.Name, key)
		}
		return "", false, err // transient — caller returns it for controller-runtime backoff
	}
	// ... existing key-extraction logic, returning ("", true, fmt.Errorf("...:%s not found")) when the
	//     key is absent from an existing secret, and (value, false, nil) on success ...
}
```

Apply the same `(val, missing, err)` shape to the key-absent-in-existing-secret path (that is a genuine "missing" → `missing=true`).

- [ ] **Step 4: Change `resolveAWSCredentials` the same way**

Change its signature to `(creds aws.Credentials, missing bool, err error)` (match the actual return type — read its current signature) and set `missing=true` only on `apierrors.IsNotFound`/key-absent, `missing=false` for transient `return ..., false, err`.

- [ ] **Step 5: Update the four call sites (~402-433)**

For each provider arm, distinguish transient from missing:

```go
	key, missing, err := r.resolveStringKey(ctx, md.Namespace, md.Spec.CredentialsSecretRef, "ANTHROPIC_API_KEY")
	if err != nil && !missing {
		return ctrl.Result{}, err // transient → controller-runtime exponential backoff
	}
	if missing {
		res := r.writeReady(ctx, &md, metav1.ConditionFalse, reasonSecretNotFound, err.Error())
		res.RequeueAfter = connection.DefaultRequeueOnRejectedAfter
		return res, nil
	}
```

Repeat for the gemini/openai arms (and the bedrock arm using `resolveAWSCredentials` with its `missing` return).

- [ ] **Step 6: Run to verify the new tests pass and the suite builds**

Run: `make test-unit-pkg PKG=./internal/controller/...`
Expected: PASS (`TestResolveStringKey_*`), no compile errors.

- [ ] **Step 7: Commit**

```bash
git add internal/controller/modeldiscovery_controller.go internal/controller/secret_resolve_test.go
git commit -m "fix(modeldiscovery): treat transient secret-read errors as retryable, not SecretNotFound"
```

### Task 5.2: Clear stale `SourceReachable` on deterministic-error returns

**Files:**
- Modify: `internal/controller/modeldiscovery_controller.go` (the `missing`/SecretNotFound return from Task 5.1, plus the InvalidConfig returns at ~390 and ~495)
- Test: `internal/controller/modeldiscovery_controller_test.go` (append)

- [ ] **Step 1: Write the failing test** (append)

```go
func TestModelDiscovery_SecretDeleted_ClearsSourceReachable(t *testing.T) {
	ctx := context.Background()
	// 1. Create a discovery CR + credential secret; reconcile to success
	//    (Ready=True, SourceReachable=True). Reuse ensureCredentialSecret and
	//    the discovery CR constructor used by existing tests in this file.
	// 2. Delete the credential secret.
	// 3. Poll the CR; assert BOTH:
	//      Ready.Reason == reasonSecretNotFound (False)
	//      SourceReachable.Status == metav1.ConditionFalse  (not stale-True)
	// Skeleton:
	//   ... setup, reach SourceReachable=True ...
	//   k8sClient.Delete(ctx, secret)
	//   deadline := time.Now().Add(10 * time.Second)
	//   for time.Now().Before(deadline) {
	//       k8sClient.Get(ctx, key, &md)
	//       sr := apimeta.FindStatusCondition(md.Status.Conditions, "SourceReachable")
	//       if sr != nil && sr.Status == metav1.ConditionFalse { return }
	//       time.Sleep(100 * time.Millisecond)
	//   }
	//   t.Fatal("SourceReachable stayed stale-True after the secret was deleted")
}
```

Fill the skeleton using `ensureCredentialSecret` and the discovery-CR helper already in `modeldiscovery_controller_test.go` (mirror an existing success-path test for the setup).

- [ ] **Step 2: Run to verify it fails**

Run: `make test-envtest-pkg PKG=./internal/controller/... FOCUS=TestModelDiscovery_SecretDeleted_ClearsSourceReachable`
Expected: FAIL — `SourceReachable` stays True (`writeReady` only touches Ready).

- [ ] **Step 3: Apply the fix** — replace the `writeReady(...SecretNotFound...)` return (from Task 5.1 Step 5) with a both-conditions write:

```go
	if missing {
		r.writeBothConditions(ctx, &md,
			metav1.ConditionFalse, reasonSecretNotFound, err.Error(),
			metav1.ConditionFalse, reasonSecretNotFound, err.Error())
		return ctrl.Result{RequeueAfter: connection.DefaultRequeueOnRejectedAfter}, nil
	}
```

Do the same substitution for the deterministic InvalidConfig returns at ~390 (baseURL) and ~495 (filter) — replace their `writeReady(...ConditionFalse, "InvalidConfig"...)` with `writeBothConditions(... Ready=False/InvalidConfig ..., SourceReachable=False/InvalidConfig ...)` followed by the same `return`. (`writeBothConditions` returns nothing; build the `ctrl.Result` explicitly.)

- [ ] **Step 4: Run to verify it passes**

Run: `make test-envtest-pkg PKG=./internal/controller/... FOCUS=TestModelDiscovery_SecretDeleted_ClearsSourceReachable`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/controller/modeldiscovery_controller.go internal/controller/modeldiscovery_controller_test.go
git commit -m "fix(modeldiscovery): clear SourceReachable=False on deterministic error paths"
```

---

## Phase 6: Guardrail pool adoption race (Finding #8, MEDIUM)

A by-name adoption (`GetGuardrailByName` returns the first pool member) can let two CRs sharing a `guardrailName` adopt the same `guardrail_id` in the boot window. Fix: only adopt by-name when this CR is the sole member of the name (`poolSize <= 1`); in a real pool, each CR must create its own row.

### Task 6.1: Gate by-name adoption on `poolSize <= 1`

**Files:**
- Modify: `internal/controller/litellmguardrail_controller.go` (adoption condition, line 446)
- Test: `internal/controller/litellmguardrail_controller_test.go` (append)

- [ ] **Step 1: Write the failing test** (append)

```go
func TestGuardRail_PoolMembers_DoNotAdoptSameRow(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetGuardrails()
	resetConnCacheSnapshot()

	ensureNoConnectionDefault(t, ctx)
	connCR := connDefaultCR()
	if err := k8sClient.Create(ctx, connCR); err != nil {
		t.Fatalf("create conn: %v", err)
	}
	t.Cleanup(func() {
		mockServer.SetMode(mock.ModeHappy)
		_ = k8sClient.Delete(context.Background(), connCR, &client.DeleteOptions{})
		time.Sleep(50 * time.Millisecond)
	})
	if snap := pollSnapshotReason(30*time.Second, reasonSynced); snap.Reason != reasonSynced {
		t.Fatalf("conn not Synced")
	}

	// Two CRs sharing the SAME guardrailName (an LB pool). Use the guardrail
	// sample-CR helper in this file; set both .Spec.GuardrailName = "pool-x"
	// and distinct metadata.Names. Same provider (no provider mismatch).
	a := guardrailSampleCR("pool-a")
	a.Spec.GuardrailName = "pool-x"
	b := guardrailSampleCR("pool-b")
	b.Spec.GuardrailName = "pool-x"
	if err := k8sClient.Create(ctx, a); err != nil {
		t.Fatalf("create a: %v", err)
	}
	if err := k8sClient.Create(ctx, b); err != nil {
		t.Fatalf("create b: %v", err)
	}

	gotA := pollGuardRailCondition(t, ctx, "pool-a", reasonSynced, 30*time.Second)
	gotB := pollGuardRailCondition(t, ctx, "pool-b", reasonSynced, 30*time.Second)

	if gotA.Status.LastRendered.GuardrailID == "" || gotB.Status.LastRendered.GuardrailID == "" {
		t.Fatalf("both pool members must persist an ID; got a=%q b=%q",
			gotA.Status.LastRendered.GuardrailID, gotB.Status.LastRendered.GuardrailID)
	}
	if gotA.Status.LastRendered.GuardrailID == gotB.Status.LastRendered.GuardrailID {
		t.Errorf("pool members collapsed onto the same guardrail_id %q (adoption race)",
			gotA.Status.LastRendered.GuardrailID)
	}
}
```

Replace `guardrailSampleCR`/`pollGuardRailCondition` with the real helper names in the file.

- [ ] **Step 2: Run to verify it fails (or flakes toward failure)**

Run: `make test-envtest-pkg PKG=./internal/controller/... FOCUS=TestGuardRail_PoolMembers_DoNotAdoptSameRow`
Expected: FAIL when the two reconciles race into shared adoption (both adopt member[0]). Run a few times to surface the race; the fix makes it deterministically pass.

- [ ] **Step 3: Apply the fix** — add the `poolSize <= 1` gate at line 446:

```go
	if persistedID == "" && existing != nil && existing.GuardrailID != "" && !anySiblingOwns && poolSize <= 1 {
		persistedID = existing.GuardrailID
		logger.V(1).Info("adopted existing LiteLLM guardrail (idempotency probe)",
			"guardrailID", persistedID)
	}
```

(`poolSize` is already in scope — returned by `checkGuardrailPool` at line 424. In a real pool, `poolSize > 1` disables by-name adoption so each CR creates its own row; a sole CR — `poolSize == 1` — still adopts a pre-existing single row.)

- [ ] **Step 4: Run to verify it passes (repeatedly)**

Run: `make test-envtest-pkg PKG=./internal/controller/... FOCUS=TestGuardRail_PoolMembers_DoNotAdoptSameRow`
Expected: PASS. Run 3× to confirm stability.

- [ ] **Step 5: Commit**

```bash
git add internal/controller/litellmguardrail_controller.go internal/controller/litellmguardrail_controller_test.go
git commit -m "fix(guardrail): disable by-name adoption in a multi-member pool (no shared-id collapse)"
```

---

## Phase 7: Robustness & observability (Findings #9, #10, #12, #13)

### Task 7.1: Re-assert the `connection_ready` gauge before the skip-when-equal short-circuit (Finding #10)

**Files:**
- Modify: `internal/controller/litellmconnection_controller.go` (`writeStatus` ~556-565; `writeReadyAndLoggingHealthy` ~643-652)

- [ ] **Step 1: Write the failing test** — `internal/controller/litellmconnection_controller_test.go` (append)

```go
func TestConnectionReadyGauge_ReassertedWhenStatusUnchanged(t *testing.T) {
	// Simulate "post-restart": gauge is 0 but the CR's Ready condition already
	// equals what the reconciler is about to write. The skip-when-equal path
	// must STILL set the gauge.
	r := &LiteLLMConnectionReconciler{ /* Client/Scheme per existing tests */ }

	// Reset the one-hot gauge to 0 across all reasons.
	for _, rk := range connectionReasonAll {
		metrics.ConnectionReady.WithLabelValues(rk).Set(0)
	}

	conn := &litellmv1alpha1.LiteLLMConnection{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: WatchNamespace, Generation: 1},
	}
	// Pre-set Ready=False/Connecting at ObservedGeneration=1 so writeStatus sees "unchanged".
	apimeta.SetStatusCondition(&conn.Status.Conditions, metav1.Condition{
		Type: conditionTypeReady, Status: metav1.ConditionFalse, Reason: reasonConnecting,
		Message: "msg", ObservedGeneration: 1,
	})
	conn.Status.ObservedGeneration = 1

	_ = r.writeStatus(context.Background(), conn, reasonConnecting, "msg") // skip-when-equal path

	if v := testutil.ToFloat64(metrics.ConnectionReady.WithLabelValues(reasonConnecting)); v != 1 {
		t.Errorf("connection_ready{Connecting}: want 1 after skip-when-equal, got %v", v)
	}
}
```

**Authoring note:** the test calls `writeStatus` directly with a CR that has no real apiserver — the status `Patch` will fail, but the gauge is set BEFORE the patch (after the fix), so the assertion still holds. If `r.Status()` panics on a nil client, build `r` with a `fake.NewClientBuilder()...Build()` and the conn pre-created. Use `reasonConnecting` (defined in the controller package) and `testutil` (`github.com/prometheus/client_golang/prometheus/testutil`).

- [ ] **Step 2: Run to verify it fails**

Run: `make test-envtest-pkg PKG=./internal/controller/... FOCUS=TestConnectionReadyGauge_ReassertedWhenStatusUnchanged`
Expected: FAIL — gauge is 0 (skip-when-equal returns before the gauge block).

- [ ] **Step 3: Apply the fix in `writeStatus`** — move the one-hot gauge block (lines 560-565) to BEFORE the skip-when-equal guard (line 556):

```go
	// §10: re-assert the one-hot ConnectionReady gauge on EVERY reconcile
	// (idempotent + cheap). Must run before the skip-when-equal short-circuit
	// so the gauge is restored after an operator restart, when the in-memory
	// gauge is 0 but the persisted Ready condition already matches.
	for _, rk := range connectionReasonAll {
		metrics.ConnectionReady.WithLabelValues(rk).Set(0)
	}
	metrics.ConnectionReady.WithLabelValues(reason).Set(1)

	if statusReadyUnchanged(conn.Status.Conditions, conn.Status.ObservedGeneration, conn.Generation, metav1.ConditionFalse, reason, message) {
		return nil
	}
```

Delete the now-duplicated gauge block that was at 560-565 (it has moved up). Update the comment at line 554 that claimed the gauge is a no-op on this path (it is not, post-restart).

- [ ] **Step 4: Apply the same move in `writeReadyAndLoggingHealthy`** — move the gauge block (lines 647-652) to BEFORE the `if readyEqual && lhEqual { return nil }` (line 643):

```go
	// §10: re-assert the one-hot gauge before the skip-when-equal short-circuit
	// (see writeStatus rationale).
	for _, rk := range connectionReasonAll {
		metrics.ConnectionReady.WithLabelValues(rk).Set(0)
	}
	metrics.ConnectionReady.WithLabelValues(readyReason).Set(1)

	if readyEqual && lhEqual {
		return nil
	}
```

Delete the duplicated gauge block formerly at 647-652.

- [ ] **Step 5: Run to verify it passes**

Run: `make test-envtest-pkg PKG=./internal/controller/... FOCUS=TestConnectionReadyGauge_ReassertedWhenStatusUnchanged`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/controller/litellmconnection_controller.go internal/controller/litellmconnection_controller_test.go
git commit -m "fix(connection): re-assert connection_ready gauge before skip-when-equal (restart-safe)"
```

### Task 7.2: `processLitellmError` keeps a valid `type` for a type-only envelope (Finding #12)

**Files:**
- Modify: `internal/litellm/client.go` (the guard at line 144)
- Test: `internal/litellm/errors_test.go` or `client_test.go` (append)

- [ ] **Step 1: Write the failing test** (append to `errors_test.go`)

```go
func TestProcessLitellmError_TypeOnlyEnvelope_KeepsType(t *testing.T) {
	body := []byte(`{"error":{"type":"not_found_error","message":"","code":""}}`)
	kind, _, _ := processLitellmError(body)
	if kind != "not_found_error" {
		t.Errorf("type-only envelope: want kind=not_found_error, got %q", kind)
	}
}

func TestProcessLitellmError_UnparseableStaysEmpty(t *testing.T) {
	kind, _, _ := processLitellmError([]byte(`<html>500</html>`))
	if kind != "" {
		t.Errorf("unparseable body: want empty kind, got %q", kind)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `./scripts/dev.sh go test -v -count=1 -run TestProcessLitellmError ./internal/litellm/...`
Expected: FAIL on the type-only case — current guard treats it as unparsed and drops the type.

- [ ] **Step 3: Apply the fix**

Read `client.go` lines ~138-160 first. The current guard is `if err := json.Unmarshal(body, &env); err != nil || env.Error.Code == "" && env.Error.Message == "" { return kindUnparsed... }`. Change the condition so a non-empty `Type` keeps the envelope:

```go
	if err := json.Unmarshal(body, &env); err != nil ||
		(env.Error.Type == "" && env.Error.Code == "" && env.Error.Message == "") {
		return kindUnparsed, capBody(body), ""
	}
```

(Match the exact return values/identifiers used in the existing code — `kindUnparsed`, the body-capping helper, and the field names on `env.Error`. Add the `env.Error.Type == "" &&` conjunct and wrap the whole empties-check in parentheses so it ANDs correctly with the unmarshal error via `||`.)

- [ ] **Step 4: Run to verify it passes**

Run: `./scripts/dev.sh go test -v -count=1 -run TestProcessLitellmError ./internal/litellm/...`
Expected: PASS (both cases).

- [ ] **Step 5: Run the whole package**

Run: `make test-unit-pkg PKG=./internal/litellm/...`
Expected: PASS (no regression on the existing `RejectedError` table tests).

- [ ] **Step 6: Commit**

```bash
git add internal/litellm/client.go internal/litellm/errors_test.go
git commit -m "fix(litellm): keep error.type for type-only LiteLLM error envelopes"
```

### Task 7.3: Guardrail steady-state increments `reconcile_total` (Finding #13)

**Files:**
- Modify: `internal/controller/litellmguardrail_controller.go` (steady-state return, line 475)
- Test: `internal/controller/litellmguardrail_controller_test.go` (append)

- [ ] **Step 1: Write the failing test** (append)

```go
func TestGuardRail_SteadyState_IncrementsReconcileTotal(t *testing.T) {
	before := testutil.ToFloat64(metrics.ReconcileTotal.WithLabelValues(guardrailKind, "success"))

	// Drive a guardrail CR to steady state (create → Synced), then trigger one
	// more reconcile that hits the hash-equal steady-state return (e.g. a
	// no-op status touch or the safety-relist). Reuse the existing guardrail
	// success-path test's setup (guardrailSampleCR + pollGuardRailCondition).
	// ... setup, reach reasonSynced ...
	// Trigger a second reconcile via a benign annotation bump:
	//   updateWithRetry(..., func(o){ o.SetAnnotations(map[string]string{"x":"1"}); return nil })
	//   pollGuardRailCondition(... reasonSynced ...)

	after := testutil.ToFloat64(metrics.ReconcileTotal.WithLabelValues(guardrailKind, "success"))
	if after <= before {
		t.Errorf("reconcile_total{guardrail,success} did not increment on steady-state (before=%v after=%v)", before, after)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `make test-envtest-pkg PKG=./internal/controller/... FOCUS=TestGuardRail_SteadyState_IncrementsReconcileTotal`
Expected: FAIL — the steady-state return at 475 omits the increment.

- [ ] **Step 3: Apply the fix** — add the increment before the steady-state `return ctrl.Result{}, nil` at line 475:

```go
		metrics.ReconcileTotal.WithLabelValues(guardrailKind, "success").Inc()
		return ctrl.Result{}, nil
```

(Place it after the `PoolSize` refresh block, immediately before the existing `return` — matching every other success return in the file.)

- [ ] **Step 4: Run to verify it passes**

Run: `make test-envtest-pkg PKG=./internal/controller/... FOCUS=TestGuardRail_SteadyState_IncrementsReconcileTotal`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/controller/litellmguardrail_controller.go internal/controller/litellmguardrail_controller_test.go
git commit -m "fix(guardrail): increment reconcile_total on the steady-state path"
```

### Task 7.4: Generalize the recreate circuit breaker to MCP/Team/A2A (Finding #9)

The breaker (`churnGuard`) is wired only on `ModelReconciler`. The sibling controllers share `probeVanishedResourceID` (the vanish/clear-ID path) but have no breaker, so a created-but-not-listed MCP/team/agent storms LiteLLM. Wire the same breaker onto the three sibling reconcilers at their recreate sites.

**Files:**
- Modify: `internal/controller/mcpserver_controller.go`, `team_controller.go`, `a2aagent_controller.go` (add `Churn *churnGuard` field + `SetupWithManager` init + Count/Record/Forget at the recreate site)
- Test: `internal/controller/churn_guard_test.go` already covers the breaker mechanics; add one envtest per controller OR rely on the shared mechanism test.

- [ ] **Step 1: Read the model controller's breaker wiring** to mirror it exactly:

Run: `grep -n "r.Churn\|Churn \*churnGuard\|newChurnGuard\|reasonRecreateThrottled\|RecreateLimit\|churnWindow\|recreateThrottleBackoff" internal/controller/model_controller.go`
Note the three touch points: (a) struct field, (b) `SetupWithManager` init `r.Churn = newChurnGuard()`, (c) the CREATE/recreate block (`Count` gate + `Record` on recreate + `Forget` on steady-state).

- [ ] **Step 2: Add the field + init to `MCPServerReconciler`**

Add `Churn *churnGuard` to the struct, and in its `SetupWithManager` add `r.Churn = newChurnGuard()`. Also add the `RecreateLimit int` field if the model controller has one (mirror its name).

- [ ] **Step 3: Wire Count/Record/Forget at the MCP recreate site**

At the MCP CREATE branch that follows a vanish-probe clear (where `serverID == ""` and it is NOT the first reconcile), add the breaker gate mirroring `model_controller.go:619-635`:

```go
		if !firstReconcile {
			limit := r.RecreateLimit
			if limit <= 0 {
				limit = DefaultRecreateLimitPerMin
			}
			if n := r.Churn.Count(key); n >= limit {
				msg := fmt.Sprintf("recreate throttled: %d recreates within %s (limit %d); created-but-not-listed. Retrying after %s.",
					n, churnWindow, limit, recreateThrottleBackoff)
				if werr := r.writeStatus(ctx, &mcp, metav1.ConditionFalse, reasonRecreateThrottled, msg); werr != nil {
					logStatusUpdateErr(logger, werr, "reason", reasonRecreateThrottled)
				}
				metrics.ReconcileTotal.WithLabelValues(mcpServerKind, "success").Inc()
				return ctrl.Result{RequeueAfter: recreateThrottleBackoff}, nil
			}
		}
```

Add `r.Churn.Record(key)` right after a successful recreate POST, and `r.Churn.Forget(key)` on the steady-state return. `key` is the CR's `types.NamespacedName` (build it from `req.NamespacedName` or `client.ObjectKeyFromObject(&mcp)`). The constants `DefaultRecreateLimitPerMin`, `churnWindow`, `recreateThrottleBackoff`, `reasonRecreateThrottled` already live in the controller package (defined for the model controller) — reuse them.

- [ ] **Step 4: Repeat Steps 2-3 for `TeamReconciler` and `A2AAgentReconciler`** at their respective recreate sites (after `probeVanishedResourceID` clears the ID and the CREATE branch fires). Use `teamKind`/`a2aAgentKind` for the metric labels.

- [ ] **Step 5: Verify it builds**

Run: `make build-operator`
Expected: success.

- [ ] **Step 6: Add an envtest** per controller asserting the breaker trips — model already has one (`grep -n "RecreateThrottled" internal/controller/model_controller_test.go` for the template). Mirror it for MCP using a mock that 200s the create but never lists the server (the router-model mock trait at mock.go:944 is the analog; for MCP you may need a mock toggle that accepts the create but omits it from the list — if absent, add a minimal `mockServer.SetMCPCreateNotListed(true)` flag mirroring the router-create logic). If adding that mock toggle is out of scope for this pass, assert the breaker mechanics via the existing `churn_guard_test.go` unit coverage and document the controller-level envtest as a follow-up in the commit message.

- [ ] **Step 7: Run the controller suite**

Run: `make test-envtest-pkg PKG=./internal/controller/...`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/controller/mcpserver_controller.go internal/controller/team_controller.go internal/controller/a2aagent_controller.go internal/controller/*_test.go
git commit -m "fix(controllers): wire recreate circuit breaker on mcpserver/team/a2aagent vanish path"
```

---

## Phase 8: Efficiency — drop duplicate per-reconcile LiteLLM round-trips (Finding #15)

Low priority; each is a localized change. Skip if time-constrained.

> **DEFERRED (2026-06-13 execution).** Not landed in the code-review-fixes
> branch. The Model sub-change (Task 8.1 Step 2) as specified is unsafe:
> `probeVanishedResourceID` returns `clear=true` for THREE reasons — 404,
> resolved-empty, AND id-drift (entry exists under a new id). The Step 9
> idempotency probe is what adopts the drifted id; skipping it on
> `probedAbsent=clear` would issue a duplicate `POST /model/new` on the
> drift path. A correct version must restructure the probe helper to
> return the resolved id and distinguish absent-vs-drift across all 4
> callers — a larger, envtest-verified change. Do this as its own plan.

### Task 8.1: Reuse the Step 7b list/GET result instead of re-fetching

**Files:**
- Modify: `internal/controller/team_controller.go` (Step 8b + Step 10 both call `ListTeamsByAlias`), `internal/controller/model_controller.go` (Step 7b probe + Step 9 CREATE both call `GetModelInfoByName`), `internal/controller/mcpserver_controller.go` (`resolveServerIDByName` issues an uncached list though Step 7b already fetched the cached list)

- [ ] **Step 1: Team — thread the Step 8b result into Step 10**

Read `team_controller.go` Step 8b (~line 509) and Step 10 (~line 570). Capture the `ListTeamsByAlias` result from Step 8b in a local variable when `TeamID != ""`, and in Step 10 reuse it instead of re-calling. Guard: only reuse when the probe ran in this reconcile.

- [ ] **Step 2: Model — reuse the Step 7b probe outcome in Step 9**

Read `model_controller.go` Step 7b (~line 531) and the Step 9 CREATE idempotency probe (~line 636). When Step 7b already determined absence (cleared the ID), skip the redundant `GetModelInfoByName` at Step 9 by carrying a `probedAbsent bool` into the CREATE branch and only probing when `!probedAbsent`.

- [ ] **Step 3: MCPServer — pass the cached list into `resolveServerIDByName`**

Read `mcpserver_controller.go` Step 7b (uses `CachedListMCPServers`) and `resolveServerIDByName` (~line 599, issues an uncached `ListMCPServers`). Change `resolveServerIDByName` to accept the already-fetched `[]MCPServerEntry` (or call `CachedListMCPServers`) instead of a fresh uncached list.

- [ ] **Step 4: Verify no behavior change via the existing controller suite**

Run: `make test-envtest-pkg PKG=./internal/controller/...`
Expected: PASS (these are pure efficiency changes; existing tests already assert mutation SHAPES with `>=` and exact-zero for delete/new, so a reduced read count must not break them — but re-run to confirm).

- [ ] **Step 5: Commit**

```bash
git add internal/controller/team_controller.go internal/controller/model_controller.go internal/controller/mcpserver_controller.go
git commit -m "perf(controllers): reuse per-reconcile list/GET results instead of re-fetching"
```

---

## Phase 9: DRY consolidation (Finding #14) — RECOMMENDED SEPARATE PLAN

**Do not execute this phase inline with the bug fixes.** Per the writing-plans Scope Check, this is a large, pure-refactor (no behavior change) that touches all six controllers and is best done AFTER Phases 1-8 land — with the new regression tests as a safety net, and as its own reviewable PR. Mixing a sweeping refactor with behavior fixes makes review and bisection much harder.

When you write that separate plan, the extraction targets are:
- `classifyMutationError` → one package-level function taking a `writeStatus` func + kind label (5 copies today).
- The inline 400-499 `fmt.Sprintf` 4xx loop + `is4xxError` + `isTransientLiteLLMError` → a single `errors.As(&litellm.RejectedError)` check on the typed `Status` field (replaces ~4 string-prefix scanners; also removes the per-call 100-string allocation).
- `onAckMissing` closure → one helper (5 copies).
- The SEC-03 duplicate-`as` check → one helper (4 copies).
- `IndexXSecretRefs` + the 5 duplicate index-key constants → one generic indexer.
- `writeStatus` → one generic `writeStatusWithRetry[T client.Object]` (and standardize MCP/Team onto the retry variant).
- `SafetyRelistRunnable` (Model + GuardRail duplicates) → one parameterized runnable.

Each extraction is a "tests stay green before and after" refactor: run `make test-full` before, extract, run `make test-full` after, commit per extraction.

---

## Phase 10: Final gates

### Task 10: Full verification before opening the PR

**Files:** none

- [ ] **Step 1: Regenerate (sanity — should be a no-op, no CRD changes)**

Run: `make gen-code gen-manifests`
Expected: no diff. If there IS a diff, investigate (none of these fixes should alter generated files).

- [ ] **Step 2: Lint**

Run: `make qa-lint`
Expected: PASS.

- [ ] **Step 3: Unit + envtest**

Run: `make test-full`
Expected: PASS (`test-unit` + `test-envtest`, race-enabled).

- [ ] **Step 4: E2E (controllers + litellm + helm touched)**

Run: `make e2e-full`
Expected: PASS (cluster KEPT after; teardown with `make cluster-down`).

- [ ] **Step 5: Security gate**

Run: `make qa-security`
Expected: PASS.

- [ ] **Step 6: Update CLAUDE.md "Common failure modes" if any new mode was discovered**

Per the repo's documentation-hygiene rule, if any fix changed a documented contract (e.g. the guardrail confirmed-absent drain, the connection_ready restart behavior), add/adjust the corresponding `### ❌ ... ✅ ...` entry in `CLAUDE.md` in the same change. Commit any doc edits.

- [ ] **Step 7: Push (the installed pre-push hook runs the full gate)**

```bash
git push -u origin fix/code-review-2026-06-13
```

Expected: pre-push gate passes; branch pushed. Open a PR to `main`.

---

## Self-Review

**Spec coverage:** Findings #1 (Phase 1), #2 (Tasks 2.2-2.4), #3 (Task 2.5), #4 (Task 3.1), #5 (Phase 4), #6 (Task 5.1), #7 (Task 3.2), #8 (Phase 6), #9 (Task 7.4), #10 (Task 7.1), #11 (Task 5.2), #12 (Task 7.2), #13 (Task 7.3), #15 (Phase 8). #14 → Phase 9 (separate plan, by design). All review findings mapped.

**Type/name consistency:** `infoMap`/`ModelInfo.Extra`/`MarshalJSON` consistent across Tasks 1.1-1.4. `onAckMissing`/`is4xxError`/`poolSize`/`checkGuardrailPool`/`churnGuard`/`probeVanishedResourceID`/`InvalidateOn401`/`lastReady`/`writeBothConditions`/`ConnectionReady`/`connectionReasonAll` all match the symbols verified in source. `ModeDelete422`/`LastModelInfoBody` are new and used consistently by their tests.

**Known authoring gaps (flagged inline, not placeholders):** Tasks 2.3/2.4/2.5/5.2/6.1/7.3 reuse existing per-controller test helpers (`mcpSampleCR`/`guardrailSampleCR`/`pollGuardRailCondition`/`ensureCredentialSecret` and the suite's status-update mechanism) whose exact names live in the respective `*_test.go` files; the engineer opens the file and substitutes the real helper name. The fix diffs in those tasks are exact; only the test scaffolding references local helpers by their role.
