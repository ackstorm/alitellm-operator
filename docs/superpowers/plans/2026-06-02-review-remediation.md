# Code-Review Remediation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Resolve the remaining 🟡/🔵 findings from `references/reviews/2026-06-02-full-code-review.md` (the 6 🔴 already landed in PR #77), fixing genuine bugs with TDD and explicitly deciding the contestable ones instead of blind-patching.

**Architecture:** The alitellm-operator is a controller-runtime v0.19.4 Kubernetes operator that reconciles LiteLLM state from CRDs (`litellm.ackstorm.ai/v1alpha1`) via HTTP. Toolchain runs in the `litellm-devtools` container — every `make` target self-routes; run targets bare. No `go` on host PATH; use `./scripts/dev.sh go ...` only when no make target fits.

**Tech Stack:** Go 1.26, controller-runtime v0.19.4, k8s.io/* v0.31.0, Ginkgo/Gomega (e2e) + stdlib `testing` (unit/envtest), Prometheus client_golang, golangci-lint.

**Honest scope note (read before starting):** Of the 9 🟡 findings, only **#2 (OAuth2Flow), #4 (reason mislabel), #6 (silent fan-in log)** are genuine bugs. **#5** is code duplication (the codebase deliberately classifies 4xx by the frozen `RejectedError.Error()` string shape — `errors.As` would be a codebase-wide refactor against a documented contract, so the real fix is DRY). **#1, #3, #7, #8, #9** are contestable — each is documented in-code as intentional, and at least two of the reviewer's proposed "fixes" would *introduce* regressions (loosen M-SEC2 / break the discovery disjoint-partition invariant). Those are **decision tasks (Group C)**, not code changes — do not implement them without an explicit decision recorded in the task.

---

## File Structure

| File | Responsibility | Tasks |
|------|----------------|-------|
| `internal/litellm/types.go` | LiteLLM HTTP request/response structs | T1 |
| `internal/litellm/types_test.go` (create) | JSON round-trip tests for request structs | T1 |
| `internal/controller/mcpserver_controller.go` | MCPServer reconciler; UPDATE arm | T1 |
| `internal/controller/modelalias_controller.go` | ModelAlias reconciler; router-settings sync | T2 |
| `internal/controller/modelalias_error_test.go` (create) | unit test for error→reason classifier | T2 |
| `internal/controller/connection_fanin.go` | shared Connection→dependent fan-in mapper | T3 |
| `internal/controller/model_controller.go` | Model reconciler; `classifyMutationError` | T4 |
| `internal/controller/litellmguardrail_controller.go` | owns shared `is4xxError` helper (reused by T4) | T4 |
| `api/litellm/v1alpha1/mcpserverdiscovery_types.go` | MCPServerDiscovery CRD godoc | T5, T6 |
| `internal/controller/mcpserverdiscovery_controller.go` | rename stale `dotted` local | T5 |
| `api/litellm/v1alpha1/modeldiscovery_types.go` | ModelDiscovery CRD godoc | T6 |
| `internal/controller/safety_relist.go` | safety-relist interval setter godoc | T7 |
| `internal/controller/rejected_message.go` | redaction regex godoc | T8 |

Decision tasks (Group C) touch `internal/metrics/metrics.go`, `internal/filters/filters.go`, `internal/litellm/endpoint.go`, `internal/litellm/list_cache.go`, `internal/controller/modeldiscovery_controller.go` **only if** the recorded decision is to change code.

---

## Group A — Genuine bug fixes (TDD)

### Task 1: Forward `OAuth2Flow` on MCPServer UPDATE (#2)

`extractMCPParams` reads `oauth2_flow` (`params_mcp.go:104`) into `extractedMCPParams.OAuth2Flow`, and the CREATE arm forwards it (`mcpserver_controller.go:639`). But `MCPServerUpdateRequest` has no `OAuth2Flow` field and the UPDATE arm (`mcpserver_controller.go:663-686`) never sets it — so any server created with `oauth2_flow` silently loses it on the first spec-change UPDATE.

**Files:**
- Modify: `internal/litellm/types.go:204-231` (add field to `MCPServerUpdateRequest`)
- Modify: `internal/controller/mcpserver_controller.go:663-686` (wire `OAuth2Flow` into the update request)
- Test: `internal/litellm/types_test.go` (create)

- [ ] **Step 1: Write the failing test**

Create `internal/litellm/types_test.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package litellm

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestMCPServerUpdateRequest_ForwardsOAuth2Flow guards against the
// review finding #2: oauth2_flow must survive a PUT /v1/mcp/server,
// matching the CREATE request which already carries the field.
func TestMCPServerUpdateRequest_ForwardsOAuth2Flow(t *testing.T) {
	req := &MCPServerUpdateRequest{
		ServerID:   "srv-123",
		OAuth2Flow: "authorization_code",
	}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"oauth2_flow":"authorization_code"`) {
		t.Errorf("oauth2_flow not serialized on update request: %s", b)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `make test-unit-pkg PKG=./internal/litellm/...`
Expected: FAIL — `req.OAuth2Flow` is an unknown field → compile error (`unknown field OAuth2Flow in struct literal`).

- [ ] **Step 3: Add the field to the struct**

In `internal/litellm/types.go`, inside `MCPServerUpdateRequest` (after the `RegistrationURL` line, mirroring the create struct's placement):

```go
	RegistrationURL           string         `json:"registration_url,omitempty"`
	OAuth2Flow                string         `json:"oauth2_flow,omitempty"`
	AllowAllKeys              *bool          `json:"allow_all_keys,omitempty"`
```

- [ ] **Step 4: Run test to verify it passes**

Run: `make test-unit-pkg PKG=./internal/litellm/...`
Expected: PASS.

- [ ] **Step 5: Wire the field in the UPDATE arm**

In `internal/controller/mcpserver_controller.go`, the `updateReq := &litellm.MCPServerUpdateRequest{...}` literal (around L663). Add `OAuth2Flow` immediately after `RegistrationURL` so the update mirrors the create arm:

```go
			RegistrationURL:           ext.RegistrationURL,
			OAuth2Flow:                ext.OAuth2Flow,
			AllowAllKeys:              ext.AllowAllKeys,
```

- [ ] **Step 6: Verify build + envtest for the mcpserver controller**

Run: `make build-operator`
Expected: clean build.
Run: `make test-envtest-pkg PKG=./internal/controller/... FOCUS=TestMCPServer`
Expected: PASS (no regression; existing MCPServer specs still green).

- [ ] **Step 7: Commit**

```bash
git add internal/litellm/types.go internal/litellm/types_test.go internal/controller/mcpserver_controller.go
git commit -m "fix(mcpserver): forward oauth2_flow on UPDATE (review #2)

MCPServerUpdateRequest had no oauth2_flow field; the UPDATE arm dropped
the extracted value, so a server created with oauth2_flow lost it on the
first spec-change PUT. Adds the field + wires it to mirror the CREATE arm.

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 2: Correct the transient-error reason label on ModelAlias (#4)

`modelalias_controller.go:128-136` routes BOTH `GetRouterSettings` and `UpdateRouterSettings` errors to `broadcastNotReady(..., reasonModelAliasRejected, ...)` regardless of error class. The `RequeueAfter` (not `return err`) behaviour is **intentional per the M-B2 comment** (avoids stalling until the 15m resync) — do NOT change that. The real bug is the **reason label**: a transient 5xx / network / 401 error is mislabelled `LiteLLMRejected`, which (a) misleads `kubectl describe` and (b) poisons the `LitellmOperatorReconcileTotal` metric — `metrics.ReasonToReconcileResult` maps `LiteLLMRejected → "rejected"`, but a 5xx belongs in `"transient_error"`.

**Files:**
- Modify: `internal/controller/modelalias_controller.go:128-136` (classify before choosing reason)
- Test: `internal/controller/modelalias_error_test.go` (create)

- [ ] **Step 1: Write the failing test**

Create `internal/controller/modelalias_error_test.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"errors"
	"testing"

	"github.com/ackstorm/alitellm-operator/internal/litellm"
)

// TestModelAliasErrorReason verifies the error→reason classifier picks
// reasonModelAliasRejected only for deterministic 4xx (non-401), and
// reasonLiteLLMUnavailable for transient (5xx / network / 401) errors —
// so the condition reason and the reconcile_total metric bucket agree
// with the actual failure class (review #4).
func TestModelAliasErrorReason(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"4xx rejected", &litellm.RejectedError{Method: "POST", Path: "/config/update", Status: 400, Code: "400"}, reasonModelAliasRejected},
		{"401 transient", &litellm.Auth401Error{Path: "/get/config/callbacks"}, reasonLiteLLMUnavailable},
		{"5xx transient", &litellm.RejectedError{Method: "GET", Path: "/get/config/callbacks", Status: 503, Code: "503"}, reasonLiteLLMUnavailable},
		{"network transient", errors.New("dial tcp: connection refused"), reasonLiteLLMUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := modelAliasErrorReason(tc.err); got != tc.want {
				t.Errorf("modelAliasErrorReason(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `make test-unit-pkg PKG=./internal/controller/...`
Expected: FAIL — `undefined: modelAliasErrorReason` (compile error).

- [ ] **Step 3: Add the classifier helper**

In `internal/controller/modelalias_controller.go`, add near the other package-level helpers (e.g. just below `filterAliveAliases`):

```go
// modelAliasErrorReason classifies a router-settings call error into the
// condition reason used by broadcastNotReady. A deterministic 4xx
// (non-401) is reasonModelAliasRejected (LiteLLMRejected); everything else
// — transient 5xx, network, 401 — is reasonLiteLLMUnavailable so the
// condition reason and the reconcile_total metric bucket (via
// metrics.ReasonToReconcileResult) match the real failure class. The
// RequeueAfter recovery in broadcastNotReady is unchanged (M-B2).
func modelAliasErrorReason(err error) string {
	var auth401 *litellm.Auth401Error
	if errors.As(err, &auth401) {
		return reasonLiteLLMUnavailable
	}
	if is4xxError(err) {
		return reasonModelAliasRejected
	}
	return reasonLiteLLMUnavailable
}
```

Confirm `errors` and the `internal/litellm` package are already imported in this file; if not, add them.

- [ ] **Step 4: Run test to verify it passes**

Run: `make test-unit-pkg PKG=./internal/controller/...`
Expected: PASS.

- [ ] **Step 5: Use the classifier at both call sites**

In `internal/controller/modelalias_controller.go`, replace the two hard-coded `reasonModelAliasRejected` arguments (the `GetRouterSettings` and `UpdateRouterSettings` error branches):

```go
	current, err := cli.GetRouterSettings(ctx)
	if err != nil {
		msg := fmt.Sprintf("GET /get/config/callbacks: %v", err)
		return r.broadcastNotReady(ctx, list.Items, modelAliasErrorReason(err), msg, snap.NormalizedRequeueOnRejectedAfter(), logger)
	}
	current.ModelGroupAlias = agg.Desired
	if err := cli.UpdateRouterSettings(ctx, current); err != nil {
		msg := fmt.Sprintf("POST /config/update: %v", err)
		return r.broadcastNotReady(ctx, list.Items, modelAliasErrorReason(err), msg, snap.NormalizedRequeueOnRejectedAfter(), logger)
	}
```

- [ ] **Step 6: Verify build + lint**

Run: `make build-operator`
Expected: clean.
Run: `make qa-lint-changed`
Expected: no new findings.

- [ ] **Step 7: Commit**

```bash
git add internal/controller/modelalias_controller.go internal/controller/modelalias_error_test.go
git commit -m "fix(modelalias): label transient router errors LiteLLMUnavailable (review #4)

GetRouterSettings/UpdateRouterSettings errors were unconditionally
labelled reasonModelAliasRejected, mislabelling transient 5xx/network/401
and mis-bucketing the reconcile_total metric (ReasonToReconcileResult maps
LiteLLMRejected->rejected). Classifier now picks Unavailable for transient,
Rejected only for deterministic 4xx. RequeueAfter recovery unchanged (M-B2).

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 3: Log dropped errors in the Connection fan-in mapper (#6)

`connectionFanIn` (`connection_fanin.go:75-83`) returns `nil` on both `c.List` and `apimeta.ExtractList` errors with no log. A transient API error during the Connection-Ready recovery fan-in silently drops all dependent re-enqueues, defeating the fan-in's purpose. Mappers cannot return an error, so the correct remedy is a `V(1)` log (the same level `logEmptyFanInNamespace` uses).

**Files:**
- Modify: `internal/controller/connection_fanin.go:75-83`

- [ ] **Step 1: Inspect the existing log helper to match style**

Run: `grep -n "func logEmptyFanInNamespace" internal/controller/connection_fanin.go`
Read that function so the new log lines match its `logr` retrieval pattern (`log.FromContext(ctx)` or a passed logger).

- [ ] **Step 2: Add V(1) logs on both error paths**

In `internal/controller/connection_fanin.go`, replace:

```go
	if err := c.List(ctx, list, client.InNamespace(ns)); err != nil {
		return nil
	}
	objs, err := apimeta.ExtractList(list)
	if err != nil {
		return nil
	}
```

with:

```go
	if err := c.List(ctx, list, client.InNamespace(ns)); err != nil {
		log.FromContext(ctx).V(1).Info("connection fan-in: List failed, dropping re-enqueue",
			"kind", kindLabel, "namespace", ns, "err", err.Error())
		return nil
	}
	objs, err := apimeta.ExtractList(list)
	if err != nil {
		log.FromContext(ctx).V(1).Info("connection fan-in: ExtractList failed, dropping re-enqueue",
			"kind", kindLabel, "err", err.Error())
		return nil
	}
```

Ensure `sigs.k8s.io/controller-runtime/pkg/log` is imported (alias `log`); if `logEmptyFanInNamespace` already uses a different accessor, reuse that exact accessor instead.

- [ ] **Step 3: Verify build + lint**

Run: `make build-operator`
Expected: clean.
Run: `make qa-lint-changed`
Expected: no new findings.

- [ ] **Step 4: Commit**

```bash
git add internal/controller/connection_fanin.go
git commit -m "fix(connection): log dropped errors in fan-in mapper (review #6)

List/ExtractList errors were swallowed silently, dropping dependent
re-enqueues during Connection-Ready recovery. Adds V(1) logs so the drop
is observable; mapper still returns nil (mappers cannot return errors).

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 4: DRY — replace `model_controller`'s inline 4xx loops with the shared helper (#5)

`model_controller.go:748-765` hand-rolls 4xx detection with two overlapping string-prefix loops. The package already exports `is4xxError(err error) bool` (`litellmguardrail_controller.go:713`), which does the same `"litellm: %d on"` prefix match — the deliberately-frozen `RejectedError.Error()` shape (see `errors.go:84-97`). This is duplication, not a behaviour bug; the fix is to reuse the helper. (Do **not** migrate to `errors.As` — that would diverge from `is4xxError`/`is4xxNon401Status`/`isTransientLiteLLMError`, which all match on the string contract by design.)

**Files:**
- Modify: `internal/controller/model_controller.go:746-766`
- Test: `internal/controller/litellmguardrail_controller.go` helper — add a unit test if none exists

- [ ] **Step 1: Check whether `is4xxError` already has a unit test**

Run: `grep -rn "is4xxError" internal/controller/*_test.go`
If a test exists, note it and skip Step 2. If not, add one in Step 2.

- [ ] **Step 2: Add a unit test for `is4xxError` (only if Step 1 found none)**

Create `internal/controller/is4xx_test.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"errors"
	"testing"

	"github.com/ackstorm/alitellm-operator/internal/litellm"
)

func TestIs4xxError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"400 rejected", &litellm.RejectedError{Method: "POST", Path: "/model/new", Status: 400, Code: "400"}, true},
		{"422 rejected", &litellm.RejectedError{Method: "POST", Path: "/model/new", Status: 422, Code: "422"}, true},
		{"500 not 4xx", &litellm.RejectedError{Method: "POST", Path: "/model/new", Status: 500, Code: "500"}, false},
		{"network not 4xx", errors.New("dial tcp: connection refused"), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := is4xxError(tc.err); got != tc.want {
				t.Errorf("is4xxError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
```

- [ ] **Step 3: Run the helper test to confirm current behaviour (baseline GREEN)**

Run: `make test-unit-pkg PKG=./internal/controller/...`
Expected: PASS — this pins `is4xxError`'s contract before the refactor.

- [ ] **Step 4: Replace the inline loops in `classifyMutationError`**

In `internal/controller/model_controller.go`, replace the whole inline block (the `errStr := err.Error()` declaration, the `is4xx := false`, and both `for` loops, L748-765) with:

```go
	errStr := err.Error()
	is4xx := is4xxError(err)
```

Leave the subsequent `if is4xx { ... }` block (which calls `rejectedMessage(opDesc, err, errStr)`) untouched — `errStr` is still consumed there.

- [ ] **Step 5: Run the helper test + verify build**

Run: `make build-operator`
Expected: clean.
Run: `make test-unit-pkg PKG=./internal/controller/...`
Expected: PASS.

- [ ] **Step 6: Verify no model-controller regression in envtest**

Run: `make test-envtest-pkg PKG=./internal/controller/... FOCUS=TestModel`
Expected: PASS (classification behaviour is preserved — same string contract).

- [ ] **Step 7: Commit**

```bash
git add internal/controller/model_controller.go internal/controller/is4xx_test.go
git commit -m "refactor(model): reuse is4xxError in classifyMutationError (review #5)

classifyMutationError duplicated 4xx detection with two inline string-
prefix loops. Replaced with the shared is4xxError helper (same frozen
RejectedError.Error() string contract). Behaviour-preserving DRY cleanup.

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Group B — Documentation / nit fixes (no behaviour change)

### Task 5: Fix MCPServerDiscovery filter-target godoc drift + rename stale `dotted` local (#10)

Post-v0.3.0 the reconciler computes `childName := md.Spec.Prefix + "-" + sourceName` (`mcpserverdiscovery_controller.go:486`) and filters against that (`filters.Apply(dotted, ...)` at L582, where the local `dotted` actually holds `<prefix>-<source>` names). The CRD godoc still documents the pre-v0.3.0 `<discovery-name>.<toolhive-namespace>.<toolhive-name>` dotted form at three sites — so a user authoring filter regexes against the documented format will never match. Doc-only correctness fix + a variable rename for clarity.

**Files:**
- Modify: `api/litellm/v1alpha1/mcpserverdiscovery_types.go` (lines ~107, ~171, ~354)
- Modify: `internal/controller/mcpserverdiscovery_controller.go:572-602` (rename `dotted` → `childNames`)

- [ ] **Step 1: Fix `MCPServerDiscoverySpec.Filters` godoc (~L107)**

Replace:

```go
	// Filters narrows the post-derivation candidate set via RE2
	// include/exclude patterns matched against the POST-DERIVATION
	// dotted three-part name (`<discovery-name>.<toolhive-namespace>.
	// <toolhive-name>`) per spec §6.5. Empty (absent) Filters means
```

with:

```go
	// Filters narrows the post-derivation candidate set via RE2
	// include/exclude patterns matched against the generated child name
	// `<spec.prefix>-<source-name>` (v0.3.0 breaking change; pre-v0.3.0
	// used a dotted three-part name). Empty (absent) Filters means
```

- [ ] **Step 2: Fix `MCPServerDiscoveryFilters` godoc (~L171)**

Replace:

```go
// MCPServerDiscoveryFilters carries the RE2 include/exclude pattern lists
// applied to the POST-DERIVATION dotted three-part name (per spec §6.5).
// The filter target is the DOTTED name, NOT the bare ToolHive object
// name; this is the most common source of user confusion at runtime and
// Will land a regression test exercising the dotted form.
```

with:

```go
// MCPServerDiscoveryFilters carries the RE2 include/exclude pattern lists
// applied to the generated child name `<spec.prefix>-<source-name>`
// (v0.3.0 breaking change; pre-v0.3.0 used a dotted three-part name).
// The filter target is the prefixed child name, NOT the bare ToolHive
// object name — the most common source of user confusion at runtime.
```

- [ ] **Step 3: Fix `MCPServerSkippedCandidate.Name` godoc (~L354)**

Replace:

```go
	// Name is the candidate dotted three-part name
	// (`<discovery-name>.<toolhive-namespace>.<toolhive-name>`) that
	// would have become the child MCPServer's metadata.name.
```

with:

```go
	// Name is the candidate child name `<spec.prefix>-<source-name>`
	// that would have become the child MCPServer's metadata.name
	// (v0.3.0 breaking change; pre-v0.3.0 used a dotted three-part name).
```

- [ ] **Step 4: Rename the stale `dotted` local in the reconciler**

In `internal/controller/mcpserverdiscovery_controller.go` (~L572-602), rename the local variable `dotted` to `childNames` at every occurrence in that scope (declaration, the `dotted[i] = c.childName` assignment, the `filters.Apply(dotted, adapted)` call, and the "keeping only kept dotted names" rebuild). Update the nearby comments that say "dotted name" to "child name" where they describe the filter target (do NOT rewrite the historical "pre-v0.3.0 was the dotted" notes — those are accurate history).

- [ ] **Step 5: Regenerate the CRD reference docs**

Run: `make gen-crd-ref-docs`
Expected: `docs/api-reference/` regenerated; the MCPServerDiscovery filter field description now reflects `<spec.prefix>-<source-name>`.

- [ ] **Step 6: Verify build + lint**

Run: `make build-operator`
Expected: clean (rename is local; no signature change).
Run: `make qa-lint-changed`
Expected: no new findings.

- [ ] **Step 7: Commit**

```bash
git add api/litellm/v1alpha1/mcpserverdiscovery_types.go internal/controller/mcpserverdiscovery_controller.go docs/api-reference/
git commit -m "docs(mcpserverdiscovery): correct filter-target format to <prefix>-<source> (review #10)

CRD godoc still documented the pre-v0.3.0 dotted three-part filter target;
the reconciler filters against <spec.prefix>-<source-name>. User regexes
against the documented form never matched. Also renames the stale 'dotted'
local to 'childNames'. Regenerated api-reference.

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 6: Clarify `+optional` on the discovery int32 count fields (#13)

`DiscoveredCount` / `GeneratedCount` are `int32` value fields carrying both `+optional` and `+kubebuilder:default=0`. `+optional` reads as misleading on a non-pointer value type (it is never absent in JSON). The CRD is correct; this is a comment clarity fix only.

**Files:**
- Modify: `api/litellm/v1alpha1/modeldiscovery_types.go` (the `DiscoveredCount`/`GeneratedCount` markers, ~L334)
- Modify: `api/litellm/v1alpha1/mcpserverdiscovery_types.go` (same fields, ~L277)

- [ ] **Step 1: Replace the misleading marker comment on each count field**

For each of the four fields (`DiscoveredCount`, `GeneratedCount` in both files), replace the bare `// +optional` line with an explanatory note while keeping the kubebuilder marker:

```go
	// Always serialized (value type, defaults to 0). The +optional marker
	// only relaxes CRD required-field validation; it is never absent.
	// +optional
	// +kubebuilder:default=0
	DiscoveredCount int32 `json:"discoveredCount"`
```

Apply the analogous edit to `GeneratedCount`.

- [ ] **Step 2: Regenerate CRD manifests + reference docs (marker change is benign but keep in sync)**

Run: `make gen-manifests gen-crd-ref-docs`
Expected: no functional CRD diff (the markers are unchanged; only the preceding comment text differs, which does not affect generated YAML). If `git diff` shows manifest changes, investigate before committing.

- [ ] **Step 3: Commit**

```bash
git add api/litellm/v1alpha1/modeldiscovery_types.go api/litellm/v1alpha1/mcpserverdiscovery_types.go
git commit -m "docs(api): clarify +optional on int32 discovery count fields (review #13)

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 7: Note GuardRail's intentional safety-relist exclusion (#11)

`SetSafetyRelistIntervals` covers MCPServer/Model/Team/A2AAgent. GuardRail is swept by `BootSweeper` but has no relist interval and is intentionally excluded. The godoc gives no signal this is deliberate, risking a future maintainer "completing the set" incorrectly.

**Files:**
- Modify: `internal/controller/safety_relist.go:73-89`

- [ ] **Step 1: Append an exclusion note to the `SetSafetyRelistIntervals` godoc**

After the existing godoc paragraph (before the `func SetSafetyRelistIntervals` line), add:

```go
// NOTE: GuardRail is intentionally NOT covered here. The guardrail
// reconciler has no RequeueAfter safety-relist path (it relies on the
// BootSweeper relist + watch events), so there is no guardrail interval
// var to set. Do not add one without also adding the reconciler-side
// requeue.
```

- [ ] **Step 2: Verify build**

Run: `make build-operator`
Expected: clean.

- [ ] **Step 3: Commit**

```bash
git add internal/controller/safety_relist.go
git commit -m "docs(controller): note GuardRail's intentional safety-relist exclusion (review #11)

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 8: Note the `claude-` redaction false-positive trade-off (#12)

`secretShapedTokenRE` (`rejected_message.go:48`) includes `claude-[A-Za-z0-9_\-]{8,}`, which would also redact an Anthropic model name (e.g. `claude-3-opus-20240229`) if it appeared in `rej.Code`/`rej.Type` on the default path. The existing comment says "Non-exhaustive by design" but does not call out this specific over-match. Comment-only.

**Files:**
- Modify: `internal/controller/rejected_message.go:40-49`

- [ ] **Step 1: Extend the regex godoc with the over-match caveat**

After the existing "Non-exhaustive by design …" sentence in the `secretShapedTokenRE` godoc block, add:

```go
// Known over-match: the claude- alternative also matches Anthropic MODEL
// names (e.g. "claude-3-opus-20240229") if one ever lands in rej.Code or
// rej.Type. Accepted: redacting a model name in a rejection message is
// harmless, and erring toward redaction is the intended bias.
```

- [ ] **Step 2: Verify build**

Run: `make build-operator`
Expected: clean.

- [ ] **Step 3: Commit**

```bash
git add internal/controller/rejected_message.go
git commit -m "docs(controller): note claude- model-name redaction over-match (review #12)

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Group C — Decision tasks (record a decision BEFORE any code)

> These four findings are contestable. Each is documented in-code as intentional, and two of the reviewer's proposed fixes would introduce regressions. **Do not write code for any of these until a decision is recorded.** Default recommendation for each is given; if the decision is "keep behaviour", the deliverable is a one-line code comment or a CLAUDE.md `Common failure modes` entry — not a behaviour change.

### Decision D1: `LitellmOperatorReconcileTotal` namespace-label cardinality (#1)

- **File:** `internal/metrics/metrics.go:40-47`
- **Tension:** the `namespace` label makes the series count `kinds × namespaces × results`, unbounded in a multi-tenant cluster. But Codex flagged it PARTIAL: the operator's typical single-watch-namespace deployment bounds it, and the label is the whole point of the metric (slice rejections by namespace).
- **Options:** (a) keep + document the cardinality assumption in the metric godoc and CLAUDE.md; (b) drop the `namespace` label (loses per-namespace slicing); (c) gate the label behind a flag.
- **Recommendation:** (a) keep + document — the metric exists specifically for namespace slicing, and multi-tenant cluster-wide watch is not the supported topology.
- [ ] Record the decision here, then implement only the chosen option.

### Decision D2: filters exclude-compile ordering (#3)

- **File:** `internal/filters/filters.go:118-159`
- **Tension:** reviewer + Codex say a bad exclude regex returns `InvalidConfig` even when includes would have surfaced `UpstreamInvalid` first. But `filters.go:85-93` and `:120-127` explicitly document this ordering as intentional per spec line 824 (`InvalidConfig` is the reserved reason for compile failures, and it precedes `UpstreamInvalid`).
- **Action:** read the design spec under `spec/` (search for the §6.3/§6.5 precedence table and "line 824"/"line 822" references). Confirm which ordering the spec mandates.
- **Recommendation:** if the in-code contract matches the spec (likely), this is a **no-op** — close the finding with a one-line note. Only reorder `compileAnchored(f.Exclude, ...)` to after the include-match loop if the spec actually mandates `UpstreamInvalid`-wins.
- [ ] Record spec finding + decision here before any change.

### Decision D3: `isClusterLocalHost` two-label form (#7)

- **File:** `internal/litellm/endpoint.go:127-139`
- **Tension:** `http://litellm.default:4000` (`<service>.<namespace>`) is a valid in-cluster address but is classified as an insecure remote (→ `MasterKeyOverPlaintextHTTP` warn, or `Ready=False` under `REQUIRE_HTTPS_REMOTE`). **But** a blanket "two-label short name == cluster-local" rule would also classify real public domains like `http://example.com` as cluster-local, weakening the M-SEC2 plaintext-master-key guard — `example.com` and `service.namespace` are syntactically indistinguishable.
- **Options:** (a) document that in-cluster endpoints must use the `.svc` / `.svc.cluster.local` / single-label form (no code change; add a `Common failure modes` entry); (b) loosen classification and accept the M-SEC2 weakening (NOT recommended).
- **Recommendation:** (a) document — preserve the conservative security posture.
- [ ] Record the decision here. If (a), add the CLAUDE.md entry; do not touch `endpoint.go` logic.

### Decision D4: `CachedListMCPServers` single-flight herd (#8)

- **File:** `internal/litellm/list_cache.go:104-125`
- **Tension:** when an inflight leader skips its store (epoch advance, #60) and waiters unblock with `out == nil`, each waiter calls `ListMCPServers` directly, bypassing the dedup guard → thundering herd under mutation storms. The code comment (L116-120) documents the fallback intent but not the herd.
- **Options:** (a) accept + expand the comment to acknowledge the herd is bounded by mutation rate (no code change); (b) re-enter the single-flight (start a fresh inflight chan) instead of a direct fetch on the nil-out path.
- **Recommendation:** (a) unless a real production herd is observed — (b) adds re-entrancy complexity to a cold path.
- [ ] Record the decision here.

### Decision D5: ModelDiscovery `GeneratedChildren` excludes skip-classified names (#9)

- **File:** `internal/controller/modeldiscovery_controller.go:618-636, 835-852`
- **Tension:** a child SSA-applied in a prior reconcile but `classifiedSkip` this reconcile drops out of `GeneratedChildren` → next reconcile re-classifies it `action="create"` (OBS-04 metric noise). **But** `generated`/`skipped`/`failed` are a disjoint partition enforced by the invariant `discoveredCount == generated + skipped + failed` (L848). Adding skip-names to `GeneratedChildren` would double-count and break that invariant — the reviewer's fix is wrong.
- **Options:** (a) wontfix + document that `GeneratedChildren` is the per-reconcile owned set and the `create`/`update` action label is best-effort (no code change); (b) decouple the OBS-04 action label from `GeneratedChildren` via a separate "ever-written" set (new state field; larger change).
- **Recommendation:** (a) wontfix + document — the metric noise is the accepted cost of the disjoint partition.
- [ ] Record the decision here.

---

## Final integration step (after all chosen tasks land)

- [ ] **Update the review report's status**

In `references/reviews/2026-06-02-full-code-review.md`, append a short "Remediation status" section recording: Group A fixed (#2/#4/#5/#6), Group B fixed (#10/#11/#12/#13), Group C decisions (D1-D5 outcomes). Commit with `docs(review): record remediation status`.

- [ ] **Run the full pre-push gate as a dry run before opening the PR**

Run: `make verify`
Expected: lint + unit + security + pre-push all green. (The installed pre-push hook will re-run the gate on `git push`; this is the dry run.)

- [ ] **Open the PR**

```bash
git push -u origin fix/review-remediation
gh pr create --base main --title "fix: review remediation (Group A bugs + B docs + C decisions)" --body-file <(echo "Resolves the remaining 🟡/🔵 findings from references/reviews/2026-06-02-full-code-review.md (6 🔴 landed in #77). Group A: #2/#4/#5/#6. Group B: #10/#11/#12/#13. Group C decisions recorded in the plan.")
```

---

## Self-Review

**Spec coverage (the 13 findings):** #1→D1, #2→T1, #3→D2, #4→T2, #5→T4, #6→T3, #7→D3, #8→D4, #9→D5, #10→T5, #11→T7, #12→T8, #13→T6. All 13 mapped.

**Placeholder scan:** no "TBD"/"handle edge cases"/"similar to Task N". Decision tasks (Group C) intentionally carry no code — they require a recorded decision first, which is correct, not a placeholder.

**Type consistency:** `modelAliasErrorReason` (T2) reuses `is4xxError` (T4-pinned) and the existing `reasonModelAliasRejected`/`reasonLiteLLMUnavailable` constants + `litellm.Auth401Error`/`litellm.RejectedError` types. `OAuth2Flow` field name (T1) matches `extractedMCPParams.OAuth2Flow` and the create-arm usage. `is4xxError` signature `(err error) bool` matches its definition in `litellmguardrail_controller.go:713`. The `dotted`→`childNames` rename (T5) is scoped to one function.

**Dependency note:** T4 pins `is4xxError`'s contract with a unit test; T2 depends on `is4xxError` existing (it already does). Execute T4's test-add (Step 2) before relying on the helper in T2 if running out of order — but both work against the existing helper, so either order is safe.
