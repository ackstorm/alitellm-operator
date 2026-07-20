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
// Semantics (spec gotcha #5): the caller must not invoke this with a nil
// block. Within a present block, a nil or empty sublist contributes NOTHING
// — an empty allowed list means "allow all" in LiteLLM, so we omit the key
// rather than send [] and risk a lock-to-zero misread.
func projectPermission(
	perm *litellmv1alpha1.PermissionSpec,
	agentNameToID map[string]string,
) (models []string, objectPermission map[string]any, missingAgents []string) {
	// models = specific model names + model access-group names, merged.
	models = append(models, perm.Models...)
	models = append(models, perm.ModelGroups...)

	objectPermission = map[string]any{}
	if len(perm.McpServers) > 0 {
		objectPermission["mcp_servers"] = perm.McpServers
	}
	if len(perm.McpGroups) > 0 {
		objectPermission["mcp_access_groups"] = perm.McpGroups
	}
	if len(perm.Agents) > 0 {
		resolved := make([]string, 0, len(perm.Agents))
		for _, name := range perm.Agents {
			id, ok := agentNameToID[name]
			if !ok {
				missingAgents = append(missingAgents, name)
				continue
			}
			resolved = append(resolved, id)
		}
		if len(resolved) > 0 {
			objectPermission["agents"] = resolved
		}
	}
	if len(perm.AgentGroups) > 0 {
		objectPermission["agent_access_groups"] = perm.AgentGroups
	}
	return models, objectPermission, missingAgents
}
