// SPDX-License-Identifier: Apache-2.0

package litellm

import "strings"

// MCPToolPrefixSeparatorDefault is the operator-side default for the
// LiteLLMConnection.spec.mcpToolPrefixSeparator field when unset. It is
// "." — the empirically-confirmed safe direction against LiteLLM
// v1.85.1's stock configuration, which forbids "." inside server_name
// regardless of the MCP_TOOL_PREFIX_SEPARATOR env var (FIX2.txt HIGH-1,
// 2026-05-22).
//
// Prior to v0.1.3 the default was "-"; users who relied on that and run
// a non-stock LiteLLM that forbids "-" must set
// spec.mcpToolPrefixSeparator explicitly to "-".
const MCPToolPrefixSeparatorDefault = "."

// SanitizeMCPServerName returns name with the LiteLLM-side forbidden
// character (the configured separator) replaced by the opposite valid
// character — BUT ONLY IF the input actually contains the forbidden
// char. Inputs without the forbidden char are returned unchanged so
// existing records stay stable across upgrade boundaries (FIX2.txt
// HIGH-9, 2026-05-22).
//
// Behavior:
//   - separator "." → if name contains ".", each is replaced with "-"; else unchanged
//   - separator "-" → if name contains "-", each is replaced with "."; else unchanged
//   - separator ""  → treated as MCPToolPrefixSeparatorDefault
//   - any other value → defensive passthrough (CEL on
//     LiteLLMConnection.spec.mcpToolPrefixSeparator already enforces the
//     {".", "-"} enum so this branch is never hit in production)
//
// The K8s-side metadata.name is left untouched — sanitization is wire-
// boundary only.
func SanitizeMCPServerName(name, separator string) string {
	forbidden := separator
	if forbidden == "" {
		forbidden = MCPToolPrefixSeparatorDefault
	}
	if !strings.Contains(name, forbidden) {
		return name
	}
	switch forbidden {
	case ".":
		return strings.ReplaceAll(name, ".", "-")
	case "-":
		return strings.ReplaceAll(name, "-", ".")
	default:
		return name
	}
}
