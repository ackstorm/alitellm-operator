// SPDX-License-Identifier: Apache-2.0

package controller

import (
	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
)

// Deny-by-default sentinels (security-critical — see the DENY-BY-DEFAULT
// contract on projectPermission). LiteLLM 1.83.10 activates the `models` /
// object_permission.agents filter as soon as the list is NON-EMPTY; it never
// validates that the elements exist. So an unset list must project a single
// value that cannot match any real resource — deny-all — rather than `[]`
// (which LiteLLM reads as "no filter" → the team inherits the master-key
// ceiling). Verified in prod on LiteLLM 1.83.10: a garbage model name → 0
// real models, the null UUID → 0 agents.
const (
	// modelDenyAllSentinel: no real model or model access-group can be named
	// this, so LiteLLM's `models` filter matches nothing.
	modelDenyAllSentinel = "__deny_all__"
	// agentDenyAllSentinel: the null UUID — object_permission.agents matches on
	// agent_id, and no agent is ever minted with the all-zero UUID.
	agentDenyAllSentinel = "00000000-0000-0000-0000-000000000000"
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
// UNCONDITIONALLY, never omitted. This is mandatory because LiteLLM's POST
// /team/update MERGES per-field on the persistent object_permission row (same
// object_permission_id across updates): a present field is replaced, `[]`
// clears it, but an OMITTED field keeps its STALE value. Omitting a
// shrunk-to-empty field therefore silently fails to revoke access — a
// Ready=Synced CR while LiteLLM still grants the removed resource (verified on
// LiteLLM 1.83.10 in prod). Every returned slice is non-nil so encoding/json
// renders `[]` (an explicit clear), never `null` (which LiteLLM's merge treats
// as "field absent" → stale-value kept).
//
// DENY-BY-DEFAULT contract (security-critical). Emitting `[]` is correct for
// mcp_servers / mcp_access_groups / agent_access_groups — LiteLLM treats an
// empty list there as fail-CLOSED (0 resources). But for `models` and
// object_permission.agents an empty list is fail-OPEN: LiteLLM reads it as "no
// filter" and the team inherits the full master-key ceiling (verified in prod:
// a brand-new team with models=[] object_permission=None saw all 427 models
// and all 7 agents). So when the block is present but the list resolves empty,
// those two fields project their deny-all sentinel instead of `[]` — a single
// value no real resource can match. The other three stay `[]`.
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
	// DENY-BY-DEFAULT: an empty models list fails OPEN in LiteLLM (no filter →
	// master-key ceiling). Substitute the deny-all sentinel so the team sees no
	// model. A non-empty list is projected verbatim.
	if len(models) == 0 {
		models = []string{modelDenyAllSentinel}
	}

	resolved := make([]string, 0, len(perm.Agents))
	for _, name := range perm.Agents {
		id, ok := agentNameToID[name]
		if !ok {
			missingAgents = append(missingAgents, name)
			continue
		}
		resolved = append(resolved, id)
	}
	// DENY-BY-DEFAULT: an unset agents list fails OPEN in LiteLLM (empty →
	// no filter → every agent). Substitute the null-UUID sentinel. Scoped to
	// the len(perm.Agents)==0 branch ONLY — a non-empty list whose names don't
	// resolve populates missingAgents and the caller aborts (AgentNotFound),
	// so the sentinel must never stand in for unresolved names.
	if len(perm.Agents) == 0 {
		resolved = []string{agentDenyAllSentinel}
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
