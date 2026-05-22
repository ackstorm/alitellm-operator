# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

### Changed
- `LiteLLMConnection.spec.mcpToolPrefixSeparator` default flipped from
  `"-"` to `"."` to match LiteLLM v1.85.1's stock validator behavior,
  which rejects `"."` in `server_name` regardless of the
  `MCP_TOOL_PREFIX_SEPARATOR` env var (FIX2.txt HIGH-1). Operators
  running a non-stock LiteLLM that forbids `"-"` in `server_name` must
  set the field explicitly to `"-"` on their Connection CR. Existing
  CRs whose YAML omits the field will pick up the new default at the
  next reconcile.

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
