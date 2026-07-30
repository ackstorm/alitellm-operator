// SPDX-License-Identifier: Apache-2.0

// Package toolhive provides the ToolHive integration plumbing —
// lazy dynamic informers for cluster-scoped reads against
// `toolhive.stacklok.dev/v1alpha1` AND `toolhive.stacklok.dev/v1beta1`
// MCPServer / VirtualMCPServer / MCPRemoteProxy objects.
//
// # Dual-version support (Phase 9, Task 09-07)
//
// The informer registers SIX dynamic informers: one for each combination of
// {v1alpha1, v1beta1} × {MCPServer, VirtualMCPServer, MCPRemoteProxy}. Each
// informer tolerates an absent CRD at startup (Phase 5 D-08) and retries
// registration on a 1-minute background ticker. A CRD kind is considered
// "ready" once at least one of its two version informers has successfully
// registered.
//
// # Dedup rule: v1alpha1 wins
//
// When the same {kind, namespace, name} object exists under both v1alpha1 and
// v1beta1, the v1alpha1 instance is the canonical entry in the discovered set.
// The v1beta1 duplicate is logged at info level with dedup_reason=alpha_wins
// and excluded from List results. This rule guarantees no behavior change for
// deployments with only v1alpha1 CRDs installed (the common case today — all
// published toolhive-operator-crds Helm charts ship v1alpha1 only).
//
// See informer.go for the Informer type and Start/List/IsReady methods.
//
// 2026-05-19 (Tier 2 plan): original implementation was bumped from v1beta1
// to v1alpha1 to match reality. All published toolhive-operator-crds charts
// (latest 0.0.106 from stacklok.github.io/toolhive, latest OCI 0.0.55 from
// ghcr.io) ship v1alpha1 ONLY. v0.28.0 repo source has both, but no published
// chart yet carries the v1beta1 declarations. The autoconfig reference at
// v1alpha1 was correct; the spec §6.5 wording was aspirational.
// v1alpha1 carries the same `image`, `transport`, `proxyMode` fields
// our informer reads — no schema impact.
package toolhive

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// MCPServerGVK is the canonical GroupVersionKind for ToolHive's
// MCPServer custom resource at v1alpha1 (the version carried by all
// currently-published toolhive-operator-crds Helm charts).
//
// This constant is preserved for backward compatibility with existing
// callers. It is equivalent to MCPServerGVKv1alpha1.
var MCPServerGVK = schema.GroupVersionKind{
	Group:   "toolhive.stacklok.dev",
	Version: "v1alpha1",
	Kind:    "MCPServer",
}

// VirtualMCPServerGVK is the canonical GroupVersionKind for ToolHive's
// VirtualMCPServer custom resource at v1alpha1 (per Phase 5 D-06).
//
// This constant is preserved for backward compatibility with existing
// callers. It is equivalent to VirtualMCPServerGVKv1alpha1.
var VirtualMCPServerGVK = schema.GroupVersionKind{
	Group:   "toolhive.stacklok.dev",
	Version: "v1alpha1",
	Kind:    "VirtualMCPServer",
}

// MCPServerListGVK is the corresponding list-kind for MCPServerGVK.
// Used when constructing UnstructuredList for cache-backed List calls.
var MCPServerListGVK = schema.GroupVersionKind{
	Group:   "toolhive.stacklok.dev",
	Version: "v1alpha1",
	Kind:    "MCPServerList",
}

// VirtualMCPServerListGVK is the corresponding list-kind for
// VirtualMCPServerGVK.
var VirtualMCPServerListGVK = schema.GroupVersionKind{
	Group:   "toolhive.stacklok.dev",
	Version: "v1alpha1",
	Kind:    "VirtualMCPServerList",
}

// MCPServerGVKv1alpha1 is the explicit v1alpha1 GVK for MCPServer.
// Identical to MCPServerGVK; provided alongside v1beta1 for clarity.
var MCPServerGVKv1alpha1 = MCPServerGVK

// VirtualMCPServerGVKv1alpha1 is the explicit v1alpha1 GVK for VirtualMCPServer.
// Identical to VirtualMCPServerGVK; provided alongside v1beta1 for clarity.
var VirtualMCPServerGVKv1alpha1 = VirtualMCPServerGVK

// MCPServerListGVKv1alpha1 is the explicit v1alpha1 list GVK for MCPServer.
// Identical to MCPServerListGVK.
var MCPServerListGVKv1alpha1 = MCPServerListGVK

// VirtualMCPServerListGVKv1alpha1 is the explicit v1alpha1 list GVK for VirtualMCPServer.
// Identical to VirtualMCPServerListGVK.
var VirtualMCPServerListGVKv1alpha1 = VirtualMCPServerListGVK

// MCPServerGVKv1beta1 is the v1beta1 GroupVersionKind for ToolHive's
// MCPServer custom resource. v0.28.0 upstream source declares this version
// in its templates; no published chart yet carries it as of 2026-05-20.
// The informer registers this alongside v1alpha1 so the operator works
// against either CRD vintage without configuration.
var MCPServerGVKv1beta1 = schema.GroupVersionKind{
	Group:   "toolhive.stacklok.dev",
	Version: "v1beta1",
	Kind:    "MCPServer",
}

// VirtualMCPServerGVKv1beta1 is the v1beta1 GroupVersionKind for ToolHive's
// VirtualMCPServer custom resource. See MCPServerGVKv1beta1 for rationale.
var VirtualMCPServerGVKv1beta1 = schema.GroupVersionKind{
	Group:   "toolhive.stacklok.dev",
	Version: "v1beta1",
	Kind:    "VirtualMCPServer",
}

// MCPServerListGVKv1beta1 is the list GVK for MCPServerGVKv1beta1.
var MCPServerListGVKv1beta1 = schema.GroupVersionKind{
	Group:   "toolhive.stacklok.dev",
	Version: "v1beta1",
	Kind:    "MCPServerList",
}

// VirtualMCPServerListGVKv1beta1 is the list GVK for VirtualMCPServerGVKv1beta1.
var VirtualMCPServerListGVKv1beta1 = schema.GroupVersionKind{
	Group:   "toolhive.stacklok.dev",
	Version: "v1beta1",
	Kind:    "VirtualMCPServerList",
}

// MCPRemoteProxyGVK is the canonical GroupVersionKind for ToolHive's
// MCPRemoteProxy custom resource at v1alpha1.
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
	Version: "v1alpha1",
	Kind:    "MCPRemoteProxy",
}

// MCPRemoteProxyListGVK is the corresponding list-kind for MCPRemoteProxyGVK.
var MCPRemoteProxyListGVK = schema.GroupVersionKind{
	Group:   "toolhive.stacklok.dev",
	Version: "v1alpha1",
	Kind:    "MCPRemoteProxyList",
}

// MCPRemoteProxyGVKv1alpha1 is the explicit v1alpha1 GVK for MCPRemoteProxy.
// Identical to MCPRemoteProxyGVK; provided alongside v1beta1 for clarity.
var MCPRemoteProxyGVKv1alpha1 = MCPRemoteProxyGVK

// MCPRemoteProxyListGVKv1alpha1 is the explicit v1alpha1 list GVK for
// MCPRemoteProxy. Identical to MCPRemoteProxyListGVK.
var MCPRemoteProxyListGVKv1alpha1 = MCPRemoteProxyListGVK

// MCPRemoteProxyGVKv1beta1 is the v1beta1 GroupVersionKind for ToolHive's
// MCPRemoteProxy custom resource. See MCPServerGVKv1beta1 for rationale.
var MCPRemoteProxyGVKv1beta1 = schema.GroupVersionKind{
	Group:   "toolhive.stacklok.dev",
	Version: "v1beta1",
	Kind:    "MCPRemoteProxy",
}

// MCPRemoteProxyListGVKv1beta1 is the list GVK for MCPRemoteProxyGVKv1beta1.
var MCPRemoteProxyListGVKv1beta1 = schema.GroupVersionKind{
	Group:   "toolhive.stacklok.dev",
	Version: "v1beta1",
	Kind:    "MCPRemoteProxyList",
}
