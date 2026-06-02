# Full code review — 2026-06-02

**Scope:** whole repo. 203 Go files, ~22k LOC non-test. Kubernetes operator,
controller-runtime v0.19.4 → LiteLLM HTTP API. Reviewed 8 package clusters in
parallel (model / team+a2a / mcp / guardrail+connection / controller-helpers+connection-pkg /
litellm+providers / api-types+cmd / toolhive+metrics+filters).

**Verdict:** solid overall — careful error classification, transport-layer
redaction, finalizer logic. One real recurring bug class (deletion-path
connection gating). Findings below, severity-ordered.

Format: `file:Lline: severity: problem. fix.`

---

## 🔴 Bugs

### #74 fix incomplete — deletion paths gate on `snap.Ready`, not `snap.Usable()`

PR #74 fixed the main reconcile paths but left every controller's
finalizer/deletion branch on the bare `.Ready` flag. `Cache.Rebuild` does NOT
enforce the `Ready=true ⇒ Client!=nil` invariant, so a poisoned
`{Ready:true, Client:nil}` snapshot reaches these branches and nil-derefs
`snap.Client.Delete*` → controller-runtime-recovered panic, poisoned reconcile,
CR stuck `Terminating`. Same class, 4 sites:

- `internal/controller/model_controller.go:L193`: 🔴 bug: delete branch `if snap.Ready` derefs `snap.Client.DeleteModel` / `GetModelInfoByName`. Change to `if snap.Usable()`.
- `internal/controller/a2aagent_controller.go:L201`: 🔴 bug: `if snap.Ready` then `resolveAgentIDByName(ctx, snap.Client, ...)` + `snap.Client.DeleteAgent`. Change to `snap.Usable()`.
- `internal/controller/mcpserver_controller.go:L200`: 🔴 bug: `if snap.Ready` then `resolveServerIDByName(... snap.Client ...)` + `snap.Client.DeleteMCPServer`. Change to `snap.Usable()`.
- `internal/controller/litellmguardrail_controller.go:L178`: 🔴 bug: `if snap.Ready && guardrailID != ""` then `snap.Client.DeleteGuardrail`. Change to `snap.Usable() && guardrailID != ""`.

Reference shape: `team_controller.go` already uses `snap.Usable()` everywhere
(L272 / L698 / L879 / L1007).

### Other bugs

- `internal/metrics/deletion_blocked_collector.go:L77-89`: 🔴 bug: `splitDeletionBlockedKey` index-overflows `parts[i]` if any kind/namespace/name contains a `\x00` byte (the separator). >2 NULs walks `i` past 2 → panic. Fix: `strings.SplitN(k, "\x00", 3)`.
- `internal/toolhive/informer.go:L503-516`: 🔴 bug: `List` returns empty list + **nil error** when *both* version queries fail. Caller treats empty as "no objects exist" → can delete all child CRs. Fix: if every version errored, return the last error, not an empty list.

---

## 🟡 Risks

- `internal/metrics/metrics.go:L40-47`: 🟡 risk: `LitellmOperatorReconcileTotal` carries a `namespace` label with no pre-touch / cap → unbounded series in multi-tenant clusters (cardinality explosion). Drop the label or document + cap.
- `internal/controller/mcpserver_controller.go:L663-688` + `internal/litellm/types.go`: 🟡 risk: `OAuth2Flow` is extracted (`params_mcp.go:L49`) and sent on CREATE but `MCPServerUpdateRequest` has no `OAuth2Flow` field → silently dropped on every UPDATE. Add field + wire into the update constructor.
- `internal/filters/filters.go:L118-131`: 🟡 risk: `compileAnchored(f.Exclude, ...)` runs before the include-match loop, so a bad exclude regex returns `InvalidConfig` even when includes matched zero candidates — spec says `UpstreamInvalid` should win. Move exclude compile after the include-match block.
- `internal/controller/modelalias_controller.go:L128-137`: 🟡 risk: `GetRouterSettings` / `UpdateRouterSettings` errors all routed to `broadcastNotReady` + `RequeueAfter` regardless of type → transient 5xx/network errors return `Result{RequeueAfter}` not `Result{}, err`, suppressing controller-runtime backoff. Outage recovery stalls at the requeue floor. Split deterministic 4xx from transient via `errors.As`.
- `internal/controller/model_controller.go:L748-765`: 🟡 risk: `classifyMutationError` detects 4xx via hand-rolled string-prefix loops instead of `errors.As(err, &rej); rej.Status >= 400 && < 500`. Fragile; typed `*litellm.RejectedError` exists for exactly this.
- `internal/controller/connection_fanin.go:L75-80`: 🟡 risk: `c.List` / `ExtractList` errors silently dropped (return nil, no log). Transient API error drops all dependent re-enqueues → dependents stall until own backoff. Log at `V(1)` like `secretToConnection` does.
- `internal/litellm/endpoint.go:L130-138`: 🟡 risk: `isClusterLocalHost` rejects the two-label `<service>.<namespace>` form (e.g. `http://litellm.default:4000`) → spurious `MasterKeyOverPlaintextHTTP` warn, or `Ready=False` under `REQUIRE_HTTPS_REMOTE`. Accept exactly-one-dot short names.
- `internal/litellm/list_cache.go:L111-121`: 🟡 risk: single-flight waiters that unblock with `out==nil` (leader skipped store on epoch advance) fall through to a direct `ListMCPServers`, bypassing the dedup guard → thundering herd under mutation storms. Low blast radius.
- `internal/controller/modeldiscovery_controller.go:L835`: 🟡 risk: `Status.GeneratedChildren` excludes `classifiedSkip` children, so a prior-applied child reappearing as skip gets dropped → re-classified `create` next pass (OBS-04 metric noise, not correctness).

---

## 🔵 Docs / nits

- `api/litellm/v1alpha1/mcpserverdiscovery_types.go:L109,L171,L354`: 🔵 nit (borderline 🟡): filter-target godoc says `<discovery-name>.<toolhive-namespace>.<toolhive-name>` (dotted three-part) but post-v0.3.0 the reconciler filters `<spec.prefix>-<source-name>` (`mcpserverdiscovery_controller.go:486`). User filters against the documented format never match. Fix all three sites + rename the stale `dotted` local in the reconciler.
- `internal/controller/safety_relist.go:L81-88`: 🔵 nit: `SetSafetyRelistIntervals` covers 4 reconcilers; GuardRail is swept by `BootSweeper` but has no relist interval — fine today, no signal it's intentionally omitted.
- `internal/controller/rejected_message.go:L48`: 🔵 nit: `claude-[A-Za-z0-9_\-]{8,}` redaction regex would also clobber model names like `claude-3-opus-20240229` if they land in `rej.Code` / `rej.Type`. Unlikely; "non-exhaustive by design."
- `api/litellm/v1alpha1/modeldiscovery_types.go:L336` + `mcpserverdiscovery_types.go:L278`: 🔵 nit: `+optional` on `int32` value fields is misleading (never absent in JSON; default 0). Cosmetic.

---

## Clean

`team_controller.go`, `team_default_runnable.go`, `mcpserverdiscovery_controller.go`,
`params_mcp.go`, `litellmconnection_controller.go`, `litellmconnection_logging.go`,
all of `internal/connection/`, `internal/identity/`, controller helpers
(`bootsweep` / `predicates` / `vanish_probe` / `status_log` / `secret_resolve` /
`ratelimit` / `mappers` / `writestatus_helpers`), `cmd/main.go`, all CRD types
except the mcpserverdiscovery godoc, `internal/litellm/{client,transport}.go` +
`internal/providers/{bedrock,openai}.go` (body-close paths correct),
`internal/substitution`, `internal/normalize`.

---

## Suggested fix order

1. The 4 deletion-path `snap.Ready` → `snap.Usable()` derefs (same root as #74, one-line each).
2. `deletion_blocked_collector.go` NUL-split panic.
3. `toolhive/informer.go` both-versions-failed nil-error.
4. `OAuth2Flow` UPDATE drop + `metrics.go` namespace-label cardinality.
5. Risks/nits as capacity allows.

---

## Independent cross-validation — Codex (gpt-5.5), 2026-06-02

Ran OpenAI Codex CLI 0.136.0 headless against the live tree to verify every
finding at its cited file:line. **Result: all 16 findings CONFIRMED (1 PARTIAL),
zero false positives, zero false negatives.**

| # | Finding | Verdict | Codex note |
|---|---------|---------|------------|
| F1 | model_controller.go:L193 | CONFIRMED | `if snap.Ready` derefs `snap.Client.DeleteModel` (L197) + `GetModelInfoByName` (L227). |
| F2 | a2aagent_controller.go:L201 | CONFIRMED | `if snap.Ready` → `resolveAgentIDByName(...,snap.Client)` (L206) + `DeleteAgent` (L211). |
| F3 | mcpserver_controller.go:L200 | CONFIRMED | `if snap.Ready` → `resolveServerIDByName(...,snap.Client)` (L208) + `DeleteMCPServer` (L213). |
| F4 | litellmguardrail_controller.go:L178 | CONFIRMED | `if snap.Ready && guardrailID != ""` derefs `DeleteGuardrail` (L179). |
| F5 | team_controller.go (reference) | CONFIRMED | Correct: `snap.Usable()` at L272/L698/L879/L1007. |
| F6 | deletion_blocked_collector.go:L77-89 | CONFIRMED | manual NUL splitter, no bounds check; >2 NULs overflow `parts[i]`. |
| F7 | toolhive/informer.go:L503-565 | CONFIRMED | both versions `continue` on error → returns `nil, nil` (L565). |
| F8 | metrics.go:L40-47 | PARTIAL | `namespace` label real; single-watch-namespace design mitigates explosion. |
| F9 | types.go + mcpserver_controller.go | CONFIRMED | `OAuth2Flow` on CREATE struct/req, absent from UPDATE struct + req. |
| F10 | filters/filters.go:L118-159 | CONFIRMED | exclude compiled (L128) before include match (L139-158). |
| F11 | modelalias_controller.go:L128-136 | CONFIRMED | both router errors → `broadcastNotReady` + `RequeueAfter`, no 4xx/5xx split. |
| F12 | model_controller.go:L748-765 | CONFIRMED | hand-rolled prefix loop 400-499, not `errors.As`. |
| F13 | connection_fanin.go:L75-80 | CONFIRMED | `List`/`ExtractList` errors `return nil` with no log. |
| F14 | litellm/endpoint.go:L130-138 | CONFIRMED | only `.svc`/`.svc.cluster.local` accepted; `litellm.default` → false. |
| F15 | litellm/list_cache.go:L104-121 | CONFIRMED | nil-out waiter calls `ListMCPServers` (L120) outside inflight guard. |
| F16 | modeldiscovery_controller.go:L835 | CONFIRMED | skip-classified children `continue` before `generated = append`. |

**False negatives:** none. Codex swept all finalizer/deletion paths — `litellmconnection`
(no HTTP call), `modelalias` (gates `Usable()`), `modeldiscovery`/`mcpserverdiscovery`
(no client call), `team` (`Usable()`) — found no additional `snap.Ready`-gated derefs.

**Rejected findings:** none. Severity, scope, citations all accurate; fix order endorsed.

**Reachability of F1-F4 panic:** confirmed — `Cache.Rebuild` does not enforce
`Ready=true ⇒ Client!=nil`, so a `{Ready:true, Client:nil}` snapshot reaches these
branches → nil-deref panic → CR stuck `Terminating`.

---

## Remediation status — 2026-06-02

**🔴 (6):** all landed in PR #77 (merged).

**🟡/🔵 (branch `fix/review-remediation`):**
- #2 — `oauth2_flow` forwarded on MCPServer UPDATE (+ `mcpRenderVersion` v1→v2 so steady-state CRs re-render once on upgrade). **Fixed.**
- #4 — ModelAlias transient router errors now labelled `LiteLLMUnavailable` (correct metric bucket). **Fixed.**
- #5 — `model_controller` 4xx detection de-duplicated onto the shared `is4xxError` helper. **Fixed (DRY, behaviour-preserving).**
- #6 — Connection fan-in mapper logs dropped `List`/`ExtractList` errors at V(1). **Fixed.**
- #10 — MCPServerDiscovery filter-target docs corrected `dotted three-part` → `<spec.prefix>-<source-name>` across all godoc sites, the `dotted`→`childNames` rename, the user guide, and regenerated CRD bases + api-reference. **Fixed.**
- #11/#12/#13 — clarifying comments (GuardRail relist exclusion, `claude-` redaction over-match, `+optional` on int32 counts). **Fixed.**

**Group C — assessed, intentionally NO change** (each is documented in-code as deliberate; the reviewer's proposed changes were either contestable or regressive):
- #1 metrics `namespace` label — kept; bounded by the single-watch-namespace topology, and per-namespace slicing is the metric's purpose.
- #3 filters exclude-compile ordering — matches the in-code spec contract (`InvalidConfig` is the reserved reason for compile failures; precedes `UpstreamInvalid`). No-op.
- #7 `isClusterLocalHost` two-label form — left conservative; loosening would misclassify public 2-label domains (`example.com`) as cluster-local and weaken the M-SEC2 plaintext-master-key guard. Use the `.svc` / single-label form for in-cluster endpoints.
- #8 `CachedListMCPServers` nil-out fallback — accepted; the direct re-fetch is bounded by mutation rate, and re-entrancy adds complexity to a cold path.
- #9 `GeneratedChildren` excludes skip-classified names — wontfix; `generated`/`skipped`/`failed` are a disjoint partition enforced by the `discoveredCount` invariant, so the proposed change would double-count. The OBS-04 `create`-vs-`update` action label is best-effort.
