# Simplify Connection writeStatus — drop Patch + skip-when-equal Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Revert `LiteLLMConnectionReconciler.writeStatus` from `Patch(MergeFrom) + statusReadyUnchanged` back to plain `Status().Update`. Delete the `statusReadyUnchanged` helper and its dedicated test file. Keep `logStatusUpdateErr` and the IsConflict swallow blocks — those are the actual noise wins.

**Architecture:** Connection's Status only carries `Conditions + ObservedGeneration`, so the Patch+skip optimisation we added avoids ~1 apiserver round-trip per 5-minute probe — marginal. The complexity (DeepCopy + skip-when-equal + dedicated helper + test) is not worth that saving, especially after benchmarking against PalenaAI/litellm-operator which uses plain Update across the board. The `logStatusUpdateErr` V(1) demotion alone closes the original WR-03 storm noise; that stays.

**Tech Stack:** Go 1.24, controller-runtime v0.19.4, k8s.io/* v0.31.0. Devtools container via `./scripts/dev.sh`.

---

### Task 1: Audit current state and confirm scope

**Files:**
- Read: `internal/controller/litellmconnection_controller.go:395-433` (current `writeStatus`)
- Read: `internal/controller/status_log.go` (helper file)
- Read: `internal/controller/status_log_test.go` (helper test file)

**Step 1: Confirm `statusReadyUnchanged` has exactly one production caller**

Run:
```
./scripts/dev.sh bash -c 'cd /workspace && grep -rn statusReadyUnchanged internal/'
```
Expected output:
```
internal/controller/litellmconnection_controller.go:406:	if statusReadyUnchanged(conn.Status.Conditions, conn.Status.ObservedGeneration, conn.Generation, status, reason, message) {
internal/controller/status_log.go:17:func statusReadyUnchanged(
internal/controller/status_log_test.go:11:func TestStatusReadyUnchanged(t *testing.T) {
internal/controller/status_log_test.go:101:			got := statusReadyUnchanged(tt.conds, tt.observedGen, tt.gen, tt.status, tt.reason, tt.message)
```

The Connection writeStatus is the single production caller. Safe to delete the helper after task 2.

**Step 2: Confirm `Patch` + `client.MergeFrom` only used by Connection writeStatus**

Run:
```
./scripts/dev.sh bash -c 'cd /workspace && grep -n "Status().Patch\|client.MergeFrom" internal/controller/litellmconnection_controller.go'
```
Expected output:
```
432:	return r.Status().Patch(ctx, conn, client.MergeFrom(orig))
```

(No other Connection-controller Patch site exists; reverting is mechanical.)

**Step 3: No commit on this task — audit only**

---

### Task 2: Revert `writeStatus` to plain Update

**Files:**
- Modify: `internal/controller/litellmconnection_controller.go:395-433`

**Step 1: Update the writeStatus body**

Replace the existing function body so it matches the pre-fce284f shape: gauge update + condition merge + `r.Status().Update`. The DeepCopy, the `statusReadyUnchanged` short-circuit, and the `client.MergeFrom(orig)` Patch all come out.

Edit `internal/controller/litellmconnection_controller.go` — replace the whole function (still starting at the `func (r *LiteLLMConnectionReconciler) writeStatus(...)` signature already in place):

```go
func (r *LiteLLMConnectionReconciler) writeStatus(
	ctx context.Context,
	conn *litellmv1alpha1.LiteLLMConnection,
	status metav1.ConditionStatus,
	reason, message string,
) error {
	cond := metav1.Condition{
		Type:               "Ready",
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: conn.Generation,
		LastTransitionTime: metav1.Now(),
	}
	apimeta.SetStatusCondition(&conn.Status.Conditions, cond)
	conn.Status.ObservedGeneration = conn.Generation

	// §10: one-hot ConnectionReady gauge. Clear all six reasons to 0,
	// then set the active reason to 1. Pattern from PATTERNS.md lines
	// 712-718.
	for _, rk := range connectionReasonAll {
		metrics.ConnectionReady.WithLabelValues(rk).Set(0)
	}
	metrics.ConnectionReady.WithLabelValues(reason).Set(1)

	// Plain Update — Connection's Status only carries Conditions +
	// ObservedGeneration so we do not need MergeFrom Patch semantics
	// (no separately-mutated fields the caller would lose to a stale
	// orig). The 409 conflict noise this can emit is demoted to V(1) by
	// logStatusUpdateErr at the call sites.
	return r.Status().Update(ctx, conn)
}
```

**Step 2: Verify the `client.MergeFrom` reference is gone**

Run:
```
./scripts/dev.sh bash -c 'cd /workspace && grep -n "client.MergeFrom" internal/controller/litellmconnection_controller.go'
```
Expected: no output.

**Step 3: Verify the `statusReadyUnchanged` call is gone**

Run:
```
./scripts/dev.sh bash -c 'cd /workspace && grep -n statusReadyUnchanged internal/controller/litellmconnection_controller.go'
```
Expected: no output.

**Step 4: Build + vet**

Run:
```
./scripts/dev.sh go build ./...
./scripts/dev.sh go vet ./internal/controller/...
```
Expected: no output, exit 0 on both.

**Step 5: Commit**

```
git add internal/controller/litellmconnection_controller.go
git commit -m "$(cat <<'EOF'
refactor(controller): revert Connection writeStatus to plain Update

The Patch + MergeFrom + statusReadyUnchanged shape introduced by
fce284f saved ~1 apiserver round-trip per 5-minute steady-state probe
on the LiteLLMConnection singleton — a marginal win that is not worth
the DeepCopy, the skip-when-equal helper, and a dedicated unit-test
file. PalenaAI/litellm-operator uses plain Status().Update across all
controllers; the V(1) demotion that logStatusUpdateErr already does at
each call site closes the WR-03 ERROR-level storm noise that was the
original problem.

Net: simpler writeStatus that matches the rest of the controllers in
this repo and the prevailing pattern in the wider LiteLLM operator
ecosystem.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 3: Delete `statusReadyUnchanged` helper + its test

**Files:**
- Modify: `internal/controller/status_log.go` (remove `statusReadyUnchanged` and now-unused imports)
- Delete: `internal/controller/status_log_test.go`

**Step 1: Strip `statusReadyUnchanged` from `status_log.go`**

Rewrite the file as:

```go
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// logStatusUpdateErr emits the standard WR-03 capture-and-log line for a
// failed status subresource update. Conflict errors (HTTP 409, normal in
// envtest and during competing reconciles) are demoted to V(1) so they no
// longer appear at the default verbosity. All other errors stay at Error
// level so genuine storms remain observable.
func logStatusUpdateErr(logger logr.Logger, err error, keysAndValues ...any) {
	if err == nil {
		return
	}
	if apierrors.IsConflict(err) {
		// Pass err itself (not err.Error()) so structured logr backends can
		// render the typed error or extract status details if they choose.
		kv := append([]any{"error", err}, keysAndValues...)
		logger.V(1).Info("status update conflict (expected; retried by controller-runtime)", kv...)
		return
	}
	logger.Error(err, "status update failed", keysAndValues...)
}
```

`apimeta` and `metav1` imports come out with the helper.

**Step 2: Delete the test file**

Run:
```
rm internal/controller/status_log_test.go
```

**Step 3: Build + vet**

Run:
```
./scripts/dev.sh go build ./...
./scripts/dev.sh go vet ./internal/controller/...
```
Expected: no output, exit 0 on both.

**Step 4: Run unit tests to confirm nothing referenced the deleted helper**

Run:
```
./scripts/dev.sh make unit
```
Expected: every package reports `ok` (the full unit suite is small, ~15s warm).

**Step 5: Commit**

```
git add internal/controller/status_log.go internal/controller/status_log_test.go
git commit -m "$(cat <<'EOF'
refactor(controller): drop statusReadyUnchanged helper + its dedicated test

statusReadyUnchanged had exactly one production caller — the
LiteLLMConnection writeStatus skip-when-equal short-circuit reverted
in the preceding commit. With that gone, the helper and its dedicated
table test (status_log_test.go) carry no weight. Remove both. The
apimeta + metav1 imports come out with the helper.

logStatusUpdateErr stays — that is the actual noise win (V(1) demotion
of apierrors.IsConflict) and is exercised across every controller's
writeStatus call site.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 4: Run focused envtest sweep to confirm nothing regressed

**Files:** none modified — verification only.

**Step 1: Run the LiteLLMConnection-targeted envtest scope (without -race, matching CI)**

Run:
```
./scripts/dev.sh make envtest-pkg PKG=./internal/controller/... FOCUS="TestConnection\\|TestLiteLLMConnection"
```
Expected: every `--- PASS:` line for the Connection probe / fast-path / finalizer suites; final `ok  github.com/ackstorm/alitellm-operator/internal/controller` with non-zero coverage.

**Step 2: Run the Team-default-runnable envtests (they read the cache snapshot — the Connection-side write must still seed Synced)**

Run:
```
./scripts/dev.sh make envtest-pkg PKG=./internal/controller/... FOCUS="TestTeamHubSeam_AC_DC1_VirtualKeysCoexist\\|TestTeamReconciler_AC_T6_ParamsPassthrough\\|TestTeamReconciler_AC_T3_Delete"
```
Expected: every test in the regex passes.

**Step 3: Confirm no leftover Patch/skip-when-equal references survive**

Run:
```
./scripts/dev.sh bash -c 'cd /workspace && grep -rn "statusReadyUnchanged\|client.MergeFrom" internal/'
```
Expected: no output.

**Step 4: No commit on this task — verification only**

---

### Task 5: Full envtest gate (the CI-equivalent run)

**Files:** none modified — verification only.

**Step 1: Run the no-race envtest (matches CI today)**

Run:
```
./scripts/dev.sh make envtest-fast
```
Expected: `ok  github.com/ackstorm/alitellm-operator/internal/controller` + `ok  github.com/ackstorm/alitellm-operator/internal/toolhive`. The 369d0b0 race fix already cleared `envtest-run -race` against the previous state of the tree; the cut we are making here only removes code paths, so the race-clean status holds — re-run `make envtest-run` if you want to double-check, but it is not the CI gate.

**Step 2: No commit on this task — verification only**

---

## Done criteria

- `git diff main` shows only two commits (the revert + the helper delete), no stray edits.
- `grep -rn "statusReadyUnchanged\|client.MergeFrom" internal/` returns no hits.
- `make envtest-fast` is green.
- `logStatusUpdateErr` still wraps every status-write error path in the seven controllers (no regression there).
- `apierrors.IsConflict` swallow blocks still sit after each Synced-path writeStatus call (unchanged by this plan).
- `reconcileImplicitDefault: connection not Ready; skipping` log still skipped when `snap.Reason == ""` (unchanged by this plan).
