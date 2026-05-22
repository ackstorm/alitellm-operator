// SPDX-License-Identifier: Apache-2.0

package litellm

import "strings"

// SanitizeMCPServerName converts a Kubernetes metadata.name into a LiteLLM-safe
// server_name + alias by replacing '.' with '-'. LiteLLM v1.83.10+ rejects '.'
// in server_name with HTTP 400 "Server name cannot contain '.'." See FIX.txt
// HIGH-1 (2026-05-22). The K8s-side metadata.name is left untouched — only the
// wire payload is rewritten.
func SanitizeMCPServerName(name string) string {
	return strings.ReplaceAll(name, ".", "-")
}
