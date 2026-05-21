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
//
// 04-03a fills newAnthropic, newGemini, newOpenAI (HTTP providers with
// shared scaffolding). 04-03b fills newKubeAI as a thin constructor that
// returns *openaiProvider with typeLabel="kubeai" (KubeAI is OpenAI
// wire-format with two divergences: baseUrl required, apiKey optional —
// handled by typeLabel parameterization rather than a separate struct).
// 04-03c fills newBedrock with the aws-sdk-go-v2 ListFoundationModels
// implementation; the registry is now COMPLETE — all 5 providers shipped.
var Registry = map[string]func(ctx context.Context, cfg ProviderConfig) (Provider, error){
	"anthropic": newAnthropic, // filled by 04-03a Task 2 (anthropic.go)
	"bedrock":   newBedrock,   // filled by 04-03c (bedrock.go)
	"gemini":    newGemini,    // filled by 04-03a Task 3 (gemini.go)
	"kubeai":    newKubeAI,    // filled by 04-03b (kubeai.go)
	"openai":    newOpenAI,    // filled by 04-03a Task 4 (openai.go)
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

// The constructors below are TASK-1 STUBS — replaced by Tasks 2/3/4
// in this same plan when each provider's full implementation lands.
// They exist here only so the Registry map and TestRegistry_HasFiveProviders
// can compile and pass at the end of Task 1 before per-provider files
// exist. Tasks 2-4 DELETE these stubs and ADD their real constructor in
// the corresponding {anthropic,gemini,openai}.go file.

// newAnthropic is implemented in anthropic.go as newAnthropicImpl; this
// thin alias keeps the Registry map literal stable while letting the
// real implementation live in its own file.
func newAnthropic(ctx context.Context, cfg ProviderConfig) (Provider, error) {
	return newAnthropicImpl(ctx, cfg)
}

// newGemini is implemented in gemini.go as newGeminiImpl; thin alias
// keeps the Registry map literal stable.
func newGemini(ctx context.Context, cfg ProviderConfig) (Provider, error) {
	return newGeminiImpl(ctx, cfg)
}

// newOpenAI is implemented in openai.go as newOpenAIImpl; thin alias
// keeps the Registry map literal stable. Per the user-mandated
// reshape (memory: kubeai-openai-consolidation), the underlying
// *openaiProvider struct carries a typeLabel field; newOpenAI sets it
// to "openai" and newKubeAI will set it to "kubeai".
func newOpenAI(ctx context.Context, cfg ProviderConfig) (Provider, error) {
	return newOpenAIImpl(ctx, cfg)
}
