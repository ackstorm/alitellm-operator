# MCP server_id Pinning + Single-Namespace Guard Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the operator pin a freshly-created LiteLLM MCP server's `server_id` to the sanitized `metadata.name` (instead of a server-minted UUID), and harden the operator so it watches exactly one namespace.

**Architecture:** Mirror the existing `LiteLLMTeam` `team_id == metadata.name` CREATE-arm pattern in the MCP server controller. Verified live against e2e LiteLLM 1.83.10: `POST /v1/mcp/server` honors a caller-supplied `server_id` (echoes it on 201). The A2A `agent_id` is deliberately **out of scope** — LiteLLM 1.83.10 ignores a supplied `agent_id` (confirmed at source: `add_agent_to_db` never reads it; Prisma mints a UUID), so it cannot be pinned operator-side. The single-namespace guard is a fail-fast startup validation, not a behavior change — the operator already watches one namespace via `cache.Options.DefaultNamespaces` with a single key.

**Tech Stack:** Go (controller-runtime v0.19.4, k8s.io/* v0.31.0), envtest, in-repo HTTP mock (`internal/litellm/mock`), kubebuilder.

## Global Constraints

- Toolchain runs through the devtools container — invoke `make` targets **bare** (they self-route); never prefix with `./scripts/dev.sh`. Host has no Go.
- Every new/modified `*.go` file outside `vendor/`, `zz_generated*.go`, `mock_*.go` MUST start with `// SPDX-License-Identifier: Apache-2.0` (pre-push gate 15).
- Behavior/contract changes update docs in the **same commit** (CLAUDE.md "Documentation hygiene").
- Pin `server_id` from the **sanitized** server name (`litellm.SanitizeMCPServerName(mcp.Name, sep)`), not raw `metadata.name` — this keeps `server_id == server_name`, consistent with `resolveServerIDByName` (which resolves by name) and the conflict-winner model.
- This is a **CREATE-arm-only** change. The UPDATE arm MUST keep the existing `status.lastRendered.ServerID` untouched (mirrors Team `UpdateKeepsExistingUUID`).
- Single-namespace deployment is assumed project-wide; `server_id`/`team_id == name` has no collision surface under that assumption.

---

## File Structure

- `internal/litellm/mock/mock.go` — MCP `POST /v1/mcp/server` handler made to honor a caller-supplied `server_id` (test fidelity to real LiteLLM 1.83.10).
- `internal/controller/mcpserver_controller.go` — CREATE arm stamps `ServerID` and pins `newServerID` from the sanitized name.
- `internal/controller/mcpserver_controller_test.go` — two new envtests: name-as-server_id on CREATE, UUID-kept on UPDATE.
- `cmd/main.go` — `validateWatchNamespace` helper + startup wiring + imports.
- `cmd/main_test.go` (create) — unit test for `validateWatchNamespace`.
- `docs/user-guide/mcp-server.md` — new `## server_id assignment` section.
- `docs/getting-started/installation.md`, `docs/developer-guide/development.md` — `WATCH_NAMESPACE` row clarified (single namespace, list rejected).
- `CLAUDE.md` — new repository-specific pattern bullet for MCP `server_id`.

---

### Task 1: Pin MCP `server_id` to the sanitized name on CREATE

**Files:**
- Modify: `internal/litellm/mock/mock.go:1348-1358` (POST handler honors supplied `server_id`)
- Modify: `internal/controller/mcpserver_controller.go:614` (createReq) and `:658` (`newServerID`)
- Test: `internal/controller/mcpserver_controller_test.go` (append two tests)
- Docs: `docs/user-guide/mcp-server.md`, `CLAUDE.md`

**Interfaces:**
- Consumes: `litellm.SanitizeMCPServerName(name, sep string) string`; `litellm.MCPServerRequest.ServerID string` (json `server_id,omitempty`, already declared at `internal/litellm/types.go:204`); mock helpers `mockServer.LastMCPBody(name) map[string]any`, `mockServer.GetMCPServerID(name) string`; test harness `mcpServerSampleCR`, `pollMCPServerCondition`, `setupReadyConnectionMCP`, `ensureNoMCPServer`, constant `pathV1MCPServer`.
- Produces: post-change invariant — on CREATE, `body["server_id"] == status.lastRendered.ServerID == litellm.SanitizeMCPServerName(mcp.Name, sep)`.

- [ ] **Step 1: Write the failing CREATE test**

Append to `internal/controller/mcpserver_controller_test.go`:

```go
// TestMCPServerReconciler_CreateUsesNameAsServerID — a freshly-created MCP
// server (empty status → CREATE arm) MUST be sent to LiteLLM with
// server_id == the sanitized server_name, and that value MUST be persisted
// in status.lastRendered.ServerID (not a server-minted UUID). Mirrors the
// Team team_id == metadata.name pattern. Verified live: LiteLLM 1.83.10
// honors a caller-supplied server_id on POST /v1/mcp/server.
func TestMCPServerReconciler_CreateUsesNameAsServerID(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetMCPServers()
	ensureNoMCPServer(t, ctx, "mcp-pin-id-test")
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionMCP(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoMCPServer(t, context.Background(), "mcp-pin-id-test")
	})

	cr := mcpServerSampleCR("mcp-pin-id-test")
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create MCPServer: %v", err)
	}

	m := pollMCPServerCondition(t, ctx, "mcp-pin-id-test", reasonSynced, 30*time.Second)

	wantID := litellm.SanitizeMCPServerName("mcp-pin-id-test", "")

	// The POST /v1/mcp/server body MUST carry server_id == sanitized name.
	body := mockServer.LastMCPBody(wantID)
	if body == nil {
		t.Fatalf("LastMCPBody(%q) is nil — mock did not capture POST body", wantID)
	}
	if id, _ := body["server_id"].(string); id != wantID {
		t.Errorf("body.server_id: want sanitized name %q, got %v", wantID, body["server_id"])
	}

	// The persisted status serverID MUST equal the sanitized name, not a UUID.
	if m.Status.LastRendered.ServerID != wantID {
		t.Errorf("status.lastRendered.serverID: want %q, got %q", wantID, m.Status.LastRendered.ServerID)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `make test-envtest-pkg PKG=./internal/controller/... FOCUS=TestMCPServerReconciler_CreateUsesNameAsServerID`
Expected: FAIL — `body.server_id` is `<nil>` (operator does not send it yet) and `status.lastRendered.serverID` is `mock-mcp-server-id-1` (minted), not the sanitized name.

- [ ] **Step 3: Make the mock honor a caller-supplied `server_id`**

In `internal/litellm/mock/mock.go`, inside the `POST /v1/mcp/server` handler, replace:

```go
		serverName, _ := reqBody["server_name"].(string)
		url, _ := reqBody["url"].(string)
		transport, _ := reqBody["transport"].(string)
		seq := m.mcpSeq.Add(1)
		serverID := fmt.Sprintf("mock-mcp-server-id-%d", seq)
```

with:

```go
		serverName, _ := reqBody["server_name"].(string)
		url, _ := reqBody["url"].(string)
		transport, _ := reqBody["transport"].(string)
		// Honor a caller-supplied server_id — LiteLLM 1.83.10 pins it on
		// POST /v1/mcp/server (verified live, 201 echoes the value). Fall
		// back to a minted UUID when absent, preserving behavior for
		// hand-managed / legacy callers that send no server_id.
		serverID, _ := reqBody["server_id"].(string)
		if serverID == "" {
			seq := m.mcpSeq.Add(1)
			serverID = fmt.Sprintf("mock-mcp-server-id-%d", seq)
		}
```

- [ ] **Step 4: Stamp `server_id` in the controller CREATE arm**

In `internal/controller/mcpserver_controller.go`, in the CREATE branch, change the `createReq` construction (currently first field `ServerName: sanitizedName,`) to add the `ServerID` field as the first field:

```go
		createReq := &litellm.MCPServerRequest{
			ServerID:                  sanitizedName, // pin server_id == sanitized name (CREATE-only; LiteLLM 1.83.10 honors it)
			ServerName:                sanitizedName,
			Alias:                     sanitizedName, // alias = server_name per 1.83.10 (D-7.1-10)
```

(leave every other `createReq` field unchanged.)

- [ ] **Step 5: Pin `newServerID` from the name, not the response**

In the same CREATE branch, change:

```go
		newServerID = result.ServerID
```

to:

```go
		// Pin to the value we supplied, not the create response — keeps
		// status correct even if LiteLLM ever echoes a different id, and
		// mirrors the Team team_id == metadata.name discipline.
		newServerID = sanitizedName
```

- [ ] **Step 6: Run the CREATE test to verify it passes**

Run: `make test-envtest-pkg PKG=./internal/controller/... FOCUS=TestMCPServerReconciler_CreateUsesNameAsServerID`
Expected: PASS.

- [ ] **Step 7: Write the UPDATE regression test**

Append to `internal/controller/mcpserver_controller_test.go`:

```go
// TestMCPServerReconciler_UpdateKeepsExistingServerID — an MCP server that
// already exists in LiteLLM under a (pre-change) server-assigned UUID MUST
// take the UPDATE arm and keep that UUID. The name-as-server_id change is
// CREATE-only; it must never rewrite an existing server's identity.
func TestMCPServerReconciler_UpdateKeepsExistingServerID(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetMCPServers()
	ensureNoMCPServer(t, ctx, "mcp-keep-uuid-test")
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionMCP(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoMCPServer(t, context.Background(), "mcp-keep-uuid-test")
	})

	// Pre-seed LiteLLM with a hand-managed server under a minted UUID, as
	// if it predated the name-as-id change.
	wireName := litellm.SanitizeMCPServerName("mcp-keep-uuid-test", "")
	preUUID := mockServer.AddHandManagedMCPServer(wireName, "https://example.com/mcp", "http")

	cr := mcpServerSampleCR("mcp-keep-uuid-test")
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create MCPServer: %v", err)
	}

	m := pollMCPServerCondition(t, ctx, "mcp-keep-uuid-test", reasonSynced, 30*time.Second)

	// Adoption → UPDATE arm: the server keeps its original UUID, NOT the name.
	if m.Status.LastRendered.ServerID != preUUID {
		t.Errorf("status.lastRendered.serverID: want preserved UUID %q, got %q",
			preUUID, m.Status.LastRendered.ServerID)
	}
	if m.Status.LastRendered.ServerID == wireName {
		t.Errorf("UPDATE arm leaked the name into server_id: got %q", wireName)
	}
}
```

- [ ] **Step 8: Run the full MCP envtest package (no regressions)**

Run: `make test-envtest-pkg PKG=./internal/controller/... FOCUS=TestMCPServer`
Expected: PASS for all `TestMCPServer*` — including the pre-existing `TestMCPServerReconciler_ReservedKeysIgnored` (a user-supplied `server_id: "evil-id"` in `spec.params` is still dropped; the operator stamps the sanitized name) and `TestMCPServerReconciler_CreateOnFirstReconcile` (its `GetMCPServerID(wireName) == status.ServerID` assertion still holds now that both equal the sanitized name).

- [ ] **Step 9: Document `server_id` assignment**

In `docs/user-guide/mcp-server.md`, add a top-level section (place it near the existing structural sections, e.g. before any "Reserved overlay keys" content):

```markdown
## server_id assignment

When the operator **creates a new** LiteLLM MCP server, it sets
`server_id == server_name` (the sanitized `metadata.name`) instead of
letting LiteLLM mint a random UUID. The value is persisted in
`status.lastRendered.serverID` and surfaces verbatim in the LiteLLM
UI / API. Verified against LiteLLM 1.83.10: `POST /v1/mcp/server` honors a
caller-supplied `server_id`.

This change is **CREATE-only**:

- **New servers** (no existing record under the name) → `server_id =
  sanitized server_name`.
- **Existing servers** (adopted by name lookup, including pre-change
  records under a server-assigned UUID) → take the UPDATE arm and **keep
  their original UUID**. The operator never rewrites an existing server's
  identity.

Caveats:

- **No automatic migration.** Existing UUID-keyed servers are not migrated.
  To adopt the name-id, delete the record in LiteLLM once; the operator
  recreates it via `POST /v1/mcp/server` with `server_id = server_name`.
- **Cross-namespace name collision is unhandled by design.** `server_id`
  derives from `metadata.name` with no namespace prefix (single-namespace
  deployment assumed — the operator watches exactly one namespace). Two
  `LiteLLMMCPServer` CRs sharing a name across namespaces would collide;
  v1alpha1 does not guard against this.

> A2A agents are **not** pinnable: LiteLLM 1.83.10 ignores a caller-supplied
> `agent_id` on `POST /v1/agents` and always mints a UUID.
```

- [ ] **Step 10: Add the CLAUDE.md repository-specific pattern bullet**

In `CLAUDE.md`, under "Repository-specific patterns", immediately after the existing **"Team `team_id` = `metadata.name` on CREATE only"** bullet, add:

```markdown
- **MCPServer `server_id` = sanitized `metadata.name` on CREATE only**
  (`mcpserver_controller.go` CREATE arm): new LiteLLM MCP servers get
  `server_id == server_name` (the sanitized name; `SanitizeMCPServerName`),
  pinned in the `POST /v1/mcp/server` body and in
  `status.lastRendered.ServerID` from the name, not the create response.
  EXISTING servers (adopted via `resolveServerIDByName` → UPDATE arm) keep
  their server-assigned UUID — CREATE-arm-only, never touch the UPDATE arm's
  id. Verified live: LiteLLM 1.83.10 honors a caller-supplied `server_id`.
  A2A `agent_id` is NOT pinnable — LiteLLM 1.83.10 ignores a supplied
  `agent_id` on `POST /v1/agents` (`add_agent_to_db` never reads it; Prisma
  mints the UUID), so `a2aagent_controller.go` keeps the server-minted id.
  User-facing docs: `docs/user-guide/mcp-server.md` § "server_id assignment".
```

- [ ] **Step 11: Lint the touched packages**

Run: `make qa-lint-changed`
Expected: no new findings.

- [ ] **Step 12: Commit**

```bash
git add internal/litellm/mock/mock.go internal/controller/mcpserver_controller.go \
        internal/controller/mcpserver_controller_test.go \
        docs/user-guide/mcp-server.md CLAUDE.md
git commit -m "feat(mcpserver): pin server_id to sanitized metadata.name on CREATE

LiteLLM 1.83.10 honors a caller-supplied server_id on POST /v1/mcp/server
(verified live), so new MCP servers get server_id == server_name instead of
a UUID. CREATE-only; the UPDATE arm keeps existing UUIDs. A2A agent_id stays
server-minted (LiteLLM ignores a supplied agent_id).

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 2: Enforce a single watch namespace at startup

**Files:**
- Modify: `cmd/main.go` (imports + `validateWatchNamespace` helper + wiring after `watchNS := envOr(...)`)
- Test: `cmd/main_test.go` (create)
- Docs: `docs/getting-started/installation.md:94`, `docs/developer-guide/development.md:164`

**Interfaces:**
- Consumes: `k8s.io/apimachinery/pkg/util/validation.IsDNS1123Label(string) []string`; existing `watchNS := envOr("WATCH_NAMESPACE", "default")` at `cmd/main.go:115`.
- Produces: `validateWatchNamespace(ns string) error` — nil for a single valid DNS-1123 namespace label, non-nil (and startup abort) otherwise.

- [ ] **Step 1: Write the failing unit test**

Create `cmd/main_test.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package main

import "testing"

func TestValidateWatchNamespace(t *testing.T) {
	tests := []struct {
		name    string
		ns      string
		wantErr bool
	}{
		{"single valid", "litellm-system", false},
		{"default", "default", false},
		{"comma list rejected", "ns1,ns2", true},
		{"space list rejected", "ns1 ns2", true},
		{"empty rejected", "", true},
		{"uppercase rejected", "NS", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateWatchNamespace(tt.ns)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateWatchNamespace(%q) err=%v, wantErr=%v", tt.ns, err, tt.wantErr)
			}
		})
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `make test-unit-pkg PKG=./cmd/...`
Expected: FAIL — `undefined: validateWatchNamespace` (compile error).

- [ ] **Step 3: Add imports to `cmd/main.go`**

In the stdlib import group (lines 6-11), add `"fmt"` and `"strings"`:

```go
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
```

In the k8s import group (after line 18 `utilruntime "k8s.io/apimachinery/pkg/util/runtime"`), add:

```go
	"k8s.io/apimachinery/pkg/util/validation"
```

- [ ] **Step 4: Add the `validateWatchNamespace` helper**

In `cmd/main.go`, immediately after the `envOr` function (ends at line 61), add:

```go
// validateWatchNamespace rejects a WATCH_NAMESPACE that is not a single
// valid Kubernetes namespace. The operator watches exactly ONE namespace
// (cache.Options.DefaultNamespaces carries a single key); a comma- or
// space-separated list is NOT supported and would otherwise be treated as
// one literal namespace that matches nothing — silently watching no CRs.
// Enforcing a DNS-1123 label fails fast on a list-looking value.
func validateWatchNamespace(ns string) error {
	if errs := validation.IsDNS1123Label(ns); len(errs) > 0 {
		return fmt.Errorf("WATCH_NAMESPACE %q is not a single valid namespace "+
			"(the operator watches exactly one namespace, not a list): %s",
			ns, strings.Join(errs, "; "))
	}
	return nil
}
```

- [ ] **Step 5: Wire the guard at startup**

In `cmd/main.go`, directly after:

```go
	watchNS := envOr("WATCH_NAMESPACE", "default")
```

insert:

```go
	if err := validateWatchNamespace(watchNS); err != nil {
		setupLog.Error(err, "invalid WATCH_NAMESPACE; aborting")
		os.Exit(1)
	}
```

- [ ] **Step 6: Run the unit test to verify it passes**

Run: `make test-unit-pkg PKG=./cmd/...`
Expected: PASS (all six sub-tests).

- [ ] **Step 7: Update the env-var docs**

In `docs/getting-started/installation.md` (the `WATCH_NAMESPACE` row, line ~94), change the description cell to:

```markdown
| `WATCH_NAMESPACE`                           | `default` (raw manifest); Helm sets it from `watchNamespace`, which defaults to the install namespace | Single namespace the operator reconciles — exactly one, not a list. A comma/space-separated value is rejected at startup (the operator aborts). Also pins the leader-election lease and `LiteLLMConnection/default`. |
```

In `docs/developer-guide/development.md` (the `WATCH_NAMESPACE` row, line ~164), change the description cell to:

```markdown
| `WATCH_NAMESPACE`                        | Override the operator's watch namespace (exactly one namespace, not a list — a comma/space-separated value is rejected at startup). Code fallback `default` when unset; the Helm chart sets it from `watchNamespace`, which defaults to the install namespace. |
```

- [ ] **Step 8: Lint the touched packages**

Run: `make qa-lint-changed`
Expected: no new findings.

- [ ] **Step 9: Commit**

```bash
git add cmd/main.go cmd/main_test.go \
        docs/getting-started/installation.md docs/developer-guide/development.md
git commit -m "feat(cmd): reject a list-valued WATCH_NAMESPACE at startup

The operator watches exactly one namespace (cache.Options.DefaultNamespaces
single key). A comma/space-separated WATCH_NAMESPACE was silently treated as
one literal namespace matching nothing; now validateWatchNamespace fails fast
via DNS-1123 label validation.

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Self-Review

**Spec coverage:**
- "MCP server_id pinning — implement now (TDD)" → Task 1 (mock fidelity + controller CREATE-arm change + CREATE test + UPDATE regression test + docs).
- "Single-namespace ID-collision assumption — doc note only" → Task 1 Step 9/10 doc notes (mcp-server.md caveat + CLAUDE.md bullet); the existing team.md note already covers `team_id`.
- "Ensure the operator watch namespace is only one, not a list" → Task 2 (startup guard + test + env-var docs). Already single by construction; the guard makes a mistaken list fail fast.
- A2A agent_id explicitly out of scope, documented as not-pinnable (verified at LiteLLM source).

**Placeholder scan:** none — every code/doc step carries literal content and exact run/expected lines.

**Type consistency:** `validateWatchNamespace(string) error` defined in Task 2 Step 4, consumed in Step 5 and tested in Step 1. `MCPServerRequest.ServerID` is pre-existing (`types.go:204`). `litellm.SanitizeMCPServerName(name, sep)` used identically in tests and controller. Mock `server_id` honoring (Task 1 Step 3) is what makes the CREATE test's `GetMCPServerID`/internal-store assertions and downstream steady-state probes resolve to the pinned id consistently.

**Compatibility checked:** `TestMCPServerReconciler_ReservedKeysIgnored` (user `server_id: "evil-id"` still dropped; operator stamps sanitized name → assertion `ServerID != "evil-id" && != ""` still holds) and `TestMCPServerReconciler_CreateOnFirstReconcile` (`GetMCPServerID(wireName) == status.ServerID`, both now the sanitized name) remain green — re-verified by Task 1 Step 8.
