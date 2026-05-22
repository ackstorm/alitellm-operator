// SPDX-License-Identifier: Apache-2.0

package litellm

import "strings"

// MCPToolPrefixSeparatorDefault matches LiteLLM's own default value for
// the MCP_TOOL_PREFIX_SEPARATOR env var ("-"). Used when the
// LiteLLMConnection CR has not explicitly set spec.mcpToolPrefixSeparator
// (so an empty snapshot value still produces correct behavior).
const MCPToolPrefixSeparatorDefault = "-"

// SanitizeMCPServerName rewrites a Kubernetes metadata.name into a
// LiteLLM-safe server_name + alias by replacing the configured MCP tool
// prefix separator with the opposite valid character.
//
// LiteLLM rejects its `MCP_TOOL_PREFIX_SEPARATOR` env value inside
// `server_name` at `POST /v1/mcp/server` time (HTTP 400 "Server name
// cannot contain '<sep>'."). The two values in scope are "." and "-";
// the helper swaps the configured separator for the other character so
// the wire payload is always accepted regardless of the LiteLLM
// instance's configuration.
//
// Behavior:
//   - separator "." → all "." in name replaced with "-"
//   - separator "-" → all "-" in name replaced with "."
//   - separator ""  → treated as the LiteLLM default ("-")
//   - any other value → defensive passthrough (CEL on
//     LiteLLMConnection.spec.mcpToolPrefixSeparator already enforces the
//     {".", "-"} enum, so this branch is never hit in production)
//
// The K8s-side metadata.name is left untouched — sanitization is wire-
// boundary only. See FIX.txt HIGH-1 (2026-05-22).
func SanitizeMCPServerName(name, separator string) string {
	switch separator {
	case ".":
		return strings.ReplaceAll(name, ".", "-")
	case "", MCPToolPrefixSeparatorDefault:
		return strings.ReplaceAll(name, "-", ".")
	default:
		return name
	}
}
