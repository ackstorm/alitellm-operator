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
