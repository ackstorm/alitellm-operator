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
