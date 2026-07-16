// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"context"
	"sync"
)

// registryMu guards Registry against concurrent map writes from the
// RegisterTestProvider seam (test setup + t.Cleanup) and reads from the
// reconciler hot path. Production callers MUST go through Lookup; test
// seams in testseam.go take the write lock.
var registryMu sync.RWMutex

// 04-03b note: newKubeAI lives in kubeai.go (real constructor). It
// constructs *openaiProvider with typeLabel="kubeai" per the
// kubeai-openai-consolidation reshape — no dedicated kubeAIProvider
// struct exists. The map entry below still references newKubeAI by name;
// the function is defined in kubeai.go and replaces the 04-03a stub
// that previously lived in this file.
//
// 04-03c note: newBedrock lives in bedrock.go (real constructor — the
// 04-03a stub that previously lived here has been removed). The
// constructor builds an aws-sdk-go-v2 client via D-05 two-path
// credential resolution and is the only provider that does NOT use the
// shared *http.Client from D-02 (the AWS SDK manages its own internal
// transport).

// Registry is the spec.type → constructor map. The Discovery reconciler
// uses this to dispatch without branching on spec.type.
// Adding a provider is exactly one row.
//
// D-01: this is the ONLY place per-type branching is permitted. The
// reconciler MUST call Registry[md.Spec.Type](ctx, cfg) directly; any
// switch on spec.type outside this file is a regression.
var Registry = map[string]func(ctx context.Context, cfg ProviderConfig) (Provider, error){
	"anthropic":  newAnthropicImpl,
	"bedrock":    newBedrock,
	"elevenlabs": newElevenLabsImpl,
	"gemini":     newGeminiImpl,
	"kubeai":     newKubeAI,
	"openai":     newOpenAIImpl,
}

// Lookup returns the constructor registered for providerType and a
// presence flag. Takes the read lock so concurrent reconciles do not
// race against test-seam writes via RegisterTestProvider (testseam.go).
// Production code should use this instead of indexing Registry directly.
func Lookup(providerType string) (func(ctx context.Context, cfg ProviderConfig) (Provider, error), bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	ctor, ok := Registry[providerType]
	return ctor, ok
}
