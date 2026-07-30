# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- **`LiteLLMModelDiscovery.spec.litellmProvider`** — optional override for the
  LiteLLM `custom_llm_provider` prefix stamped on each generated child's
  `litellm_params.model` (the pricing / cost-tracking key). Decouples *how*
  models are discovered (`spec.type`, OpenAI wire shape) from *which* provider
  LiteLLM bills them under. Enables correct cost tracking for OpenAI-compatible
  gateways (OpenRouter, Together, Groq, vLLM) without a new provider `type`.
  CEL-restricted to `spec.type: openai`. Changing it updates existing children
  in place (`POST /model/update`), never recreates.

### Changed
- **BREAKING (discovery): ToolHive `v1alpha1` support removed — the operator
  now reads `toolhive.stacklok.dev/v1beta1` ONLY.** `LiteLLMMCPServerDiscovery`
  requires the **`toolhive-operator-crds` chart >= 0.41.0** (or any ToolHive
  install that serves v1beta1). Upstream 0.41.0 marks v1alpha1
  `deprecated: true` on `MCPServer`, `VirtualMCPServer` and `MCPRemoteProxy`
  and moves `storage: true` to v1beta1, so v1beta1 is what every current
  ToolHive install serves.
  - **Upgrade impact:** on a cluster serving v1alpha1 only, the informer never
    registers and every `LiteLLMMCPServerDiscovery` goes
    `Ready=False, reason=SourceUnreachable`. Per the D-09 atomic-refresh
    contract, existing generated children are left in place untouched (not
    pruned) until the source becomes readable again. Upgrade the
    `toolhive-operator-crds` chart to recover; no CR edits are needed.
  - The dual-version informer set and its `v1alpha1`-wins dedup store
    (`dedup_reason=alpha_wins`) are gone with it — the informer registers 3
    GVKs instead of 6, and `List` no longer merges across versions.
- **BREAKING (observability): Prometheus metric prefix renamed
  `litellm_` → `alitellm_`.** The operator-owned metrics that carried
  the upstream-confusing `litellm_` prefix now use the project's
  `alitellm_` prefix, matching the operator name and disambiguating
  operator metrics from LiteLLM proxy metrics:
  - `litellm_operator_*` → `alitellm_operator_*`
    (`reconcile_total`, `cascade_drain_overdue_total`,
    `deletion_orphaned_total`, `deletion_blocked`, `conflicts_total`).
  - `litellm_api_request_duration_seconds` →
    `alitellm_api_request_duration_seconds`.
  - `litellm_api_errors_total` → `alitellm_api_errors_total`.
- **BREAKING (observability): the remaining unprefixed metrics are now
  namespaced too, finishing the rename above.** In a shared Prometheus
  `reconcile_total` is nobody's metric in particular; all operator-owned
  families now carry the `alitellm_operator_` prefix:
  - `reconcile_total` → **`alitellm_operator_reconcile_outcome_total`**
    (the coarse `{success, error, requeued}` counter). The name changed
    rather than just gaining a prefix because `alitellm_operator_reconcile_total`
    was already taken by the richer reason-derived counter.
  - `discovery_refresh_total`, `discovery_generated_count`,
    `discovery_failed_total`, `child_cr_writes_total`,
    `drift_corrected_total`, `connection_ready`, `cr_status_age_seconds`
    → same names under `alitellm_operator_`.
- **BREAKING (observability): `alitellm_operator_reconcile_total`'s
  `namespace` label is now `cr_namespace`.** A metric label that collides
  with a target label reaches the TSDB renamed to `exported_namespace`, so
  every dashboard grouping `by (namespace)` was silently grouping by the
  *operator pod's* namespace instead of the CR's.
  `alitellm_operator_deletion_blocked` gets the same treatment.
  Update any Grafana dashboards, recording rules, or Prometheus alerts
  referencing the old names or the old label.

### Added
- **Grafana dashboards shipped by the chart** (`metrics.dashboards.enabled`,
  default `false`). Four dashboards live in `examples/grafana/` and are
  mirrored into the chart by `make helm-sync`; the chart renders one ConfigMap
  per dashboard, labelled for the kube-prometheus-stack Grafana sidecar (no
  Prometheus Operator CRD needed). They are split by metric ownership —
  `operator/` (this operator's own metrics, folder *aLiteLLM Operator*) and
  `litellm/` (the proxy's callback metrics plus the `litellm-exporter`
  DB-derived ones, folder *aLiteLLM*) — via `metrics.dashboards.folders`.
  Pre-push gate 14b now fails on drift between `examples/grafana/` and the
  chart copies. See `examples/grafana/README.md`.

### Fixed
- **`alitellm_operator_cascade_drain_overdue_total` never appeared on
  `/metrics`.** A labelled counter is absent until its first `.Inc()`, so a
  dashboard panel or alert on it read "No data" instead of the flat 0 that
  actually held. Its label values are now pre-touched at init like every other
  enumerated family.
- **Model UI showed "Unknown date" — operator now stamps
  `created_at`/`updated_at` on model create.** In OSS (non-premium)
  LiteLLM the Models UI reads these from the `model_info` JSON blob
  (`proxy_server.get_model_info_with_id` only copies the DB columns into
  `model_info` when `premium_user is True`). `POST /model/new` is the
  only endpoint that persists the blob, so `model_controller.go` now
  stamps both timestamps (RFC3339 UTC) on create and on the D-02
  delete+recreate path — same mechanism as the existing `created_by`
  stamp. Adopted/out-of-band rows cannot be back-stamped.
- **Chart CRD drift — PR #25 + PR #38 schema changes did not reach
  v0.7.0 users.** `deploy/helm/alitellm-operator/crd-sources/` was
  regenerated and now matches `config/crd/bases/`:
  - `LiteLLMConnection.spec.endpoint` gains `pattern`, `maxLength=2048`,
    and three CEL XValidation rules (scheme / userinfo / whitespace)
    at admission. Without this, invalid endpoints were only caught at
    reconcile time via `litellm.ValidateEndpoint` (still in the binary).
  - `LiteLLMMCPServerDiscovery` + `LiteLLMModelDiscovery`
    `status.skippedCandidates[].reason` enum updated to include
    `Conflict` (renamed from `DuplicateDiscovery` per ADR-0001).
    Without this, the v0.7.0 binary's `Reason: "Conflict"` writes
    would be apiserver-rejected on the first cross-Discovery name
    collision, stranding the Discovery in a reconcile loop.
- **GuardRail stuck `Ready=False` after operator restart.**
  `bootsweep.isStuckReadyFalse` was missing the
  `*litellmv1alpha1.LiteLLMGuardRail` case (added in v0.3.1 to
  `BootSweeper.Start`'s enumerated kinds but never to the classifier),
  so the boot-time safety re-enqueue never fired for guardrails.
  Combined with the `connectionReadyTransition` predicate firing on
  the initial-list `Create` event BEFORE the Connection reconciler's
  first probe populates the cache, every operator restart left
  guardrails permanently `Ready=False, Reason=LiteLLMUnavailable`
  until the next spec edit. Observed in prod 2026-05-27 on
  `LiteLLMGuardRail/credential-filter` ~3h after a restart while the
  Connection had been `Ready=True` for 4 days.

### Changed
- **`make helm-sync-check` is now enforced.** Three new gates ensure
  the chart bundle in `deploy/helm/alitellm-operator/crd-sources/`
  never falls out of sync with `config/crd/bases/`:
  - PR CI lint job (`.github/workflows/ci.yml`) — mandatory pre-merge
    gate; branch protection blocks merge on failure.
  - Pre-push hook (`scripts/pre-push-check.sh` gate 14b) — local
    fast-fail mirroring the existing `go mod tidy` drift gate.
  - Release pipeline (`.github/workflows/release.yml`) — belt + braces;
    a drifted main blocks `chore(release): v*` before goreleaser
    packages the chart.
  - `make helm-sync` was previously documented but invoked nowhere
    automatically. The v0.7.0 release shipped stale CRDs as a direct
    consequence.

### Changed (BREAKING)
- **`SkippedCandidate.Reason` enum rename: `DuplicateDiscovery` →
  `Conflict`** (ADR-0001). Applies to both
  `LiteLLMModelDiscovery.status.skippedCandidates[].reason` and
  `LiteLLMMCPServerDiscovery.status.skippedCandidates[].reason`. The
  CRD validation enum is updated; CRs that previously read or
  alert-matched on `reason=DuplicateDiscovery` must switch to
  `reason=Conflict`. The Prometheus metric
  `discovery_skipped_total{reason}` label value is renamed
  identically. Behavior is unchanged in this PR (first-create-wins
  on cross-Discovery collisions); alpha-last-wins ownership transfer
  between Discoveries is deferred to a follow-up PR (requires a
  get-then-update path to replace `metadata.ownerReferences`).

### Added
- **Alpha-last-wins conflict resolution (ADR-0001):** new shared
  package `internal/controller/conflict` providing `Key`,
  `ResolveWinner`, `IsLoser`, and a `Ready=False, Reason=Conflict`
  status-condition helper used by per-CR resolvers. Concept doc:
  `docs/concepts/conflict-resolution.md`; decision record:
  `references/adr/0001-alpha-last-wins-conflict-resolution.md`.
- **`LiteLLMMCPServer` resolver for sanitization-collapse:** when two
  CRs in the same namespace sanitize to the same LiteLLM `server_name`
  (e.g. `foo.bar` and `foo-bar` both → `foo-bar` under separator `.`),
  the CR whose `<namespace>/<name>` sorts last wins; the other
  short-circuits with `Ready=False, Reason=Conflict, Message="superseded
  by <ns>/<name>"`. Self-watch promotes the loser when the winner is
  deleted.
- **Metric `litellm_operator_conflicts_total{kind,role}`:** counter
  incremented by the resolver on every loser/winner transition.
  Pre-touched for `kind=MCPServer, role={loser,winner}`.
- **Events `ConflictDetected` / `ConflictWon`** on the affected
  `LiteLLMMCPServer` CRs.

### Changed
- **`LiteLLMMCPServerDiscovery` intra-discovery dedup direction
  (BREAKING):** flipped from first-seen-wins to alpha-last-wins. When
  two upstream ToolHive objects in different namespaces share the same
  `metadata.name` within a single Discovery, the entry with the
  alpha-LAST `(sourceNamespace, sourceName)` ASC key now wins; earlier
  occurrences are skipped with `Reason=NameCollision` (unchanged). The
  parent's `NameCollision` status condition continues to fire. Status
  output order is unchanged (sort happens before render). Deployments
  that relied on the prior first-wins survivor will see a different
  child generated under the colliding name.

### Note (follow-up PR)
- Cross-`LiteLLMMCPServerDiscovery` and cross-`LiteLLMModelDiscovery`
  collisions resolve first-create-wins with `Reason=Conflict` skip on
  the alpha-second Discovery. Full alpha-last-wins ownership transfer
  (replacing `metadata.ownerReferences` across SSA field managers) is
  deferred to a follow-up PR.
- **Configurable deletion policy (#23):** `spec.deletionPolicy`
  (`Orphan` | `Delete`, default `Orphan`) on `LiteLLMModel`,
  `LiteLLMTeam`, `LiteLLMMCPServer`, `LiteLLMA2AAgent`, and
  `LiteLLMGuardRail`. Default `Orphan` preserves REL-06 anti-storm
  semantics (finalizer is removed even when the LiteLLM-side delete
  cannot be confirmed). Set to `Delete` to block finalizer removal
  until LiteLLM acks — suitable for GitOps deployments where Argo/Flux
  must not see "synced" while a backend resource still exists.
  Annotation `litellm.ackstorm.ai/deletion-policy-override` provides
  per-CR runtime break-glass without mutating spec. Discovery-owned
  children always resolve to `Orphan` regardless of spec/annotation
  so vanish-detection cannot deadlock. `LiteLLMConnection` is
  excluded — its finalizer runs no LiteLLM HTTP call.
- Metric `litellm_operator_deletion_orphaned_total{kind}` increments
  on every `Orphan` finalizer-removed-without-ack path.
- Metric `litellm_operator_deletion_blocked{kind,namespace,name}`
  emits 1 per CR currently stuck in Terminating under `Delete` policy.
- Events `LiteLLMDeleteOrphaned` (Normal) and `LiteLLMDeleteBlocked`
  (Warning) on the affected code paths.
- Examples `examples/example-deploy/10-strict-deletion-model.yaml`
  and `examples/example-deploy/11-strict-deletion-team.yaml`
  documenting the `Delete` opt-in and break-glass annotation.
- mcpserver: every key modeled in `litellm.MCPServerRequest` (`auth_type`,
  `credentials`, `mcp_access_groups`, `allowed_tools`, `tool_name_to_*`,
  `extra_headers`, `static_headers`, `command`, `args`, `env`,
  `authorization_url`, `token_url`, `registration_url`, `oauth2_flow`,
  `allow_all_keys`, `available_on_public_internet`) is now forwarded
  verbatim from `spec.params` on both CREATE and UPDATE (FIX5 H-1).
- mcpserver: `spec.params.extra_headers` accepts both map (inject) and
  list (forward-from-client) shapes per LiteLLM 1.83.10 `Union[Dict,List]`
  (FIX5 H-2).
- mcpserver: `spec.params.access_groups` accepted as alias for
  `mcp_access_groups` (`mcp_access_groups` wins when both present)
  (FIX5 LOW-4).
- crd: `categories=litellm` added to all seven CRDs — `kubectl get
  litellm -A` now lists every CR in one go (FIX6 L-3).
- crd: `modeldiscovery` and `mcpserverdiscovery` print columns are
  now symmetric: `Type | Ready | Reason | Discovered | Generated |
  Age` (FIX6 H-1).

### Changed

#### BREAKING

- **`LiteLLMConnection.spec.endpoint` validation tightened (#25):**
  endpoints are now validated at admission and at wire-level.
  Endpoints must match
  `^https?://[^@\s?#]+(:[0-9]{1,5})?(/[^\s?#]*)?$` and pass three
  additional CEL XValidation rules (scheme, no userinfo, no
  whitespace). Wire-level: `litellm.ValidateEndpoint` additionally
  rejects raw Unicode hosts (Punycode required), out-of-range ports,
  opaque URIs, and control characters. Invalid endpoints surface as
  `Ready=False reason=InvalidEndpoint` with no requeue (Spec edit
  retriggers).

  Upgrade audit — run this one-liner BEFORE upgrading to find
  `LiteLLMConnection` objects that would be rejected by the new
  validation. Fix the endpoint values (or temporarily quarantine the
  resources) before the upgrade lands:

  ```bash
  kubectl get litellmconnections.litellm.ackstorm.ai -A \
    -o jsonpath='{range .items[*]}{.metadata.namespace}/{.metadata.name}{"\t"}{.spec.endpoint}{"\n"}{end}' \
    | awk -F'\t' '
        $2 !~ /^https?:\/\/[^@[:space:]?#]+(:[0-9]+)?(\/[^[:space:]?#]*)?$/ {
          print "REJECT", $0
        }'
  ```

  Output `REJECT <ns>/<name> <endpoint>` lines mark resources that
  need remediation. An empty output means all current Connection
  objects satisfy the new contract.

- **RBAC scope-down (#21):** The operator's manager role is now a
  namespaced `Role` (`alitellm-operator-role`) + `RoleBinding`
  (`alitellm-operator-rolebinding`) bound in `.Release.Namespace`,
  replacing the cluster-wide `ClusterRole` + `ClusterRoleBinding`
  named `alitellm-operator-manager-{role,rolebinding}`. The `-manager-`
  infix is dropped. Upgrades replace the binding under the new name;
  Helm's 3-way merge creates the new binding before removing the old,
  but verify in your environment if the operator pod restarts during
  the upgrade window.

  ClusterRoles retained (cluster-scoped APIs):
  - `alitellm-operator-metrics-auth-role`
  - `alitellm-operator-metrics-reader`
  - `alitellm-operator-toolhive-reader`

### Deprecated

### Removed

### Fixed
- model: LiteLLM 1.85.1 schema drift — the model id is now sent inside
  `model_info.id` on `POST /model/update` (both FIX4 H-1 adoption stamp
  and the UPDATE drift-correction path). Top-level `id` was retired
  from the request body; LiteLLM 1.85.1's body parser rejected it with
  the misleading 400 `Authentication Error, model not found` and every
  `LiteLLMModel` reconcile landed in `Ready=False reason=LiteLLMRejected`
  in production. 1.83.x sites: pin operator to v0.3.0 or earlier
  (FIX7 H-1).

### Security
- Operator now emits a loud `Error`-level boxed startup banner when `LITELLM_OPERATOR_DANGEROUSLY_LOG_BODIES=true`, naming the env var verbatim and stating that request/response bodies (which carry substituted provider API keys) will be logged. Issue #26.

### BREAKING CHANGES
- **`LiteLLMModel.status.lastRendered.litellmModelID` renamed to
  `modelID`** for symmetry with `TeamID` / `AgentID` / `ServerID`
  siblings (FIX6 H-2). Alpha-API breakage policy: in-place rename,
  no conversion webhook. Existing in-cluster CRs will see the
  reconciler repopulate the new key on next reconcile; the orphan
  `litellmModelID` may sit in etcd until the next status replacement.

## [0.3.0] - TBD

### BREAKING CHANGES
- **mcpserverdiscovery: `spec.prefix` is now required.** Pre-v0.3.0
  `LiteLLMMCPServerDiscovery` CRs without `spec.prefix` will fail
  admission on `kubectl apply` after upgrade. Add the field (DNS-1123
  label, MaxLength=30) to every existing CR in your gitops tree
  BEFORE upgrading (FIX4.txt H-2).
- **mcpserverdiscovery: children are renamed.**
  - Pre-v0.3.0 K8s child name: `<discovery>.<source-ns>.<source-name>`
  - v0.3.0 K8s child name:     `<spec.prefix>-<source-name>`

  The source-namespace component is dropped. The user picks
  `spec.prefix` to disambiguate across discoveries.

  All currently-managed K8s MCPServer CRs will be deleted on
  upgrade (their finalizer fires → LiteLLM DELETE → fine) and
  recreated under the new name → POST /v1/mcp/server registers
  fresh records on the LiteLLM side. The LiteLLM-side records
  under the OLD names become orphans; manual cleanup is required
  via LiteLLM's admin UI or the `/v1/mcp/server` API. See
  `docs/releases/v0.3.0-migration.md` for the full migration
  checklist.

### Added
- mcpserverdiscovery: `NameCollision` status condition fires
  (`Status=True`, `Reason=NameCollision`) when two upstream ToolHive
  objects from different namespaces produce the same
  `<spec.prefix>-<source-name>` child name within a single discovery.
  The first occurrence wins; later occurrences are dropped into
  `status.skippedCandidates[Reason=NameCollision]`. Loud-fail, not
  silent-merge — rename one upstream or split the discovery into
  prefix-distinct ones to resolve (FIX4.txt H-2).
- observability: one-shot startup INFO log of `identity.Operator()`
  so the audit literal the binary will stamp into LiteLLM payloads
  is observable at boot without an external probe (FIX4.txt H-1).
- tests: body-capture regression guards on `/model/new` and
  `/model/update` asserting `model_info.created_by`/`updated_by`
  reach the wire (FIX4.txt H-1).

### Fixed
- model: stamp `model_info.updated_by` on the adoption-by-name
  branch in `model_controller.go` so pre-v0.2.0 entries (and any
  out-of-band entries) flip from `Created By: Unknown` to
  `alitellm-operator/<version>` the moment the controller first
  touches the row (FIX4.txt H-1 root cause for the prod symptom
  observed on v0.2.0).
- mcpserver / a2aagent: stamp `created_by` + `updated_by` on
  CREATE and `updated_by` on UPDATE — best-effort into the
  respective freeform bag (`mcp_info` for MCPServer,
  `agent_card_params` for A2AAgent). The LiteLLM UI may not
  surface these values on MCPServer / A2AAgent rows today (no
  native audit column), but the values are persisted on the
  LiteLLM side and visible to any probe (FIX4.txt H-1 symmetry).
  Team stamping was attempted but reverted — e2e AC-T4 against
  LiteLLM 1.83.10 showed metadata-bag stamping breaks the
  /team/new bootstrap path; tracked as inventory case (b) until
  LiteLLM ships a native audit column for /team/*.

### Documentation
- `references/litellm-auth-model.md`: clarify the operator uses the
  LiteLLM master key against every endpoint it calls; the transient
  401 observed in the FIX4 evidence was a secret-rotation race, not
  a LiteLLM-side policy tightening. Virtual keys are out of scope
  for v0.3.0 (FIX4.txt L-3 close-out).
- `docs/releases/v0.3.0-migration.md`: full migration guide for the
  MCPServerDiscovery rename (spec.prefix + child-name shape).

## [0.2.0] - 2026-05-22

### Changed
- `LiteLLMConnection.spec.mcpToolPrefixSeparator` default flipped from
  `"-"` to `"."` to match LiteLLM v1.85.1's stock validator behavior,
  which rejects `"."` in `server_name` regardless of the
  `MCP_TOOL_PREFIX_SEPARATOR` env var (FIX2.txt HIGH-1). Operators
  running a non-stock LiteLLM that forbids `"-"` in `server_name` must
  set the field explicitly to `"-"` on their Connection CR. Existing
  CRs whose YAML omits the field will pick up the new default at the
  next reconcile.
- **BREAKING (install layout):** dropped kustomize `namePrefix:
  alitellm-operator-` from `config/default/kustomization.yaml`.
  Resources are now named explicitly in their source files. Net
  effect on a fresh install:
  - Deployment: `alitellm-operator-controller-manager` → `alitellm-operator`
  - ServiceAccount: `alitellm-operator-controller-manager` → `alitellm-operator`
  - Service (metrics): `alitellm-operator-controller-manager-metrics-service`
    → `alitellm-operator-metrics`
  - ClusterRole/Binding names: still prefixed `alitellm-operator-*`
    (re-added explicitly in source), no scope change.
  - Pod label selector: `control-plane: controller-manager` →
    `control-plane: alitellm-operator`.
  Existing installs upgraded via Helm will create the NEW resources
  alongside the old ones; the previous `alitellm-operator-controller-manager`
  Deployment / SA / Service must be manually deleted before the
  upgrade is clean. Plan a `helm uninstall && helm install` cycle
  on upgrade to v0.2.0.

### Deprecated

### Removed

### Fixed
- MCPServer sanitizer no longer rewrites already-safe inputs. Names
  like `test-exa-mcp` that previously got mangled into `test.exa.mcp`
  on the v0.1.2 upgrade boundary now pass through unchanged (FIX2.txt
  HIGH-9).
- MCPServer reconciler adopts a pre-existing LiteLLM record created
  under the K8s `metadata.name` when the sanitized name has no record
  but the raw name does. Heals upgrade-orphans without manual
  `kubectl` intervention; applies on both the CREATE branch
  (probe-before-POST) and the finalizer name-resolve path.

### Security

## [0.1.0] - TBD

### Added
- Initial alpha release of alitellm-operator.
- CRDs under `litellm.ackstorm.ai/v1alpha1`:
  LiteLLMConnection, LiteLLMTeam, LiteLLMModel, LiteLLMA2AAgent,
  LiteLLMMCPServer, LiteLLMMCPServerDiscovery, LiteLLMModelDiscovery.
- Controllers reconciling each CRD against an external LiteLLM proxy.
- Helm chart (`deploy/helm/alitellm-operator`) and kustomize overlays
  (`config/default`) for installation.
- Release pipeline: goreleaser-driven, cosign keyless OIDC signing,
  CycloneDX SBOM (HRD-09).

[Unreleased]: https://github.com/ackstorm/alitellm-operator/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/ackstorm/alitellm-operator/releases/tag/v0.1.0
