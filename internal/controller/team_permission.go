// SPDX-License-Identifier: Apache-2.0

package controller

import (
	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
)

// projectPermission renders a typed spec.permission block into the two
// LiteLLM team body fields it maps to: the top-level `models` list (merged
// specific model names + model access-group names) and the nested
// `object_permission` object (mcp_servers, mcp_access_groups, agents,
// agent_access_groups).
//
// agentNameToID resolves human-friendly A2A agent NAMES to the agent_id
// UUIDs LiteLLM enforces on object_permission.agents (names are silently
// ignored by LiteLLM). Names absent from the map are returned in
// missingAgents so the caller can requeue (reason=AgentNotFound) instead of
// silently dropping agents; the successfully-resolved agents are still
// projected.
//
// ALWAYS-EMIT contract (security-critical — reverses the original omit-empty
// design). When the block is present the operator OWNS `models` AND all four
// `object_permission` sub-fields, and emits EVERY one of them
// UNCONDITIONALLY — as an empty list `[]` when the CR leaves it empty, never
// omitted. This is mandatory because LiteLLM's POST /team/update MERGES
// per-field on the persistent object_permission row (same
// object_permission_id across updates): a present field is replaced, `[]`
// clears it, but an OMITTED field keeps its STALE value. Omitting a
// shrunk-to-empty field therefore silently fails to revoke access — a
// Ready=Synced CR while LiteLLM still grants the removed resource (verified on
// LiteLLM 1.83.10 in prod). Every returned slice is non-nil so encoding/json
// renders `[]` (an explicit clear), never `null` (which LiteLLM's merge treats
// as "field absent" → stale-value kept).
func projectPermission(
	perm *litellmv1alpha1.PermissionSpec,
	agentNameToID map[string]string,
) (models []string, objectPermission map[string]any, missingAgents []string) {
	// models = specific model names + model access-group names, merged.
	// make(…, 0, …) guarantees a non-nil slice so an emptied block serializes
	// as JSON `[]`, not `null`.
	models = make([]string, 0, len(perm.Models)+len(perm.ModelGroups))
	models = append(models, perm.Models...)
	models = append(models, perm.ModelGroups...)

	resolved := make([]string, 0, len(perm.Agents))
	for _, name := range perm.Agents {
		id, ok := agentNameToID[name]
		if !ok {
			missingAgents = append(missingAgents, name)
			continue
		}
		resolved = append(resolved, id)
	}

	// Every sub-field emitted unconditionally (as [] when empty) — see the
	// ALWAYS-EMIT contract above.
	objectPermission = map[string]any{
		"mcp_servers":         emptyIfNil(perm.McpServers),
		"mcp_access_groups":   emptyIfNil(perm.McpGroups),
		"agents":              resolved,
		"agent_access_groups": emptyIfNil(perm.AgentGroups),
	}

	return models, objectPermission, missingAgents
}

// emptyIfNil coerces a nil slice to a non-nil empty slice so encoding/json
// renders `[]` (an explicit LiteLLM clear) rather than `null` (merged as
// "field absent" → stale value kept). See the ALWAYS-EMIT contract on
// projectPermission.
func emptyIfNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
