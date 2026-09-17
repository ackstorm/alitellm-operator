// SPDX-License-Identifier: Apache-2.0

package litellm

import (
	"encoding/json"
	"reflect"
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

func TestModelInfoUnmarshalPopulatesExtra(t *testing.T) {
	const raw = `{"id":"info-1","supports_vision":true,"max_input_tokens":1048576}`

	var mi ModelInfo
	if err := json.Unmarshal([]byte(raw), &mi); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if mi.ID != "info-1" {
		t.Errorf("ID = %q, want info-1", mi.ID)
	}
	if mi.Extra["supports_vision"] != true {
		t.Errorf("Extra[supports_vision] = %v, want true", mi.Extra["supports_vision"])
	}
	// Typed fields must NOT be duplicated into Extra — MarshalJSON would then
	// emit them twice.
	if _, dup := mi.Extra["id"]; dup {
		t.Error("Extra must not carry the typed `id` key")
	}
}

func TestModelInfoUnmarshalMarshalRoundTrip(t *testing.T) {
	const raw = `{"id":"info-1","created_by":"alitellm-operator","supports_vision":true}`

	var mi ModelInfo
	if err := json.Unmarshal([]byte(raw), &mi); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	b, err := json.Marshal(mi)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal round-trip: %v", err)
	}
	if got["id"] != "info-1" || got["created_by"] != "alitellm-operator" || got["supports_vision"] != true {
		t.Errorf("round-trip lost fields: %s", b)
	}
	// No key must appear duplicated — json.Unmarshal into a map would not
	// show a duplicate anyway, so assert field count matches exactly.
	if len(got) != 3 {
		t.Errorf("round-trip: want 3 keys, got %d: %s", len(got), b)
	}
}

// TestModelInfoUnmarshalPreservesNullCapability guards a real LiteLLM
// payload shape: GET /model/info returns JSON `null` (not an absent key)
// for a capability the model does not declare. The key must land in Extra
// with a nil value — not be dropped — so a future guard like `if v != nil`
// in the decode/re-encode loop would show up here as a red test rather
// than silently start eating null-valued keys.
func TestModelInfoUnmarshalPreservesNullCapability(t *testing.T) {
	const raw = `{"id":"info-1","supports_vision":null}`

	var mi ModelInfo
	if err := json.Unmarshal([]byte(raw), &mi); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Two-value lookup: Extra["absent-key"] would also read nil, so only
	// the ok-bool distinguishes "present with null value" from "absent".
	v, ok := mi.Extra["supports_vision"]
	if !ok {
		t.Fatal("Extra missing supports_vision key — null-valued capabilities must not be dropped")
	}
	if v != nil {
		t.Errorf("Extra[supports_vision] = %v, want nil", v)
	}

	b, err := json.Marshal(mi)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"supports_vision":null`) {
		t.Errorf("re-marshal must preserve the null capability: %s", b)
	}
}

// TestModelInfoUnmarshalTypedOnlyLeavesExtraNil covers the len(all)==0
// branch: a payload carrying only typed keys must leave Extra nil, not an
// empty non-nil map, matching MarshalJSON's len==0 no-op convention
// (CR-16: empty ModelInfo must round-trip to "{}", not "{}"+dangling map).
func TestModelInfoUnmarshalTypedOnlyLeavesExtraNil(t *testing.T) {
	const raw = `{"id":"info-1"}`

	var mi ModelInfo
	if err := json.Unmarshal([]byte(raw), &mi); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if mi.Extra != nil {
		t.Errorf("Extra = %#v, want nil for a typed-keys-only payload", mi.Extra)
	}
}

// TestModelInfoTypedKeysMatchStructTags makes modelInfoTypedKeys drift
// loud: it derives the expected key set from the ModelInfo struct's json
// tags via reflection (test-only — the shipped decode/encode path stays
// reflection-free) and asserts it matches modelInfoTypedKeys exactly. A
// field added to the struct without a matching entry in the hand-written
// list would otherwise decode into Extra AND stay typed, double-emitting
// the key on re-marshal with nothing to catch it.
func TestModelInfoTypedKeysMatchStructTags(t *testing.T) {
	want := map[string]bool{}
	typ := reflect.TypeOf(ModelInfo{})
	for i := 0; i < typ.NumField(); i++ {
		tag := typ.Field(i).Tag.Get("json")
		if tag == "-" {
			continue // Extra itself
		}
		name, _, _ := strings.Cut(tag, ",") // strip ",omitempty"
		want[name] = true
	}

	got := map[string]bool{}
	for _, k := range modelInfoTypedKeys {
		if got[k] {
			t.Fatalf("modelInfoTypedKeys has a duplicate entry %q", k)
		}
		got[k] = true
	}

	for k := range want {
		if !got[k] {
			t.Errorf("modelInfoTypedKeys missing struct field key %q — it will decode into Extra and double-emit on re-marshal", k)
		}
	}
	for k := range got {
		if !want[k] {
			t.Errorf("modelInfoTypedKeys has stale key %q — no matching struct field", k)
		}
	}
}
