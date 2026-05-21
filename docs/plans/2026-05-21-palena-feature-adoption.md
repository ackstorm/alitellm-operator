# Palena Feature Adoption — ClientFactory, Enterprise-License Classifier, Constants Consolidation

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Adopt three patterns from `github.com/PalenaAI/litellm-operator` into `alitellm-operator`: (1) a `ClientFactory` interface for `*litellm.Client` construction; (2) a typed `*litellm.APIError` plus `IsEnterpriseLicenseError` classifier surfaced as a `Ready=False reason=EnterpriseLicenseRequired` condition; (3) a single-source `internal/controller/common.go` for finalizer / condition-reason / annotation constants currently scattered across seven controllers.

**Architecture:** Phase 1 adds the constants file and migrates the seven controllers + connection-reconciler reason consts onto it (mechanical rename, no behavior change). Phase 2 introduces a typed `*APIError` returned by `Client.makeRequest` for non-401 4xx (Error() string is byte-identical to today's `fmt.Errorf` output so existing `is4xxNon401Status` prefix-matching keeps working); on top of it we add `IsEnterpriseLicenseError(err) bool` and a new `ReasonEnterpriseLicenseRequired` condition reason, wired into the four LiteLLM-writing reconcilers (Team, Model, MCPServer, A2AAgent). Phase 3 adds `internal/litellm/factory.go` with a `ClientFactory` interface + `DefaultFactory`, threaded through `LiteLLMConnectionReconciler` (the only `*Client` producer) so tests can inject behavioral fakes without standing up an httptest mock server.

**Tech Stack:** Go 1.24.13, controller-runtime v0.19.4, k8s.io/* v0.31.0, envtest, Ginkgo v2. Toolchain via `./scripts/dev.sh` (host has no Go).

---

## Branching + worktree

Run this on a fresh feature branch in an isolated worktree:

```bash
git fetch origin
git worktree add -b feat/palena-adoption ../alitellm-operator-palena-adoption origin/main
cd ../alitellm-operator-palena-adoption
```

All paths below are relative to repo root.

## Verification gates per phase

Before committing any task that touches controllers:
```bash
./scripts/dev.sh make unit
./scripts/dev.sh make envtest-fast
```
Before merging the branch:
```bash
./scripts/dev.sh make test-all
./scripts/dev.sh make security
make pre-push
```

---

## Phase 1 — Constants consolidation (item #16)

Centralizes 7 scattered finalizer constants and the connection-reconciler reason block into one file. Pure mechanical rename — no behavior change. **No new public API**, package-private constants only.

### Task 1.1: Create `internal/controller/common.go` skeleton

**Files:**
- Create: `internal/controller/common.go`

**Step 1: Write the file**

```go
// SPDX-License-Identifier: Apache-2.0

package controller

// This file is the single source of truth for cross-controller string
// constants — finalizer names, condition reasons, annotation/label keys
// — that were previously scattered across the per-kind controller files.
//
// Add new constants here, not in the kind-specific file, so alerting
// tooling and operators can grep one location for stable strings.

const (
	// Finalizers — one per Kind. Each kind owns its own finalizer string
	// so cross-Kind drift never wedges deletion; do NOT collapse into a
	// single shared finalizer.
	FinalizerConnection         = "litellm.ackstorm.ai/connection-cache-cleanup"
	FinalizerModel              = "models.litellm.ackstorm.ai/finalizer"
	FinalizerTeam               = "teams.litellm.ackstorm.ai/finalizer"
	FinalizerA2AAgent           = "a2aagents.litellm.ackstorm.ai/finalizer"
	FinalizerMCPServer          = "mcpservers.litellm.ackstorm.ai/finalizer"
	FinalizerModelDiscovery     = "modeldiscoveries.litellm.ackstorm.ai/finalizer"
	FinalizerMCPServerDiscovery = "mcpserverdiscoveries.litellm.ackstorm.ai/finalizer"

	// Condition reason strings emitted by LiteLLMConnection reconciler.
	// Other reconcilers SetStatusCondition with type=Ready and copy these
	// reason values so alert routes can filter on a stable set.
	ReasonSynced             = "Synced"
	ReasonConnecting         = "Connecting"
	ReasonAbsent             = "Absent"
	ReasonUnreachable        = "Unreachable"
	ReasonBadMasterKey       = "BadMasterKey"
	ReasonSecretNotFound     = "SecretNotFound"
	ReasonLiteLLMUnavailable = "LiteLLMUnavailable"

	// Reason added in Phase 2 (kept in this block for cohesion even though
	// the wiring lands later — declaring it now prevents a churn commit
	// later that touches common.go again).
	ReasonEnterpriseLicenseRequired = "EnterpriseLicenseRequired"

	// Label keys for resources created by discovery controllers.
	LabelGeneratedBy = "litellm.ackstorm.ai/generated-by"
)
```

**Step 2: Verify it compiles**

Run: `./scripts/dev.sh go build ./internal/controller/...`
Expected: clean exit (no callers yet).

**Step 3: Commit**

```bash
git add internal/controller/common.go
git commit -m "feat(controller): add common.go constants file (no callers yet)"
```

### Task 1.2: Migrate `litellmconnection_controller.go` to use common consts

**Files:**
- Modify: `internal/controller/litellmconnection_controller.go:41-52`

**Step 1: Delete the local `connectionFinalizer` const at L41 and the seven `reason*` consts in the block starting L45**

Replace this:
```go
const connectionFinalizer = "litellm.ackstorm.ai/connection-cache-cleanup"
```
…and the entire `const ( reasonSynced … reasonLiteLLMUnavailable )` block, with nothing (delete it). The constants now live in `common.go`.

**Step 2: Run grep to find every in-file reference, then rename**

```bash
grep -n "connectionFinalizer\|reasonSynced\|reasonConnecting\|reasonAbsent\|reasonUnreachable\|reasonBadMasterKey\|reasonSecretNotFound\|reasonLiteLLMUnavailable" internal/controller/litellmconnection_controller.go internal/controller/litellmconnection_controller_test.go internal/controller/litellmconnection_finalizer_test.go internal/controller/litellmconnection_fastpath_test.go internal/controller/litellmconnection_proxy_test.go internal/controller/fastpath_test.go
```

Rename each match per the mapping:
- `connectionFinalizer` → `FinalizerConnection`
- `reasonSynced` → `ReasonSynced`
- `reasonConnecting` → `ReasonConnecting`
- `reasonAbsent` → `ReasonAbsent`
- `reasonUnreachable` → `ReasonUnreachable`
- `reasonBadMasterKey` → `ReasonBadMasterKey`
- `reasonSecretNotFound` → `ReasonSecretNotFound`
- `reasonLiteLLMUnavailable` → `ReasonLiteLLMUnavailable`

Use Edit `replace_all` per identifier inside each file (one Edit per identifier per file).

**Step 3: Build**

Run: `./scripts/dev.sh go build ./...`
Expected: clean.

**Step 4: Run unit + envtest focused on connection**

Run:
```bash
./scripts/dev.sh make envtest-pkg PKG=./internal/controller/... FOCUS="LiteLLMConnection|FastPath"
```
Expected: green.

**Step 5: Commit**

```bash
git add -u internal/controller
git commit -m "refactor(controller): migrate connection reconciler reasons + finalizer to common.go"
```

### Task 1.3: Migrate Model reconciler finalizer

**Files:**
- Modify: `internal/controller/model_controller.go:43` (delete local const)
- Modify: `internal/controller/model_controller.go` (rename all `modelFinalizer` → `FinalizerModel`)
- Modify: `internal/controller/model_controller_test.go`, `internal/controller/model_ac_n3_test.go`, `internal/controller/idempotency_test.go`, `internal/controller/idempotency_long_test.go`, `internal/controller/leak_test.go`, `internal/controller/scope_ac_n4_test.go`, `internal/controller/metrics_scrape_test.go` (rename references)

**Step 1: Find every reference**

```bash
grep -rn "modelFinalizer" internal/controller/
```

**Step 2: Rename with Edit `replace_all`** per file. Delete the `const modelFinalizer = ...` line in `model_controller.go:43`.

**Step 3: Build + targeted envtest**

```bash
./scripts/dev.sh go build ./...
./scripts/dev.sh make envtest-pkg PKG=./internal/controller/... FOCUS="ModelReconciler"
```
Expected: green.

**Step 4: Commit**

```bash
git add -u internal/controller
git commit -m "refactor(controller): migrate Model finalizer to common.go"
```

### Task 1.4: Migrate Team reconciler finalizer

**Files:**
- Modify: `internal/controller/team_controller.go:45` (delete local const) + all refs
- Modify: `internal/controller/team_controller_test.go`, `internal/controller/team_default_runnable.go`, `internal/controller/team_default_runnable_test.go`, `internal/controller/team_hubseam_test.go`

**Step 1:** `grep -rn "teamFinalizer" internal/controller/`
**Step 2:** Edit `replace_all` `teamFinalizer` → `FinalizerTeam` in each file. Delete `const teamFinalizer = ...` at L45.
**Step 3:** Build + envtest focus `TeamReconciler`.
**Step 4:** Commit `refactor(controller): migrate Team finalizer to common.go`.

### Task 1.5: Migrate A2AAgent reconciler finalizer

**Files:**
- Modify: `internal/controller/a2aagent_controller.go:40` + `internal/controller/a2aagent_controller_test.go`, `internal/controller/a2aagent_ac_n3_test.go`

**Step 1:** `grep -rn "a2aAgentFinalizer" internal/controller/`
**Step 2:** Edit `replace_all` `a2aAgentFinalizer` → `FinalizerA2AAgent`. Delete the `const` line.
**Step 3:** Build + envtest focus `A2AAgentReconciler`.
**Step 4:** Commit.

### Task 1.6: Migrate MCPServer reconciler finalizer

**Files:**
- Modify: `internal/controller/mcpserver_controller.go:40` + `mcpserver_controller_test.go`, `mcpserver_ac_n3_test.go`

**Step 1:** `grep -rn "mcpServerFinalizer" internal/controller/`
**Step 2:** Edit `replace_all` `mcpServerFinalizer` → `FinalizerMCPServer`. Delete the `const` line.
**Step 3:** Build + envtest focus `MCPServerReconciler`.
**Step 4:** Commit.

### Task 1.7: Migrate ModelDiscovery + MCPServerDiscovery finalizers

**Files:**
- Modify: `internal/controller/modeldiscovery_controller.go:89` + `modeldiscovery_controller_test.go`, `modeldiscovery_ac_n3_test.go`
- Modify: `internal/controller/mcpserverdiscovery_controller.go:85` + `mcpserverdiscovery_controller_test.go`, `mcpserverdiscovery_ac_n3_test.go`

**Step 1:** `grep -rn "modelDiscoveryFinalizer\|mcpServerDiscoveryFinalizer" internal/controller/`
**Step 2:** Edit `replace_all` `modelDiscoveryFinalizer` → `FinalizerModelDiscovery` and `mcpServerDiscoveryFinalizer` → `FinalizerMCPServerDiscovery`. Delete the `const` lines.
**Step 3:** Build + envtest focus `ModelDiscovery|MCPServerDiscovery`.
**Step 4:** Commit `refactor(controller): migrate Discovery finalizers to common.go`.

### Task 1.8: Phase 1 gate

**Step 1:** Full unit + envtest:
```bash
./scripts/dev.sh make test-all
```
Expected: all green.

**Step 2:** Confirm no stale local consts:
```bash
grep -rn "Finalizer\s*=\s*\"\|^\s*reason[A-Z][a-zA-Z]*\s*=\s*\"" internal/controller/ | grep -v common.go
```
Expected: zero hits (other than incidental matches in struct tags / strings).

No commit (verification only).

---

## Phase 2 — Enterprise-license classifier (item #19)

Add a typed `*litellm.APIError` returned for non-401 4xx responses (parallel to existing `*Auth401Error`), keeping `Error()` byte-identical to the current `fmt.Errorf` output so callers using `is4xxNon401Status` prefix matching keep working. Layer `IsEnterpriseLicenseError(err) bool` on top, then wire it into the four LiteLLM-writing reconcilers.

### Task 2.1: Write failing test for `APIError.Error()` byte-compat

**Files:**
- Create: `internal/litellm/errors_apierror_test.go`

**Step 1: Write the test**

```go
// SPDX-License-Identifier: Apache-2.0

package litellm

import (
	"errors"
	"strings"
	"testing"
)

// TestAPIError_StringFormat_BackwardCompat asserts the new *APIError
// formats identically to the historical fmt.Errorf format produced by
// makeRequest's 4xx branch. Several reconciler call sites (notably
// internal/controller/team_controller.go::is4xxNon401Status) prefix-
// match on that string; APIError must preserve byte-for-byte format.
func TestAPIError_StringFormat_BackwardCompat(t *testing.T) {
	cases := []struct {
		name string
		e    *APIError
		want string
	}{
		{
			name: "403 known code",
			e:    &APIError{StatusCode: 403, Method: "POST", Path: "/team/new", Code: "403", Transient: false},
			want: "litellm: 403 on POST /team/new (code=403)",
		},
		{
			name: "5xx transient suffix",
			e:    &APIError{StatusCode: 503, Method: "GET", Path: "/health", Code: "503", Transient: true},
			want: "litellm: 503 on GET /health (code=503, transient)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.e.Error(); got != tc.want {
				t.Errorf("want %q, got %q", tc.want, got)
			}
		})
	}
}

// TestAPIError_ErrorsAs asserts callers can errors.As to *APIError.
func TestAPIError_ErrorsAs(t *testing.T) {
	var src error = &APIError{StatusCode: 403}
	var dst *APIError
	if !errors.As(src, &dst) {
		t.Fatalf("errors.As to *APIError failed")
	}
	if dst.StatusCode != 403 {
		t.Errorf("StatusCode mismatch")
	}
	_ = strings.Contains // keep import live
}
```

**Step 2: Run, confirm failure**

Run: `./scripts/dev.sh go test ./internal/litellm/ -run TestAPIError -v`
Expected: FAIL — `APIError` type undefined.

### Task 2.2: Add `APIError` type to `errors.go`

**Files:**
- Modify: `internal/litellm/errors.go`

**Step 1: Append the type below the existing `Auth401Error` block**

```go
// APIError is the typed error Client.makeRequest returns on any non-2xx,
// non-401 HTTP response. It parallels *Auth401Error: callers can use
// errors.As(err, &apiErr) to inspect StatusCode and parsed envelope
// fields (Code, Message) without string parsing.
//
// The Error() string is byte-identical to the format produced by the
// historical fmt.Errorf path in makeRequest (4xx: "litellm: %d on %s %s
// (code=%s)"; 5xx/other: "...(code=%s, transient)"). Several controller
// helpers (e.g. team_controller.go::is4xxNon401Status) prefix-match on
// that exact format — do NOT change Error() output without auditing
// every prefix-match call site.
type APIError struct {
	StatusCode int
	Method     string
	Path       string
	// Code is the parsed `code` field from the LiteLLM error envelope
	// (or the raw status code as a string if the envelope was unparsed).
	Code string
	// Message is the parsed `message` field from the LiteLLM error
	// envelope. Empty when the body was unparseable. Reconcilers
	// surface this to status condition messages — keep it short and
	// quote-stable.
	Message string
	// Body is the raw response body (capped to 4 KiB by the caller) for
	// classifier helpers like IsEnterpriseLicenseError that need to
	// substring-search the body. NOT logged in Error() — §9.1.
	Body []byte
	// Transient is true if the caller's classify() mapped this to
	// KindTransient (5xx / network). 4xx-non-401 → false.
	Transient bool
}

// Error implements the error interface. Format is byte-identical to the
// historical fmt.Errorf strings — see type doc comment.
func (e *APIError) Error() string {
	if e.Transient {
		return fmt.Sprintf("litellm: %d on %s %s (code=%s, transient)", e.StatusCode, e.Method, e.Path, e.Code)
	}
	return fmt.Sprintf("litellm: %d on %s %s (code=%s)", e.StatusCode, e.Method, e.Path, e.Code)
}

// IsEnterpriseLicenseError returns true when err (or any error wrapped
// by it) is a *APIError with StatusCode==403 and a body containing the
// substring "enterprise" (case-insensitive). LiteLLM 1.83.10 OSS returns
// HTTP 403 on enterprise-only fields (tags, etc.) with a body like
// `{"error":{"message":"... Enterprise users only ..."}}`. Reconcilers
// branch on this to surface a stable reason
// (ReasonEnterpriseLicenseRequired) instead of bubbling a raw error.
func IsEnterpriseLicenseError(err error) bool {
	if err == nil {
		return false
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	if apiErr.StatusCode != http.StatusForbidden {
		return false
	}
	hay := strings.ToLower(string(apiErr.Body) + " " + apiErr.Message)
	return strings.Contains(hay, "enterprise")
}
```

**Step 2: Add `"strings"` to the import block** of `errors.go`.

**Step 3: Run the test from Task 2.1**

Run: `./scripts/dev.sh go test ./internal/litellm/ -run TestAPIError -v`
Expected: PASS.

**Step 4: Commit**

```bash
git add internal/litellm/errors.go internal/litellm/errors_apierror_test.go
git commit -m "feat(litellm): add typed APIError + IsEnterpriseLicenseError classifier"
```

### Task 2.3: Wire `makeRequest` to return `*APIError`

**Files:**
- Modify: `internal/litellm/client.go:146-160` (the 4xx and 5xx branches)

**Step 1: Write a failing test asserting `errors.As(*APIError)` resolves for a 403**

Create: `internal/litellm/client_apierror_test.go`

```go
// SPDX-License-Identifier: Apache-2.0

package litellm

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-logr/logr"
)

func TestMakeRequest_403_ReturnsAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"message":"Enterprise users only","code":"403","type":"enterprise"}}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "sk-test", logr.Discard())
	_, err := c.makeRequest(context.Background(), http.MethodPost, "/team/new", map[string]string{"x": "y"})
	if err == nil {
		t.Fatalf("want error on 403, got nil")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want errors.As to resolve *APIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != 403 {
		t.Errorf("StatusCode = %d, want 403", apiErr.StatusCode)
	}
	if !IsEnterpriseLicenseError(err) {
		t.Errorf("IsEnterpriseLicenseError = false, want true")
	}
}

func TestMakeRequest_503_ReturnsAPIErrorTransient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "sk-test", logr.Discard())
	_, err := c.makeRequest(context.Background(), http.MethodGet, "/health", nil)
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want *APIError on 5xx, got %T: %v", err, err)
	}
	if !apiErr.Transient {
		t.Errorf("Transient = false, want true on 5xx")
	}
}
```

**Step 2: Run, confirm failure**

Run: `./scripts/dev.sh go test ./internal/litellm/ -run TestMakeRequest_403_ReturnsAPIError -v`
Expected: FAIL — current makeRequest returns `fmt.Errorf`, not `*APIError`.

**Step 3: Modify `client.go` 4xx + 5xx branches**

Replace the body of the `resp.StatusCode >= 400 && resp.StatusCode < 500` branch (currently at `internal/litellm/client.go:146-151`) with:

```go
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		_, msg, code := processLitellmError(respBody)
		if code == "" {
			code = fmt.Sprintf("%d", resp.StatusCode)
		}
		return nil, &APIError{
			StatusCode: resp.StatusCode,
			Method:     method,
			Path:       path,
			Code:       code,
			Message:    msg,
			Body:       respBody,
			Transient:  false,
		}
```

Replace the default / 5xx branch (currently `internal/litellm/client.go:153-159`) with:

```go
	default:
		_, msg, code := processLitellmError(respBody)
		if code == "" {
			code = fmt.Sprintf("%d", resp.StatusCode)
		}
		return nil, &APIError{
			StatusCode: resp.StatusCode,
			Method:     method,
			Path:       path,
			Code:       code,
			Message:    msg,
			Body:       respBody,
			Transient:  true,
		}
```

**Step 4: Run the new test + the existing client + transport tests**

```bash
./scripts/dev.sh go test ./internal/litellm/ -v
```
Expected: all green. **Pay particular attention** to `internal/litellm/model_test.go` and `internal/litellm/client_test.go` — they assert on error string formats; the byte-identical `Error()` should keep them green. If any fail, the format differs — fix `APIError.Error()` before changing tests.

**Step 5: Run controller tests touching the prefix-match path**

```bash
./scripts/dev.sh make envtest-pkg PKG=./internal/controller/... FOCUS="TeamReconciler"
```
Expected: green — `is4xxNon401Status` should still match because the error string format is preserved.

**Step 6: Commit**

```bash
git add internal/litellm/client.go internal/litellm/client_apierror_test.go
git commit -m "refactor(litellm): return typed *APIError from makeRequest 4xx/5xx"
```

### Task 2.4: Wire `IsEnterpriseLicenseError` into TeamReconciler

**Files:**
- Modify: `internal/controller/team_controller.go` (locations where `Eventf(...,"LiteLLMRejected"...)` or 4xx-classification happen)

**Step 1: Locate every 4xx-handling branch in TeamReconciler**

Run:
```bash
grep -n "is4xxNon401Status\|LiteLLMRejected\|classifyMutationError" internal/controller/team_controller.go
```

For each branch that classifies a 4xx as "LiteLLMRejected" / writes Ready=False, add an early check:

```go
	if litellm.IsEnterpriseLicenseError(err) {
		writeStatus(... ReasonEnterpriseLicenseRequired, err.Error() ...)
		emitEvent(r.Recorder, &team, corev1.EventTypeWarning,
			ReasonEnterpriseLicenseRequired, "LiteLLM rejected request: %s", err.Error())
		// Terminal — do NOT requeue (mirrors existing 4xx-permanent semantics).
		return ctrl.Result{}, nil
	}
```

(Adapt to the local naming of the writeStatus helper and the local CR variable. Inspect each call site to confirm the right placement.)

**Step 2: Write a failing envtest exercising the 403-enterprise path**

Create or append to `internal/controller/team_controller_test.go` a new Ginkgo `It`:

```go
It("surfaces ReasonEnterpriseLicenseRequired when LiteLLM returns 403 enterprise on POST /team/new", func() {
	mockServer.RegisterTeamCreate(func(_ map[string]any) (int, string) {
		return http.StatusForbidden, `{"error":{"message":"Enterprise users only","code":"403","type":"enterprise"}}`
	})
	team := &v1alpha1.LiteLLMTeam{ /* spec triggering create */ }
	Expect(k8sClient.Create(ctx, team)).To(Succeed())
	Eventually(func(g Gomega) {
		var got v1alpha1.LiteLLMTeam
		g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(team), &got)).To(Succeed())
		cond := meta.FindStatusCondition(got.Status.Conditions, "Ready")
		g.Expect(cond).NotTo(BeNil())
		g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(cond.Reason).To(Equal(ReasonEnterpriseLicenseRequired))
	}, "10s", "200ms").Should(Succeed())
})
```

(Adapt the mock-server hook signature to whatever `internal/litellm/mock/mock.go` exposes; if `RegisterTeamCreate` doesn't exist, add a one-shot hook there or use an existing test-double pattern in the package.)

**Step 3: Run the test**

Run:
```bash
./scripts/dev.sh make envtest-pkg PKG=./internal/controller/... FOCUS="ReasonEnterpriseLicenseRequired"
```
Expected: FAIL until the controller branch lands; PASS after.

**Step 4: Commit**

```bash
git add internal/controller/team_controller.go internal/controller/team_controller_test.go internal/litellm/mock/mock.go
git commit -m "feat(controller/team): surface ReasonEnterpriseLicenseRequired on 403 enterprise"
```

### Task 2.5: Wire `IsEnterpriseLicenseError` into Model, MCPServer, A2AAgent

Repeat the Task 2.4 pattern for each of the three reconcilers. One commit per reconciler so blame stays clean.

**Files (per kind):**
- `internal/controller/model_controller.go` + `internal/controller/model_controller_test.go`
- `internal/controller/mcpserver_controller.go` + `internal/controller/mcpserver_controller_test.go`
- `internal/controller/a2aagent_controller.go` + `internal/controller/a2aagent_controller_test.go`

**Step 1 (per kind):** Find the 4xx-classification branch in the reconciler.
**Step 2:** Insert the `if litellm.IsEnterpriseLicenseError(err) { ... ReasonEnterpriseLicenseRequired ... }` early branch.
**Step 3:** Add a Ginkgo test driving the kind's POST/UPDATE through a 403-enterprise mock response, asserting the condition reason.
**Step 4:** Run focused envtest, expect green.
**Step 5:** Commit `feat(controller/<kind>): surface ReasonEnterpriseLicenseRequired on 403 enterprise`.

### Task 2.6: Phase 2 gate

**Step 1:** Full test-all + security:
```bash
./scripts/dev.sh make test-all
./scripts/dev.sh make security
```
Expected: green. govulncheck must still ack-list match (no new advisories).

No commit.

---

## Phase 3 — ClientFactory interface (item #21)

Only one production call site builds a `*litellm.Client` (`litellmconnection_controller.go:303`). Factory lives on `LiteLLMConnectionReconciler`; nil-coalesces to `litellm.DefaultFactory`. Tests inject behavioral fakes without spinning up an httptest server.

### Task 3.1: Create `internal/litellm/factory.go`

**Files:**
- Create: `internal/litellm/factory.go`

**Step 1: Write the file**

```go
// SPDX-License-Identifier: Apache-2.0

package litellm

import (
	"github.com/go-logr/logr"
)

// ClientFactory abstracts construction of *Client so reconcilers can be
// unit-tested with a behavioral fake without standing up an httptest
// mock server. The interface is intentionally narrow — one method,
// matching NewClient's signature — so we can add it on existing
// reconciler structs as an optional field with nil-coalesce to
// DefaultFactory.
//
// Invariant: implementations MUST NOT pool *Client instances. The
// LiteLLMConnection reconciler relies on a fresh *Client per Rebuild
// (D-03 — see internal/connection/cache.go); a pooling factory would
// re-export a stale RoundTripper across master-key rotations.
type ClientFactory interface {
	New(endpoint, masterKey string, log logr.Logger) *Client
}

// defaultClientFactory is the production implementation: a thin wrapper
// around NewClient. Stateless — exported as the package-level
// DefaultFactory singleton.
type defaultClientFactory struct{}

func (defaultClientFactory) New(endpoint, masterKey string, log logr.Logger) *Client {
	return NewClient(endpoint, masterKey, log)
}

// DefaultFactory is the production-default ClientFactory. Reconcilers
// nil-coalesce their LiteLLMClientFactory field to this value at the
// call site, so the field can be left zero in cmd/main.go.
var DefaultFactory ClientFactory = defaultClientFactory{}
```

**Step 2: Write a failing test exercising the factory interface**

Create: `internal/litellm/factory_test.go`

```go
// SPDX-License-Identifier: Apache-2.0

package litellm

import (
	"testing"

	"github.com/go-logr/logr"
)

func TestDefaultFactory_New_ProducesNonNilClient(t *testing.T) {
	c := DefaultFactory.New("http://localhost:4000", "sk-x", logr.Discard())
	if c == nil {
		t.Fatalf("DefaultFactory.New returned nil")
	}
}

// recordingFactory is a behavioral test-double: it records the args
// every reconciler call uses and returns a *Client wired to a stub
// transport. Lets the LiteLLMConnection reconciler test assert which
// (endpoint, masterKey) pair was used per probe.
type recordingFactory struct {
	Calls []struct {
		Endpoint  string
		MasterKey string
	}
}

func (f *recordingFactory) New(endpoint, masterKey string, log logr.Logger) *Client {
	f.Calls = append(f.Calls, struct {
		Endpoint  string
		MasterKey string
	}{endpoint, masterKey})
	return NewClient(endpoint, masterKey, log)
}

func TestRecordingFactory_RecordsCalls(t *testing.T) {
	f := &recordingFactory{}
	_ = f.New("http://a", "sk-1", logr.Discard())
	_ = f.New("http://b", "sk-2", logr.Discard())
	if len(f.Calls) != 2 {
		t.Fatalf("Calls = %d, want 2", len(f.Calls))
	}
	if f.Calls[1].Endpoint != "http://b" {
		t.Errorf("Calls[1].Endpoint = %q, want http://b", f.Calls[1].Endpoint)
	}
}
```

**Step 3: Run**

Run: `./scripts/dev.sh go test ./internal/litellm/ -run TestDefaultFactory -v -count=1`
Expected: green.

**Step 4: Commit**

```bash
git add internal/litellm/factory.go internal/litellm/factory_test.go
git commit -m "feat(litellm): add ClientFactory interface + DefaultFactory"
```

### Task 3.2: Thread `LiteLLMClientFactory` field through `LiteLLMConnectionReconciler`

**Files:**
- Modify: `internal/controller/litellmconnection_controller.go:106` (struct) + `:303` (call site)

**Step 1: Add field to the struct definition**

Locate `type LiteLLMConnectionReconciler struct` at L106. Append below the existing recorder field:

```go
	// LiteLLMClientFactory constructs *litellm.Client instances for the
	// probe path. Nil → use litellm.DefaultFactory (production default).
	// Unit tests inject a recording / behavioral fake to assert which
	// (endpoint, masterKey) pair each Rebuild used without standing up
	// an httptest mock server.
	LiteLLMClientFactory litellm.ClientFactory
```

**Step 2: Modify the call site at L303**

Replace:
```go
liteLLMClient := litellm.NewClient(conn.Spec.Endpoint, string(masterKeyValue), r.Log.WithName("probe"))
```
…with:
```go
factory := r.LiteLLMClientFactory
if factory == nil {
	factory = litellm.DefaultFactory
}
liteLLMClient := factory.New(conn.Spec.Endpoint, string(masterKeyValue), r.Log.WithName("probe"))
```

**Step 3: Write a failing test that injects a recording factory**

Append to `internal/controller/litellmconnection_controller_test.go` (or a new file `litellmconnection_factory_test.go`):

```go
// TestLiteLLMConnectionReconciler_UsesInjectedClientFactory asserts the
// reconciler routes *Client construction through the injected
// ClientFactory rather than calling litellm.NewClient directly. Drives
// one Rebuild via a successful probe and asserts the factory recorded
// the expected (endpoint, masterKey) pair.
func TestLiteLLMConnectionReconciler_UsesInjectedClientFactory(t *testing.T) {
	// ... mirror the existing connection-reconciler envtest harness ...
	// ... inject &recordingFactory{} via LiteLLMClientFactory ...
	// ... drive one Reconcile ...
	// ... assert factory.Calls has exactly one entry with the expected fields ...
}
```

(Detailed wiring depends on how `suite_test.go` constructs `connReconciler` at L311 — copy the existing pattern, add the `LiteLLMClientFactory` field assignment.)

**Step 4: Run**

```bash
./scripts/dev.sh make envtest-pkg PKG=./internal/controller/... FOCUS="UsesInjectedClientFactory"
```
Expected: PASS.

**Step 5: Confirm the production path still works**

```bash
./scripts/dev.sh make envtest-pkg PKG=./internal/controller/... FOCUS="LiteLLMConnection|FastPath"
```
Expected: green (DefaultFactory used implicitly when field left nil).

**Step 6: Commit**

```bash
git add internal/controller/litellmconnection_controller.go internal/controller/litellmconnection_controller_test.go
git commit -m "feat(controller): inject ClientFactory into LiteLLMConnectionReconciler"
```

### Task 3.3: Phase 3 gate

**Step 1:** Full test-all + security:
```bash
./scripts/dev.sh make test-all
./scripts/dev.sh make security
```
Expected: green.

**Step 2:** E2E smoke (kept cluster):
```bash
./scripts/dev.sh make e2e-keep
```
Expected: green. Confirms `cmd/main.go` (which does NOT set `LiteLLMClientFactory`) implicitly resolves to `DefaultFactory` and the probe loop still works end-to-end.

No commit.

---

## Final gate (before push)

```bash
./scripts/dev.sh make test-all      # unit + envtest
./scripts/dev.sh make security      # gosec + govulncheck + fuzz-short
make pre-push                       # 15 gates (host-only — gitleaks/trufflehog containers)
```

Every gate must be green. If `make pre-push` flags SPDX header drift on the new files (`common.go`, `factory.go`, `factory_test.go`, `errors_apierror_test.go`, `client_apierror_test.go`, `litellmconnection_factory_test.go`), confirm each starts with `// SPDX-License-Identifier: Apache-2.0`.

Push:
```bash
git push -u origin feat/palena-adoption
```

Open PR using the project's PR template; reference this plan path in the description.

---

## Reference

- Source repo: https://github.com/PalenaAI/litellm-operator
- Palena `common.go` reviewed: `internal/controller/common.go` (commit on `main` as of 2026-05-20)
- Adoption decision notes: see chat transcript on 2026-05-21 — items 16 (constants), 19 (enterprise classifier), 21 (ClientFactory) accepted; 17 (emitEvent nil-guard), 18 (computeSpecHash — ackstorm has stronger canonicalJSON variant), 20 (auto-rollback — not applicable, no instance controller) rejected.
