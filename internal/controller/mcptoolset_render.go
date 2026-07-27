// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
	"github.com/ackstorm/alitellm-operator/internal/litellm"
)

// renderToolsetTools flattens spec.from into LiteLLM's {server_id, tool_name}
// pair list, in declaration order, de-duplicated (first occurrence wins).
//
// resolveServer translates a spec.from[].server value into the server_id sent
// to LiteLLM. It MUST be total and never fail: an unresolvable name is
// returned verbatim, which is precisely correct when the user supplied a raw
// server_id UUID for an adopted server. No sanitization is applied — with
// MCP_TOOL_PREFIX_SEPARATOR="-", SanitizeMCPServerName maps `-` to `.` and
// would mangle a UUID.
//
// Deliberately NO validation: LiteLLM accepts a nonexistent server_id or
// tool_name with 201 and simply grants nothing (verified on 1.93.0), so a
// typo yields an inert toolset rather than an error anywhere. Empty tool
// names are dropped because they can only ever be inert.
//
// The return is ALWAYS non-nil so the request body serializes `tools: []` —
// an explicit clear — rather than `null`.
func renderToolsetTools(
	from []litellmv1alpha1.MCPToolsetServerTools,
	resolveServer func(name string) string,
) []litellm.MCPToolsetTool {
	out := make([]litellm.MCPToolsetTool, 0, len(from))
	seen := make(map[litellm.MCPToolsetTool]struct{}, len(from))
	for _, entry := range from {
		serverID := resolveServer(entry.Server)
		for _, tool := range entry.Tools {
			if tool == "" {
				continue
			}
			pair := litellm.MCPToolsetTool{ServerID: serverID, ToolName: tool}
			if _, dup := seen[pair]; dup {
				continue
			}
			seen[pair] = struct{}{}
			out = append(out, pair)
		}
	}
	return out
}

// serverIDResolver returns a resolver for spec.from[].server suitable for
// renderToolsetTools.
//
// It reads the named LiteLLMMCPServer CR from the informer cache (no HTTP, no
// LiteLLM call) and returns its status.lastRendered.serverID. Every other
// case — no such CR, a Get error, or a CR that has not been reconciled yet
// (empty ServerID) — returns the input VERBATIM.
//
// This is translation, not validation. It NEVER fails and NEVER parks the CR.
// Rationale: the operator pins server_id to the sanitized metadata.name only
// on CREATE, so an ADOPTED MCP server carries a server-minted UUID whose name
// differs from its id; the lookup covers that transparently while a raw UUID
// typed directly into spec.from[].server still works via the fallback.
//
// The fallback deliberately returns the input rather than "" for an unsynced
// CR: an empty server_id would be stored by LiteLLM as a blank-server pair,
// which is strictly worse than an inert but legible one, and the next
// reconcile (once the MCPServer CR syncs) corrects it via the drift hash.
func serverIDResolver(ctx context.Context, c client.Client, namespace string) func(string) string {
	return func(name string) string {
		if name == "" {
			return name
		}
		var srv litellmv1alpha1.LiteLLMMCPServer
		key := types.NamespacedName{Namespace: namespace, Name: name}
		if err := c.Get(ctx, key, &srv); err != nil {
			return name // not found, or transient — forward verbatim
		}
		if id := srv.Status.LastRendered.ServerID; id != "" {
			return id
		}
		return name // CR exists but unsynced — forward verbatim, never ""
	}
}
