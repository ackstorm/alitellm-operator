# FIX2.txt Remediation Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Land remediations for all 11 findings in `FIX2.txt` (2026-05-22, v0.1.2 post-deploy smoke-test): three HIGH (sanitizer default-direction footgun + hyphen-name regression + stale-status-after-restart + cold-start race), five MEDIUM (toolhive dedup log volume, opaque LiteLLM error messages, audit identity, startup thundering herd), three LOW (rejection observability metric, FIX.txt status traceability, GitOps default-stripping, toolhive v1beta1 startup-log clarity).

**Architecture:**
- **HIGH-9 + HIGH-1 share one root cause** (sanitizer mangles safe inputs against wrong-direction default) — land together as one atomic commit per the locked build order (HIGH-9 first, HIGH-1 combined). Three coupled changes: (a) sanitizer no-op-on-safe-input, (b) flip Connection CRD default `"-"` → `"."`, (c) orphan-adoption fallback in `resolveServerIDByName` that tries the pre-sanitize K8s name when the sanitized name has no record.
- **HIGH-2** ships as two coupled sub-fixes: (a) deterministic 4xx errors return `Result{RequeueAfter: requeueOnRejectedAfter}` (default 5m, min 1m, max 1h — locked); (b) on operator boot, a one-shot `manager.Runnable` enqueues every CR whose `status.observedGeneration` matches but `Ready=False`, ensuring the "upgrade fixes things" scenario recovers without operator intervention.
- **MEDIUM-3 (Option B locked)** is a SetupWithManager pattern change in five+ controllers: extend the Connection-watch predicate to fire on `CreateEvent` + `UpdateEvent(Ready edge)` + `GenericEvent(cache snapshot publish)`, and each fire enqueues ALL child CRs in the Connection's namespace. NO separate per-controller boot-reconcile sweep — HIGH-2's `BootSweeper` already covers cold-start re-reconcile.
- **MEDIUM-4** investigation FIRST: the throttle already exists at `internal/toolhive/informer.go:441-453` with a working `sync.Once`-initialized `dedupLogThrottle`. Per FIX2.txt MEDIUM-4, 22 lines × 3 minutes = 66 emits IS the expected behavior of a 60s window across 22 unique `(kind, ns, name)` tuples — the prior plan's contract was per-tuple-per-window, not global. Resolution per FIX2.txt option: **drop to V(2) by default**, add a single startup line listing the deprecated v1alpha1 tuples, and add a coalesced `dedup-summary` periodic log (one line, all names).
- **MEDIUM-5** widens the LiteLLM HTTP error type to carry the parsed `processLitellmError` message; reconcilers surface it in `condition.Message` (already truncated to 512 bytes by `processLitellmError`).
- **MEDIUM-8** introduces `internal/identity` populated via ldflags (`-X .../identity.Version=$(VERSION)`), wired into `ModelInfo.CreatedBy/UpdatedBy` on Model create + update, and propagated to Team/MCPServer/A2AAgent request builders where LiteLLM accepts the field.
- **MEDIUM-10** adds a shared `golang.org/x/time/rate.Limiter` on `internal/litellm.Client` (default 5 rps, burst 10), configurable via `LiteLLMConnection.spec.maxRequestsPerSecond`.
- **LOW-6** Prometheus metric `litellm_operator_reconcile_total{kind, namespace, result}` exposed via the existing operator-runtime metrics endpoint.
- **LOW-11** verify first: `internal/toolhive/informer.go:210-214` says BOTH v1alpha1 + v1beta1 are registered; the prod startup log only listing v1alpha1 is a logging artifact, not a functional miss. Fix is to emit a single coalesced INFO line at startup that lists all four registered GVKs honestly.
- **LOW-12** adds a runtime default-applier on Connection-snapshot load that logs once per Connection when the field is empty and the resolved default is applied.
- **LOW-7** is doc-only: STATUS blocks on FIX.txt + FIX2.txt entries (no superseding deletes).

**Tech Stack:** Go 1.24.13, controller-runtime v0.19.4, k8s.io v0.31.0, Ginkgo v2 (e2e), envtest (controller), `golang.org/x/time/rate`. All commands executed via `./scripts/dev.sh` (devtools container). Pre-push gates non-negotiable; `make pre-push` runs before each push to `main`.

**Working directory:** Current branch (`main`). Each task is one atomic commit per project release convention. No bundled commits across findings except where two findings share a root cause (Task 1: HIGH-1 + HIGH-9).

**File anchors (verified 2026-05-22):**
- Sanitizer + default: `internal/litellm/sanitize.go:34`, `api/litellm/v1alpha1/litellmconnection_types.go:69`
- `resolveServerIDByName`: `internal/controller/mcpserver_controller.go:162` (call site), `:456` (filter loop)
- Deterministic 4xx return site (Team): `internal/controller/team_controller.go:1118`; A2AAgent: `:626`; Model + MCPServer: search for `"LiteLLMRejected"` in respective controllers
- Connection watch site: `internal/controller/predicates.go:connectionReadyTransition` (from prior FIX.txt plan, Task 11)
- Dedup log site: `internal/toolhive/informer.go:441-460`
- ModelInfo audit fields: `internal/litellm/types.go:14-30`; controller pass-through sites: `internal/controller/model_controller.go:466, 507, 530`
- LiteLLM error wrap: `internal/litellm/client.go:135-159`; envelope parser: `internal/litellm/errors.go:89`
- Connection cache (snapshot loader): `grep -rn "type Cache\|Snapshot\b" internal/connection/` (resolve at task time)

---

## Task 0: Diagnostic envtest first — confirm MEDIUM-4 and LOW-11 root causes

Justification: FIX2.txt MEDIUM-4 and LOW-11 are reported from log inspection, not code review. Both `internal/toolhive/informer.go` and the prior FIX.txt plan (Task 7) already claim a throttle exists and that both API versions are registered. Before writing code, write tests that pin the current contract and surface the actual gap.

**Files:**
- Read-only: `internal/toolhive/informer.go`, `internal/toolhive/informer_test.go`, `internal/toolhive/throttle_test.go`
- Modify: `internal/toolhive/informer_test.go` (add diagnostic tests)

**Step 1: Write the diagnostic tests**

```go
// internal/toolhive/informer_test.go (append)

// TestInformer_FIX2_M4_ThrottleBehaviorIsPerTuplePerWindow asserts the
// documented contract: at most one INFO line per (kind, ns, name) tuple
// per dedupLogWindow. Sub-window collisions for the same tuple emit at
// V(2). N distinct tuples may each emit once per window — that yields
// N lines per window, which IS the expected behavior. This test pins
// the contract; FIX2.txt MEDIUM-4 observation of "22 lines/min" is
// resolved as WAI from this test and the next task drops the default
// verbosity instead of changing the throttle math.
func TestInformer_FIX2_M4_ThrottleBehaviorIsPerTuplePerWindow(t *testing.T) {
    t.Parallel()
    throttle := newDedupLogThrottle()
    window := 60 * time.Second

    keys := []dedupKey{
        {Kind: "MCPServer", Namespace: "mcp", Name: "context7"},
        {Kind: "MCPServer", Namespace: "mcp", Name: "exa"},
        {Kind: "MCPServer", Namespace: "mcp", Name: "google-calendar"},
    }
    // Each key first-call emits once.
    for _, k := range keys {
        if !throttle.shouldLog(k, window) {
            t.Fatalf("first call should emit: %v", k)
        }
    }
    // Second call per key is throttled.
    for _, k := range keys {
        if throttle.shouldLog(k, window) {
            t.Fatalf("second call should be throttled: %v", k)
        }
    }
    // Different keys are NOT throttled by each other.
    fresh := dedupKey{Kind: "MCPServer", Namespace: "other", Name: "x"}
    if !throttle.shouldLog(fresh, window) {
        t.Fatalf("fresh key must emit even when other tuples are within window")
    }
}

// TestInformer_FIX2_L11_BothVersionsRegistered probes the registered
// listGVKs set returned by Informer (extracted via a small accessor)
// to assert both v1alpha1 AND v1beta1 are in the registered set for
// each of MCPServer and VirtualMCPServer.
func TestInformer_FIX2_L11_BothVersionsRegistered(t *testing.T) {
    // Implementation depends on whether tryRegister exposes its
    // registered set. If not, add a read-only accessor:
    //   func (i *Informer) RegisteredGVKs() []schema.GroupVersionKind
    // (returns a copy under i.mu.RLock()).
    //
    // Then assert len == 4 and both versions appear for each kind.
    t.Skip("Implement after adding RegisteredGVKs() accessor — Step 3 of this task")
}
```

**Step 2: Run the tests**

```
./scripts/dev.sh make unit-pkg PKG=./internal/toolhive/
```

Expected:
- `TestInformer_FIX2_M4_ThrottleBehaviorIsPerTuplePerWindow` PASS (throttle math is correct).
- `TestInformer_FIX2_L11_BothVersionsRegistered` SKIP (needs accessor).

**Step 3: Add `RegisteredGVKs()` accessor + un-skip the L-11 test**

In `internal/toolhive/informer.go`, after `tryRegister` records each successful registration, also record the GVK in a new field `i.registered []schema.GroupVersionKind` (guarded by `i.mu`). Expose:

```go
// RegisteredGVKs returns a snapshot of GVKs the Informer has successfully
// registered dynamic informers for. Read-only; safe for concurrent use.
func (i *Informer) RegisteredGVKs() []schema.GroupVersionKind {
    i.mu.RLock()
    defer i.mu.RUnlock()
    out := make([]schema.GroupVersionKind, len(i.registered))
    copy(out, i.registered)
    return out
}
```

Un-skip the test, set up a fake RESTMapper that serves both versions, instantiate the Informer, run `tryRegister`, assert `len(RegisteredGVKs()) == 4` and both versions appear for each kind.

**Step 4: Re-run**

```
./scripts/dev.sh make unit-pkg PKG=./internal/toolhive/
```

Two outcomes for the L-11 test:
- **PASS** → registration is correct; LOW-11 is a logging artifact (Task 4 fixes the log).
- **FAIL** → real bug. File a separate child task before proceeding; revise the plan; do not silently broaden Task 4.

**Step 5: Commit**

```
git add internal/toolhive/informer.go internal/toolhive/informer_test.go
git commit -m "test(toolhive): pin dedup throttle + GVK registration contracts (FIX2.txt M-4, L-11)

Diagnostic tests for FIX2.txt MEDIUM-4 and LOW-11. The throttle math is
per-(kind, ns, name) per 60s window — 22 tuples emit 22 lines/min as
designed. LOW-11 prod symptom is a startup-log artifact, not a
registration miss (v1alpha1 + v1beta1 both register today)."
```

---

## Task 1: Sanitizer no-op-on-safe + flip default + orphan adoption (HIGH-9 + HIGH-1, combined PR per locked build order)

Justification: HIGH-9 (regression — sanitizer mangles already-safe names like `test-exa-mcp`) and HIGH-1 (default direction wrong) share one root cause: the sanitizer unconditionally rewrites when separator is set, against a default that encoded an assumption inconsistent with LiteLLM v1.85.1's empirically-observed behavior. Per locked decisions, HIGH-9 leads (restore prod) and HIGH-1 combines into the same PR. Three coupled changes: (a) sanitizer no-op-on-safe-input, (b) flip Connection CRD default `"-"` → `"."`, (c) orphan adoption fallback in `resolveServerIDByName`.

**Files:**
- Modify: `internal/litellm/sanitize.go`
- Modify: `internal/litellm/sanitize_test.go`
- Modify: `api/litellm/v1alpha1/litellmconnection_types.go:69` (kubebuilder default `"-"` → `"."`)
- Modify: `internal/controller/mcpserver_controller.go:162` (orphan-adoption fallback in finalizer name-resolve)
- Modify: `internal/controller/mcpserver_controller.go:456` (filter loop in `resolveServerIDByName`)
- Modify: `config/crd/bases/litellm.ackstorm.ai_litellmconnections.yaml` (regenerated via `make manifests`)
- Modify: `deploy/helm/alitellm-operator/templates/crds.yaml` or equivalent rendered CRD (regenerated via the helm chart sync target)
- Add: `CHANGELOG.md` entry under unreleased flagging the default change

**Step 1: Write the failing sanitizer test**

Append to `internal/litellm/sanitize_test.go`:

```go
// TestSanitizeMCPServerName_FIX2_NoOpOnSafeInput asserts that an input
// without the forbidden character is returned UNCHANGED, preserving
// pre-v0.1.2 server_name values across upgrade boundaries. Regression
// for FIX2.txt HIGH-9.
func TestSanitizeMCPServerName_FIX2_NoOpOnSafeInput(t *testing.T) {
    t.Parallel()
    tests := []struct {
        name      string
        in        string
        separator string
        want      string
    }{
        // HIGH-9 regression: hyphen-name with sep=- has no forbidden char.
        {"hyphen-name with sep=-", "test-exa-mcp", "-", "test-exa-mcp"},
        // sep=. and no dots — no rewrite.
        {"hyphen-name with sep=.", "test-exa-mcp", ".", "test-exa-mcp"},
        // sep=. and dots present — DO rewrite.
        {"dotted with sep=.", "a.b.c", ".", "a-b-c"},
        // sep=- and hyphens present — DO rewrite (existing behavior).
        {"hyphens with sep=-", "a-b-c", "-", "a.b.c"},
        // Edge: empty separator falls back to default ("-") AND input is safe.
        {"hyphen-name with sep=empty", "test-exa-mcp", "", "test-exa-mcp"},
        // Edge: empty separator + dots → no rewrite (forbidden char is "-", not in input).
        {"dotted with sep=empty", "a.b.c", "", "a.b.c"},
    }
    for _, tc := range tests {
        tc := tc
        t.Run(tc.name, func(t *testing.T) {
            t.Parallel()
            if got := SanitizeMCPServerName(tc.in, tc.separator); got != tc.want {
                t.Fatalf("SanitizeMCPServerName(%q, %q) = %q, want %q", tc.in, tc.separator, got, tc.want)
            }
        })
    }
}
```

**Step 2: Run to verify failure**

```
./scripts/dev.sh go test ./internal/litellm/ -run TestSanitizeMCPServerName_FIX2_NoOpOnSafeInput -v
```

Expected: FAIL — current code mangles `test-exa-mcp` (sep=`-`) to `test.exa.mcp`.

**Step 3: Apply the sanitizer fix**

Replace `internal/litellm/sanitize.go` body (keep license header + package + import):

```go
// MCPToolPrefixSeparatorDefault is the operator-side default for the
// LiteLLMConnection.spec.mcpToolPrefixSeparator field when unset. It is
// "." (a dot) — the empirically-confirmed safe direction against LiteLLM
// v1.85.1's stock configuration, which forbids '.' inside server_name
// regardless of the env var's value (FIX2.txt HIGH-1, 2026-05-22).
//
// Prior to v0.1.3 the default was "-"; users who relied on that and run
// a non-stock LiteLLM that forbids '-' must set spec.mcpToolPrefixSeparator
// explicitly to "-".
const MCPToolPrefixSeparatorDefault = "."

// SanitizeMCPServerName returns name with the LiteLLM-side forbidden
// character (the configured separator) replaced by the opposite valid
// character, BUT ONLY IF the input actually contains the forbidden char.
// Inputs without the forbidden char are returned unchanged so existing
// records stay stable across upgrade boundaries (FIX2.txt HIGH-9).
//
// Behavior:
//   - separator "." → if name contains ".", replace each with "-"; else unchanged.
//   - separator "-" → if name contains "-", replace each with "."; else unchanged.
//   - separator ""  → treated as MCPToolPrefixSeparatorDefault.
//   - any other value → defensive passthrough (CEL on the spec field already
//     enforces {".", "-"} so this branch is never hit in production).
//
// The K8s-side metadata.name is left untouched — sanitization is wire-
// boundary only.
func SanitizeMCPServerName(name, separator string) string {
    forbidden := separator
    if forbidden == "" {
        forbidden = MCPToolPrefixSeparatorDefault
    }
    if !strings.Contains(name, forbidden) {
        return name
    }
    switch forbidden {
    case ".":
        return strings.ReplaceAll(name, ".", "-")
    case "-":
        return strings.ReplaceAll(name, "-", ".")
    default:
        return name
    }
}
```

**Step 4: Update the kubebuilder default**

In `api/litellm/v1alpha1/litellmconnection_types.go:69`:

```go
// FROM:
// +kubebuilder:default:="-"
// TO:
// +kubebuilder:default:="."
```

Update the surrounding comment block (lines 45-71) to flip the description of which value is default and which is the legacy override. Mention FIX2.txt HIGH-1 alongside the existing FIX.txt HIGH-1 reference.

**Step 5: Regenerate CRDs + helm chart manifests**

```
./scripts/dev.sh make generate manifests
make helm-sync   # or whatever target renders the Helm CRDs from config/crd/bases/
```

If `make helm-sync` doesn't exist, look for the existing chart-sync target (recent commit `9e0b669` mentions "sync CRDs"). Confirm the rendered `deploy/helm/alitellm-operator/templates/crds.yaml` (or equivalent) carries the new default.

**Step 6: Write the orphan-adoption test (envtest)**

Append to `internal/controller/mcpserver_controller_test.go` (or a new `mcpserver_fix2_h9_test.go`):

```go
// TestMCPServer_FIX2_H9_OrphanAdoption asserts that an MCPServer whose
// LiteLLM record was created under the PRE-SANITIZE name is adopted by
// the reconciler when sanitization changes (or its no-op-on-safe behavior
// reveals it). Regression for FIX2.txt HIGH-9.
func TestMCPServer_FIX2_H9_OrphanAdoption(t *testing.T) {
    ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
    defer cancel()

    ns := makeTestNamespace(t, "fix2-h9")
    connCR := newReadyConnection(t, ns, "litellm")
    defer func() { _ = k8sClient.Delete(context.Background(), connCR, &client.DeleteOptions{}) }()

    // Pre-plant a LiteLLM-side record under the K8s metadata.name as it would
    // exist for an MCPServer created under v0.1.1 (no sanitization).
    const k8sName = "test-exa-mcp"
    const litellmServerID = "72452234-c5ec-4a9e-8e25-da41f02d422b"
    plantFakeLiteLLMMCPServer(t, k8sName, litellmServerID, "http://exa.invalid")

    mcp := &litellmv1alpha1.LiteLLMMCPServer{
        ObjectMeta: metav1.ObjectMeta{Name: k8sName, Namespace: ns},
        Spec: litellmv1alpha1.MCPServerSpec{
            ConnectionRef: corev1.LocalObjectReference{Name: connCR.Name},
            Endpoint:      "http://exa.invalid",
            Transport:     "http",
        },
    }
    if err := k8sClient.Create(ctx, mcp); err != nil {
        t.Fatalf("create MCPServer: %v", err)
    }
    t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), mcp, &client.DeleteOptions{}) })

    // The reconciler MUST adopt the pre-existing record and reach Ready=True
    // WITHOUT a duplicate POST (which would 4xx on duplicate name).
    waitForReady(t, mcp, 30*time.Second)

    refreshed := &litellmv1alpha1.LiteLLMMCPServer{}
    if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(mcp), refreshed); err != nil {
        t.Fatalf("get refreshed MCPServer: %v", err)
    }
    if got := refreshed.Status.LastRendered.ServerID; got != litellmServerID {
        t.Fatalf("ServerID not adopted: got %q, want %q", got, litellmServerID)
    }
}
```

If helper `plantFakeLiteLLMMCPServer` doesn't exist, add a minimal one to `internal/litellm/mock/` (read existing helpers first; mirror the pattern, do NOT invent a new mock convention).

**Step 7: Apply the orphan-adoption fallback**

In `internal/controller/mcpserver_controller.go`, modify `resolveServerIDByName` so that when the LiteLLM-side server-name lookup misses the SANITIZED name, it retries once with the original K8s `metadata.name` (pre-sanitize). Add comment:

```go
// FIX2.txt HIGH-9 (2026-05-22): adopt a LiteLLM record that was created
// under the pre-sanitize name. This is a one-shot fallback used after
// the sanitizer's no-op-on-safe-input change made the wire payload differ
// from the previous version's behavior. Idempotent: when no orphan
// exists, the second probe returns ErrNotFound and the caller falls
// through to the CREATE arm as before.
```

Concretely: read the existing `resolveServerIDByName` body (around L450-470); after the first probe returns nothing, if `sanitized != mcp.Name`, probe again with `mcp.Name` (raw). Return the orphan's `server_id` if found.

**Step 8: Run all sanitizer + MCPServer envtests**

```
./scripts/dev.sh make unit-pkg PKG=./internal/litellm/
./scripts/dev.sh make envtest-pkg PKG=./internal/controller/... FOCUS=TestMCPServer
```

Expected: PASS for everything, including the new HIGH-9 test, the existing FIX.txt HIGH-1 dotted-name test (now still passes — input contains `.` so the sanitizer DOES rewrite), and the new HIGH-9 no-op test.

**Step 9: Add CHANGELOG entry**

In `CHANGELOG.md`, under an unreleased section, add:

```markdown
## Unreleased

### Changed (breaking-ish — operator default behavior)
- `LiteLLMConnection.spec.mcpToolPrefixSeparator` default flipped from
  `"-"` to `"."` to match LiteLLM v1.85.1's stock validator behavior
  (FIX2.txt HIGH-1). Users running a non-stock LiteLLM that forbids `"-"`
  inside `server_name` must set the field explicitly to `"-"` on their
  Connection CR. Existing CRs whose YAML omits the field will pick up
  the new default at the next reconcile.

### Fixed
- MCPServer sanitizer no longer rewrites already-safe inputs, fixing a
  regression where `test-exa-mcp` (hyphen, no dots) was mangled into
  `test.exa.mcp` on the v0.1.2 upgrade path (FIX2.txt HIGH-9).
- MCPServer reconciler adopts a pre-existing LiteLLM record created
  under the K8s `metadata.name` when the sanitized name has no record
  but the raw name does. Heals upgrade-orphans without manual `kubectl`
  intervention.
```

**Step 10: Commit (one atomic commit — these are coupled)**

```
git add internal/litellm/sanitize.go internal/litellm/sanitize_test.go \
        api/litellm/v1alpha1/litellmconnection_types.go \
        config/crd/bases/ deploy/helm/ \
        internal/controller/mcpserver_controller.go \
        internal/controller/mcpserver_fix2_h9_test.go \
        CHANGELOG.md
git commit -m "fix(mcp): sanitizer no-op on safe input + adopt orphan + flip default to '.' (FIX2.txt H-9, H-1)

Restore v0.1.1 prod posture (HIGH-9): the v0.1.2 sanitizer mangled
already-safe names like 'test-exa-mcp' into 'test.exa.mcp', orphaning
every pre-v0.1.2 manual MCPServer on the upgrade boundary. Make the
sanitizer a no-op when the input lacks the forbidden character, and add
an orphan-adoption fallback to resolveServerIDByName: when the sanitized
name has no LiteLLM record, retry once with the raw K8s metadata.name.

Flip the operator-side default for spec.mcpToolPrefixSeparator from '-'
to '.' (HIGH-1, locked decision). LiteLLM v1.85.1 stock config rejects
'.' in server_name regardless of MCP_TOOL_PREFIX_SEPARATOR; the new
default makes OOTB deploys work. Users running a non-stock LiteLLM that
forbids '-' must set the field explicitly to '-' on their Connection CR."
```

---

## Task 2: Periodic requeue on deterministic 4xx + boot reconcile sweep (HIGH-2)

Justification: deterministic upstream errors (LiteLLMRejected, SecretNotFound) currently return `ctrl.Result{}` — no requeue. After the operator restarts on a new image that fixes the upstream-rejection root cause, the affected CRs stay in Ready=False until manually poked. Two coupled changes: (a) return `RequeueAfter` for deterministic errors (with a Connection-spec knob); (b) a one-shot boot-time reconcile sweep so the upgrade-fixes-things scenario heals without external action.

**Files:**
- Modify: `api/litellm/v1alpha1/litellmconnection_types.go` (add `RequeueOnRejectedAfter`)
- Modify: status-write call sites in all 5 reconcilers that emit `"LiteLLMRejected"`/`"SecretNotFound"`:
  - `internal/controller/team_controller.go` (multiple sites — `:626`, `:1118`, `:932`, `:945`, `:987`)
  - `internal/controller/a2aagent_controller.go:626`
  - `internal/controller/model_controller.go` (search `"LiteLLMRejected"`)
  - `internal/controller/mcpserver_controller.go` (search `"LiteLLMRejected"`)
  - `internal/controller/modeldiscovery_controller.go`, `mcpserverdiscovery_controller.go` (if they emit these reasons)
- Add: `cmd/main.go` — wire a new `BootSweeper` Runnable into `mgr.Add(...)`
- Create: `internal/controller/bootsweep.go` (the Runnable)
- Tests: add `internal/controller/bootsweep_test.go`, append `_fix2_h2_test.go` per affected controller

**Step 1: Write the failing requeue test**

```go
// internal/controller/mcpserver_fix2_h2_test.go
// SPDX-License-Identifier: Apache-2.0
package controller

// TestMCPServer_FIX2_H2_RequeueOnDeterministicReject asserts that a CR
// stuck on LiteLLMRejected recovers WITHOUT external mutation when the
// fake upstream flips from 400 → 201 after the configured
// requeueOnRejectedAfter interval has elapsed.
func TestMCPServer_FIX2_H2_RequeueOnDeterministicReject(t *testing.T) {
    ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
    defer cancel()

    ns := makeTestNamespace(t, "fix2-h2")
    connCR := newReadyConnection(t, ns, "litellm")
    // Set Connection.spec.requeueOnRejectedAfter to 5s (test-friendly).
    setConnRequeueOnRejected(t, connCR, 5*time.Second)
    defer func() { _ = k8sClient.Delete(context.Background(), connCR, &client.DeleteOptions{}) }()

    // Configure fake LiteLLM to reject the first POST then accept.
    fakeMCP.RejectNextN(1)
    defer fakeMCP.Reset()

    mcp := &litellmv1alpha1.LiteLLMMCPServer{
        ObjectMeta: metav1.ObjectMeta{Name: "flaps-then-ok", Namespace: ns},
        Spec: litellmv1alpha1.MCPServerSpec{
            ConnectionRef: corev1.LocalObjectReference{Name: connCR.Name},
            Endpoint:      "http://example.invalid",
            Transport:     "http",
        },
    }
    if err := k8sClient.Create(ctx, mcp); err != nil {
        t.Fatalf("create MCPServer: %v", err)
    }
    // Initial reconcile lands LiteLLMRejected.
    waitForCondition(t, mcp, "Ready", metav1.ConditionFalse, 10*time.Second)
    // Within ~5s + slack, the requeue fires and the next reconcile succeeds.
    waitForCondition(t, mcp, "Ready", metav1.ConditionTrue, 20*time.Second)
}
```

**Step 2: Add `RequeueOnRejectedAfter` to the Connection spec**

In `api/litellm/v1alpha1/litellmconnection_types.go`, add a new field to `LiteLLMConnectionSpec`:

```go
// RequeueOnRejectedAfter controls how often the reconciler retries CRs
// that hit a deterministic upstream error (LiteLLMRejected, SecretNotFound).
// Default 5m. Range [1m, 1h] enforced via CEL XValidation below (Go
// time.Duration has no built-in kubebuilder Min/Max marker).
// FIX2.txt HIGH-2 (2026-05-22, decision locked).
//
// +optional
// +kubebuilder:default:="5m"
// +kubebuilder:validation:Pattern=`^(?:[0-9]+(?:\.[0-9]+)?(?:ns|us|µs|ms|s|m|h))+$`
// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('1m') && duration(self) <= duration('1h')",message="requeueOnRejectedAfter must be between 1m and 1h"
RequeueOnRejectedAfter metav1.Duration `json:"requeueOnRejectedAfter,omitempty"`
```

**Step 3: Apply the requeue at every deterministic-error return site**

For each `writeStatus(ctx, &cr, metav1.ConditionFalse, "LiteLLMRejected", ...)` call site, find the surrounding `return ctrl.Result{}, nil` and change it to:

```go
return ctrl.Result{RequeueAfter: connSnap.Spec.RequeueOnRejectedAfter.Duration}, nil
```

Where `connSnap` is the cached Connection snapshot (each reconciler already holds one; resolve the exact field name per controller — for Team it's likely `snap`).

Mirror for `SecretNotFound`.

**Step 4: Run focused envtest**

```
./scripts/dev.sh make envtest-pkg PKG=./internal/controller/... FOCUS=TestMCPServer_FIX2_H2_RequeueOnDeterministicReject TIMEOUT=5m
```

Expected: PASS.

**Step 5: Write the boot-sweep failing test**

```go
// internal/controller/bootsweep_test.go
// SPDX-License-Identifier: Apache-2.0
package controller

// TestBootSweeper_FIX2_H2_EnqueuesStuckReadyFalse asserts that on
// operator boot, every CR whose status.observedGeneration matches
// metadata.generation but Ready=False is enqueued for one immediate
// reconcile pass.
func TestBootSweeper_FIX2_H2_EnqueuesStuckReadyFalse(t *testing.T) {
    // Pre-seed apiserver with 3 CRs across 2 kinds, each at Ready=False
    // with observedGeneration == metadata.generation. Spawn BootSweeper.
    // Assert each CR is enqueued exactly once via a test-only
    // recording reconciler queue.
    ...
}
```

**Step 6: Implement BootSweeper**

Create `internal/controller/bootsweep.go`:

```go
// SPDX-License-Identifier: Apache-2.0
package controller

import (
    "context"
    "time"

    "k8s.io/apimachinery/pkg/api/meta"
    "sigs.k8s.io/controller-runtime/pkg/client"
    "sigs.k8s.io/controller-runtime/pkg/event"

    litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
)

// BootSweeper is a one-shot manager.Runnable that enqueues every CR
// whose status.observedGeneration matches but Ready != True. This heals
// the FIX2.txt HIGH-2 "upgrade fixes things but status sticky" scenario
// without needing operator action.
type BootSweeper struct {
    Client client.Client
    // Enqueuer is a callback that re-enqueues a single object onto its
    // owning reconciler. Injected by main.go which holds the per-kind
    // controller refs.
    Enqueuer func(obj client.Object)
}

func (b *BootSweeper) Start(ctx context.Context) error {
    // Give the cache a beat to hydrate.
    select {
    case <-ctx.Done():
        return nil
    case <-time.After(2 * time.Second):
    }
    kinds := []client.ObjectList{
        &litellmv1alpha1.LiteLLMTeamList{},
        &litellmv1alpha1.LiteLLMModelList{},
        &litellmv1alpha1.LiteLLMA2AAgentList{},
        &litellmv1alpha1.LiteLLMMCPServerList{},
        &litellmv1alpha1.LiteLLMModelDiscoveryList{},
        &litellmv1alpha1.LiteLLMMCPServerDiscoveryList{},
    }
    for _, listObj := range kinds {
        if err := b.Client.List(ctx, listObj); err != nil {
            continue
        }
        if err := meta.EachListItem(listObj, func(o runtime.Object) error {
            obj, ok := o.(client.Object)
            if !ok {
                return nil
            }
            if !isStuckReadyFalse(obj) {
                return nil
            }
            b.Enqueuer(obj)
            return nil
        }); err != nil {
            // Best-effort sweep; do not crash the manager.
            continue
        }
    }
    return nil
}

// isStuckReadyFalse returns true when the object's observedGeneration
// matches metadata.generation AND the Ready condition is False/Unknown.
func isStuckReadyFalse(obj client.Object) bool {
    // Use reflection or interface{ GetStatusConditions() }; resolve
    // implementation at task time — the project may already have a
    // common interface for this (search the controller package).
    ...
}
```

Wire it into `cmd/main.go`:

```go
sweeper := &controller.BootSweeper{
    Client:   mgr.GetClient(),
    Enqueuer: makeEnqueuer(teamCtrl, modelCtrl, ...),
}
if err := mgr.Add(sweeper); err != nil { ... }
```

`makeEnqueuer` is a small dispatcher that returns the right `EventHandler.Generic(...)` channel per kind. Read the existing controller setup in `cmd/main.go` first to find the established pattern.

**Step 7: Run the boot-sweep test**

```
./scripts/dev.sh make envtest-pkg PKG=./internal/controller/... FOCUS=TestBootSweeper_FIX2_H2 TIMEOUT=5m
```

Expected: PASS.

**Step 8: Commit (single commit — both sub-fixes ship together)**

```
git add api/litellm/v1alpha1/litellmconnection_types.go \
        config/crd/bases/ deploy/helm/ \
        internal/controller/*.go cmd/main.go
git commit -m "fix(controllers): periodic requeue on deterministic 4xx + boot sweep (FIX2.txt H-2)

Deterministic upstream errors (LiteLLMRejected, SecretNotFound)
previously returned ctrl.Result{} (no requeue). After the controller's
rate-limiter dropped the item from its queue, only an external mutation
or operator restart could trigger another retry — which itself dedupes
to the same condition and never advances lastTransitionTime.

Add Connection.spec.requeueOnRejectedAfter (default 5m); deterministic
errors now return ctrl.Result{RequeueAfter: <that value>}.

Add a one-shot BootSweeper manager.Runnable that, after a 2s cache-
hydration delay, enqueues every CR whose observedGeneration matches
generation but Ready != True. Heals the 'upgrade fixes things but
status sticky' scenario observed on the v0.1.2 EKS deploy."
```

---

## Task 3: Connection-watch Create + Update(Ready edge) + GenericEvent fan-in (MEDIUM-3 Option B)

Justification: Option B locked per FIX2.txt decisions. The existing `connectionReadyTransition` predicate fires only on False→True UPDATE events; extend it to also fire on CreateEvent (cold cache hydration) and GenericEvent (cache snapshot publish). Each fire enqueues ALL child CRs in the Connection's namespace. The per-controller boot-reconcile sweep is intentionally NOT added here — HIGH-2's `BootSweeper` from Task 2 already handles boot-time recovery; MEDIUM-3 is purely about event-driven fan-in.

**Files:**
- Modify: `internal/controller/predicates.go` (extend `connectionReadyTransition`)
- Modify: 5+ controllers' `SetupWithManager` (predicate consumers already wired via prior FIX.txt plan; verify the mapping function lists all children in the namespace — NOT just children referencing the specific Connection by name, since v1alpha1 enforces the Connection singleton at the namespace level)
- Modify: connection cache (search `internal/connection/`) — expose a `Subscribe() <-chan event.GenericEvent` accessor that emits one GenericEvent per cache snapshot publish

**Step 1: Write the failing cold-start envtest**

```go
// internal/controller/team_fix2_m3_test.go
// SPDX-License-Identifier: Apache-2.0
package controller

// TestTeam_FIX2_M3_ColdStartConnectionReady asserts a pre-seeded Ready
// Connection + an unreconciled Team reaches Ready=True without
// intervention within 30s of manager start. Regression for FIX2.txt
// MEDIUM-3.
func TestTeam_FIX2_M3_ColdStartConnectionReady(t *testing.T) {
    ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
    defer cancel()

    ns := makeTestNamespace(t, "fix2-m3")
    // Pre-seed BEFORE starting any reconciler: Connection Ready=True
    // already present in the apiserver when the manager starts.
    connCR := newReadyConnection(t, ns, "litellm")
    team := newSampleTeam(t, ns, connCR.Name)
    if err := k8sClient.Create(ctx, team); err != nil {
        t.Fatalf("create Team: %v", err)
    }
    // Start manager AFTER both CRs exist.
    startNewManager(t, ctx)
    waitForReady(t, team, 30*time.Second)
}
```

**Step 2: Relax `connectionReadyTransition`**

In `internal/controller/predicates.go`, modify the predicate to fire on:
- `CreateEvent` when the new Connection has Ready=True (already in prior FIX.txt code — verify).
- `UpdateEvent` on False→True (existing).
- `GenericEvent` (new) — wired by the connection cache when it snapshot-flips out of `Connecting` post-boot.

If `connectionReadyTransition` from the prior plan already handles `CreateEvent`, this step is a no-op (verify by reading `internal/controller/predicates.go`).

**Step 3: Wire the connection cache to emit a GenericEvent on snapshot transition**

In the connection cache (search `internal/connection/`), find the place that publishes a new snapshot. When the snapshot transitions from `Connecting` (or empty) to `Ready=True`, emit a GenericEvent on a channel watched by all 5 child controllers via `source.Channel`.

If no GenericEvent channel exists yet, add a `cache.Subscribe() <-chan event.GenericEvent` accessor and wire it in each child controller's `SetupWithManager`:

```go
.WatchesRawSource(&source.Channel{Source: connCache.Subscribe()},
    handler.EnqueueRequestsFromMapFunc(r.connectionToTeams),
    builder.WithPredicates(/* no predicate — channel events are pre-filtered */))
```

**Step 4: Run the test**

```
./scripts/dev.sh make envtest-pkg PKG=./internal/controller/... FOCUS=TestTeam_FIX2_M3_ColdStartConnectionReady TIMEOUT=5m
./scripts/dev.sh make envtest-run   # full regression sweep
```

Expected: PASS.

**Step 5: Commit**

```
git add internal/controller/*.go internal/connection/*.go
git commit -m "fix(controllers): fan-in child reconciles on cold-start + cache snapshot flip (FIX2.txt M-3)

The existing connection-ready watch fires only on False→True UPDATE
events. On cold-start the Connection arrives at Ready=True in the very
first cache snapshot — no transition event — so child reconcilers
that ran during the brief Connecting cache window stay stuck at
Ready=False, reason=LiteLLMUnavailable.

Have the connection cache emit a GenericEvent on each snapshot
transition out of Connecting. Child controllers (mcpserver, model,
a2aagent, team, mcpserverdiscovery, modeldiscovery) subscribe via
WatchesRawSource + the existing connectionToXxx mapping function."
```

---

## Task 4: Demote toolhive dedup log + add startup once-shot summary (MEDIUM-4 + LOW-11 log clarity)

Justification: Task 0's diagnostic confirms the throttle math is correct (per-tuple-per-window). The prod symptom of "22 lines/min" is the 60s-window cap working as designed against 22 unique tuples. Per FIX2.txt MEDIUM-4 option: drop the per-tuple INFO to V(2), emit a single coalesced summary line on first detection and at most once per 5m thereafter, and add a single startup line listing all four registered GVKs (closes LOW-11 startup-log clarity).

**Files:**
- Modify: `internal/toolhive/informer.go:441-460` (dedup log emit site)
- Modify: `internal/toolhive/informer.go:tryRegister` (startup summary)
- Modify: `internal/toolhive/informer_test.go` + `throttle_test.go`

**Step 1: Write the failing tests**

```go
// internal/toolhive/informer_test.go (append)

// TestInformer_FIX2_M4_DefaultVerbosityIsQuiet asserts that at default
// (V(0)) verbosity, the dedup INFO firehose is silent. A single coalesced
// summary line fires at most once per 5 min listing the deduped tuples.
func TestInformer_FIX2_M4_DefaultVerbosityIsQuiet(t *testing.T) {
    logBuf := &recordingLogger{verbosity: 0}
    inf := &Informer{Log: logBuf}
    // Simulate 100 dedup events across 22 unique tuples within 1s.
    for i := 0; i < 100; i++ {
        inf.recordCollision(dedupKey{Kind: "MCPServer", Namespace: "mcp", Name: fmt.Sprintf("t%d", i%22)})
    }
    inf.flushDedupLogs()

    if got := logBuf.CountWithMessage("toolhive dedup: v1alpha1 wins"); got != 0 {
        t.Fatalf("default-verbosity dedup INFO must be silent, got %d lines", got)
    }
    if got := logBuf.CountWithMessage("toolhive dedup summary"); got != 1 {
        t.Fatalf("expected 1 coalesced summary line, got %d", got)
    }
}

// TestInformer_FIX2_L11_StartupSummaryListsAllGVKs asserts that on
// successful registration, a single INFO line is emitted that lists all
// 4 registered GVKs explicitly (not just v1alpha1).
func TestInformer_FIX2_L11_StartupSummaryListsAllGVKs(t *testing.T) {
    // Use the existing fake RESTMapper that serves both versions.
    ...
    // After tryRegister:
    line := logBuf.FindMessage("toolhive informers registered")
    if !strings.Contains(line, "v1alpha1") || !strings.Contains(line, "v1beta1") {
        t.Fatalf("startup line must list both versions: got %q", line)
    }
}
```

**Step 2: Apply the demotion + summary**

In `internal/toolhive/informer.go`, replace the per-collision INFO emit (L441-460) with:

```go
// FIX2.txt MEDIUM-4 (2026-05-22): the per-tuple dedup log fires at V(2)
// only. At default verbosity we emit a single coalesced summary line at
// most once per dedupSummaryWindow listing the dedupped tuples.
for _, ck := range store.collisions {
    i.Log.V(2).Info("toolhive dedup: v1alpha1 wins",
        "kind", ck.Kind, "namespace", ck.Namespace, "name", ck.Name,
    )
}
if len(store.collisions) > 0 && i.dedupSummaryThrottle.shouldLog("", dedupSummaryWindow) {
    names := dedupKeyNames(store.collisions)
    i.Log.Info("toolhive dedup summary: v1alpha1 wins",
        "kind_count", len(uniqueKinds(store.collisions)),
        "tuple_count", len(store.collisions),
        "examples", firstN(names, 10),
    )
}
```

Where `dedupSummaryWindow = 5 * time.Minute` (new const).

For LOW-11, modify `tryRegister` to accumulate registered GVKs and emit one summary line after the loop:

```go
i.Log.Info("toolhive informers registered",
    "gvks", gvkStrings(i.registered),  // ["mcpserver/v1alpha1","mcpserver/v1beta1",...]
    "count", len(i.registered),
)
```

Drop the per-GVK DEBUG line at L9-21 (the comment block describing it stays — that's the contract).

**Step 3: Run the tests**

```
./scripts/dev.sh make unit-pkg PKG=./internal/toolhive/
```

Expected: PASS.

**Step 4: Commit**

```
git add internal/toolhive/informer.go internal/toolhive/informer_test.go
git commit -m "perf(toolhive): coalesce dedup log + honest startup line (FIX2.txt M-4, L-11)

The per-tuple dedup INFO fires 22 lines/min on a 22-MCPServer cluster
even with the existing per-tuple-per-60s throttle. Demote to V(2) and
emit a single coalesced 'dedup summary' INFO line at most once per 5m
listing the deduped tuples.

Replace the per-GVK DEBUG startup line with a single 'toolhive informers
registered' INFO that lists all 4 registered GVKs honestly — fixes the
v0.1.2 startup-log artifact (FIX2.txt LOW-11) where only v1alpha1
appeared even though v1beta1 was also registered."
```

---

## Task 5: Surface LiteLLM response body in condition.Message (MEDIUM-5)

Justification: today every 4xx writes the same generic `"LiteLLM rejected POST /<path>: litellm: 400 on POST /<path> (code=400)"` to `condition.Message`. The parsed error envelope (`processLitellmError`) already extracts the message — it's just never threaded into the condition.

**Files:**
- Modify: `internal/litellm/client.go:135-159` (the 4xx return arms)
- Modify: `internal/litellm/errors.go` — add a new typed error `*RejectedError{Path, Code, Message, Body}` and have the 4xx return arms return it
- Modify: all reconcilers that emit `LiteLLMRejected` — surface `rejectedErr.Message` in `condition.Message`
- Add: tests in `internal/litellm/client_test.go` and per-controller `_fix2_m5_test.go`

**Step 1: Write the failing test**

```go
// internal/litellm/client_test.go (append)

// TestClient_FIX2_M5_RejectedErrorCarriesDetail asserts that a 4xx
// response with a parseable envelope returns a *RejectedError whose
// .Message is the parsed envelope's message, not the generic
// "litellm: 400 on POST <path>" string.
func TestClient_FIX2_M5_RejectedErrorCarriesDetail(t *testing.T) {
    srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
        w.WriteHeader(400)
        _, _ = w.Write([]byte(`{"error":{"message":"Server name cannot contain '.'.","type":"server_name_invalid","code":"400"}}`))
    }))
    defer srv.Close()

    c, _ := NewClient(srv.URL, "sk-test")
    _, err := c.CreateMCPServer(context.Background(), &MCPServerRequest{ServerName: "a.b.c"})
    if err == nil {
        t.Fatal("expected error")
    }
    var rej *RejectedError
    if !errors.As(err, &rej) {
        t.Fatalf("expected *RejectedError, got %T: %v", err, err)
    }
    if rej.Message != "Server name cannot contain '.'." {
        t.Fatalf("Message: got %q, want %q", rej.Message, "Server name cannot contain '.'.")
    }
}
```

**Step 2: Add `*RejectedError` in `internal/litellm/errors.go`**

```go
// RejectedError is returned for any non-2xx, non-401 response. Carries
// the parsed envelope message so callers can surface it in condition
// messages without re-parsing. FIX2.txt MEDIUM-5 (2026-05-22).
type RejectedError struct {
    Path    string
    Status  int
    Code    string
    Message string // from envelope error.message, truncated by processLitellmError
}

func (e *RejectedError) Error() string {
    if e.Message != "" {
        return fmt.Sprintf("litellm: %d on %s (%s): %s", e.Status, e.Path, e.Code, e.Message)
    }
    return fmt.Sprintf("litellm: %d on %s (code=%s)", e.Status, e.Path, e.Code)
}
```

In `internal/litellm/client.go:146-151`, replace the generic `fmt.Errorf(...)` 4xx return with:

```go
_, msg, code := processLitellmError(respBody)
if code == "" {
    code = fmt.Sprintf("%d", resp.StatusCode)
}
return nil, &RejectedError{
    Path:    fmt.Sprintf("%s %s", method, path),
    Status:  resp.StatusCode,
    Code:    code,
    Message: msg,
}
```

**Step 3: Thread `.Message` into condition writes**

In each reconciler's `writeStatus(ctx, &cr, metav1.ConditionFalse, "LiteLLMRejected", msg)` call, derive `msg` from the typed error when available:

```go
msg := fmt.Sprintf("LiteLLM rejected %s: %s", op, err.Error())
var rejErr *litellm.RejectedError
if errors.As(err, &rejErr) && rejErr.Message != "" {
    msg = fmt.Sprintf("LiteLLM rejected %s: %s", op, rejErr.Message)
}
```

Keep `msg` short — `kubectl get -o wide` truncates at ~80 chars. Consider clipping to 200 bytes before writing.

**Step 4: Add an envtest assertion in one controller (MCPServer)**

```go
// TestMCPServer_FIX2_M5_RejectedMessageInCondition asserts the
// LiteLLM-side reason ("Server name cannot contain '.'.") appears
// inside Ready=False condition.Message.
func TestMCPServer_FIX2_M5_RejectedMessageInCondition(t *testing.T) {
    ...
    refreshed := &litellmv1alpha1.LiteLLMMCPServer{}
    _ = k8sClient.Get(ctx, key, refreshed)
    cond := meta.FindStatusCondition(refreshed.Status.Conditions, "Ready")
    if !strings.Contains(cond.Message, "Server name cannot contain") {
        t.Fatalf("condition.Message missing LiteLLM detail: %q", cond.Message)
    }
}
```

**Step 5: Run tests**

```
./scripts/dev.sh make unit-pkg PKG=./internal/litellm/
./scripts/dev.sh make envtest-pkg PKG=./internal/controller/... FOCUS=TestMCPServer_FIX2_M5
```

Expected: PASS.

**Step 6: Commit**

```
git add internal/litellm/errors.go internal/litellm/client.go internal/litellm/client_test.go \
        internal/controller/*.go
git commit -m "feat(litellm): RejectedError type carries parsed body message (FIX2.txt M-5)

condition.Message for every LiteLLMRejected was the same generic string
('litellm: 400 on POST /<path> (code=400)') even though the LiteLLM
envelope's .error.message field had the actionable detail. Add a typed
*RejectedError (Path, Status, Code, Message) and have all 5 reconcilers
surface its .Message in condition.Message. Truncated by processLitellmError
to 512 bytes; reconcilers further clip to 200 bytes for kubectl-friendly
wide output."
```

---

## Task 6: Operator identity + ModelInfo audit fields (MEDIUM-8)

Justification: LiteLLM UI shows "Created By: Unknown" because the operator writes `ModelInfo{}` zero-valued at every CREATE/UPDATE site. Decision logged 2026-05-22 in FIX2.txt M-8: literal `alitellm-operator/<version>` via a new `internal/identity` package + ldflags injection, applied at every kind where LiteLLM accepts an audit identifier.

**Files:**
- Create: `internal/identity/identity.go`
- Create: `internal/identity/identity_test.go`
- Modify: `internal/controller/model_controller.go:466, 507, 530` (CREATE / D-02 recreate / UPDATE)
- Modify: `.goreleaser.yml` + `.goreleaser.prerelease.yml` + `.goreleaser.snapshot.yml` — `ldflags` injection
- Modify: `Dockerfile` (if it does the local build) — `ldflags` propagation
- Modify: `Makefile` — `go build` targets propagate `LDFLAGS` with `-X .../identity.Version=$(VERSION)`
- Modify: `internal/controller/team_controller.go`, `mcpserver_controller.go`, `a2aagent_controller.go` — populate the audit field if LiteLLM's API for that kind accepts it (probe first; see step 4)
- Tests: `_fix2_m8_test.go` per controller

**Step 1: Create `internal/identity`**

```go
// internal/identity/identity.go
// SPDX-License-Identifier: Apache-2.0
package identity

// Version is overridden by ldflags at build time
// (-X github.com/ackstorm/alitellm-operator/internal/identity.Version=X.Y.Z).
// Default "dev" applies when running under `go run` or `go test`.
var Version = "dev"

// Operator returns the audit identity literal threaded into LiteLLM
// /model/new, /model/update, /team/new, /v1/mcp/server, /a2a/agent requests.
// Format: "alitellm-operator/<version>". FIX2.txt MEDIUM-8 (2026-05-22).
func Operator() string {
    return "alitellm-operator/" + Version
}
```

```go
// internal/identity/identity_test.go
// SPDX-License-Identifier: Apache-2.0
package identity

import (
    "strings"
    "testing"
)

func TestOperator_DefaultVersion(t *testing.T) {
    got := Operator()
    if !strings.HasPrefix(got, "alitellm-operator/") {
        t.Fatalf("Operator() must start with 'alitellm-operator/': %q", got)
    }
}

func TestOperator_LDFlagsInjection(t *testing.T) {
    saved := Version
    defer func() { Version = saved }()
    Version = "1.2.3"
    if got, want := Operator(), "alitellm-operator/1.2.3"; got != want {
        t.Fatalf("Operator(): got %q, want %q", got, want)
    }
}
```

**Step 2: Wire ldflags injection**

In `.goreleaser.yml` (and `.prerelease.yml`, `.snapshot.yml`), under `builds[].ldflags`:

```yaml
ldflags:
  - -s -w
  - -X github.com/ackstorm/alitellm-operator/internal/identity.Version={{.Version}}
```

In `Makefile`, add `LDFLAGS ?= -X github.com/ackstorm/alitellm-operator/internal/identity.Version=$(VERSION)` and propagate to `go build`.

**Step 3: Write the failing test for Model (capture wire body)**

```go
// internal/controller/model_fix2_m8_test.go (append or new file)

func TestModel_FIX2_M8_CreateBodyHasOperatorIdentity(t *testing.T) {
    ...
    waitForReady(t, model, 30*time.Second)

    body := lastFakeModelCreateBody(t)
    var payload struct {
        ModelInfo struct {
            CreatedBy string `json:"created_by"`
            UpdatedBy string `json:"updated_by"`
        } `json:"model_info"`
    }
    _ = json.Unmarshal(body, &payload)
    want := "alitellm-operator/" + identity.Version
    if payload.ModelInfo.CreatedBy != want {
        t.Fatalf("created_by: got %q, want %q", payload.ModelInfo.CreatedBy, want)
    }
    if payload.ModelInfo.UpdatedBy != want {
        t.Fatalf("updated_by: got %q, want %q", payload.ModelInfo.UpdatedBy, want)
    }
}

// Idempotency: second reconcile sends UpdatedBy without changing CreatedBy.
func TestModel_FIX2_M8_UpdateBodyHasOperatorIdentity(t *testing.T) {
    ...
}
```

**Step 4: Apply at the three Model controller sites**

In `internal/controller/model_controller.go`:

- `:466` (CREATE arm):
  ```go
  ModelInfo: litellm.ModelInfo{
      CreatedBy: identity.Operator(),
      UpdatedBy: identity.Operator(),
  },
  ```
- `:507` (D-02 recreate arm):
  ```go
  ModelInfo: litellm.ModelInfo{
      ID:        "",
      CreatedBy: identity.Operator(),
      UpdatedBy: identity.Operator(),
  },
  ```
- `:530` (UPDATE arm):
  ```go
  ModelInfo: litellm.ModelInfo{
      UpdatedBy: identity.Operator(),
  },
  ```

Do NOT set `CreatedAt` / `UpdatedAt` — LiteLLM stamps those server-side.

**Step 5: Probe LiteLLM API for Team / MCPServer / A2AAgent audit acceptance**

For each kind, look at the request type's existing JSON tags + the captured OpenAPI snapshot in `spec/`:

```
grep -n "created_by\|updated_by\|creator" spec/litellm-openapi.json | head -30
```

For each kind where the schema accepts an audit field, mirror the Model population in the controller's request build site. If the field is missing for a kind, write a one-line comment in the controller noting that LiteLLM 1.85.1 schema does not surface the field and skip — do NOT speculate field names.

**Step 6: Run tests + envtest**

```
./scripts/dev.sh make unit-pkg PKG=./internal/identity/
./scripts/dev.sh make envtest-pkg PKG=./internal/controller/... FOCUS=TestModel_FIX2_M8
```

Expected: PASS.

**Step 7: Commit**

```
git add internal/identity/ internal/controller/*.go .goreleaser*.yml Makefile
git commit -m "feat(model): populate model_info.created_by/updated_by with operator identity (FIX2.txt M-8)

LiteLLM UI Models table column 'Created By' showed 'Unknown' for every
operator-managed model. The model_info.created_by/updated_by fields
existed in the request type but were never set.

Add internal/identity (ldflags-injected via .goreleaser.yml) returning
'alitellm-operator/<version>'. Populate at the three Model controller
sites: CREATE, UPDATE, D-02 recreate. Mirror in Team / MCPServer /
A2AAgent request builders where the LiteLLM 1.85.1 schema accepts the
field; skip otherwise."
```

---

## Task 7: Shared rate limiter on `internal/litellm.Client` (MEDIUM-10)

Justification: on boot the operator fires ~30 writes/s in a 1-second window. A modestly stressed proxy could 5xx-then-backoff. Add a shared `golang.org/x/time/rate.Limiter` on the Client, configurable on Connection.spec.

**Files:**
- Modify: `internal/litellm/client.go` — embed `*rate.Limiter`, acquire before each `httpClient.Do`
- Modify: `api/litellm/v1alpha1/litellmconnection_types.go` — add `MaxRequestsPerSecond` + `MaxBurst`
- Modify: connection cache loader — pass the values into `NewClient`
- Test: `internal/litellm/client_ratelimit_test.go`

**Step 1: Write the failing test**

```go
// TestClient_FIX2_M10_RateLimiterSpacesStorm asserts that N concurrent
// Do() calls against a fake server complete in approximately (N/rps)
// seconds, not all-at-once. Regression for FIX2.txt MEDIUM-10.
func TestClient_FIX2_M10_RateLimiterSpacesStorm(t *testing.T) {
    srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
        w.WriteHeader(200)
        _, _ = w.Write([]byte(`{}`))
    }))
    defer srv.Close()

    c, _ := NewClient(srv.URL, "sk-test", WithRateLimit(5, 5))
    var wg sync.WaitGroup
    start := time.Now()
    for i := 0; i < 25; i++ {
        wg.Add(1)
        go func() {
            defer wg.Done()
            _, _ = c.makeRequest(context.Background(), "GET", "/health", nil, nil)
        }()
    }
    wg.Wait()
    if elapsed := time.Since(start); elapsed < 3*time.Second {
        t.Fatalf("rate limiter not active: 25 requests at 5 rps should take ~4s, got %v", elapsed)
    }
}
```

**Step 2: Add the limiter + option**

In `internal/litellm/client.go`:

```go
type Client struct {
    ...
    limiter *rate.Limiter // nil → unlimited
}

type ClientOption func(*Client)

func WithRateLimit(rps float64, burst int) ClientOption {
    return func(c *Client) {
        if rps > 0 {
            c.limiter = rate.NewLimiter(rate.Limit(rps), burst)
        }
    }
}

// In makeRequest, before httpClient.Do:
if c.limiter != nil {
    if err := c.limiter.Wait(ctx); err != nil {
        return nil, fmt.Errorf("rate limiter wait: %w", err)
    }
}
```

**Step 3: Wire from Connection.spec**

Add fields to `LiteLLMConnectionSpec`:

```go
// MaxRequestsPerSecond caps the sustained rate of HTTP requests to LiteLLM.
// Defaults to 5 (FIX2.txt MEDIUM-10). Set to 0 to disable rate limiting
// (NOT recommended — boot storms can trigger 5xx).
//
// +optional
// +kubebuilder:default:=5
// +kubebuilder:validation:Minimum=0
// +kubebuilder:validation:Maximum=1000
MaxRequestsPerSecond int32 `json:"maxRequestsPerSecond,omitempty"`

// MaxBurst is the token-bucket burst size paired with MaxRequestsPerSecond.
// Defaults to 10.
//
// +optional
// +kubebuilder:default:=10
// +kubebuilder:validation:Minimum=0
// +kubebuilder:validation:Maximum=1000
MaxBurst int32 `json:"maxBurst,omitempty"`
```

In the connection cache loader, pass `WithRateLimit(float64(spec.MaxRequestsPerSecond), int(spec.MaxBurst))` into `NewClient`.

**Step 4: Run the test + a quick envtest sanity check**

```
./scripts/dev.sh make unit-pkg PKG=./internal/litellm/
./scripts/dev.sh make envtest-pkg PKG=./internal/controller/... FOCUS=TestConnection
```

Expected: PASS.

**Step 5: Commit**

```
git add internal/litellm/client.go internal/litellm/client_ratelimit_test.go \
        api/litellm/v1alpha1/litellmconnection_types.go \
        internal/connection/*.go config/crd/bases/ deploy/helm/
git commit -m "feat(litellm): shared rate limiter on Client (FIX2.txt M-10)

Boot-time thundering herd: ~30 writes/s in a 1-second window after
leader-election can push a stressed LiteLLM proxy into 5xx territory,
triggering the operator's own backoff loop. Add a golang.org/x/time/rate
limiter on the Client (default 5 rps, burst 10), configurable via
LiteLLMConnection.spec.maxRequestsPerSecond + maxBurst."
```

---

## Task 8: Prometheus reconcile_total + rejected_total metrics (LOW-6)

Justification: provider rejection patterns (Bedrock IAM scope, Gemini project allowlist, OpenAI model registry) look identical at the K8s surface — same generic 400. Add a Prometheus counter with `kind`, `namespace`, `result` labels so the dashboard surfaces failure shape without a kubectl describe sweep.

**Files:**
- Create: `internal/metrics/metrics.go`
- Modify: each reconciler — increment the counter at every status-write site
- Modify: `cmd/main.go` — register the counter with the controller-runtime metrics registry
- Tests: `internal/metrics/metrics_test.go`

**Step 1: Add the counter**

```go
// internal/metrics/metrics.go
// SPDX-License-Identifier: Apache-2.0
package metrics

import (
    "github.com/prometheus/client_golang/prometheus"
    "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// ReconcileTotal tracks reconcile outcomes per CR kind. FIX2.txt LOW-6
// (2026-05-22). Result values: "synced", "rejected", "transient_error",
// "secret_missing", "unreachable".
var ReconcileTotal = prometheus.NewCounterVec(
    prometheus.CounterOpts{
        Namespace: "litellm_operator",
        Name:      "reconcile_total",
        Help:      "Total reconcile passes per kind/namespace/result.",
    },
    []string{"kind", "namespace", "result"},
)

func init() {
    metrics.Registry.MustRegister(ReconcileTotal)
}
```

**Step 2: Wire into each reconciler**

At every `writeStatus(...)` call site, add `metrics.ReconcileTotal.WithLabelValues(<kind>, <ns>, <result>).Inc()` mapping:
- `Ready=True` → `"synced"`
- `LiteLLMRejected` → `"rejected"`
- `Unreachable` → `"unreachable"`
- `SecretNotFound` → `"secret_missing"`
- `LiteLLMUnavailable` → `"transient_error"`

Helper: extract to a small `func recordReconcileMetric(kind, ns, reason string)` that maps reason → result.

**Step 3: Test**

```go
func TestMetrics_FIX2_L6_ResultLabel(t *testing.T) {
    ReconcileTotal.Reset()
    recordReconcileMetric("Model", "default", "LiteLLMRejected")
    if got := testutil.ToFloat64(ReconcileTotal.WithLabelValues("Model", "default", "rejected")); got != 1 {
        t.Fatalf("counter not incremented")
    }
}
```

**Step 4: Run**

```
./scripts/dev.sh make unit-pkg PKG=./internal/metrics/
```

Expected: PASS.

**Step 5: Commit**

```
git add internal/metrics/ internal/controller/*.go cmd/main.go
git commit -m "feat(metrics): expose litellm_operator_reconcile_total{kind,ns,result} (FIX2.txt L-6)

Per-kind reconcile-outcome counter wired into the controller-runtime
metrics endpoint. Dashboard can now slice 'Bedrock has 1 stuck model'
out of {kind=Model, result=rejected, namespace=ackstorm} without a
kubectl describe sweep."
```

---

## Task 9: Runtime-default loader for `mcpToolPrefixSeparator` (LOW-12)

Justification: Flux/GitOps tools strip unset fields, so the kubebuilder default doesn't land in the apiserver object. The operator reads empty string and Task 1's sanitizer treats `""` as the default — which is fine, but the user has no clue what default is in force. Add a one-shot log per Connection.

**Files:**
- Modify: connection cache loader (search `internal/connection/`)
- Add: a small `sync.Map` of "logged-once" Connection NamespacedNames
- Tests: `internal/connection/cache_test.go`

**Step 1: Write the failing test**

```go
// TestCache_FIX2_L12_LogsDefaultOnEmptyField asserts that when a
// Connection is loaded with empty mcpToolPrefixSeparator, the loader
// emits exactly one INFO line per Connection per pod lifetime announcing
// the resolved default.
func TestCache_FIX2_L12_LogsDefaultOnEmptyField(t *testing.T) {
    ...
    cache.LoadOrUpdate(connWithoutSep)
    cache.LoadOrUpdate(connWithoutSep)
    cache.LoadOrUpdate(connWithoutSep)

    if got := logBuf.CountWithMessage("mcpToolPrefixSeparator default applied"); got != 1 {
        t.Fatalf("expected exactly one INFO line, got %d", got)
    }
}
```

**Step 2: Apply in the loader**

In the cache's snapshot-build path, when the field is empty:

```go
if conn.Spec.MCPToolPrefixSeparator == "" {
    if _, seen := c.defaultLogged.LoadOrStore(types.NamespacedName{...}.String(), struct{}{}); !seen {
        c.log.Info("mcpToolPrefixSeparator default applied",
            "namespace", conn.Namespace, "name", conn.Name,
            "default", litellm.MCPToolPrefixSeparatorDefault,
            "note", "field is unset in spec; consider pinning explicitly under GitOps")
    }
}
```

**Step 3: Run**

```
./scripts/dev.sh make unit-pkg PKG=./internal/connection/
```

Expected: PASS.

**Step 4: Commit**

```
git add internal/connection/*.go
git commit -m "feat(connection): log resolved mcpToolPrefixSeparator default once per Connection (FIX2.txt L-12)

Flux/GitOps pipelines strip unset fields, so the kubebuilder default
on LiteLLMConnection.spec.mcpToolPrefixSeparator doesn't land in the
apiserver representation. The operator already handles empty-string as
the resolved default, but admins running under GitOps had no signal
that the default was in play. Emit one INFO line per Connection per
pod lifetime when the field is empty."
```

---

## Task 10: STATUS blocks on FIX.txt + FIX2.txt entries (LOW-7)

Justification: FIX.txt HIGH-1 wording still describes the dotted-name bug as if unfixed even though v0.1.2 shipped a fix attempt. Now that FIX2.txt HIGH-1 + HIGH-9 supersede with the correct fix, mark BOTH files' entries with explicit STATUS blocks so traceability survives.

**Files:**
- Modify: `FIX.txt`
- Modify: `FIX2.txt`

**Step 1: Append STATUS block to FIX.txt HIGH-1, HIGH-2, MEDIUM-3, LOW-5, LOW-6, LOW-7**

Pattern (at the top of each entry, before SYMPTOM):

```
STATUS: SHIPPED in v0.1.2 (commits 20ade71, 44fb0cb, bfc0d8c).
        See FIX2.txt HIGH-1 + HIGH-9 for residual default-direction
        and hyphen-name regression. Final fix in v0.1.3 (this branch).
```

Per entry, vary the commit refs and the FIX2 cross-reference.

**Step 2: Append STATUS block to FIX2.txt entries as commits land**

This step is performed *after* each prior task's commit. Skip until the final-gate task — at that point, sweep through FIX2.txt and stamp:

```
STATUS: SHIPPED in v0.1.3 (commit <hash>). See plan
        docs/plans/2026-05-22-fix2-txt-remediation.md Task N.
```

**Step 3: Commit**

```
git add FIX.txt FIX2.txt
git commit -m "docs(fixtxt): STATUS blocks on shipped FIX.txt and FIX2.txt entries (FIX2.txt L-7)"
```

---

## Task 11: Full pre-commit + pre-push gate sweep

**Files:** none.

**Step 1: Full envtest sweep**

```
./scripts/dev.sh make envtest-run
```

Expected: PASS (no race, no flakes).

**Step 2: E2E full sweep**

```
make cluster-down
./scripts/dev.sh make e2e-full
```

Expected: PASS. If any e2e Ginkgo spec fails, treat as regression — investigate before declaring done.

**Step 3: Security sweep**

```
./scripts/dev.sh make security
```

Expected: PASS — gosec + govulncheck + fuzz-short. If govulncheck surfaces a NEW HIGH advisory, file a separate task to evaluate (do NOT blanket-add to the ack-list).

**Step 4: Pre-push gate**

```
make pre-push
```

Expected: 15/15 gates PASS — gitleaks (origin/main..HEAD scope), trufflehog, large-file, sensitive-files, LICENSE+README, origin remote, govulncheck ack-list 1:1, `go mod tidy` clean, SPDX headers.

If any gate fails, fix root cause. NEVER `--no-verify`.

**Step 5: Push**

```
git push origin main
```

If the release pipeline is to be triggered, follow the `chore(release): vX.Y.Z` commit pattern documented in `CLAUDE.md`. The expected next version is `v0.1.3` since the changes include both a default-flip (breaking-ish for non-stock LiteLLM deploys) and new spec fields.

---

## Done criteria

- All 11 FIX2.txt findings have a landed commit; FIX2.txt entries carry STATUS: SHIPPED blocks.
- `make envtest-run`, `make e2e-full`, `make security`, `make pre-push` all PASS.
- New tests added (minimum):
  - Sanitizer: 6 new table cases covering no-op-on-safe + sep variants (Task 1).
  - HIGH-9 envtest: orphan adoption of pre-v0.1.2 MCPServer (Task 1).
  - HIGH-2 envtest: deterministic-error periodic requeue (Task 2).
  - HIGH-2 envtest: BootSweeper enqueues stuck CRs (Task 2).
  - MEDIUM-3 envtest: cold-start Connection→Team fan-in within 30s (Task 3).
  - MEDIUM-4 unit: per-tuple throttle contract (Task 0) + default-verbosity silence (Task 4).
  - LOW-11 unit: startup-line lists both versions (Task 4).
  - MEDIUM-5 unit + envtest: RejectedError carries body message (Task 5).
  - MEDIUM-8 unit + envtest: ModelInfo.created_by/updated_by populated (Task 6).
  - MEDIUM-10 unit: rate-limiter spaces 25 calls at 5 rps ≥3s (Task 7).
  - LOW-6 unit: counter increments per result (Task 8).
  - LOW-12 unit: one INFO per Connection per pod lifetime (Task 9).
- No `--no-verify`, no ack-list expansion, no removed gates.
- HIGH-1 prod symptom verified absent against a stock LiteLLM v1.85.1: a stock-default Connection produces 22/22 MCPServerDiscovery children Ready=True.
- HIGH-9 prod symptom verified absent: a pre-existing `test-exa-mcp` MCPServer survives upgrade (no rewrite, no orphan, no LiteLLMRejected).
- HIGH-2 prod symptom verified absent: a CR stuck on LiteLLMRejected recovers within 5m (default `requeueOnRejectedAfter`) once upstream accepts; an operator restart on the fixed image heals every stuck CR within ~30s of pod Ready.
- MEDIUM-3 prod symptom verified absent: a Team / A2AAgent CR reaches Ready=True within 30s of operator boot against a pre-existing Ready Connection.
- MEDIUM-4 prod symptom verified absent: default operator log shows 0 "toolhive dedup: v1alpha1 wins" lines per minute; one summary line per 5m at most.
- MEDIUM-5 prod symptom verified absent: `kubectl describe litellmmcpserver <name>` shows the actionable LiteLLM message in `Conditions[].Message`.
- MEDIUM-8 prod symptom verified absent: LiteLLM UI Models table shows `alitellm-operator/v0.1.3` in "Created By" column for every operator-managed model.
- MEDIUM-10 prod symptom verified absent: operator startup against a 50-CR cluster spaces writes at ≤5 rps + burst 10 (visible in fake-LiteLLM access log; verified in envtest).
- LOW-6 metric `litellm_operator_reconcile_total{kind, namespace, result}` queryable from `/metrics`.
- LOW-11 prod symptom verified absent: startup log emits one "toolhive informers registered" INFO line listing all 4 GVKs.
- LOW-12 prod symptom verified absent: a Flux-managed Connection without `mcpToolPrefixSeparator` set emits exactly one "default applied" INFO line per pod lifetime.

---

## Resolved decisions (locked 2026-05-22 in FIX2.txt header)

All prior open questions are resolved by the FIX2.txt "Decisions locked"
block. Recorded here as a recap so executing-plans / subagent-driven runs
don't need to re-open them:

1. **HIGH-1:** option (a) — flip kubebuilder default `"-"` → `"."`. No
   server-side probe. CHANGELOG release note required (Task 1, step 9).
2. **HIGH-2:** combined approach. `RequeueAfter: 5m` on deterministic
   errors AND a one-shot boot reconcile sweep. Configurable via
   `spec.requeueOnRejectedAfter` (default 5m, min 1m, max 1h — enforced
   via CEL XValidation, Task 2 step 2).
3. **HIGH-9:** sanitizer no-op-on-safe + adopt-by-original-name. With
   the HIGH-1 default flip applied, the regression vanishes for any K8s
   name that was already valid under v0.1.1 (Task 1).
4. **MEDIUM-3:** Option B — Create + Update(Ready edge) + GenericEvent
   on the Connection-watch predicate; each fire enqueues ALL child CRs
   in the namespace. Skip the per-controller boot sweep — HIGH-2's
   BootSweeper covers cold-start (Task 3).
5. **MEDIUM-8:** identity literal `alitellm-operator/<version>` via
   ldflags. Apply to Model unconditionally; probe Team/MCPServer/A2AAgent
   endpoints before populating (Task 6 step 5).
6. **Per-CR provenance (MEDIUM-8 NOT-IN-SCOPE):** explicit defer in
   FIX2.txt M-8 ("ackstorm/test-team@alitellm-operator/v0.1.3" form).
   Not planned.
7. **MEDIUM-4 / LOW-6 / LOW-7 / LOW-11 / LOW-12:** no open decisions —
   implement as written in each entry's FIX block.

**Build order (locked):** HIGH-9 → HIGH-1 (same PR) → HIGH-2 → MEDIUM
band in parallel → LOW band as polish.

**One residual decision-tree branch (Task 0 outcome path):** if Task 0's
LOW-11 test FAILS (registration is broken, not just a logging artifact),
LOW-11 becomes a substantive code change instead of a log demotion.
Task 0 step 4 says "file a separate child task; revise the plan; do not
silently broaden Task 4." That branch is the only outcome that requires
plan amendment — all other tasks are fully specified.
