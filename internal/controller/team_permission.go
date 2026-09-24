// SPDX-License-Identifier: Apache-2.0

package controller

// Deny-by-default sentinels. LiteLLM reads an EMPTY `models` or
// object_permission.agents list as "no filter" (fail-OPEN → master-key
// ceiling: a team with models=[] saw all 427 models), so a closed team must
// carry a value no real resource matches.
const (
	// modelDenyAllSentinel is LiteLLM's own SpecialModelNames.no_default_models:
	// get_complete_model_list drops it from every catalog, so a closed team
	// lists zero models instead of a phantom row. Same value alitellm-auth uses.
	modelDenyAllSentinel = "no-default-models"
	// agentDenyAllSentinel: agents match on agent_id; none is minted all-zero.
	agentDenyAllSentinel = "00000000-0000-0000-0000-000000000000"
)

// closedTeamGrant is the ONLY grant a LiteLLMTeam projects: a fully closed
// team opened solely by its attached unified access groups. Access groups
// ADD over the sentinels (verified 1.93.0, e2e AG-04), so nothing here is
// relaxed when groups are attached.
//
// ALWAYS-EMIT (security-critical): POST /team/update merges per field and
// keeps an OMITTED field's stale value (the v0.7.25 revocation leak), so every
// key is sent on every write, as `[]` when empty — never omitted, never null.
// mcp_servers, mcp_access_groups, agent_access_groups and mcp_toolsets fail
// CLOSED on `[]` and take no sentinel.
func closedTeamGrant(accessGroupIDs []string) map[string]any {
	return map[string]any{
		"models": []string{modelDenyAllSentinel},
		"object_permission": map[string]any{
			"mcp_servers":         []string{},
			"mcp_access_groups":   []string{},
			"agents":              []string{agentDenyAllSentinel},
			"agent_access_groups": []string{},
			"mcp_toolsets":        []string{},
		},
		"access_group_ids": emptyIfNil(accessGroupIDs),
	}
}

// emptyIfNil coerces nil to a non-nil empty slice so encoding/json renders
// `[]` (an explicit LiteLLM clear), not `null` (merged as "absent").
func emptyIfNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
