# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
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
