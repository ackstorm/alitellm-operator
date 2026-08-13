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

// missingRefs collects spec.permission entries that name a LiteLLM resource
// the operator could not resolve to an id. The caller parks + requeues the
// Team on a non-empty list (ordering dependency with the CRs that create
// those resources) rather than silently under-granting.
type missingRefs struct {
	Agents       []string
	Toolsets     []string
	AccessGroups []string
}

// projectPermission renders a typed spec.permission block into the two
// LiteLLM team body fields it maps to: the top-level `models` list (merged
// specific model names + model access-group names) and the nested
// `object_permission` object (mcp_servers, mcp_access_groups, agents,
// agent_access_groups, mcp_toolsets).
//
// agentNameToID resolves human-friendly A2A agent NAMES to the agent_id
// UUIDs LiteLLM enforces on object_permission.agents (names are silently
// ignored by LiteLLM). toolsetNameToID does the same for MCP toolset names →
// toolset_id UUIDs on object_permission.mcp_toolsets. Names absent from
// either map are returned in the corresponding missingRefs slice so the
// caller can requeue (reason=AgentNotFound / ToolsetNotFound) instead of
// silently dropping the grant; successfully-resolved entries are still
// projected.
//
// ALWAYS-EMIT contract (security-critical — reverses the original omit-empty
// design). When the block is present the operator OWNS `models` AND all five
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
// mcp_servers / mcp_access_groups / agent_access_groups / mcp_toolsets —
// LiteLLM treats an empty list there as fail-CLOSED (0 resources). But for
// `models` and object_permission.agents an empty list is fail-OPEN: LiteLLM
// reads it as "no filter" and the team inherits the full master-key ceiling
// (verified in prod: a brand-new team with models=[] object_permission=None
// saw all 427 models and all 7 agents). So when the block is present but the
// list resolves empty, those two fields project their deny-all sentinel
// instead of `[]` — a single value no real resource can match. The other four
// stay `[]`.
//
// mcp_toolsets specifically takes NO sentinel, and that asymmetry is
// load-bearing: LiteLLM's toolset check reads "granted is None or id not in
// granted → deny", so an empty list already denies everything (verified on
// 1.93.0 — an ungranted key gets `403 API key does not have access to toolset
// '<uuid>'`). Adding a sentinel here in the name of consistency would inject a
// bogus UUID into a filter that is already correct, so do not.
//
// The third return value, accessGroupIDs, is NOT part of object_permission: it
// projects onto the team's TOP-LEVEL `access_group_ids` from
// spec.permission.accessGroups (unified access-group NAMES resolved to ids via
// accessGroupNameToID). It joins mcp_toolsets in the no-sentinel group — an
// access group is a GRANT that only ADDS permissions, so an empty list widens
// nothing and is already fail-CLOSED. It still obeys the ALWAYS-EMIT contract
// (non-nil, so an emptied list clears the attachment instead of keeping it).
func projectPermission(
	perm *litellmv1alpha1.PermissionSpec,
	agentNameToID map[string]string,
	toolsetNameToID map[string]string,
	accessGroupNameToID map[string]string,
) (models []string, objectPermission map[string]any, accessGroupIDs []string, missing missingRefs) {
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

	resolved, missingAgents := resolveNames(perm.Agents, agentNameToID)
	missing.Agents = missingAgents
	// DENY-BY-DEFAULT: an unset agents list fails OPEN in LiteLLM (empty →
	// no filter → every agent). Substitute the null-UUID sentinel. Scoped to
	// the len(perm.Agents)==0 branch ONLY — a non-empty list whose names don't
	// resolve populates missingAgents and the caller aborts (AgentNotFound),
	// so the sentinel must never stand in for unresolved names.
	if len(perm.Agents) == 0 {
		resolved = []string{agentDenyAllSentinel}
	}

	// MCP toolset names → toolset_id UUIDs. NO deny-by-default sentinel: the
	// LiteLLM toolset check is already fail-CLOSED on an empty list (see the
	// contract note above), so `[]` is the correct "grant nothing".
	resolvedToolsets, missingToolsets := resolveNames(perm.McpToolsets, toolsetNameToID)
	missing.Toolsets = missingToolsets

	// Unified access-group names → access_group_id UUIDs. NO deny-by-default
	// sentinel: an empty access_group_ids grants nothing (there is no group to
	// widen through), so the field is already fail-CLOSED — same reasoning as
	// mcp_toolsets. A sentinel here would inject a bogus id into a filter that
	// is already correct.
	resolvedAccessGroups, missingAccessGroups := resolveNames(perm.AccessGroups, accessGroupNameToID)
	missing.AccessGroups = missingAccessGroups

	// Every sub-field emitted unconditionally (as [] when empty) — see the
	// ALWAYS-EMIT contract above.
	objectPermission = map[string]any{
		"mcp_servers":         emptyIfNil(perm.McpServers),
		"mcp_access_groups":   emptyIfNil(perm.McpGroups),
		"agents":              resolved,
		"agent_access_groups": emptyIfNil(perm.AgentGroups),
		"mcp_toolsets":        resolvedToolsets, // non-nil; [] is a valid fail-closed clear
	}

	return models, objectPermission, resolvedAccessGroups, missing
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
