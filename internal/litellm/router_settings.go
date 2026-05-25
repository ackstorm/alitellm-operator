// SPDX-License-Identifier: Apache-2.0

package litellm

import (
	"context"
	"encoding/json"
	"fmt"
)

const (
	configCallbacksPath = "/get/config/callbacks"
	configUpdatePath    = "/config/update"
)

// GetRouterSettings issues GET /get/config/callbacks and returns the
// router_settings block split into ModelGroupAlias (typed) and Extra
// (opaque pass-through of every other key).
//
// Empty or absent router_settings returns a zero-value *RouterSettings
// (Extra and ModelGroupAlias both empty — safe to mutate at the call site).
func (c *Client) GetRouterSettings(ctx context.Context) (*RouterSettings, error) {
	raw, err := c.makeRequest(ctx, "GET", configCallbacksPath, nil)
	if err != nil {
		return nil, err
	}
	var env ConfigCallbacksResponse
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("litellm: decode GET %s: %w", configCallbacksPath, err)
	}
	out := &RouterSettings{
		Extra:           map[string]any{},
		ModelGroupAlias: map[string]string{},
	}
	for k, v := range env.RouterSettings {
		if k == "model_group_alias" {
			if v == nil {
				continue
			}
			m, ok := v.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("litellm: router_settings.model_group_alias is not an object (got %T)", v)
			}
			for kk, vv := range m {
				sv, ok := vv.(string)
				if !ok {
					return nil, fmt.Errorf("litellm: router_settings.model_group_alias[%q] is not a string (got %T)", kk, vv)
				}
				out.ModelGroupAlias[kk] = sv
			}
			continue
		}
		out.Extra[k] = v
	}
	return out, nil
}

// UpdateRouterSettings issues POST /config/update with a body shaped as
// `{"router_settings": {<extra keys>..., "model_group_alias": {<map>}}}`.
// Callers are expected to have read-merged via GetRouterSettings first so
// keys outside model_group_alias survive the round-trip.
func (c *Client) UpdateRouterSettings(ctx context.Context, rs *RouterSettings) error {
	if rs == nil {
		return fmt.Errorf("litellm: UpdateRouterSettings: nil RouterSettings")
	}
	merged := make(map[string]any, len(rs.Extra)+1)
	for k, v := range rs.Extra {
		merged[k] = v
	}
	alias := make(map[string]any, len(rs.ModelGroupAlias))
	for k, v := range rs.ModelGroupAlias {
		alias[k] = v
	}
	merged["model_group_alias"] = alias
	body := map[string]any{"router_settings": merged}
	_, err := c.makeRequest(ctx, "POST", configUpdatePath, body)
	return err
}
