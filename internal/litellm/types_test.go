// SPDX-License-Identifier: Apache-2.0

package litellm

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestMCPServerUpdateRequest_ForwardsOAuth2Flow guards against review
// finding #2: oauth2_flow must survive a PUT /v1/mcp/server, matching the
// CREATE request which already carries the field.
func TestMCPServerUpdateRequest_ForwardsOAuth2Flow(t *testing.T) {
	req := &MCPServerUpdateRequest{
		ServerID:   "srv-123",
		OAuth2Flow: "authorization_code",
	}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"oauth2_flow":"authorization_code"`) {
		t.Errorf("oauth2_flow not serialized on update request: %s", b)
	}
}

func TestModelInfo_MarshalJSON_MergesExtra(t *testing.T) {
	mi := ModelInfo{
		CreatedBy: "alitellm-operator/test",
		Extra: map[string]any{
			"base_model": "gpt-4o-mini",
			"tier":       "paid",
			"custom_key": "v1",
		},
	}
	b, err := json.Marshal(mi)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"created_by", "base_model", "tier", "custom_key"} {
		if _, ok := got[k]; !ok {
			t.Errorf("marshaled model_info missing key %q; got %s", k, b)
		}
	}
}

func TestModelInfo_MarshalJSON_EmptyStaysEmpty(t *testing.T) {
	// CR-16: an empty ModelInfo must NOT serialize "id":"" (omitempty) and
	// must produce a bare object (no spurious keys).
	b, err := json.Marshal(ModelInfo{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(b) != "{}" {
		t.Errorf("empty ModelInfo: want {}, got %s", b)
	}
}

func TestModelInfo_MarshalJSON_TypedFieldWinsOverExtra(t *testing.T) {
	// Operator overlay (typed field) must win over a colliding Extra key.
	mi := ModelInfo{CreatedBy: "operator", Extra: map[string]any{"created_by": "user"}}
	b, _ := json.Marshal(mi)
	var got map[string]any
	_ = json.Unmarshal(b, &got)
	if got["created_by"] != "operator" {
		t.Errorf("created_by: want operator (typed wins), got %v", got["created_by"])
	}
}
