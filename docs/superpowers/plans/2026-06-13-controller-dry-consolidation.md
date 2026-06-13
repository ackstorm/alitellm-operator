# Controller DRY Consolidation Implementation Plan (Finding #14)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

> **⛔ EXECUTION GATE — DO NOT START UNTIL THE BUG-FIX BRANCH HAS MERGED.**
> This plan refactors the exact functions edited by `docs/superpowers/plans/2026-06-13-code-review-fixes.md`
> (`classifyMutationError`, `onAckMissing`, `writeStatus`, `is4xxError`, the deletion switches). Running
> the two in parallel produces guaranteed merge conflicts, and this refactor's entire safety model is
> "tests stay green" — which requires the bug-fix regression tests to already be in the tree. Start from
> `main` AFTER the bug-fix PR merges.

**Goal:** Collapse the 6-controller copy-paste (4xx classification, `onAckMissing`, `classifyMutationError`, the SEC-03 duplicate-`as` check, the secret-ref indexers, `writeStatus`, the safety-relist runnable) into shared, single-source helpers — **zero behavior change**.

**Architecture:** Each phase extracts ONE duplicated pattern into a package-level helper (or generic), then migrates each controller to call it, one commit per extraction. The invariant is behavioral: `make test-full` (race-enabled, whole controller package) is GREEN before and after every phase. No new functionality, no contract changes.

**Tech Stack:** Go 1.26 (generics available), controller-runtime v0.19.4. Same test infra as the bug-fix plan (pure Go `testing` + envtest + `internal/litellm/mock`).

---

## Conventions

- **Safety net, run before AND after every phase:** `make test-full` (= `test-unit` + race-enabled `test-envtest`). A phase is only "done" when this is green with no test edits (refactors must not require test changes — if a test breaks, the refactor changed behavior; fix the refactor, not the test).
- **Locate by grep, not line number** — the bug-fix branch shifted every line. Each task starts with a `grep` "Step 0" to re-locate.
- **One extraction per commit.** Commit message: `refactor(controller): <what> into shared helper (no behavior change)`.
- **New shared file:** put package-level helpers in `internal/controller/shared_helpers.go` (create once, SPDX header `// SPDX-License-Identifier: Apache-2.0`, `package controller`).
- **Lint after each phase:** `make qa-lint-changed`.
- **No CRD / generated-file changes** anywhere in this plan.

**Confirmed current state (re-verify with grep at execution time):**
- `classifyMutationError` — 5 methods: `model_controller.go`, `mcpserver_controller.go`, `team_controller.go`, `a2aagent_controller.go`, `litellmguardrail_controller.go`. Signature (4 of them): `(ctx, <cr>, logger, err, opDesc string) (ctrl.Result, error)`.
- 4xx predicates: `is4xxError` (guardrail file), `isTransientLiteLLMError` + `is4xxNon401Status` (team file), plus inline `for code := 400; code < 500` loops inside the mcp/a2a/team `classifyMutationError` copies.
- `litellm.RejectedError` has `Status int` (errors.go:64) and `litellm.IsNotFound` already uses `errors.As(err, &rej) && rej.Status == 404` (errors.go:122) — the typed approach is already partially adopted.
- `onAckMissing` — 5 inline closures in the deletion paths (model, mcpserver, a2aagent, guardrail, team).
- SEC-03 duplicate-`as` check — 4 inline blocks (model, team, mcpserver, a2aagent). (Guardrail has no `spec.secrets` at top level — its secrets nest under `spec.guardrails`; confirm before assuming a 5th copy.)
- `IndexXSecretRefs` — 5 funcs. Index-key constants: **4 share** `.spec.secrets[*].secretRef.name` (`SecretRefIndexField`, `MCPServerSecretRefIndexField`, `TeamSecretRefIndexField`, `A2AAgentSecretRefIndexField`); **guardrail differs**: `GuardrailSecretRefIndexField = ".spec.guardrails.secrets[*].secretRef.name"`. The *extraction logic* is shared; the *key constant* and the *secrets accessor* differ for guardrail.
- `writeStatus` — 5 methods (model, a2aagent, guardrail use `retry.RetryOnConflict`; mcpserver, team use bare `Status().Update`/`Patch`). Connection's `writeStatus` is a DIFFERENT shape — leave it out of scope.
- Safety-relist runnables: `ModelSafetyRelistRunnable` (model_controller.go) and `GuardRailSafetyRelistRunnable` (guardrail file) are structurally identical.

---

## Phase 0: Branch + baseline

### Task 0: Branch and confirm green baseline

- [ ] **Step 1: Branch from the merged main**

```bash
cd /home/coder/workspace/local/alitellm-operator
git checkout main && git pull --ff-only
git checkout -b refactor/controller-dry-consolidation
```

- [ ] **Step 2: Confirm the safety net is green BEFORE touching anything**

Run: `make test-full`
Expected: PASS. (If red, STOP — the baseline must be green or the "stays green" invariant is meaningless.)

- [ ] **Step 3: Create the shared helpers file**

Create `internal/controller/shared_helpers.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package controller

// shared_helpers.go holds package-level helpers extracted from the
// per-controller copy-paste (DRY consolidation, finding #14). Each helper
// is behavior-identical to the inline code it replaces.
```

- [ ] **Step 4: Commit the scaffold**

```bash
git add internal/controller/shared_helpers.go
git commit -m "refactor(controller): add shared_helpers.go scaffold"
```

---

## Phase A: Typed 4xx classification (replaces string-prefix scanners)

Replace `is4xxError`, the inline `for code := 400; code < 500` loops, and any string-prefix `is4xxNon401Status`/`isTransientLiteLLMError` internals with `errors.As(err, &litellm.RejectedError)` on the typed `Status`. This is both DRY and a latent-correctness fix (string-prefix breaks if the error is ever wrapped).

### Task A.1: Add typed predicates to `shared_helpers.go`

- [ ] **Step 0: Re-read the current predicates**

Run: `grep -n "func is4xxError\|func isTransientLiteLLMError\|func is4xxNon401Status" internal/controller/*.go` and read each body (note exactly what each returns, especially `isTransientLiteLLMError`'s "true on non-4xx" semantics and `is4xxNon401Status`'s `wantStatus` arg).

- [ ] **Step 1: Add the typed helpers**

Append to `internal/controller/shared_helpers.go`:

```go
import (
	"errors"

	"github.com/ackstorm/alitellm-operator/internal/litellm"
)

// rejectedStatus extracts the HTTP status of a *litellm.RejectedError, or 0
// if err is not a RejectedError (nil, transport/5xx, or Auth401Error).
func rejectedStatus(err error) int {
	var rej *litellm.RejectedError
	if errors.As(err, &rej) {
		return rej.Status
	}
	return 0
}

// is4xxStatus reports whether err is a deterministic LiteLLM 4xx rejection.
// Uses the typed RejectedError.Status (errors.As-based) so it survives error
// wrapping — unlike the legacy error-string prefix scan.
func is4xxStatus(err error) bool {
	s := rejectedStatus(err)
	return s >= 400 && s < 500
}
```

- [ ] **Step 2: Verify it compiles**

Run: `make build-operator`
Expected: success.

- [ ] **Step 3: Add focused unit tests** in `internal/controller/shared_helpers_test.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"errors"
	"fmt"
	"testing"

	"github.com/ackstorm/alitellm-operator/internal/litellm"
)

func TestIs4xxStatus_TypedAndWrapped(t *testing.T) {
	base := &litellm.RejectedError{Status: 422, Method: "POST", Path: "/model/new"}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"bare 422", base, true},
		{"wrapped 422", fmt.Errorf("context: %w", base), true},
		{"bare 404", &litellm.RejectedError{Status: 404}, true},
		{"500 not 4xx", &litellm.RejectedError{Status: 500}, false},
		{"plain error", errors.New("boom"), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := is4xxStatus(tc.err); got != tc.want {
				t.Errorf("is4xxStatus(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
```

Run: `make test-unit-pkg PKG=./internal/controller/...` → Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/controller/shared_helpers.go internal/controller/shared_helpers_test.go
git commit -m "refactor(controller): add typed is4xxStatus helper (errors.As on RejectedError.Status)"
```

### Task A.2: Migrate callers to `is4xxStatus`, delete the duplicates

- [ ] **Step 0: List every call site**

Run: `grep -rn "is4xxError\|isTransientLiteLLMError\|is4xxNon401Status\|code := 400" internal/controller/`

- [ ] **Step 1: Replace `is4xxError(...)` calls with `is4xxStatus(...)`** in every file that uses it (guardrail + the model/mcp/a2a deletion 4xx cases the bug-fix branch added). Then **delete** the `is4xxError` definition (guardrail file ~713).

- [ ] **Step 2: Rewrite `isTransientLiteLLMError`** (team file) to `return err != nil && !is4xxStatus(err)` (preserving its "Auth401 handled by caller first" contract — verify the call sites still gate Auth401 before calling it).

- [ ] **Step 3: Rewrite `is4xxNon401Status(err, wantStatus)`** (team file) to use `rejectedStatus(err) == wantStatus` (it already conceptually wants a specific status; confirm by reading its callers).

- [ ] **Step 4: Replace the inline `for code := 400; code < 500` loops** inside the mcp/a2a/team `classifyMutationError` copies with `is4xxStatus(err)`.

- [ ] **Step 5: Safety net**

Run: `make test-full`
Expected: PASS, no test edits. (If a `-shuffle` flake appears, it is pre-existing #74, not this refactor — re-run; confirm on CI.)

- [ ] **Step 6: Commit**

```bash
git add internal/controller/
git commit -m "refactor(controller): replace string-prefix 4xx scanners with typed is4xxStatus"
```

---

## Phase B: Shared `onAckMissing` factory

The 5 deletion-path closures are byte-identical modulo the kind constant + object pointer (for the Event recorder + metrics). Extract a factory.

### Task B.1: Add `newAckMissingFn` to `shared_helpers.go`

- [ ] **Step 0: Read all 5 closures**

Run: `grep -n "onAckMissing := func" internal/controller/*.go` and read each (confirm they only differ in `kind`, `obj` for `Recorder.Eventf`, and the `metrics.*` kind label).

- [ ] **Step 1: Add the factory**

```go
import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ackstorm/alitellm-operator/internal/controller/deletionpolicy"
	"github.com/ackstorm/alitellm-operator/internal/metrics"
)

// newAckMissingFn builds the deletion-path "ack-missing" handler shared by
// all controllers. Under Delete it records DeletionBlocked, emits a Warning
// Event, and returns a non-nil error (finalizer retained); under Orphan it
// records DeletionOrphanedTotal, emits a Normal Event, and returns nil
// (caller drains the finalizer). Behavior-identical to the inline closures.
func newAckMissingFn(
	rec record.EventRecorder,
	obj client.Object,
	kind, namespace, name string,
	policy deletionpolicy.Policy, // match the actual type returned by deletionpolicy.Resolve
) func(reason string) error {
	return func(reason string) error {
		if policy == deletionpolicy.Delete {
			metrics.DeletionBlocked.Record(kind, namespace, name)
			rec.Eventf(obj, corev1.EventTypeWarning, "LiteLLMDeleteBlocked",
				"deletionPolicy=Delete and LiteLLM ack missing (%s); finalizer retained", reason)
			return fmt.Errorf("delete blocked: %s", reason)
		}
		metrics.DeletionOrphanedTotal.WithLabelValues(kind).Inc()
		rec.Eventf(obj, corev1.EventTypeNormal, "LiteLLMDeleteOrphaned",
			"deletionPolicy=Orphan and LiteLLM ack missing (%s); finalizer removed; entry may persist", reason)
		return nil
	}
}
```

(Reconcile the exact `deletionpolicy.Policy` type name + `metrics.DeletionBlocked.Record` signature against the source — copy the inline closure verbatim, only parameterizing `kind`/`obj`/`namespace`/`name`/`policy`.)

- [ ] **Step 2: Compile**

Run: `make build-operator` → Expected: success.

- [ ] **Step 3: Migrate each controller** — replace the inline `onAckMissing := func(reason string) error {...}` with:

```go
onAckMissing := newAckMissingFn(r.Recorder, &model, modelKind, model.Namespace, model.Name, policy)
```

(Substitute the correct receiver field for the recorder — `r.Recorder` — the CR pointer, and the kind constant per controller.) Do all 5.

- [ ] **Step 4: Safety net**

Run: `make test-full` → Expected: PASS, no test edits.

- [ ] **Step 5: Commit**

```bash
git add internal/controller/
git commit -m "refactor(controller): extract shared onAckMissing factory"
```

---

## Phase C: Shared `classifyMutationError`

The 4 non-guardrail copies share the same 3-branch logic (Auth401 → InvalidateOn401 + writeStatus + nil; 4xx → LiteLLMRejected writeStatus + nil; transient → return err). The varying pieces: the CR object (for writeStatus + metrics kind) and the snapshot (for `NormalizedRequeueOnRejectedAfter`).

### Task C.1: Add a generic/shared classifier

- [ ] **Step 0: Diff the 5 copies**

Run: `for f in model mcpserver team a2aagent litellmguardrail; do echo "== $f =="; sed -n '/func.*classifyMutationError/,/^}/p' internal/controller/${f}_controller.go; done` and confirm the only differences are the concrete CR type, the `kind` label, and the writeStatus receiver.

- [ ] **Step 1: Design the shared signature**

Because `writeStatus` is a method on each reconciler with the same signature shape, pass it as a closure. Add to `shared_helpers.go`:

```go
import (
	"context"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/ackstorm/alitellm-operator/internal/connection"
)

// classifyMutationError is the shared LiteLLM-mutation error classifier.
// writeStatusFn is the caller's writeStatus method bound to its CR
// (func(ctx, status, reason, message) error). invalidate is r.Cache.InvalidateOn401.
// requeueAfter is snap.NormalizedRequeueOnRejectedAfter(). Returns the same
// (ctrl.Result, error) the inline copies returned.
func classifyMutationError(
	ctx context.Context,
	logger logr.Logger,
	err error,
	opDesc, kind string,
	writeStatusFn func(ctx context.Context, status metav1.ConditionStatus, reason, message string) error,
	invalidate func(),
	requeueAfter func() metav1.Duration, // or time.Duration — match NormalizedRequeueOnRejectedAfter's return
) (ctrl.Result, error) {
	// ... paste the model controller's classifyMutationError body verbatim,
	// replacing r.writeStatus(...) → writeStatusFn(...), r.Cache.InvalidateOn401()
	// → invalidate(), the kind constant → kind, and the requeue source →
	// requeueAfter(). Keep the metrics.* increments (parameterized by kind).
}
```

(Reconcile the exact return type of `NormalizedRequeueOnRejectedAfter` and the `metrics` calls against source. The body is a mechanical copy of the model controller's version with the 4 substitutions.)

- [ ] **Step 2: Compile** — `make build-operator` → success.

- [ ] **Step 3: Migrate each controller's method to delegate**

Replace each `func (r *XReconciler) classifyMutationError(...)` body with a one-line delegation:

```go
func (r *ModelReconciler) classifyMutationError(ctx context.Context, model *litellmv1alpha1.LiteLLMModel, logger logr.Logger, err error, opDesc string) (ctrl.Result, error) {
	snap := r.Cache.Snapshot()
	return classifyMutationError(ctx, logger, err, opDesc, modelKind,
		func(c context.Context, s metav1.ConditionStatus, reason, msg string) error { return r.writeStatus(c, model, s, reason, msg) },
		r.Cache.InvalidateOn401, snap.NormalizedRequeueOnRejectedAfter)
}
```

Keep the per-controller method as a thin shim (preserves all call sites). Do all 5. **Note:** the guardrail copy may differ slightly (read it) — if it does, either reconcile it into the shared helper via an option, or leave guardrail's as-is and migrate only the 4 identical ones (document the exception).

- [ ] **Step 4: Safety net** — `make test-full` → PASS, no test edits.

- [ ] **Step 5: Commit**

```bash
git add internal/controller/
git commit -m "refactor(controller): extract shared classifyMutationError; thin per-controller shims"
```

---

## Phase D: Shared SEC-03 duplicate-`as` check

### Task D.1: Extract `checkDuplicateSecretAs`

- [ ] **Step 0: Read the 4 blocks**

Run: `grep -n "duplicate as value\|SEC-03" internal/controller/*.go` and read each (they iterate `spec.secrets`, build a `map[string]struct{}`, return an InvalidConfig message on the first dup).

- [ ] **Step 1: Add the helper** (the secrets element type is `litellmv1alpha1.SecretSubstitution` — confirm the field name `.As`):

```go
import litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"

// checkDuplicateSecretAs returns a non-empty InvalidConfig message if any two
// entries share an `as` value (SEC-03), else "".
func checkDuplicateSecretAs(secrets []litellmv1alpha1.SecretSubstitution, kind string) string {
	seen := make(map[string]struct{}, len(secrets))
	for _, e := range secrets {
		if _, dup := seen[e.As]; dup {
			return fmt.Sprintf("spec.secrets[]: duplicate as value %q (SEC-03: must be unique within a %s)", e.As, kind)
		}
		seen[e.As] = struct{}{}
	}
	return ""
}
```

- [ ] **Step 2: Migrate each call site** to:

```go
if msg := checkDuplicateSecretAs(model.Spec.Secrets, "LiteLLMModel"); msg != "" {
	if werr := r.writeStatus(ctx, &model, metav1.ConditionFalse, "InvalidConfig", msg); werr != nil {
		logStatusUpdateErr(logger, werr, "reason", "InvalidConfig")
	}
	metrics.ReconcileTotal.WithLabelValues(modelKind, "success").Inc()
	return ctrl.Result{}, nil
}
```

(Preserve each controller's exact post-detection action — match the existing message-kind string and metric label.)

- [ ] **Step 3: Safety net** — `make test-full` → PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/controller/
git commit -m "refactor(controller): extract shared SEC-03 duplicate-as check"
```

---

## Phase E: Shared secret-ref indexer logic

The 5 `IndexXSecretRefs` funcs share the extraction loop; only the concrete CR type + secrets accessor differ (guardrail reads `spec.guardrails[].secrets`, the others read `spec.secrets`). Keep the per-kind key CONSTANTS (guardrail's differs) and the per-kind `Index*` funcs (controller-runtime needs concrete `client.Object`), but route their bodies through one helper.

### Task E.1: Extract `secretRefNames`

- [ ] **Step 0: Read all 5 funcs**

Run: `grep -n "func Index.*SecretRefs" internal/controller/*.go` + read bodies.

- [ ] **Step 1: Add the helper**

```go
// secretRefNames returns the deduped SecretRef.Name values from a secrets
// slice, for field-indexer registration.
func secretRefNames(secrets []litellmv1alpha1.SecretSubstitution) []string {
	if len(secrets) == 0 {
		return nil
	}
	out := make([]string, 0, len(secrets))
	for _, s := range secrets {
		if s.SecretRef.Name != "" {
			out = append(out, s.SecretRef.Name)
		}
	}
	return out
}
```

(Confirm the field path `s.SecretRef.Name` matches the source; reconcile dedup behavior if the originals dedup.)

- [ ] **Step 2: Migrate each `Index*SecretRefs`** to a thin body, e.g.:

```go
func IndexModelSecretRefs(o client.Object) []string {
	m, ok := o.(*litellmv1alpha1.LiteLLMModel)
	if !ok {
		return nil
	}
	return secretRefNames(m.Spec.Secrets)
}
```

For guardrail, flatten its nested `spec.guardrails[].secrets` into a single slice before calling `secretRefNames` (or loop and append) — its shape differs, so its `Index*` func keeps the flattening but reuses `secretRefNames` per group. **Leave all 5 key CONSTANTS unchanged** (guardrail's path genuinely differs).

- [ ] **Step 3: Safety net** — `make test-full` → PASS (the indexer is exercised by the cross-namespace / secret-rotation tests).

- [ ] **Step 4: Commit**

```bash
git add internal/controller/
git commit -m "refactor(controller): share secret-ref indexer extraction via secretRefNames"
```

---

## Phase F: Generic `writeStatusWithRetry[T]` + standardize MCP/Team

Highest-risk phase — do it last among extractions. The 3 retry-based copies (model/a2a/guardrail) are identical modulo concrete type; mcp/team use a bare update. Standardizing mcp/team onto the retry variant is a behavior change (adds conflict-retry) — acceptable and an improvement, but call it out in the commit and verify the existing conflict-handling call sites still behave (they catch `IsConflict` and return nil; retry makes that path rarer, not different).

### Task F.1: Extract the generic helper

- [ ] **Step 0: Diff the 5 writeStatus bodies**

Run: `for f in model a2aagent litellmguardrail mcpserver team; do echo "== $f =="; sed -n '/func (r \*.*Reconciler) writeStatus/,/^}/p' internal/controller/${f}_controller.go; done`. Confirm the retry-3 share structure (capture desired lastRendered/observedGen, RetryOnConflict, re-Get fresh typed object, SetStatusCondition, Status().Update, propagate back, recordReconcileMetric). Note the exact extra state each propagates.

- [ ] **Step 1: Design**

If all writeStatus variants share the "set Ready condition + observedGen + record metric" core, add a generic:

```go
import "k8s.io/client-go/util/retry"

// writeStatusWithRetry applies a Ready condition to obj's status under
// RetryOnConflict (re-Get + re-apply on 409). getFresh returns a fresh typed
// copy; apply mutates that copy's status; update persists it. This is the
// shared core of the per-controller writeStatus methods.
func writeStatusWithRetry[T client.Object](
	ctx context.Context,
	c client.Client,
	key client.ObjectKey,
	fresh T,
	apply func(obj T),
) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := c.Get(ctx, key, fresh); err != nil {
			return err
		}
		apply(fresh)
		return c.Status().Update(ctx, fresh)
	})
}
```

**Caveat:** if the per-controller writeStatus methods carry meaningfully different extra state (e.g. `lastRendered` propagation back onto the caller's object, divergent metric labels), this generic may not cleanly absorb all 5. If so, scope this phase DOWN to: (a) keep the 5 methods, (b) standardize mcp/team to use `retry.RetryOnConflict` matching the other three's body (eliminating the bare-Update divergence), and (c) defer the full generic. The divergence fix is the high-value part; the generic is optional polish.

- [ ] **Step 2: Migrate** the 3 retry copies to delegate to `writeStatusWithRetry`, and convert mcp/team's bare update to the same. Keep each per-controller method as a typed shim that builds the `apply` closure (condition + observedGen + lastRendered) and calls `recordReconcileMetric` after.

- [ ] **Step 3: Safety net** — `make test-full` → PASS, no test edits. Pay attention to any test asserting status-write counts; a retry adds a re-Get, not an extra Update on the happy path.

- [ ] **Step 4: Commit**

```bash
git add internal/controller/
git commit -m "refactor(controller): standardize writeStatus on RetryOnConflict; share generic core"
```

---

## Phase G: Parameterize the safety-relist runnable

### Task G.1: Unify `ModelSafetyRelistRunnable` + `GuardRailSafetyRelistRunnable`

- [ ] **Step 0: Diff the two types**

Run: `sed -n '/type ModelSafetyRelistRunnable/,/^}/p' internal/controller/model_controller.go; sed -n '/type GuardRailSafetyRelistRunnable/,/^}/p' internal/controller/litellmguardrail_controller.go` and their `Start` methods. Confirm fields (`Client, Namespace, Interval, Log, RequeueCh`) and `Start` body are identical except the `client.ObjectList` type listed.

- [ ] **Step 1: Add a generic runnable** to `shared_helpers.go`:

```go
import (
	"time"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

// SafetyRelistRunnable periodically enqueues all CRs of one kind so the
// safety re-list detects out-of-band LiteLLM drift. listFn lists the CRs and
// returns their reconcile requests; the runnable pushes them onto RequeueCh
// every Interval until ctx is done.
type SafetyRelistRunnable struct {
	Interval  time.Duration
	Log       logr.Logger
	RequeueCh chan<- event.GenericEvent
	Enqueue   func(ctx context.Context) // lists + sends; built per-kind by the caller
}

func (s *SafetyRelistRunnable) Start(ctx context.Context) error {
	t := time.NewTicker(s.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			s.Enqueue(ctx)
		}
	}
}
```

(Reconcile against the ACTUAL `Start` bodies — they may push `event.GenericEvent`s built from a list. Capture the list+send as the `Enqueue` closure so the generic holds no kind-specific types. If the real bodies differ from this sketch, mirror them exactly.)

- [ ] **Step 2: Replace both concrete runnables** with `SafetyRelistRunnable` instances wired in each `SetupWithManager` (build the `Enqueue` closure that lists the kind and sends requeues). Delete `ModelSafetyRelistRunnable` and `GuardRailSafetyRelistRunnable`.

- [ ] **Step 3: Safety net** — `make test-full` → PASS. (Safety-relist behavior is covered by the relist-recovery tests; confirm they pass unchanged.)

- [ ] **Step 4: Commit**

```bash
git add internal/controller/
git commit -m "refactor(controller): unify model/guardrail safety-relist runnables"
```

---

## Phase H: Final gates

### Task H: Full verification + push

- [ ] **Step 1: Generated-file sanity** — `make gen-code gen-manifests` → Expected: no diff.
- [ ] **Step 2: Lint** — `make qa-lint` → PASS.
- [ ] **Step 3: Full tests** — `make test-full` → PASS.
- [ ] **Step 4: E2E** — `make e2e-full` → PASS (controllers heavily touched).
- [ ] **Step 5: Security** — `make qa-security` → PASS.
- [ ] **Step 6: Update `CLAUDE.md`** — the "Repository-specific patterns" section claims per-controller copies of these helpers; update it to point at `shared_helpers.go` as the single source (documentation-hygiene rule). Commit doc edits.
- [ ] **Step 7: Push** — `git push -u origin refactor/controller-dry-consolidation` (pre-push gate runs). Open a PR to `main`. Keep it a **separate PR** from the bug-fix PR for clean review.

---

## Self-Review

**Coverage of finding #14's extraction targets:** classifyMutationError (Phase C), 4xx string-prefix scanners → typed (Phase A), onAckMissing (Phase B), SEC-03 dup-as (Phase D), IndexXSecretRefs (Phase E), writeStatus + mcp/team divergence (Phase F), SafetyRelistRunnable (Phase G). All listed targets mapped.

**Behavior-preservation guard:** every phase gates on `make test-full` green with NO test edits — the definition of a safe refactor. Phase F is flagged as the one with a deliberate (improving) behavior change (mcp/team gain conflict-retry) and is explicitly de-scopable to "fix the divergence only."

**Correctness caveats surfaced (not placeholders):**
- The secret-ref index CONSTANTS are NOT all identical — guardrail's path differs (`.spec.guardrails.secrets[*]...`). Phase E shares the extraction loop only, keeps the constants.
- The guardrail `classifyMutationError` and `writeStatus` may diverge from the other four; Phases C/F instruct reading them first and either reconciling via an option or documenting the exception rather than forcing a unification.
- Each phase opens with a `grep`/`sed` "Step 0" because the bug-fix branch shifted all line numbers — locate by symbol, not line.

**Type/name consistency:** `is4xxStatus`/`rejectedStatus`, `newAckMissingFn`, `classifyMutationError` (shared) vs the per-controller shims, `checkDuplicateSecretAs`, `secretRefNames`, `writeStatusWithRetry[T]`, `SafetyRelistRunnable` — each defined once in `shared_helpers.go` and referenced consistently. `litellm.RejectedError.Status` and `litellm.IsNotFound` confirmed present in source.
