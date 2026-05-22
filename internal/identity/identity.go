// SPDX-License-Identifier: Apache-2.0

// Package identity provides the operator's audit identity for upstream
// LiteLLM writes. The Version global is overridden at link time via
// ldflags (-X github.com/ackstorm/alitellm-operator/internal/identity.Version=X.Y.Z),
// matching the version embedded in the goreleaser build. The default
// "dev" applies under `go run` / `go test`.
//
// FIX2.txt MEDIUM-8 (2026-05-22): LiteLLM's Models UI showed
// "Created By: Unknown" for every operator-managed model because the
// model_info.created_by / updated_by fields were left zero-valued.
// Operator() returns the literal "alitellm-operator/<Version>" that
// reconcilers stamp on every CREATE + UPDATE wire payload where the
// LiteLLM schema accepts an audit field.
package identity

// Version is overridden at link time by goreleaser:
//
//	-X github.com/ackstorm/alitellm-operator/internal/identity.Version=v0.1.3
//
// Default "dev" applies under non-release builds (go run, go test,
// IDE debug runs).
var Version = "dev"

// Operator returns the audit identity literal threaded into LiteLLM
// /model/new, /model/update, /team/new, /v1/mcp/server, /a2a/agent
// requests where the schema accepts an audit field.
//
// Format: "alitellm-operator/<Version>".
func Operator() string {
	return "alitellm-operator/" + Version
}
