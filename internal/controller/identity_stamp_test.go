// SPDX-License-Identifier: Apache-2.0

// identity_stamp_test.go — FIX4.txt H-1 regression guards on the
// stampMCPIdentity / stampA2AIdentity helpers. These run as pure-unit
// tests (no envtest, no LiteLLM HTTP) because the helpers are
// data-only mutators on a freeform bag. They assert:
//
//  1. CREATE stamp injects BOTH created_by + updated_by.
//  2. UPDATE stamp injects ONLY updated_by (preserves LiteLLM's
//     immutable-creator audit semantics).
//  3. Existing keys in the bag are preserved (the stamp does not
//     clobber user-supplied mcp_info / agent_card_params data).
//  4. nil-bag inputs are tolerated (the stamp allocates).
//
// The controller-layer call sites are exercised by the envtest
// reconcile suites and the e2e LiteLLM round-trip in /test/e2e — this
// file locks the helper contract in isolation so future refactors of
// the bag-key shape are caught at the unit layer first.

package controller

import (
	"testing"

	"github.com/ackstorm/alitellm-operator/internal/identity"
	"github.com/ackstorm/alitellm-operator/internal/litellm"
)

func TestStampMCPIdentity_CreateInjectsBoth(t *testing.T) {
	got := stampMCPIdentity(nil, true)
	if got == nil {
		t.Fatalf("stampMCPIdentity(nil, true) returned nil; want allocated map")
	}
	if cb, _ := got["created_by"].(string); cb != identity.Operator() {
		t.Errorf("created_by: got %q, want %q", cb, identity.Operator())
	}
	if ub, _ := got["updated_by"].(string); ub != identity.Operator() {
		t.Errorf("updated_by: got %q, want %q", ub, identity.Operator())
	}
}

func TestStampMCPIdentity_UpdateOmitsCreatedBy(t *testing.T) {
	got := stampMCPIdentity(nil, false)
	if _, ok := got["created_by"]; ok {
		t.Errorf("updated_by-only stamp leaked created_by: %v", got)
	}
	if ub, _ := got["updated_by"].(string); ub != identity.Operator() {
		t.Errorf("updated_by: got %q, want %q", ub, identity.Operator())
	}
}

func TestStampMCPIdentity_PreservesExistingKeys(t *testing.T) {
	bag := map[string]any{
		"description": "user-set value",
		"team_owner":  "alpha",
	}
	got := stampMCPIdentity(bag, true)
	if got["description"] != "user-set value" {
		t.Errorf("description clobbered: %v", got)
	}
	if got["team_owner"] != "alpha" {
		t.Errorf("team_owner clobbered: %v", got)
	}
}

func TestStampA2AIdentity_CreateInjectsBoth(t *testing.T) {
	cfg := &litellm.AgentConfig{AgentName: "x"}
	stampA2AIdentity(cfg, true)
	if cfg.AgentCardParams == nil {
		t.Fatalf("AgentCardParams not allocated by stampA2AIdentity")
	}
	if cb, _ := cfg.AgentCardParams["created_by"].(string); cb != identity.Operator() {
		t.Errorf("created_by: got %q, want %q", cb, identity.Operator())
	}
	if ub, _ := cfg.AgentCardParams["updated_by"].(string); ub != identity.Operator() {
		t.Errorf("updated_by: got %q, want %q", ub, identity.Operator())
	}
}

func TestStampA2AIdentity_UpdateOmitsCreatedBy(t *testing.T) {
	cfg := &litellm.AgentConfig{
		AgentName:       "x",
		AgentCardParams: map[string]any{"url": "https://existing"},
	}
	stampA2AIdentity(cfg, false)
	if _, ok := cfg.AgentCardParams["created_by"]; ok {
		t.Errorf("updated_by-only stamp leaked created_by: %v", cfg.AgentCardParams)
	}
	if cfg.AgentCardParams["url"] != "https://existing" {
		t.Errorf("existing url key clobbered: %v", cfg.AgentCardParams)
	}
	if ub, _ := cfg.AgentCardParams["updated_by"].(string); ub != identity.Operator() {
		t.Errorf("updated_by: got %q, want %q", ub, identity.Operator())
	}
}
