# FIX.txt Remediation Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Land remediations for all 7 findings in `FIX.txt` (2026-05-22, v0.1.1 prod smoke-test): two HIGH blockers (MCPServerDiscovery dot sanitize, KubeAI api_base overlay), one MEDIUM (auto-recovery), one MEDIUM-design-pending (per-CR reachability probe), three LOW (orphan GC, OpenAI canonical doc, toolhive log spam).

**Architecture:**
- HIGHs land first as small, surgical, atomic commits with unit + envtest coverage. No new packages, no new abstractions. H-1 introduces a single `internal/litellm` helper for name sanitization; H-2 reuses the existing `paramsMap` overlay site (parallel to bedrock region overlay at `modeldiscovery_controller.go:838`).
- LOWs land next (LOW-5 is a behavior nudge with regression test, LOW-6 is example YAML only, LOW-7 is log-level + once-per-window dedup in `internal/toolhive/informer.go`).
- MEDIUM-3 (backoff cap + Connection-event fan-in) lands as the last code change. It touches five controllers' `SetupWithManager` and must be verified by envtest with deterministic transient errors before commit.
- MEDIUM-4 (per-CR reachability probe) is NOT implemented inline — design surface is too large for plan-level tasks (new condition type, `spec.probe` flag, synthetic-call cost, multi-kind). Task at end produces a `SPEC.md` via `/gsd:spec-phase` and stops there.

**Tech Stack:** Go 1.24.13, controller-runtime v0.19.4, k8s.io v0.31.0, Ginkgo v2 (e2e), envtest (controller). All commands executed via `./scripts/dev.sh` (devtools container). Pre-push gates non-negotiable; `make pre-push` runs before each push to `main`.

**Working directory:** Current branch (`main`). Each task is one atomic commit per the project's release convention. No bundled commits across findings.

**File anchors (verified 2026-05-22):**
- H-1 CREATE site: `internal/controller/mcpserver_controller.go:360-369` (`createReq := &litellm.MCPServerRequest{...}`)
- H-1 UPDATE site: `internal/controller/mcpserver_controller.go:387-396` (`updateReq := &litellm.MCPServerUpdateRequest{...}`)
- H-1 name-resolve sites: `internal/controller/mcpserver_controller.go:162` (`resolveServerIDByName(...mcp.Name...)`), :312 (`"server_name": mcp.Name`), :456 (`if e.ServerName == name`)
- H-1 request types: `internal/litellm/mcp.go:18` (`CreateMCPServer`), :31 (`UpdateMCPServer`)
- H-2 overlay site: `internal/controller/modeldiscovery_controller.go:835-839` (Step 2: typed-field overlay)
- H-2 provider type const: `internal/controller/modeldiscovery_controller.go:106` (`providerTypeKubeAI = "kubeai"`)
- L-5 vanish-detection: `internal/controller/modeldiscovery_controller.go:648-714` (already runs on every Reconcile — see Task 14 for diagnostic + regression test)
- L-7 dedup log: `internal/toolhive/informer.go:106-110` (`s.log.Info("toolhive dedup: v1alpha1 wins", ...)`)
- M-3 SetupWithManager sites: `mcpserver_controller.go:568`, `model_controller.go:720`, `a2aagent_controller.go:702`, `team_controller.go:1202`, `mcpserverdiscovery_controller.go:915`, `modeldiscovery_controller.go:1260`

---

## Task 1: Add `SanitizeMCPServerName` helper to `internal/litellm`

Justification: H-1 fix needs the same sanitize at four call sites (CREATE, UPDATE, name-resolve, status-write). A single helper keeps them in sync and gives future call sites the right primitive.

**Files:**
- Create: `internal/litellm/sanitize.go`
- Test: `internal/litellm/sanitize_test.go`

**Step 1: Write the failing test**

```go
// internal/litellm/sanitize_test.go
// SPDX-License-Identifier: Apache-2.0

package litellm

import "testing"

func TestSanitizeMCPServerName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"no dots", "abc", "abc"},
		{"single dot", "a.b", "a-b"},
		{"three-part discovery", "test-toolhive-discovery.mcp.context7", "test-toolhive-discovery-mcp-context7"},
		{"trailing dot", "abc.", "abc-"},
		{"only dots", "...", "---"},
		{"empty", "", ""},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := SanitizeMCPServerName(tc.in); got != tc.want {
				t.Fatalf("SanitizeMCPServerName(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
```

**Step 2: Run the test to verify it fails**

```
./scripts/dev.sh go test ./internal/litellm/ -run TestSanitizeMCPServerName -v
```
Expected: build error `undefined: SanitizeMCPServerName`.

**Step 3: Write the minimal implementation**

```go
// internal/litellm/sanitize.go
// SPDX-License-Identifier: Apache-2.0

package litellm

import "strings"

// SanitizeMCPServerName converts a Kubernetes metadata.name into a LiteLLM-safe
// server_name + alias by replacing '.' with '-'. LiteLLM v1.83.10+ rejects '.'
// in server_name with HTTP 400 "Server name cannot contain '.'." See FIX.txt
// HIGH-1 (2026-05-22). The K8s-side metadata.name is left untouched — only
// the wire payload is rewritten.
func SanitizeMCPServerName(name string) string {
	return strings.ReplaceAll(name, ".", "-")
}
```

**Step 4: Run the test to verify it passes**

```
./scripts/dev.sh go test ./internal/litellm/ -run TestSanitizeMCPServerName -v
```
Expected: `PASS`.

**Step 5: Commit**

```
git add internal/litellm/sanitize.go internal/litellm/sanitize_test.go
git commit -m "feat(litellm): add SanitizeMCPServerName helper (FIX.txt H-1)"
```

---

## Task 2: Apply `SanitizeMCPServerName` to MCPServer CREATE / UPDATE / lookup paths

**Files:**
- Modify: `internal/controller/mcpserver_controller.go:162` (name-resolve), :312 (status-write key), :361 (CREATE), :362 (Alias), :389 (UPDATE), :456 (list filter)

**Step 1: Write the failing test (capture-the-request unit test against the fake client)**

Append to `internal/controller/mcpserver_controller_test.go` (or create a small file `mcpserver_sanitize_test.go` alongside if cleaner):

```go
// internal/controller/mcpserver_sanitize_test.go
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
)

// TestMCPServer_FIX_H1_DottedName asserts that an MCPServer with a dotted
// metadata.name is sent to LiteLLM with ServerName + Alias sanitized via
// dot-to-dash replacement. Regression for FIX.txt HIGH-1.
func TestMCPServer_FIX_H1_DottedName(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ns := makeTestNamespace(t, "fix-h1")
	connCR := newReadyConnection(t, ns, "litellm")
	defer func() { _ = k8sClient.Delete(context.Background(), connCR, &client.DeleteOptions{}) }()

	const dotted = "test-toolhive-discovery.mcp.context7"
	mcp := &litellmv1alpha1.LiteLLMMCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: dotted, Namespace: ns},
		Spec: litellmv1alpha1.MCPServerSpec{
			ConnectionRef: corev1.LocalObjectReference{Name: connCR.Name},
			Endpoint:      "http://example.invalid",
			Transport:     "http",
		},
	}
	if err := k8sClient.Create(ctx, mcp); err != nil {
		t.Fatalf("create dotted MCPServer: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), mcp, &client.DeleteOptions{}) })

	want := "test-toolhive-discovery-mcp-context7"
	waitForFakeMCPCreate(t, want)

	req := lastFakeMCPCreate(t)
	if req.ServerName != want || req.Alias != want {
		t.Fatalf("sanitized wire payload: got ServerName=%q Alias=%q, want %q for both",
			req.ServerName, req.Alias, want)
	}
	if strings.Contains(req.ServerName, ".") || strings.Contains(req.Alias, ".") {
		t.Fatalf("dot leaked into LiteLLM payload: %s", cmp.Diff(want, req.ServerName))
	}
}
```

(If `waitForFakeMCPCreate` / `lastFakeMCPCreate` helpers don't exist in the test suite yet, add minimal ones using the existing fake-client recording infrastructure in `internal/litellm/mock/`. Check `internal/controller/mcpserver_controller_test.go` for the established pattern first — DO NOT invent a new mocking convention.)

**Step 2: Run the test to verify it fails**

```
./scripts/dev.sh make envtest-pkg PKG=./internal/controller/... FOCUS=TestMCPServer_FIX_H1_DottedName
```
Expected: FAIL — captured ServerName is `test-toolhive-discovery.mcp.context7` (dots preserved). If the fake-LiteLLM rejects dotted names like prod does, the test fails earlier with `LiteLLMRejected` instead — that's also a valid red signal.

**Step 3: Apply the sanitize at all four sites**

Modify `internal/controller/mcpserver_controller.go`:

- Line 361 — CREATE arm:
  ```go
  sanitizedName := litellm.SanitizeMCPServerName(mcp.Name)
  createReq := &litellm.MCPServerRequest{
      ServerName:    sanitizedName,
      Alias:         sanitizedName,
      URL:           mcp.Spec.Endpoint,
      ...
  }
  ```
- Line 388 — UPDATE arm:
  ```go
  updateReq := &litellm.MCPServerUpdateRequest{
      ServerID:      mcp.Status.LastRendered.ServerID,
      ServerName:    litellm.SanitizeMCPServerName(mcp.Name),
      ...
  }
  ```
- Line 162 — name-resolve (finalizer fallback): wrap the `mcp.Name` arg with `litellm.SanitizeMCPServerName(mcp.Name)` so the resolver searches by the LiteLLM-side name (NOT the K8s name).
- Line 312 — status-write `"server_name"` map entry: replace with sanitized form (this is what `LastRendered` stores; without the sanitize, an UPDATE on the next reconcile would not match what LiteLLM has).
- Line 456 — `if e.ServerName == name` filter inside `resolveServerIDByName`: the `name` parameter passed in is now sanitized (per the line-162 change), so no further edit here — but READ the surrounding 20 lines and confirm the comparison still holds. If `resolveServerIDByName` has any other internal `mcp.Name` reference, sanitize there too.

**Step 4: Run the test + the existing MCPServer suite**

```
./scripts/dev.sh make envtest-pkg PKG=./internal/controller/... FOCUS=TestMCPServer
```
Expected: PASS for `TestMCPServer_FIX_H1_DottedName` AND for the full existing MCPServer test set (no regressions).

**Step 5: Commit**

```
git add internal/controller/mcpserver_controller.go internal/controller/mcpserver_sanitize_test.go
git commit -m "fix(mcpserver): sanitize dots in LiteLLM server_name + alias (FIX.txt H-1)

LiteLLM v1.83.10+ rejects '.' in server_name with HTTP 400. MCPServerDiscovery
generates dotted metadata.name (<discovery>.<source-ns>.<source-name>), which
blocked 22/22 children in prod (v0.1.1 EKS smoke-test, 2026-05-22).

Sanitize ServerName + Alias at the wire boundary (CREATE, UPDATE, finalizer
name-resolve, status.lastRendered). K8s-side metadata.name is unchanged."
```

---

## Task 3: Add MCPServerDiscovery → MCPServer envtest for dotted child names

**Files:**
- Modify: `internal/controller/mcpserverdiscovery_controller_test.go` (append a Describe block)

**Step 1: Write the failing test**

```go
// Append to internal/controller/mcpserverdiscovery_controller_test.go

// TestMCPServerDiscovery_FIX_H1_DottedChildren asserts that a Discovery
// generating dotted child names succeeds at LiteLLM registration (no 400
// "Server name cannot contain '.'."). Regression for FIX.txt HIGH-1.
func TestMCPServerDiscovery_FIX_H1_DottedChildren(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	ns := makeTestNamespace(t, "fix-h1-disc")
	connCR := newReadyConnection(t, ns, "litellm")
	defer func() { _ = k8sClient.Delete(context.Background(), connCR, &client.DeleteOptions{}) }()

	// Plant a fake toolhive index with three MCPServers across two namespaces:
	// expect 3 dotted children of the form <discovery>.<source-ns>.<source-name>.
	plantFakeToolHiveServers(t, []toolHiveFixture{
		{ns: "mcp", name: "context7", transport: "http", url: "http://context7.invalid"},
		{ns: "mcp", name: "exa", transport: "sse", url: "http://exa.invalid"},
		{ns: "other", name: "search", transport: "http", url: "http://search.invalid"},
	})

	disc := &litellmv1alpha1.LiteLLMMCPServerDiscovery{
		ObjectMeta: metav1.ObjectMeta{Name: "test-toolhive-discovery", Namespace: ns},
		Spec: litellmv1alpha1.MCPServerDiscoverySpec{
			ConnectionRef: corev1.LocalObjectReference{Name: connCR.Name},
			ToolHive:      litellmv1alpha1.ToolHiveSourceSpec{ /* enable both GVKs */ },
		},
	}
	if err := k8sClient.Create(ctx, disc); err != nil {
		t.Fatalf("create Discovery: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), disc, &client.DeleteOptions{}) })

	// Each child name contains two dots; after sanitize, the LiteLLM payload
	// must contain only dashes.
	waitForChildrenReady(t, disc, 3, 60*time.Second)

	for _, req := range allFakeMCPCreates(t) {
		if strings.Contains(req.ServerName, ".") {
			t.Fatalf("dot leaked into ServerName=%q (FIX H-1 regression)", req.ServerName)
		}
		if strings.Contains(req.Alias, ".") {
			t.Fatalf("dot leaked into Alias=%q (FIX H-1 regression)", req.Alias)
		}
	}
}
```

(Inspect the existing test suite for `plantFakeToolHiveServers` and `waitForChildrenReady` — both are likely already named differently. Use whatever this codebase calls them. The point of this test is the wire-payload assertion, not the toolhive plumbing.)

**Step 2: Run the test to verify it fails (with sanitize not yet applied — should now PASS after Task 2, so this test exists primarily as regression guard)**

```
./scripts/dev.sh make envtest-pkg PKG=./internal/controller/... FOCUS=TestMCPServerDiscovery_FIX_H1_DottedChildren
```
Expected: PASS (Task 2 already landed the fix). If you want to see it red first, `git stash` the controller edits from Task 2 → run → PASS = `unsanitized.` expected to FAIL → `git stash pop`.

**Step 3: (skipped — implementation already in Task 2)**

**Step 4: Re-run the focused test**

```
./scripts/dev.sh make envtest-pkg PKG=./internal/controller/... FOCUS=TestMCPServerDiscovery_FIX_H1_DottedChildren
```
Expected: PASS.

**Step 5: Commit**

```
git add internal/controller/mcpserverdiscovery_controller_test.go
git commit -m "test(mcpserverdiscovery): regression for dotted child names (FIX.txt H-1)"
```

---

## Task 4: Add KubeAI `api_base` overlay in `modeldiscovery_controller.go`

**Files:**
- Modify: `internal/controller/modeldiscovery_controller.go:835-839` (Step 2: typed-field overlay)

**Step 1: Write the failing test**

Append to `internal/controller/modeldiscovery_controller_test.go`:

```go
// TestModelDiscovery_FIX_H2_KubeAIAPIBaseOverlay asserts that kubeai Discovery
// children carry spec.params.api_base = discovery.spec.baseUrl, parallel to
// the bedrock spec.region → aws_region_name overlay. Regression for FIX.txt
// HIGH-2.
func TestModelDiscovery_FIX_H2_KubeAIAPIBaseOverlay(t *testing.T) {
	const baseURL = "http://kubeai.kubeai.svc/openai/v1"

	md := &litellmv1alpha1.LiteLLMModelDiscovery{
		ObjectMeta: metav1.ObjectMeta{Name: "kubeai-test", Namespace: "default"},
		Spec: litellmv1alpha1.ModelDiscoverySpec{
			Type:    "kubeai",
			BaseURL: baseURL,
		},
	}
	r := &LiteLLMModelDiscoveryReconciler{ /* minimal init or table-driven via the existing helper */ }
	child, err := r.renderChild(md, "kubeai-test-foo", "qwen3-4b", "hosted_vllm", "default")
	if err != nil {
		t.Fatalf("renderChild: %v", err)
	}

	var params map[string]any
	if err := json.Unmarshal(child.Spec.Params.Raw, &params); err != nil {
		t.Fatalf("unmarshal child.Spec.Params: %v", err)
	}
	if got, want := params["api_base"], baseURL; got != want {
		t.Fatalf("api_base overlay missing: got %v, want %s", got, want)
	}
	if got, want := params["model"], "hosted_vllm/qwen3-4b"; got != want {
		t.Errorf("model overlay regressed: got %v, want %s", got, want)
	}
}

// TestModelDiscovery_FIX_H2_UserAPIBaseWins asserts that a user-supplied
// discovery.spec.params.api_base takes precedence over the auto-overlay
// (matches bedrock region precedence behavior — user wins).
func TestModelDiscovery_FIX_H2_UserAPIBaseWins(t *testing.T) {
	const baseURL = "http://kubeai.kubeai.svc/openai/v1"
	const userOverride = "http://user-override.example/v1"

	md := &litellmv1alpha1.LiteLLMModelDiscovery{
		ObjectMeta: metav1.ObjectMeta{Name: "kubeai-test", Namespace: "default"},
		Spec: litellmv1alpha1.ModelDiscoverySpec{
			Type:    "kubeai",
			BaseURL: baseURL,
			Params:  runtime.RawExtension{Raw: []byte(`{"api_base":"` + userOverride + `"}`)},
		},
	}
	r := &LiteLLMModelDiscoveryReconciler{}
	child, err := r.renderChild(md, "kubeai-test-foo", "qwen3-4b", "hosted_vllm", "default")
	if err != nil {
		t.Fatalf("renderChild: %v", err)
	}
	var params map[string]any
	_ = json.Unmarshal(child.Spec.Params.Raw, &params)
	if got, want := params["api_base"], userOverride; got != want {
		t.Fatalf("user-supplied api_base must win: got %v, want %s", got, want)
	}
}
```

**Step 2: Run the tests to verify they fail**

```
./scripts/dev.sh make unit-pkg PKG=./internal/controller/...
```
Expected: `TestModelDiscovery_FIX_H2_KubeAIAPIBaseOverlay` FAIL with `api_base overlay missing: got <nil>`. `TestModelDiscovery_FIX_H2_UserAPIBaseWins` will PASS already (user-supplied is preserved by the existing decode-then-overlay structure) — but landing it now locks the precedence contract.

**Step 3: Apply the overlay**

In `internal/controller/modeldiscovery_controller.go`, modify the typed-field overlay block at line 835-839:

```go
// Step 2: typed-field overlay (D-07).
paramsMap["model"] = litellmProvider + "/" + rawID
if md.Spec.Type == providerTypeBedrock {
    paramsMap["aws_region_name"] = md.Spec.Region
}
// FIX.txt H-2 (2026-05-22): kubeai requires api_base on every child so the
// LiteLLM proxy can route inference requests. Parallel to the bedrock
// region overlay above. User-supplied params.api_base wins because the
// user's bag is decoded BEFORE this overlay block — but we explicitly
// guard with a presence check to make precedence visible in the diff.
if md.Spec.Type == providerTypeKubeAI {
    if _, userSet := paramsMap["api_base"]; !userSet {
        paramsMap["api_base"] = md.Spec.BaseURL
    }
}
```

Note: the existing bedrock overlay (line 838) unconditionally writes `aws_region_name` — which means user-supplied `aws_region_name` is silently overwritten. That is the established behavior. FIX.txt H-2 test plan step 2 explicitly says kubeai should match bedrock precedence — re-read FIX.txt L149: "the spec value takes precedence — match bedrock region behavior." There is a contradiction here: bedrock's current code overwrites user value; FIX.txt says match bedrock by user winning. Resolve by reading the bedrock test (`modeldiscovery_controller_test.go:91-116, 420`): if those tests cover the precedence question, follow them. If they don't, default to **user wins** for kubeai because that matches the spirit of "merged on top" in FIX.txt. Document the choice in the code comment.

**Step 4: Run the tests to verify they pass**

```
./scripts/dev.sh make unit-pkg PKG=./internal/controller/...
```
Expected: both new tests PASS. Existing modeldiscovery suite PASS unchanged.

Run envtest as well:

```
./scripts/dev.sh make envtest-pkg PKG=./internal/controller/... FOCUS=TestModelDiscovery
```

**Step 5: Commit**

```
git add internal/controller/modeldiscovery_controller.go internal/controller/modeldiscovery_controller_test.go
git commit -m "feat(modeldiscovery): kubeai api_base overlay (FIX.txt H-2)

KubeAI Discovery children registered at LiteLLM successfully but had no
api_base in spec.params, so inference-time requests failed (no route for
hosted_vllm/<id>). Add a second typed-field overlay rule parallel to the
bedrock spec.region → aws_region_name overlay: when type=kubeai, write
spec.params.api_base = spec.baseUrl into every generated child. User-supplied
params.api_base wins.

Confirmed against prod 2026-05-22 EKS deployment: workaround was manual
discovery.spec.params.api_base — auto-overlay closes the gap."
```

---

## Task 5: Update `CONTEXT.md` D-07 to document both overlays

**Files:**
- Modify: project docs — find the D-07 anchor. Search:

```
grep -rn "D-07\|typed-field overlay\|aws_region_name overlay" docs/ references/ CONTEXT.md 2>/dev/null
```

The exact file location depends on what survives in this repo (may be `references/CONTEXT.md`, `docs/architecture/...`, or inline-only in the controller). Update wherever the bedrock overlay is documented to also list the kubeai overlay.

**Step 1: Locate the doc anchor (read-only)**

Run the grep above and read the resolved file's D-07 section.

**Step 2: Update the doc**

Add a paragraph next to the existing bedrock overlay description:

> **kubeai api_base overlay:** When `spec.type == "kubeai"`, the reconciler writes `spec.params.api_base = spec.baseUrl` into every generated child Model unless the user has explicitly set `spec.params.api_base`. Parallel to the bedrock `spec.region → aws_region_name` overlay; both belong to the D-07 typed-field overlay rule set. Required for LiteLLM to route `hosted_vllm/<model>` inference requests (FIX.txt H-2, 2026-05-22).

**Step 3: Run docs sanity build**

```
make docs-build
```
Expected: site builds with no broken-link warnings.

**Step 4: Commit**

```
git add <doc-path>
git commit -m "docs(D-07): document kubeai api_base overlay alongside bedrock (FIX.txt H-2)"
```

---

## Task 6: Document OpenAI canonical-only filter pattern in example manifest

**Files:**
- Modify: `examples/example-deploy/03-modeldiscovery.yaml` (or wherever the openai example lives — verify with `ls examples/`)

**Step 1: Locate the example (read-only)**

```
grep -rln "type: openai" examples/ | head
```

**Step 2: Update the example**

Add a commented exclude block near the openai Discovery's `filters:` section:

```yaml
filters:
  # Optional: drop dated OpenAI variants (e.g. gpt-4o-mini-2024-07-18),
  # keeping only canonical rolling aliases (gpt-4o-mini, gpt-4.1, ...).
  # Confirmed parity with ackstorm-litellm-autoconfig pre-filter
  # (FIX.txt LOW-6, 2026-05-22). On 2026-05-22 EKS smoke-test this dropped
  # 117 → 49 children, matching the Python predecessor 1:1.
  # exclude:
  #   - ".*[0-9]$"
```

**Step 3: Verify YAML still parses**

```
./scripts/dev.sh bash -c "kubectl apply --dry-run=client -f examples/example-deploy/03-modeldiscovery.yaml"
```
Expected: no errors.

**Step 4: Commit**

```
git add examples/example-deploy/03-modeldiscovery.yaml
git commit -m "docs(examples): document OpenAI canonical-only filter pattern (FIX.txt L-6)"
```

---

## Task 7: Demote toolhive dedup log + once-per-window dedup

**Files:**
- Modify: `internal/toolhive/informer.go:106-110` (the `s.log.Info("toolhive dedup: v1alpha1 wins", ...)` line)
- Test: `internal/toolhive/informer_test.go` (add or extend)

**Step 1: Read the surrounding 40 lines**

```
read internal/toolhive/informer.go offset=80 limit=60
```
Confirm the dedup logic shape. If a `lastLoggedAt map[string]time.Time` already exists, extend it. If not, add a small `sync.Mutex`-guarded map keyed by `(kind, namespace, name)` with a 60s window.

**Step 2: Write the failing test**

```go
// Adjust to the test patterns already in informer_test.go.
func TestInformer_DedupLog_OncePerWindow(t *testing.T) {
	logBuf := &recordingLogger{}
	s := &state{log: logBuf, /* ... */}

	// Simulate ten dedup events for the same (kind, ns, name) within < 60s.
	for i := 0; i < 10; i++ {
		s.recordDedupCollision("MCPServer", "mcp", "context7")
	}

	got := logBuf.CountWithMessage("toolhive dedup: v1alpha1 wins")
	if got != 1 {
		t.Fatalf("expected 1 dedup log line per (kind, ns, name) per 60s window, got %d", got)
	}
}
```

**Step 3: Run to verify it fails**

```
./scripts/dev.sh go test ./internal/toolhive/ -run TestInformer_DedupLog_OncePerWindow -v
```
Expected: 10 log lines, FAIL.

**Step 4: Apply the fix**

In `internal/toolhive/informer.go`:
- Demote `s.log.Info("toolhive dedup: v1alpha1 wins", ...)` to `s.log.V(2).Info(...)`.
- Add a 60s-window dedup guard so that at V(0) (default verbosity) at most one INFO line per `(kind, namespace, name)` tuple fires per window. Implementation: a `map[string]time.Time` keyed by `kind|ns|name` with last-emitted timestamp, guarded by `sync.Mutex`. Emit at INFO if `time.Since(last) >= 60*time.Second`, else V(2).

(If a more invasive rewrite of the dedup-log path is needed, scope it tight — caller surfaces should not change.)

**Step 5: Run the test + the broader informer suite**

```
./scripts/dev.sh go test ./internal/toolhive/ -v
```
Expected: PASS for new test + existing suite.

**Step 6: Commit**

```
git add internal/toolhive/informer.go internal/toolhive/informer_test.go
git commit -m "perf(toolhive): rate-limit 'v1alpha1 wins' dedup log to once-per-60s (FIX.txt L-7)

Operator log spam: one INFO line per MCPServer per reconcile (22 servers × poll
interval = a lot of churn). Demote to V(2) by default; emit at V(0) at most
once per (kind, namespace, name) tuple per 60s window."
```

---

## Task 8: Add diagnostic envtest for LOW-5 (filter mutation → sub-second prune)

Note: `modeldiscovery_controller.go:648-714` already enumerates `existingChildren` and deletes vanished entries on every Reconcile. FIX.txt L-5 reports that this didn't happen in prod within the observation window. Hypothesis: either the controller wasn't re-enqueued on the patch, or the prune block was short-circuited by an earlier error path. The fix is **diagnostic first**: write a test that proves the contract; if it FAILS without code change, the prune logic has a real bug to track down. If it PASSES, the prod symptom may be a metric-update-only artifact and we close L-5 as resolved-by-test.

**Files:**
- Modify: `internal/controller/modeldiscovery_controller_test.go`

**Step 1: Write the diagnostic test**

```go
// TestModelDiscovery_FIX_L5_PruneOnFilterMutation asserts that mutating
// spec.filters.exclude on a Discovery drives a prune pass within one
// reconcile (sub-second envtest latency), not on the next refresh tick.
// Regression for FIX.txt LOW-5.
func TestModelDiscovery_FIX_L5_PruneOnFilterMutation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Use an in-test mock provider returning a stable 5-model set.
	mockProvider := newMockProvider([]string{"a", "b", "c", "d", "e"})
	registerMockProvider(t, mockProvider)

	md := &litellmv1alpha1.LiteLLMModelDiscovery{
		ObjectMeta: metav1.ObjectMeta{Name: "fix-l5", Namespace: "default"},
		Spec: litellmv1alpha1.ModelDiscoverySpec{
			Type:    "mock-test-provider",
			Refresh: litellmv1alpha1.RefreshSpec{Interval: metav1.Duration{Duration: 10 * time.Minute}},
		},
	}
	if err := k8sClient.Create(ctx, md); err != nil {
		t.Fatalf("create Discovery: %v", err)
	}
	waitForChildrenCount(t, md, 5, 30*time.Second)

	// Mutate filters to exclude 3 of the 5.
	patch := client.MergeFrom(md.DeepCopy())
	md.Spec.Filters.Exclude = []string{"^[abc]$"}
	if err := k8sClient.Patch(ctx, md, patch); err != nil {
		t.Fatalf("patch filters: %v", err)
	}

	// Assert the K8s child set drains to 2 within bounded time (NOT the
	// refresh interval).
	waitForChildrenCount(t, md, 2, 10*time.Second)
}
```

**Step 2: Run the test**

```
./scripts/dev.sh make envtest-pkg PKG=./internal/controller/... FOCUS=TestModelDiscovery_FIX_L5_PruneOnFilterMutation TIMEOUT=5m
```

Two outcomes:

- **PASS** — the prune-on-generation-change behavior is already correct. The prod symptom is likely a metric-only stale-read or kubectl-side caching. Add an explanatory comment to the test referencing FIX.txt L-5 and the resolution, then commit. Skip Task 9.
- **FAIL** — the prune pass does NOT run on generation change. Proceed to Task 9.

**Step 3: Commit (only if PASS in Step 2)**

```
git add internal/controller/modeldiscovery_controller_test.go
git commit -m "test(modeldiscovery): regression for prune-on-filter-mutation (FIX.txt L-5)

Asserts the prune contract: filter mutation drives sub-second child drain,
not refresh-interval-tick. Test PASSES against current code, so the prod
symptom (117→49 took ~6 min) is filed as observability/metric artifact,
not a missing prune branch."
```

---

## Task 9: (Conditional) Force prune-on-render in `modeldiscovery_controller.Reconcile`

**Only execute if Task 8 step 2 returned FAIL.**

**Files:**
- Modify: `internal/controller/modeldiscovery_controller.go` around line 648 (Step 11 Vanish detection block)
- Modify: `internal/controller/mcpserverdiscovery_controller.go` (same pattern if it exists)

**Step 1: Add a focused failing test (skip if Task 8's test is already red)**

The test from Task 8 IS the failing test.

**Step 2: Apply the fix**

In `modeldiscovery_controller.Reconcile`, ensure the vanish-detection block at L648 ALWAYS runs after re-rendering the expected child set — i.e. NOT gated by any `refreshDue` / `firstReconcile` / similar condition. Idempotent: when expected == actual the loop is a no-op.

Read the surrounding 40 lines and find the conditional that's bypassing the prune. Common culprits:
- `if !refreshDue { return ctrl.Result{...}, nil }` short-circuit
- An `if firstReconcile` branch that skips prune

Remove the gate. Add a comment:

```go
// FIX.txt L-5 (2026-05-22): the vanish-detection / prune pass MUST run
// on every Reconcile after re-rendering, NOT only on refresh-interval
// ticks. Filter mutations bump .metadata.generation and re-enqueue the
// CR; if prune is gated on the refresh tick, those mutations don't
// propagate until the next interval (default 10m) elapses.
```

**Step 3: Re-run the Task 8 test**

```
./scripts/dev.sh make envtest-pkg PKG=./internal/controller/... FOCUS=TestModelDiscovery_FIX_L5_PruneOnFilterMutation TIMEOUT=5m
```
Expected: PASS.

**Step 4: Run the full modeldiscovery suite for regression check**

```
./scripts/dev.sh make envtest-pkg PKG=./internal/controller/... FOCUS=TestModelDiscovery
```
Expected: full PASS.

**Step 5: Commit**

```
git add internal/controller/modeldiscovery_controller.go internal/controller/modeldiscovery_controller_test.go
git commit -m "fix(modeldiscovery): always prune on render, not only on refresh tick (FIX.txt L-5)

Filter mutations bump .metadata.generation; the vanish-detection pass must
run on every Reconcile, not just on the refresh-interval tick. Idempotent
(no-op when expected == actual).

Prod symptom: openai discovery patched at 08:34Z took ~6 min for child set
to drain 117 → 49 (status flipped immediately; K8s children lagged)."
```

If `mcpserverdiscovery_controller.go` shares the pattern, apply the same fix there in a separate commit:

```
git commit -m "fix(mcpserverdiscovery): always prune on render (FIX.txt L-5)"
```

---

## Task 10: Cap controller-runtime workqueue backoff in five controllers

Note: M-3 has two sub-fixes; Task 10 is the backoff cap (smaller blast radius, ships first), Task 11 is the Connection-Ready fan-in.

**Files:**
- Modify: `internal/controller/mcpserver_controller.go:568` (SetupWithManager)
- Modify: `internal/controller/model_controller.go:720`
- Modify: `internal/controller/a2aagent_controller.go:702`
- Modify: `internal/controller/team_controller.go:1202`
- Modify: `internal/controller/mcpserverdiscovery_controller.go:915`
- Modify: `internal/controller/modeldiscovery_controller.go:1260`

**Step 1: Write the failing envtest**

```go
// Add to internal/controller/mcpserver_controller_test.go (or a new
// fix_med3_test.go file alongside).
//
// TestMCPServer_FIX_M3_TransientErrorBackoffCap asserts that a flapping
// transient error (500 for first N requests, 201 thereafter) recovers
// within bounded wall-clock time (<=60s), not the default unbounded
// controller-runtime exponential backoff. Regression for FIX.txt M-3(a).
func TestMCPServer_FIX_M3_TransientErrorBackoffCap(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// Configure the fake LiteLLM to 500 the first 5 POST /v1/mcp/server
	// requests, then 201.
	fakeMCP.SetTransientFailures(5)
	defer fakeMCP.ResetTransientFailures()

	ns := makeTestNamespace(t, "fix-m3")
	connCR := newReadyConnection(t, ns, "litellm")
	mcp := &litellmv1alpha1.LiteLLMMCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "flapper", Namespace: ns},
		Spec: litellmv1alpha1.MCPServerSpec{
			ConnectionRef: corev1.LocalObjectReference{Name: connCR.Name},
			Endpoint:      "http://example.invalid",
			Transport:     "http",
		},
	}
	if err := k8sClient.Create(ctx, mcp); err != nil {
		t.Fatalf("create MCPServer: %v", err)
	}
	start := time.Now()
	waitForReady(t, mcp, 60*time.Second)
	if elapsed := time.Since(start); elapsed > 60*time.Second {
		t.Fatalf("recovery exceeded backoff cap: %v (expected <60s with 30s cap)", elapsed)
	}
}
```

**Step 2: Run the test to verify it fails**

```
./scripts/dev.sh make envtest-pkg PKG=./internal/controller/... FOCUS=TestMCPServer_FIX_M3_TransientErrorBackoffCap TIMEOUT=5m
```
Expected: FAIL (timeout — default backoff is unbounded; 5 errors push the next retry past the 60s assertion).

**Step 3: Apply the cap to all six SetupWithManager sites**

For each controller, modify the `ctrl.NewControllerManagedBy(mgr).For(...).Complete(r)` chain to add `WithOptions(controller.Options{RateLimiter: ...})`. Example for `mcpserver_controller.go:568`:

```go
import (
    "k8s.io/client-go/util/workqueue"
    "sigs.k8s.io/controller-runtime/pkg/controller"
    "sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func (r *MCPServerReconciler) SetupWithManager(mgr ctrl.Manager) error {
    return ctrl.NewControllerManagedBy(mgr).
        For(&litellmv1alpha1.LiteLLMMCPServer{}, builder.WithPredicates()).
        Watches(
            &corev1.Secret{},
            handler.EnqueueRequestsFromMapFunc(r.secretToMCPServers),
        ).
        WithOptions(controller.Options{
            // FIX.txt M-3(a) (2026-05-22): cap exponential backoff at 30s so
            // transient errors (LiteLLM 500, secret-rotation lag, brief
            // upstream restart) don't strand a CR in a 5-10min backoff
            // queue. Non-transient errors (LiteLLMRejected, SecretNotFound)
            // already short-circuit via `return ctrl.Result{}, nil` upstream
            // of the rate-limiter, so they don't hot-loop.
            RateLimiter: workqueue.NewItemExponentialFailureRateLimiter(
                200*time.Millisecond, 30*time.Second,
            ),
        }).
        Named("mcpserver").
        Complete(r)
}
```

Repeat verbatim (with controller-specific names) for the other five sites.

Important: controller-runtime v0.19.4 expects `workqueue.TypedRateLimiter[reconcile.Request]` in some method signatures — the untyped `workqueue.NewItemExponentialFailureRateLimiter` may not compile against the field directly. If so, wrap with `workqueue.DefaultControllerRateLimiter`-style adapter or use `workqueue.NewTypedItemExponentialFailureRateLimiter[reconcile.Request]`. Resolve the exact API at compile time — do not guess. (Confirm via `go doc sigs.k8s.io/controller-runtime/pkg/controller.Options` inside the devtools container.)

**Step 4: Re-run the failing test**

```
./scripts/dev.sh make envtest-pkg PKG=./internal/controller/... FOCUS=TestMCPServer_FIX_M3_TransientErrorBackoffCap TIMEOUT=5m
```
Expected: PASS within 60s wall-clock.

**Step 5: Run the full envtest suite for regression check**

```
./scripts/dev.sh make envtest-run
```
Expected: full PASS (no race, no flake on the existing suite).

**Step 6: Commit**

```
git add internal/controller/*.go
git commit -m "fix(controllers): cap workqueue exponential backoff at 30s (FIX.txt M-3a)

After an upstream LiteLLM restart, transient errors could push the next
retry 5-10 min out under the default unbounded backoff. Cap at 30s via
workqueue.NewItemExponentialFailureRateLimiter(200ms, 30s) across all
six controllers (mcpserver, model, a2aagent, team, mcpserverdiscovery,
modeldiscovery).

Non-transient errors (LiteLLMRejected, SecretNotFound) already
short-circuit upstream of the rate-limiter so they don't hot-loop."
```

---

## Task 11: Watch `LiteLLMConnection` Ready transitions to fan-in re-enqueue

**Files:**
- Modify: same five controllers (Task 10), adding a `.Watches(&LiteLLMConnection{}, ...)` chain.

**Step 1: Write the failing envtest**

```go
// TestMCPServer_FIX_M3_ConnectionFanIn asserts that when a previously
// failing LiteLLMConnection flips Ready=True, all MCPServers in the same
// namespace re-reconcile within bounded time (<=10s). Regression for
// FIX.txt M-3(b).
func TestMCPServer_FIX_M3_ConnectionFanIn(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	ns := makeTestNamespace(t, "fix-m3-fanin")
	// Create a Connection that is NOT Ready (e.g. probe fails).
	connCR := newUnreachableConnection(t, ns, "litellm")
	mcp := &litellmv1alpha1.LiteLLMMCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "waits-for-conn", Namespace: ns},
		Spec: litellmv1alpha1.MCPServerSpec{
			ConnectionRef: corev1.LocalObjectReference{Name: connCR.Name},
			Endpoint:      "http://example.invalid",
			Transport:     "http",
		},
	}
	if err := k8sClient.Create(ctx, mcp); err != nil {
		t.Fatalf("create MCPServer: %v", err)
	}

	// MCPServer is stuck Ready=False with ConnectionNotReady.
	waitForCondition(t, mcp, "Ready", metav1.ConditionFalse, 10*time.Second)

	// Flip the Connection to Ready.
	start := time.Now()
	makeConnectionReady(t, connCR)

	// Assert MCPServer reconciles within <=10s WITHOUT any direct CR mutation.
	waitForCondition(t, mcp, "Ready", metav1.ConditionTrue, 10*time.Second)
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("Connection-Ready fan-in took %v, expected <=10s", elapsed)
	}
}
```

**Step 2: Run to verify failure**

```
./scripts/dev.sh make envtest-pkg PKG=./internal/controller/... FOCUS=TestMCPServer_FIX_M3_ConnectionFanIn TIMEOUT=5m
```
Expected: FAIL — without the watch, the MCPServer reconciler doesn't know about the Connection's Ready flip until its own queue ticks (could be minutes).

**Step 3: Add the watch in five controllers**

Pattern (apply to each of mcpserver, model, a2aagent, team, mcpserverdiscovery, modeldiscovery — note: modeldiscovery is the same family of consumers but verify whether it actually depends on Connection's Ready before adding):

```go
import (
    handler "sigs.k8s.io/controller-runtime/pkg/handler"
    "sigs.k8s.io/controller-runtime/pkg/builder"
    "sigs.k8s.io/controller-runtime/pkg/predicate"
)

// In SetupWithManager:
.Watches(
    &litellmv1alpha1.LiteLLMConnection{},
    handler.EnqueueRequestsFromMapFunc(r.connectionToMCPServers),
    builder.WithPredicates(connectionReadyTransition()),
)
```

Where `connectionReadyTransition()` is a shared predicate (consider putting it in `internal/controller/predicates.go` if no such file exists yet):

```go
// internal/controller/predicates.go
// SPDX-License-Identifier: Apache-2.0

package controller

import (
    "sigs.k8s.io/controller-runtime/pkg/event"
    "sigs.k8s.io/controller-runtime/pkg/predicate"

    litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
)

// connectionReadyTransition fires only when a LiteLLMConnection's Ready
// condition flips False → True. Used by mcpserver / model / a2aagent /
// team / mcpserverdiscovery controllers to re-enqueue all child CRs in the
// same namespace after an upstream LiteLLM restart recovers (FIX.txt M-3b).
func connectionReadyTransition() predicate.Predicate {
    return predicate.Funcs{
        UpdateFunc: func(e event.UpdateEvent) bool {
            oldConn, ok1 := e.ObjectOld.(*litellmv1alpha1.LiteLLMConnection)
            newConn, ok2 := e.ObjectNew.(*litellmv1alpha1.LiteLLMConnection)
            if !ok1 || !ok2 {
                return false
            }
            return !isConnReady(oldConn) && isConnReady(newConn)
        },
        CreateFunc: func(e event.CreateEvent) bool {
            conn, ok := e.Object.(*litellmv1alpha1.LiteLLMConnection)
            return ok && isConnReady(conn)
        },
        DeleteFunc: func(_ event.DeleteEvent) bool { return false },
        GenericFunc: func(_ event.GenericEvent) bool { return false },
    }
}

func isConnReady(c *litellmv1alpha1.LiteLLMConnection) bool {
    for _, cond := range c.Status.Conditions {
        if cond.Type == "Ready" && cond.Status == "True" {
            return true
        }
    }
    return false
}
```

And per-controller mapping function (each one filters to CRs in the same namespace that reference this Connection):

```go
// In mcpserver_controller.go (and analogous in the other four):
func (r *MCPServerReconciler) connectionToMCPServers(ctx context.Context, obj client.Object) []reconcile.Request {
    conn, ok := obj.(*litellmv1alpha1.LiteLLMConnection)
    if !ok {
        return nil
    }
    var list litellmv1alpha1.LiteLLMMCPServerList
    if err := r.List(ctx, &list,
        client.InNamespace(conn.Namespace),
        client.MatchingFields{connectionRefIndexerKey: conn.Name}); err != nil {
        return nil
    }
    out := make([]reconcile.Request, 0, len(list.Items))
    for i := range list.Items {
        out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{
            Name: list.Items[i].Name, Namespace: list.Items[i].Namespace,
        }})
    }
    return out
}
```

If `connectionRefIndexerKey` doesn't exist, add the field indexer in the same controller's `SetupWithManager` registration path (mirror the existing `teamNameIndexerKey` pattern at `internal/controller/<kind>_controller.go`).

**Step 4: Re-run the failing test**

```
./scripts/dev.sh make envtest-pkg PKG=./internal/controller/... FOCUS=TestMCPServer_FIX_M3_ConnectionFanIn TIMEOUT=5m
```
Expected: PASS, recovery <=10s.

**Step 5: Run the full envtest suite for regression check**

```
./scripts/dev.sh make envtest-run
```
Expected: full PASS.

**Step 6: Commit**

```
git add internal/controller/predicates.go internal/controller/*.go
git commit -m "feat(controllers): watch LiteLLMConnection Ready transitions (FIX.txt M-3b)

After an upstream LiteLLM restart, child controllers (mcpserver, model,
a2aagent, team, mcpserverdiscovery) didn't observe the Connection's
Ready-flip and stayed in the backoff queue. Add a shared predicate
'connectionReadyTransition' that fires only on False→True and a per-
controller mapping that re-enqueues every CR in the same namespace
referencing the recovered Connection.

Pairs with M-3a (backoff cap): even a worst-case backoff slot now
gets pre-empted by the Connection-Ready event."
```

---

## Task 12: Run full pre-commit gate

**Files:** none.

**Step 1: Full envtest**

```
./scripts/dev.sh make envtest-run
```
Expected: PASS.

**Step 2: E2E full sweep**

```
./scripts/dev.sh make e2e-full
```
Expected: PASS.

**Step 3: Security + pre-push**

```
./scripts/dev.sh make security
make pre-push
```
Expected: PASS for both. govulncheck ack-list, gitleaks, trufflehog, SPDX header, license, go.mod tidy, etc.

**Step 4: Push**

```
git push origin main
```

If pre-push fails, fix root cause — NEVER `--no-verify`.

---

## Task 13: Defer M-4 to a SPEC phase

M-4 (Ready=True does not validate reachability) is Phase 4-5 scope per FIX.txt. It introduces a new condition (`Probed`), a new spec flag (`spec.probe.enabled`), and per-kind synthetic-call cost decisions. Plan-level tasks would be guesses without a SPEC.

**Files:**
- Create: `.planning/specs/2026-05-22-per-cr-reachability-probe.md` (or wherever `/gsd:spec-phase` deposits its output — verify via the skill).

**Step 1: Invoke the spec-phase skill (manual user action)**

Tell the user: "M-4 is intentionally deferred. Run `/gsd:spec-phase per-cr-reachability-probe` to produce a SPEC.md with: per-kind probe semantics (A2AAgent invoke, Model embed/chat, MCPServer session), `spec.probe.enabled` placement on each CRD, new `Probed` condition lifecycle, cost ceiling (rate-limit per CR), surfacing LiteLLM's own `/v1/mcp/server/health` where available. Then `/gsd:plan-phase` once the SPEC is locked."

**Step 2: Commit a placeholder note (optional)**

If the project convention requires a tracking artifact, append a line to `FIX.txt` or `ROADMAP.md`:

```
M-4 per-CR reachability probe — deferred to dedicated SPEC phase (see
.planning/specs/2026-05-22-per-cr-reachability-probe.md, 2026-05-22).
```

```
git add FIX.txt # or ROADMAP.md
git commit -m "docs(roadmap): track M-4 reachability probe spec deferral (FIX.txt M-4)"
```

---

## Done criteria

- All 7 findings have either a landed commit or (for M-4) a SPEC tracking artifact.
- `make envtest-run`, `make e2e-full`, `make security`, `make pre-push` all PASS.
- New tests added: H-1 unit + envtest, H-2 unit ×2, L-5 envtest (PASS validates contract), L-7 unit, M-3a envtest, M-3b envtest.
- No `--no-verify`, no ack-list expansion, no removed gates.
- HIGH-1 prod symptom verified absent: a dotted-name MCPServerDiscovery against the real LiteLLM produces children at Ready=True.
- HIGH-2 prod symptom verified absent: a kubeai Discovery against the real LiteLLM produces a child whose `spec.params.api_base` matches `spec.baseUrl`.
