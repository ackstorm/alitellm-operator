// SPDX-License-Identifier: MIT

package litellm

import "testing"

func TestIsAgentConfigKey_AcceptsAccessGroupTags(t *testing.T) {
	for _, k := range []string{"agent_access_groups", "access_groups"} {
		if !IsAgentConfigKey(k) {
			t.Errorf("IsAgentConfigKey(%q) = false; the tag must not raise UnknownParamKey", k)
		}
	}
	if IsAgentConfigKey("model_info") {
		t.Error("model_info must stay unknown")
	}
}
