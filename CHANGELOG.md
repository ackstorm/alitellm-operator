# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

### Changed

### Deprecated

### Removed

### Fixed

### Security

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
