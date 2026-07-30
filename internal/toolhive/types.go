// SPDX-License-Identifier: Apache-2.0

// Package toolhive provides the ToolHive integration plumbing —
// lazy dynamic informers for cluster-scoped reads against
// `toolhive.stacklok.dev/v1beta1` MCPServer / VirtualMCPServer /
// MCPRemoteProxy objects.
//
// # Single-version support (v1beta1 only)
//
// The informer registers THREE dynamic informers, one per kind. Each
// tolerates an absent CRD at startup (Phase 5 D-08) and retries
// registration on a 1-minute background ticker.
//
// 2026-07-30: v1alpha1 support was removed. Upstream toolhive-operator-crds
// 0.41.0 marks v1alpha1 `deprecated: true` on all three kinds and moved
// `storage: true` to v1beta1, so v1beta1 is the version every current
// ToolHive install serves. The dual-version informer set and its
// v1alpha1-wins dedup store existed only to bridge the vintage gap and are
// gone with it. Clusters old enough to serve v1alpha1 ONLY are not
// supported — upgrade the toolhive-operator-crds chart.
//
// See informer.go for the Informer type and Start/List/IsReady methods.
package toolhive

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// MCPServerGVK is the GroupVersionKind for ToolHive's MCPServer custom
// resource.
var MCPServerGVK = schema.GroupVersionKind{
	Group:   "toolhive.stacklok.dev",
	Version: "v1beta1",
	Kind:    "MCPServer",
}

// VirtualMCPServerGVK is the GroupVersionKind for ToolHive's
// VirtualMCPServer custom resource (per Phase 5 D-06).
var VirtualMCPServerGVK = schema.GroupVersionKind{
	Group:   "toolhive.stacklok.dev",
	Version: "v1beta1",
	Kind:    "VirtualMCPServer",
}

// MCPRemoteProxyGVK is the GroupVersionKind for ToolHive's MCPRemoteProxy
// custom resource.
//
// MCPRemoteProxy fronts an MCP server that already lives outside the
// cluster and already speaks HTTP — ToolHive runs only a proxy Deployment
// for it, no workload StatefulSet. Discovery treats it exactly like the
// other two kinds: the endpoint comes from `status.url`, the transport
// from `status.transport` (empty in practice, so the D-09 "http" default
// applies). All three CRDs ship in the same upstream
// toolhive-operator-crds chart, so none of them is optional.
//
// Note the endpoint shape difference, which is deliberately NOT special-
// cased: `MCPServer.status.url` carries a `/mcp` path suffix, while
// `MCPRemoteProxy.status.url` — like `VirtualMCPServer.status.url` — is
// path-less. Both forms are already served at the root by the proxy, and
// the path-less form has been in production via VirtualMCPServer since
// the Phase 5 rollout, so `status.url` is propagated verbatim.
var MCPRemoteProxyGVK = schema.GroupVersionKind{
	Group:   "toolhive.stacklok.dev",
	Version: "v1beta1",
	Kind:    "MCPRemoteProxy",
}

// discoverableGVKs is the full set the Informer registers and serves.
var discoverableGVKs = []schema.GroupVersionKind{
	MCPServerGVK,
	VirtualMCPServerGVK,
	MCPRemoteProxyGVK,
}
