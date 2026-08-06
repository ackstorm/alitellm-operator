# YAGNI Quick Wins — Dead Code + Doc Fixes + Examples Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Delete verified-dead code (~800-1000 LOC), fix stale docs, and add missing examples — first of six plans from the 2026-07-15 YAGNI review (decisions in memory file `simplification-refactor-decisions.md`).

**Architecture:** Pure deletion + documentation sweep. No new behavior, no new abstractions. Every deletion target was verified dead (defined/written but never read in production) by a 5-agent audit. Verification = existing suite stays green + grep-empty for removed symbols.

**Tech Stack:** Go 1.26.4 (via devtools container — host has NO Go), controller-gen, kubebuilder markers, Ginkgo e2e.

## Global Constraints

- Host has no Go toolchain. Run bare `make <target>` — every toolchain target self-routes into the devtools container. NEVER prefix with `./scripts/dev.sh`.
- Every `*.go` file keeps its `// SPDX-License-Identifier: Apache-2.0` first line.
- Docs updated in the SAME commit as the code change they describe (CLAUDE.md documentation-hygiene rule).
- CRD field changes require regen in the same commit: `make gen-code gen-manifests gen-crd-ref-docs helm-sync`.
- Never `git push --no-verify`. The pre-push hook runs the full gate.
- When YOUR deletion orphans a helper/import/test fixture, delete that too. Do NOT delete pre-existing dead code outside this plan's scope.
- Commit messages: conventional commits, short imperative subject <72 chars.

---

### Task 1: Delete 4 never-incremented metric families

**Files:**
- Modify: `internal/metrics/metrics.go` (vars at :71-79, :81-90, :92-100, :129-140; registration lines in `init()` at ~:333-335, ~:338; pre-touch loops at ~:366-383 and the DiscoverySkippedTotal loop below them; orphaned label enums `apiRequestStatuses` :278, `apiErrorStatuses` :281, `allOperations` :258-264, `discoverySkippedReasons` :288-292)
- Modify: `internal/metrics/metrics_test.go:100-105` (registry-membership map entries)

**Interfaces:**
- Consumes: nothing.
- Produces: `internal/metrics` package WITHOUT `ReconcileDurationSeconds`, `LiteLLMAPIRequestDurationSeconds`, `LiteLLMAPIErrorsTotal`, `DiscoverySkippedTotal`. All other families untouched.

- [ ] **Step 1: Prove the four families have zero production callers**

Run:
```bash
grep -rn "ReconcileDurationSeconds\|LiteLLMAPIRequestDurationSeconds\|LiteLLMAPIErrorsTotal\|DiscoverySkippedTotal" --include='*.go' internal/ cmd/ | grep -v "internal/metrics/"
```
Expected: empty output (only definition + test-map references exist). If anything else appears, STOP and report — the audit was wrong.

- [ ] **Step 2: Delete the four var blocks from `internal/metrics/metrics.go`**

Delete these four complete var declarations including their doc comments:
- `var ReconcileDurationSeconds = prometheus.NewHistogramVec(...)` (`reconcile_duration_seconds`)
- `var LiteLLMAPIRequestDurationSeconds = prometheus.NewHistogramVec(...)` (`alitellm_api_request_duration_seconds`)
- `var LiteLLMAPIErrorsTotal = prometheus.NewCounterVec(...)` (`alitellm_api_errors_total`)
- `var DiscoverySkippedTotal = prometheus.NewCounterVec(...)` (`discovery_skipped_total`)

- [ ] **Step 3: Delete their registration + pre-touch + orphaned enums**

In `init()`:
- Remove `ReconcileDurationSeconds,`, `LiteLLMAPIRequestDurationSeconds,`, `LiteLLMAPIErrorsTotal,`, `DiscoverySkippedTotal,` from the `MustRegister(...)` call.
- Delete the pre-touch loops that reference them:
```go
// reconcile_duration_seconds{kind} — 7 combos.
for _, k := range allKinds {
    ReconcileDurationSeconds.WithLabelValues(k)
}
// alitellm_api_request_duration_seconds{operation, status}.
for _, op := range allOperations {
    for _, st := range apiRequestStatuses {
        LiteLLMAPIRequestDurationSeconds.WithLabelValues(op, st)
    }
}
// alitellm_api_errors_total{operation, status}.
for _, op := range allOperations {
    for _, st := range apiErrorStatuses {
        LiteLLMAPIErrorsTotal.WithLabelValues(op, st)
    }
}
```
plus the `DiscoverySkippedTotal` pre-touch loop (uses `discoverySkippedReasons`).
- Then check each of these label enums for remaining users: `allOperations`, `apiRequestStatuses`, `apiErrorStatuses`, `discoverySkippedReasons`. Delete any that are now unreferenced (expected: all four — verify with `grep -n <name> internal/metrics/metrics.go`).

- [ ] **Step 4: Update `internal/metrics/metrics_test.go`**

Remove the four entries from the registry-membership map at :100-105:
```go
"ReconcileDurationSeconds":         ReconcileDurationSeconds,
"LiteLLMAPIRequestDurationSeconds": LiteLLMAPIRequestDurationSeconds,
"LiteLLMAPIErrorsTotal":            LiteLLMAPIErrorsTotal,
"DiscoverySkippedTotal":            DiscoverySkippedTotal,
```
Check the rest of the test file for other assertions on these families (metric-name lists, pre-touch count assertions) and remove those too.

- [ ] **Step 5: Verify build + tests**

Run: `make test-unit-pkg PKG=./internal/metrics/...`
Expected: PASS. Then `make test-unit` — expected PASS.

- [ ] **Step 6: Sweep docs for the four metric names**

Run: `grep -rn "reconcile_duration_seconds\|alitellm_api_request_duration_seconds\|alitellm_api_errors_total\|discovery_skipped_total" docs/ references/ CLAUDE.md --include='*.md' | grep -v site/`
Remove any doc rows describing the deleted metrics (same commit).

- [ ] **Step 7: Commit**

```bash
git add internal/metrics/ docs/ references/
git commit -m "chore(metrics): delete 4 never-incremented metric families"
```

---

### Task 2: Delete typed Team create/update (Raw variants are the only prod path)

**Files:**
- Modify: `internal/litellm/team.go` (delete `CreateTeam` :12-23 and `UpdateTeam` :25-37; KEEP `DeleteTeam`, `CreateTeamRaw`, `UpdateTeamRaw`, `ListTeamsByAlias`)
- Modify: `internal/litellm/types.go` (delete `NewTeamRequest` + `UpdateTeamRequest` struct defs; KEEP `DeleteTeamRequest` — used by `DeleteTeam`)
- Modify: `internal/litellm/team_test.go` (delete/migrate tests exercising the typed variants)
- Check-only: `internal/litellm/mock/mock.go`, `internal/controller/team_controller.go`, `cmd/main.go` (grep hits are expected to be comments/handler routes — adjust comments, do not change behavior)

**Interfaces:**
- Consumes: nothing.
- Produces: `litellm.Client` Team surface = `CreateTeamRaw`, `UpdateTeamRaw`, `DeleteTeam`, `ListTeamsByAlias` only.

- [ ] **Step 1: Map every reference**

Run: `grep -rn "CreateTeam(\|UpdateTeam(\|NewTeamRequest\|UpdateTeamRequest" --include='*.go' internal/ cmd/ test/`
Expected reference classes: definitions (team.go, types.go), tests (team_test.go), comments (team_controller.go ~:138 explains why Raw exists — keep that comment but reword so it no longer names the deleted structs), possibly mock.go route comments. If a PRODUCTION call site appears, STOP and report.

- [ ] **Step 2: Delete `CreateTeam` and `UpdateTeam` from `internal/litellm/team.go`**

Delete both methods including doc comments. Update `CreateTeamRaw`'s doc comment — it currently says "The typed CreateTeam helper drops nil pointers via `,omitempty` JSON tags"; reword to past tense without referencing a now-nonexistent symbol, e.g.:
```go
// CreateTeamRaw issues POST /team/new with a freeform map[string]any body.
//
// The operator's "clearing budget" wire contract (spec §6.7 line 1194)
// requires explicit JSON null for absent budget keys; a typed struct with
// `,omitempty` tags would drop them, so the reconciler builds the body as
// a map and posts it verbatim.
```

- [ ] **Step 3: Delete `NewTeamRequest` + `UpdateTeamRequest` from `internal/litellm/types.go`**

Delete both struct definitions with doc comments. Keep `DeleteTeamRequest` and `TeamListEntry`/`TeamListResponse`.

- [ ] **Step 4: Fix tests**

In `internal/litellm/team_test.go`: tests covering `CreateTeam`/`UpdateTeam` wire behavior — if an equivalent Raw-variant test already exists, delete the typed test; if the typed test is the ONLY coverage of a wire assertion (e.g. response decode), port it to `CreateTeamRaw`/`UpdateTeamRaw` with a `map[string]any` body.

- [ ] **Step 5: Verify**

Run: `make test-unit-pkg PKG=./internal/litellm/...`
Expected: PASS. Then `grep -rn "NewTeamRequest\|UpdateTeamRequest" --include='*.go' . | grep -v .gocache` — expected empty.

- [ ] **Step 6: Commit**

```bash
git add internal/litellm/
git commit -m "chore(litellm): delete unused typed Team create/update; Raw variants are the sole path"
```

---

### Task 3: Delete the auth-header escape hatch (`LITELLM_OPERATOR_AUTH_HEADER`)

**Files:**
- Modify: `internal/litellm/client.go` (delete `authHeaderKind` type + `AuthBearer`/`AuthXLiteLLMAPIKey` consts :20-35, `EnvAuthHeader` :37-41, `defaultAuthHeader` :43-45, `authHeader` struct field :55, the env `switch` in `NewClient` :124-131, and the header-selection branch in `makeRequest` — replace with a single unconditional `Authorization: Bearer`)
- Modify: `internal/litellm/client_test.go` (2 references — delete the x-litellm-api-key test case)
- Modify: `deploy/helm/alitellm-operator/values.yaml` (~:86 mentions the env var in a comment — remove the mention)

**Interfaces:**
- Consumes: nothing.
- Produces: `Client` always sends `Authorization: Bearer <masterKey>`.

- [ ] **Step 1: Locate the header-set site in makeRequest**

Run: `grep -n "authHeader\|x-litellm-api-key\|Authorization" internal/litellm/client.go`
The request-construction code switches on `c.authHeader`. Note the exact lines.

- [ ] **Step 2: Delete the enum machinery and collapse the header set**

Delete the type, consts, struct field, and `NewClient` env switch listed above. In `makeRequest`, replace the switch/branch with the single line it defaults to:
```go
req.Header.Set("Authorization", "Bearer "+c.masterKey)
```
Remove the now-unused `os` import from client.go if nothing else uses it (check with `grep -n '"os"' internal/litellm/client.go` and `grep -n "os\." internal/litellm/client.go`).

- [ ] **Step 3: Fix tests + values.yaml comment**

Delete the `x-litellm-api-key` test case(s) in `client_test.go`. Remove the `LITELLM_OPERATOR_AUTH_HEADER` mention from `values.yaml` (comment-only). Sweep docs: `grep -rn "LITELLM_OPERATOR_AUTH_HEADER\|x-litellm-api-key" docs/ references/ CLAUDE.md --include='*.md' | grep -v site/` — remove stale mentions.

- [ ] **Step 4: Verify**

Run: `make test-unit-pkg PKG=./internal/litellm/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/litellm/ deploy/helm/ docs/ references/
git commit -m "chore(litellm): delete LITELLM_OPERATOR_AUTH_HEADER escape hatch; Bearer always"
```

---

### Task 4: Delete providers/deps.go + registry alias wrappers

**Files:**
- Delete: `internal/providers/deps.go`
- Modify: `internal/providers/registry.go` (map points at `new*Impl` directly; delete the 4 alias funcs :69-95 and the "constructors below are thin aliases" comment block :65-67)

**Interfaces:**
- Consumes: `newAnthropicImpl`/`newGeminiImpl`/`newOpenAIImpl`/`newElevenLabsImpl` (defined in their provider files, unchanged), `newKubeAI` (real constructor in kubeai.go, unchanged), `newBedrock` (real constructor in bedrock.go, unchanged).
- Produces: `Registry` map semantics unchanged; `Lookup` + `registryMu` + test seams unchanged.

- [ ] **Step 1: Verify deps.go is redundant**

Run: `grep -n "aws-sdk-go-v2" internal/providers/bedrock.go internal/providers/deps.go`
Expected: bedrock.go imports `config`, `credentials`, `service/bedrock` directly (lines ~13-16); deps.go blank-imports the same three. Then delete `internal/providers/deps.go`.

- [ ] **Step 2: Repoint the Registry map and delete the aliases**

In `registry.go`, change the map literal to:
```go
var Registry = map[string]func(ctx context.Context, cfg ProviderConfig) (Provider, error){
	"anthropic":  newAnthropicImpl,
	"bedrock":    newBedrock,
	"elevenlabs": newElevenLabsImpl,
	"gemini":     newGeminiImpl,
	"kubeai":     newKubeAI,
	"openai":     newOpenAIImpl,
}
```
Delete the four alias functions `newAnthropic`, `newGemini`, `newOpenAI`, `newElevenLabs` and their doc comments, plus the "thin aliases" intro comment. Keep `Lookup`, `registryMu`, and the 04-03b/04-03c notes about `newKubeAI`/`newBedrock` (still accurate). Trim the map's doc comment sentences that narrate the fill-in history ("04-03a fills...") — keep the D-01 dispatch rule paragraph.

- [ ] **Step 3: Verify**

Run: `make test-unit-pkg PKG=./internal/providers/...`
Expected: PASS (17 `RegisterTestProvider` call sites and `SetTestBaseURL` unaffected).

- [ ] **Step 4: Commit**

```bash
git add internal/providers/
git commit -m "chore(providers): delete redundant deps.go and registry alias wrappers"
```

---

### Task 5: Delete dead `connection.Cache.log` field and `ConnectionSnapshot.Generation`

**Files:**
- Modify: `internal/connection/cache.go` (delete `log logr.Logger` field :86-88, drop the `log` param from `NewCache` :96-104, delete `placeholder.Generation = cur.Generation` at :240 + the WR-04 Generation sentences in the `InvalidateOn401` doc comment :205-210)
- Modify: `internal/connection/snapshot.go` (delete the `Generation` field + its doc comment)
- Modify: `internal/controller/litellmconnection_controller.go` (remove `Generation: conn.Generation,` from the ~8 `ConnectionSnapshot{...}` literals at :214, :299, :320, :346, :367, :387, :449 and any others `grep -n "Generation: conn.Generation" internal/controller/litellmconnection_controller.go` finds — NOTE: `conn.Status.ObservedGeneration != conn.Generation` at :274 is the CR's generation, NOT the snapshot field; leave it)
- Modify: `cmd/main.go:202` (`NewCache(...)` call — drop the logger arg)
- Modify: `internal/connection/cache_test.go` + any suite tests calling `NewCache(` (drop the arg) or asserting `.Generation` on snapshots (delete those assertions)

**Interfaces:**
- Consumes: nothing.
- Produces: `func NewCache() *Cache`; `ConnectionSnapshot` without `Generation`.

- [ ] **Step 1: Prove Generation has no dependent readers**

Run: `grep -rn "snap.Generation\|Snapshot().Generation\|\.Generation" --include='*.go' internal/controller/ internal/connection/ | grep -v "conn.Generation\|ObservedGeneration\|conn.GetGeneration"`
Expected: only `snapshot.go` (definition), `cache.go:240` (placeholder copy), controller struct-literal write sites, and possibly test assertions. No dependent-reconciler reads. If a read appears in model/team/mcp/a2a/guardrail/modelalias controllers, STOP and report.

- [ ] **Step 2: Delete the field, the log field, and fix the constructor**

New constructor:
```go
// NewCache constructs a *Cache with an empty (nil) snapshot pointer and a
// cap=1 event channel. No probe has yet completed; Snapshot on the
// returned Cache returns the zero-value ConnectionSnapshot (D-04 "do not
// mutate" signal).
func NewCache() *Cache {
	return &Cache{
		ch: make(chan event.GenericEvent, 1),
	}
}
```
Remove the `logr` import from cache.go if now unused. In `InvalidateOn401`, the placeholder becomes:
```go
placeholder := ConnectionSnapshot{
	Ready:  false,
	Reason: "BadMasterKey",
}
if cur := c.snapshot.Load(); cur != nil {
	placeholder.Client = cur.Client
}
```
Update the WR-04 comment sentences that explain Generation preservation (keep the Client-preservation rationale).

- [ ] **Step 3: Fix all callers**

Run: `grep -rln "NewCache(" --include='*.go' cmd/ internal/ | xargs grep -n "NewCache("` — update every call to `connection.NewCache()`. Remove `Generation:` lines from the connection controller literals. Delete test assertions on `.Generation`.

- [ ] **Step 4: Verify**

Run: `make test-unit` then `make test-envtest-pkg PKG=./internal/controller/... FOCUS=TestConnection`
Expected: PASS both.

- [ ] **Step 5: Commit**

```bash
git add internal/connection/ internal/controller/ cmd/
git commit -m "chore(connection): delete dead Cache.log field and unused Snapshot.Generation"
```

---

### Task 6: Remove no-op seams in the discovery controllers

**Files:**
- Modify: `internal/controller/modeldiscovery_controller.go` (`transientApierror` :954-966+ and its call sites :711, :764, :801, :807; `sanitizeError` :1396-1415+ and its call sites)
- Modify: `internal/controller/mcpserverdiscovery_controller.go` (its `sanitizeError`/`transientApierror` calls at :454-469, :721, :750, :774)

**Interfaces:**
- Consumes: nothing.
- Produces: unchanged behavior — this task only removes documented no-ops and inlines a pass-through.

- [ ] **Step 1: Classify each call site before touching anything**

Read the code around each grep hit:
```bash
grep -n "transientApierror\|sanitizeError" internal/controller/modeldiscovery_controller.go internal/controller/mcpserverdiscovery_controller.go
```
Rules:
- A `_ = transientApierror(err)` discard (the :801 site's comment says "retained as documentation of the future split") → DELETE the line and its comment.
- A REAL use of `transientApierror` (its bool feeds the OBS-04 `child_cr_writes_total` result label) → KEEP the function and that call.
- `sanitizeError(err)` returns `err.Error()` verbatim ("seam reserved for future surfaces") → inline `err.Error()` at every call site in BOTH files, then delete the function(s). If mcpserverdiscovery has its own copy, delete that too.

- [ ] **Step 2: Apply the deletions/inlines per the classification**

If after removing discards `transientApierror` has zero remaining callers, delete it as well; if it has real callers, leave it (it is then not dead).

- [ ] **Step 3: Verify**

Run: `make test-unit && make qa-lint-changed`
Expected: PASS / no new lint findings.

- [ ] **Step 4: Commit**

```bash
git add internal/controller/
git commit -m "chore(discovery): drop no-op seams (sanitizeError pass-through, documented discards)"
```

---

### Task 7: Delete informational-only `paramsKeys`/`infoKeys`/`agentCardKeys` status fields (Model keeps its)

**Files:**
- Modify: `api/litellm/v1alpha1/team_types.go:275-284` (delete `ParamsKeys` from `TeamLastRenderedStatus`)
- Modify: `api/litellm/v1alpha1/mcpserver_types.go:206-215` (delete `ParamsKeys` from `MCPServerLastRenderedStatus`)
- Modify: `api/litellm/v1alpha1/a2aagent_types.go:205-220` (delete `ParamsKeys` + `AgentCardKeys` from `A2ALastRenderedStatus`)
- Modify: `api/litellm/v1alpha1/litellmguardrail_types.go:245-273` (delete `ParamsKeys` + `InfoKeys` AND the godoc paragraph promising a "safer delete-and-recreate path" that was never implemented)
- Modify: write sites — `internal/controller/team_controller.go:681`, `internal/controller/mcpserver_controller.go:725` (+ the explanatory note at :125), `internal/controller/a2aagent_controller.go:569-570`, `internal/controller/litellmguardrail_controller.go:488-489`
- DO NOT TOUCH: `api/litellm/v1alpha1/model_types.go` `ParamsKeys`/`InfoKeys`/`InfoHash` — load-bearing (shrinkage → delete-and-recreate, model_controller.go:800-812)
- Regenerate: `zz_generated.deepcopy.go`, `config/crd/bases/*.yaml`, `deploy/helm/alitellm-operator/crd-sources/*.yaml`, `docs/api-reference/`

**Interfaces:**
- Consumes: nothing.
- Produces: Team/MCP/A2A/GuardRail `lastRendered` status = `{hash, <kind>ID, at}` only.

- [ ] **Step 1: Prove the fields are write-only on these four kinds**

```bash
grep -rn "ParamsKeys\|InfoKeys\|AgentCardKeys" --include='*.go' internal/controller/ | grep -v model_controller | grep -v _test
```
Expected: only the four write sites listed above (plus possibly helper computation feeding them). Any READ outside model_controller.go → STOP and report.

- [ ] **Step 2: Delete fields from the four types files**

Remove the struct fields + doc comments. In `litellmguardrail_types.go`, also delete the overpromising godoc paragraph (:245-249). Leave `Hash`, `TeamID`/`ServerID`/`AgentID`/`GuardrailID`, `DefinitionLocation`, `PoolSize`, `At` untouched.

- [ ] **Step 3: Delete the write sites + orphaned helpers**

Remove the assignments at the four controller sites. Then check whether the key-extraction expressions they used (e.g. a `sortedKeys(...)`-style helper or inline `maps`/`sort` loops) became orphaned — delete only what YOUR removal orphaned. Update mcpserver_controller.go:125's explanatory comment (it documents the field as informational — now gone).

- [ ] **Step 4: Fix tests**

```bash
grep -rn "ParamsKeys\|InfoKeys\|AgentCardKeys" --include='*_test.go' internal/ test/ | grep -v model
```
Delete assertions on the removed fields (envtest status assertions). Keep Model-side tests.

- [ ] **Step 5: Regenerate all derived artifacts**

Run: `make gen-code gen-manifests gen-crd-ref-docs helm-sync`
Expected: `zz_generated.deepcopy.go`, CRD YAMLs under `config/crd/bases/` and `deploy/helm/.../crd-sources/`, and `docs/api-reference/` all updated. `git status` shows only expected regenerated files.

- [ ] **Step 6: Verify**

Run: `make test-unit && make test-envtest`
Expected: PASS. (Envtest matters here — CRD schema changed.)

- [ ] **Step 7: Commit**

```bash
git add api/ internal/ config/ deploy/helm/ docs/api-reference/
git commit -m "chore(api): drop write-only paramsKeys/infoKeys status fields from Team/MCP/A2A/GuardRail"
```

---

### Task 8: Fix stale docs — separator default, 17→18 gates

**Files:**
- Modify: `api/litellm/v1alpha1/mcpserver_types.go:42` (godoc: "(default `-`)" → "(default `.`)")
- Modify: `examples/example-deploy/01-litellmconnection.yaml:51` (commented example — make the comment state default `.` and that `-` is the non-stock override)
- Modify: `Makefile:300`, `scripts/install-hooks.sh:6`, `references/makefile.md:240`, `docs/developer-guide/contributions.md:83` ("17-gate"/"17 gates" → 18)
- Regenerate: CRD YAML + api-reference (godoc change flows into both)

**Interfaces:** none — docs only.

- [ ] **Step 1: Fix the godoc**

`api/litellm/v1alpha1/mcpserver_types.go:42`: change
```
// LiteLLMConnection's `spec.mcpToolPrefixSeparator` (default `-`), swapping
```
to
```
// LiteLLMConnection's `spec.mcpToolPrefixSeparator` (default `.`), swapping
```
(The actual kubebuilder default is `.` — deliberate since v0.2.0, FIX2 HIGH-1, validated against LiteLLM v1.85.1's stock validator. Do NOT change the default itself.)

- [ ] **Step 2: Fix the example comment**

In `examples/example-deploy/01-litellmconnection.yaml`, make the commented line + surrounding comment read:
```yaml
  # mcpToolPrefixSeparator: "-"   # default is "."; set "-" ONLY for a
  #                               # non-stock LiteLLM whose validator
  #                               # rejects "-" in server_name
```

- [ ] **Step 3: Fix the gate count in all four locations**

Replace "17-gate" → "18-gate" in `Makefile:300` and `scripts/install-hooks.sh:6` and `references/makefile.md:240`; "(17 gates)" → "(18 gates)" in `docs/developer-guide/contributions.md:83`. (Gate 14b helm-sync-drift is the uncounted one. NOTE: if the helm-only plan later removes gate 14b, the count reverts — acceptable, that plan owns the update.)

- [ ] **Step 4: Regenerate + verify**

Run: `make gen-manifests gen-crd-ref-docs helm-sync`
Then: `grep -rn "default \`-\`" api/ docs/api-reference/ config/ deploy/helm/ | grep -v site/` — expected empty.

- [ ] **Step 5: Commit**

```bash
git add api/ config/ deploy/helm/ docs/ examples/ Makefile scripts/ references/
git commit -m "docs: fix stale separator default and pre-push gate count"
```

---

### Task 9: Document `LITELLM_OPERATOR_REQUIRE_HTTPS_REMOTE`

**Files:**
- Modify: `deploy/helm/alitellm-operator/values.yaml` (add to the `extraEnv` example block around :95)
- Modify: `docs/user-guide/connection.md` (add a "Transport security" subsection near the existing M-SEC2 material)

**Interfaces:** none — docs only. The flag is already implemented (`litellmconnection_controller.go:81`, checked at :379) and tested.

- [ ] **Step 1: values.yaml**

Extend the commented `extraEnv` example (~:95) with:
```yaml
#   extraEnv:
#     # Hard-reject a plaintext http:// endpoint on a REMOTE LiteLLM
#     # (in-cluster *.svc / loopback endpoints stay exempt). Default:
#     # warn-only log marker MasterKeyOverPlaintextHTTP.
#     - name: LITELLM_OPERATOR_REQUIRE_HTTPS_REMOTE
#       value: "true"
```

- [ ] **Step 2: docs/user-guide/connection.md**

Add under the endpoint/security discussion:
```markdown
### Enforcing HTTPS for remote endpoints

By default a remote `http://` endpoint only logs a warning
(`MasterKeyOverPlaintextHTTP`) — in-cluster `http://litellm.<ns>.svc`
is the common, acceptable deployment. To hard-reject plaintext-HTTP
remotes instead (`Ready=False`, `reason=InsecureEndpoint`, terminal
until `spec.endpoint` is edited):

```bash
kubectl set env -n litellm-system deploy/alitellm-operator \
  LITELLM_OPERATOR_REQUIRE_HTTPS_REMOTE=true
```

Loopback, `*.svc`, and bare service-name hosts are always classified
in-cluster and exempt (see `litellm.ClassifyEndpointTransport`).
```

- [ ] **Step 3: Verify docs build**

Run: `make docs-build`
Expected: mkdocs build succeeds, no broken-link warnings for the touched page.

- [ ] **Step 4: Commit**

```bash
git add deploy/helm/ docs/
git commit -m "docs: surface LITELLM_OPERATOR_REQUIRE_HTTPS_REMOTE in values and user guide"
```

---

### Task 10: Examples — elevenlabs + unexercised knobs

**Files:**
- Create: `examples/example-deploy/12-modeldiscovery-elevenlabs.yaml` (adapt from `config/samples/modeldiscovery-elevenlabs.yaml`; check `ls examples/example-deploy/` first and use the next free number prefix — if 12 is taken, take the next)
- Modify: `examples/example-deploy/01-litellmconnection.yaml` (uncomment/add the tuning knobs with explanatory comments)
- Modify: the existing guardrail, mcpserverdiscovery, mcpserver, and a2aagent example files in `examples/` (add unexercised fields, commented where they change behavior)

**Interfaces:** none — example YAML only. Every field added MUST exist in the CRD schema (verify against `docs/api-reference/`).

- [ ] **Step 1: Elevenlabs example**

Copy `config/samples/modeldiscovery-elevenlabs.yaml` → `examples/example-deploy/<NN>-modeldiscovery-elevenlabs.yaml`. Adjust metadata to match the examples convention (drop the `managed-by: kustomize` label; match neighboring examples' labels/namespace). Keep the CEL-rule comment block and the `STORE_MODEL_IN_DB` note.

- [ ] **Step 2: Connection tuning knobs**

In `01-litellmconnection.yaml`, add (commented, with the real defaults stated):
```yaml
  # --- optional tuning (defaults shown) ---
  # requeueOnRejectedAfter: "5m"   # retry cadence after a LiteLLM 4xx
  # maxRequestsPerSecond: 0        # 0 = unlimited outbound HTTP to LiteLLM
  # maxBurst: 0                    # token-bucket burst (only with maxRequestsPerSecond > 0)
```
Verify each name + default against `docs/api-reference/litellm.ackstorm.ai.md` before writing (the defaults above must be corrected if the api-reference disagrees — the api-reference is authoritative).

- [ ] **Step 3: Remaining knob examples**

- GuardRail example: add a second sample (or extend) showing `policyTemplate: <value from api-reference enum>` and a multi-element `mode: [pre_call, post_call]`.
- MCPServerDiscovery example: add commented `toolhive: { namespaces: [...], kinds: [MCPServer] }` and a `filters: { include: [...] }` block.
- MCPServer + A2AAgent examples: add commented `deletionPolicy: Delete` with the one-line explanation ("default Orphan leaves the LiteLLM entry behind on CR delete").
- Team example: add a commented `secrets:` block mirroring the Model example's shape.
Every field: check spelling against `docs/api-reference/` — do not invent enum values; if `policyTemplate` has no documented enum, use the value from the guardrail controller's validation (`grep -n policyTemplate internal/controller/litellmguardrail_controller.go api/litellm/v1alpha1/litellmguardrail_types.go`).

- [ ] **Step 4: Validate the YAML against the live schema**

Run: `make cluster-up` (if no kept cluster exists) then:
```bash
./scripts/dev.sh kubectl apply --dry-run=server -f examples/example-deploy/
```
Expected: every file passes server-side validation (CEL + schema). Commented-out fields obviously aren't validated — for each knob also do one temporary uncommented `--dry-run=server` apply of the modified files, then re-comment.

- [ ] **Step 5: Commit**

```bash
git add examples/
git commit -m "docs(examples): add elevenlabs discovery and surface unexercised spec knobs"
```

---

### Task 11: Final gate

**Files:** none new.

- [ ] **Step 1: Full local verification**

Run in order:
```bash
make qa-lint
make test-unit
make test-envtest
make e2e-full        # CRD schema changed in Task 7 → e2e required per CLAUDE.md
```
Expected: all PASS. (Local envtest flakiness caveat: mass ~30s poll-timeouts on a starved host are environmental — verify on CI's Envtest job if that shape appears.)

- [ ] **Step 2: Push via hook (runs the 18-gate pre-push automatically)**

Branch + PR per repo convention (branch protection on main):
```bash
git checkout -b chore/yagni-quick-wins
git push -u origin chore/yagni-quick-wins
gh pr create --fill
```
Expected: pre-push hook green, then CI (Lint/Unit/Envtest/Security/E2E) green on the PR.

---

## Self-Review Notes

- **Spec coverage:** all approved dead-code items from the decisions memo are covered EXCEPT `RegisteredGVKs()` (deferred to the ToolHive v1beta1 plan — informer is rewritten there; double-touching wastes a review cycle). Consolidation, relist, fan-out, Team-default, helm-only are Plans 2-6, not this plan.
- **Deletion-safety pattern:** every task starts with a grep step that re-proves deadness before deleting; a surprise production reference is a STOP-and-report, not an improvisation.
- **Type consistency:** `NewCache()` (Task 5) matches all listed callers; no task references symbols another task deletes (Task ordering: metrics/team/auth/providers are independent; Task 7 regen runs before Task 8's regen — both idempotent).
