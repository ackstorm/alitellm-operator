# ElevenLabs Model Discovery Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `elevenlabs` as a `LiteLLMModelDiscovery` provider type that lists models from the ElevenLabs `GET /v1/models` API and generates child `LiteLLMModel` CRs with `litellm_params.model: elevenlabs/<model_id>`.

**Architecture:** ElevenLabs is a hosted SaaS with a single public endpoint and a mandatory API key — the exact profile of the existing `anthropic`/`gemini` providers. We therefore mirror them: `credentialsSecretRef` required, `region`+`baseUrl` forbidden (CEL), a fixed production base URL in `defaultBaseURLs`, and a dedicated provider file. The wire format differs from every existing provider (auth header `xi-api-key`, response is a **bare JSON array** of `{model_id, name}`, no pagination), so it gets its own `elevenlabs.go` rather than reusing the OpenAI struct. The discovered ID maps to LiteLLM verbatim (`elevenlabs/<id>`), so the controller's child-builder needs **no** new typed-field overlay.

**Tech Stack:** Go 1.26.4, controller-runtime v0.19.4, kubebuilder v4.4.0 markers (CRD enum + CEL XValidation), `net/http` + `encoding/json` (no new deps), `httptest` for provider tests. All toolchain runs via the devtools container — every `make` target self-routes (host has no Go).

## Global Constraints

- Every new `*.go` file MUST start with `// SPDX-License-Identifier: Apache-2.0` (pre-push gate 15).
- No new third-party dependencies — `elevenlabs.go` uses only stdlib (`context`, `encoding/json`, `fmt`, `io`, `net/http`).
- Credential hygiene (§9.1 / MDISC-15): the API key travels in a header only; it MUST NEVER appear in returned error strings, `Candidate` fields, or logs. 401/403 errors carry a synthetic `fmt.Errorf("status %d")` cause — never the response body.
- HTTP contract (REL-04 / PATTERNS.md L277): `defer drainAndClose(resp.Body)` immediately after `Do`; read with `io.ReadAll(io.LimitReader(resp.Body, 4<<20))`.
- Error classification: 401/403 → `*ProviderAuthError{Provider: "elevenlabs"}`; other 4xx + 5xx → plain wrapped error with status code (no body); decode error → plain wrapped error (NOT `*ProviderAuthError`).
- Provider-dispatch D-01: the ONLY permitted `switch md.Spec.Type` is the credential-resolution switch in `modeldiscovery_controller.go`; all provider dispatch goes through `providers.Lookup`.
- Documentation hygiene: CRD/docs changes ship in the SAME commit as the code (CLAUDE.md rule). `make gen-manifests` + `make gen-crd-ref-docs` regenerate generated artifacts.
- The secret key for elevenlabs is `ELEVENLABS_API_KEY` (fixed, per the provider-table convention).
- The production base URL is `https://api.elevenlabs.io/v1`; `List` appends `/models` → `https://api.elevenlabs.io/v1/models`.

---

## File Structure

| File | Responsibility | Action |
|------|----------------|--------|
| `internal/providers/elevenlabs.go` | The provider: struct, constructor, `Type()`, `List()` | Create |
| `internal/providers/elevenlabs_test.go` | Provider unit tests (httptest) | Create |
| `internal/providers/registry.go` | Register `"elevenlabs"` → constructor + thin alias | Modify |
| `internal/providers/util.go` | Add `defaultBaseURLs["elevenlabs"]` | Modify |
| `internal/providers/interface.go` | Add `elevenlabs` to `Type()` / `ProviderConfig` godoc | Modify |
| `api/litellm/v1alpha1/modeldiscovery_types.go` | Enum value, CEL rule, godoc matrices | Modify |
| `internal/controller/modeldiscovery_controller.go` | `providerTypeElevenLabs` const + credential `case` | Modify |
| `config/samples/modeldiscovery-elevenlabs.yaml` | Discovery CR sample | Create |
| `config/samples/kustomization.yaml` | List the new sample | Modify |
| `docs/user-guide/model-discovery.md` | Provider matrix row + worked example | Modify |
| `config/crd/...` + `docs/api-reference/...` | Regenerated CRD + ref docs | Generated (no hand edit) |

---

## Task 1: ElevenLabs provider implementation (TDD)

**Files:**
- Create: `internal/providers/elevenlabs.go`
- Test: `internal/providers/elevenlabs_test.go`
- Modify: `internal/providers/util.go` (add default base URL)
- Modify: `internal/providers/registry.go` (register constructor)

**Interfaces:**
- Consumes: `ProviderConfig{APIKey, BaseURL, HTTPClient}`, `Candidate{ID, DisplayName}`, `Provider` interface, `*ProviderAuthError{Provider, Cause}`, `errMissingAPIKey`, `errNilHTTPClient`, `baseURLFor(providerType, cfgBaseURL string) string`, `drainAndClose(io.ReadCloser)` — all from `internal/providers`.
- Produces: `func newElevenLabsImpl(ctx context.Context, cfg ProviderConfig) (Provider, error)` and `func newElevenLabs(ctx context.Context, cfg ProviderConfig) (Provider, error)` (thin alias registered in `Registry["elevenlabs"]`); concrete type `*elevenlabsProvider` with `Type() string` returning `"elevenlabs"` and `List(ctx) ([]Candidate, error)`; const `providerTypeElevenLabs = "elevenlabs"`.

- [ ] **Step 1: Add the production base URL to `util.go`**

In `internal/providers/util.go`, add the `elevenlabs` entry to `defaultBaseURLs` (keep alphabetical-ish ordering after `anthropic`). The map currently is:

```go
var defaultBaseURLs = map[string]string{
	"anthropic": "https://api.anthropic.com/v1",
	"gemini":    "https://generativelanguage.googleapis.com/v1beta",
	"openai":    "https://api.openai.com/v1",
}
```

Change it to:

```go
var defaultBaseURLs = map[string]string{
	"anthropic":  "https://api.anthropic.com/v1",
	"elevenlabs": "https://api.elevenlabs.io/v1",
	"gemini":     "https://generativelanguage.googleapis.com/v1beta",
	"openai":     "https://api.openai.com/v1",
}
```

- [ ] **Step 2: Write the failing test file**

Create `internal/providers/elevenlabs_test.go`. The canary key, bare-array response shape (`[{"model_id":...,"name":...}]`), and `xi-api-key` header assertion are the ElevenLabs-specific parts; the auth/5xx/canary/registry structure mirrors `kubeai_test.go`.

```go
// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// canaryElevenLabsKey is the synthetic key the canary asserts is never
// logged or surfaced (MDISC-15 / §9.1).
const canaryElevenLabsKey = "xi-canary-XYZ-FAKE-elevenlabs"

// TestElevenLabs_HappyPath parses the bare-array /v1/models response and
// asserts the xi-api-key header carries the key (NOT Authorization).
func TestElevenLabs_HappyPath(t *testing.T) {
	var gotKey, gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("xi-api-key")
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"model_id":"eleven_multilingual_v2","name":"Eleven Multilingual v2"},{"model_id":"scribe_v1","name":"Scribe v1"}]`))
	}))
	defer srv.Close()

	p, err := newElevenLabs(context.Background(), ProviderConfig{
		Type: "elevenlabs", APIKey: canaryElevenLabsKey, BaseURL: srv.URL, HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("newElevenLabs err: %v", err)
	}
	if got := p.Type(); got != "elevenlabs" {
		t.Fatalf("Type() = %q; want elevenlabs", got)
	}
	cands, err := p.List(context.Background())
	if err != nil {
		t.Fatalf("List err: %v", err)
	}
	if len(cands) != 2 {
		t.Fatalf("got %d candidates; want 2", len(cands))
	}
	if cands[0].ID != "eleven_multilingual_v2" || cands[0].DisplayName != "Eleven Multilingual v2" {
		t.Errorf("cands[0] = %+v; want {eleven_multilingual_v2, Eleven Multilingual v2}", cands[0])
	}
	if cands[1].ID != "scribe_v1" {
		t.Errorf("cands[1].ID = %q; want scribe_v1", cands[1].ID)
	}
	if gotKey != canaryElevenLabsKey {
		t.Errorf("xi-api-key = %q; want %q", gotKey, canaryElevenLabsKey)
	}
	if gotAuth != "" {
		t.Errorf("Authorization header must be unset; got %q", gotAuth)
	}
	if gotPath != "/v1/models" && gotPath != "/models" {
		// srv.URL has no path, so baseURLFor(srv.URL)+"/models" → "/models".
		// Production base ".../v1" makes it "/v1/models". Accept both shapes.
		t.Errorf("request path = %q; want .../models", gotPath)
	}
}

// TestElevenLabs_MissingAPIKey_ReturnsConstructorError verifies the
// constructor synchronously rejects an empty APIKey (CEL requires
// credentialsSecretRef; this is the in-process backstop).
func TestElevenLabs_MissingAPIKey_ReturnsConstructorError(t *testing.T) {
	_, err := newElevenLabs(context.Background(), ProviderConfig{
		Type: "elevenlabs", APIKey: "", HTTPClient: http.DefaultClient,
	})
	if err == nil {
		t.Fatal("empty APIKey: want err; got nil")
	}
}

// TestElevenLabs_NilHTTPClient_ReturnsConstructorError verifies the
// universal HTTPClient gate.
func TestElevenLabs_NilHTTPClient_ReturnsConstructorError(t *testing.T) {
	_, err := newElevenLabs(context.Background(), ProviderConfig{
		Type: "elevenlabs", APIKey: canaryElevenLabsKey, HTTPClient: nil,
	})
	if err == nil {
		t.Fatal("nil HTTPClient: want err; got nil")
	}
}

// TestElevenLabs_401_ReturnsProviderAuthError verifies 401 → AuthFailed.
func TestElevenLabs_401_ReturnsProviderAuthError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	p, err := newElevenLabs(context.Background(), ProviderConfig{
		Type: "elevenlabs", APIKey: canaryElevenLabsKey, BaseURL: srv.URL, HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("newElevenLabs err: %v", err)
	}
	_, listErr := p.List(context.Background())
	var target *ProviderAuthError
	if !errors.As(listErr, &target) {
		t.Fatalf("401: want *ProviderAuthError; got %T %v", listErr, listErr)
	}
	if target.Provider != "elevenlabs" {
		t.Errorf("target.Provider = %q; want elevenlabs", target.Provider)
	}
}

// TestElevenLabs_5xx_ReturnsPlainError verifies 5xx → Unreachable (NOT AuthFailed).
func TestElevenLabs_5xx_ReturnsPlainError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	p, err := newElevenLabs(context.Background(), ProviderConfig{
		Type: "elevenlabs", APIKey: canaryElevenLabsKey, BaseURL: srv.URL, HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("newElevenLabs err: %v", err)
	}
	_, listErr := p.List(context.Background())
	if listErr == nil {
		t.Fatal("5xx: want err; got nil")
	}
	var target *ProviderAuthError
	if errors.As(listErr, &target) {
		t.Fatalf("5xx: must NOT be *ProviderAuthError; got %T", listErr)
	}
}

// TestElevenLabs_CredentialCanary enforces MDISC-15 / §9.1: even when the
// upstream echoes the key in a 401 body, it must not appear in the error.
func TestElevenLabs_CredentialCanary(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"detail":{"message":"Invalid API key: ` + canaryElevenLabsKey + `"}}`))
	}))
	defer srv.Close()

	p, err := newElevenLabs(context.Background(), ProviderConfig{
		Type: "elevenlabs", APIKey: canaryElevenLabsKey, BaseURL: srv.URL, HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("newElevenLabs err: %v", err)
	}
	_, listErr := p.List(context.Background())
	if listErr == nil {
		t.Fatal("401: want err; got nil")
	}
	if strings.Contains(listErr.Error(), canaryElevenLabsKey) {
		t.Fatalf("error string leaked canary: %s", listErr.Error())
	}
}

// TestElevenLabs_Registry_Routes verifies the registry entry resolves to
// the real constructor.
func TestElevenLabs_Registry_Routes(t *testing.T) {
	ctor, ok := Registry["elevenlabs"]
	if !ok {
		t.Fatal("Registry has no elevenlabs entry")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	p, err := ctor(context.Background(), ProviderConfig{
		Type: "elevenlabs", APIKey: canaryElevenLabsKey, BaseURL: srv.URL, HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("Registry[elevenlabs] err: %v", err)
	}
	if p.Type() != "elevenlabs" {
		t.Errorf("Type() = %q; want elevenlabs", p.Type())
	}
}
```

- [ ] **Step 3: Run the test to verify it fails (does not compile)**

Run: `make test-unit-pkg PKG=./internal/providers/...`
Expected: FAIL — compile error `undefined: newElevenLabs` (and `Registry["elevenlabs"]` lookup returns not-ok). This confirms the test is wired before the implementation exists.

- [ ] **Step 4: Write the provider implementation**

Create `internal/providers/elevenlabs.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// providerTypeElevenLabs is the wire-level discriminator for the
// ElevenLabs provider — used in Type(), endpoint resolution, and
// *ProviderAuthError.Provider. Extracted as a const so goconst stays
// quiet across the in-file occurrences.
const providerTypeElevenLabs = "elevenlabs"

// elevenlabsProvider holds the resolved per-CR state for one
// ModelDiscovery instance pointing at ElevenLabs. Same lifecycle as
// anthropicProvider / geminiProvider — built fresh by the reconciler
// each refresh; the manager-owned *http.Client owns connection reuse
// (D-02).
//
// ElevenLabs is a hosted SaaS with a single public endpoint, so
// spec.baseUrl is CEL-forbidden for this type and baseURL is "" in
// production (List resolves via defaultBaseURLs["elevenlabs"]); tests
// inject via cfg.BaseURL or SetTestBaseURL.
type elevenlabsProvider struct {
	apiKey     string
	httpClient *http.Client
	baseURL    string
}

// newElevenLabsImpl is the real constructor. ElevenLabs requires an API
// key (CEL requires spec.credentialsSecretRef) and the shared HTTP
// client. Validation order matches the other HTTP providers.
func newElevenLabsImpl(ctx context.Context, cfg ProviderConfig) (Provider, error) {
	_ = ctx
	if cfg.APIKey == "" {
		return nil, errMissingAPIKey
	}
	if cfg.HTTPClient == nil {
		return nil, errNilHTTPClient
	}
	return &elevenlabsProvider{
		apiKey:     cfg.APIKey,
		httpClient: cfg.HTTPClient,
		baseURL:    cfg.BaseURL,
	}, nil
}

// Type returns the discriminator literal "elevenlabs". Used by the
// reconciler for metrics labels only.
func (p *elevenlabsProvider) Type() string { return providerTypeElevenLabs }

// List issues GET <baseURL>/models with the key in the xi-api-key header
// (NOT Authorization, NOT the URL query — keeps the key out of every URL
// string per the gemini H1 posture). ElevenLabs returns a BARE JSON
// array (no {"data":[...]} envelope, no pagination) of model objects;
// we project model_id → Candidate.ID and name → Candidate.DisplayName.
//
// Error classification mirrors the other HTTP providers (MDISC-19):
//   - 401/403 → *ProviderAuthError (reason=AuthFailed; permanent). Cause
//     is synthetic — the response body is NEVER included (§9.1; the
//     upstream may echo the key in its error detail).
//   - other 4xx + 5xx → plain wrapped error with status code (NO body).
//   - decode err → plain wrapped error (NOT *ProviderAuthError).
//
// 4MB body cap (PATTERNS.md L277); REL-04 drain+close deferred.
func (p *elevenlabsProvider) List(ctx context.Context) ([]Candidate, error) {
	endpoint := baseURLFor(providerTypeElevenLabs, p.baseURL) + "/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("elevenlabs: build request: %w", err)
	}
	req.Header.Set("xi-api-key", p.apiKey)
	req.Header.Set("Accept", "application/json")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		// %w wrap only — net/http err strings may carry the URL (no key in it).
		return nil, fmt.Errorf("elevenlabs: transport error: %w", err)
	}
	defer drainAndClose(resp.Body) // REL-04

	switch {
	case resp.StatusCode == http.StatusUnauthorized,
		resp.StatusCode == http.StatusForbidden:
		return nil, &ProviderAuthError{
			Provider: providerTypeElevenLabs,
			Cause:    fmt.Errorf("status %d", resp.StatusCode),
		}
	case resp.StatusCode >= 400:
		return nil, fmt.Errorf("elevenlabs: list models: status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("elevenlabs: read response: %w", err)
	}

	// ElevenLabs /v1/models returns a bare array, NOT an OpenAI-style
	// {"data":[...]} envelope. Each element carries model_id (routable ID)
	// and name (human label). Capability flags (can_do_text_to_speech,
	// etc.) are intentionally ignored — narrowing is the user's job via
	// spec.filters, consistent with every other provider.
	var decoded []struct {
		ModelID string `json:"model_id"`
		Name    string `json:"name"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, fmt.Errorf("elevenlabs: decode response: %w", err)
	}

	candidates := make([]Candidate, 0, len(decoded))
	for _, m := range decoded {
		candidates = append(candidates, Candidate{
			ID:          m.ModelID,
			DisplayName: m.Name,
		})
	}
	return candidates, nil
}
```

- [ ] **Step 5: Register the constructor in `registry.go`**

In `internal/providers/registry.go`, add the map row (keep ordering after `bedrock`) inside the `Registry` literal:

```go
var Registry = map[string]func(ctx context.Context, cfg ProviderConfig) (Provider, error){
	"anthropic":  newAnthropic, // filled by 04-03a Task 2 (anthropic.go)
	"bedrock":    newBedrock,   // filled by 04-03c (bedrock.go)
	"elevenlabs": newElevenLabs, // elevenlabs.go (hosted SaaS, bare-array /v1/models)
	"gemini":     newGemini,    // filled by 04-03a Task 3 (gemini.go)
	"kubeai":     newKubeAI,    // filled by 04-03b (kubeai.go)
	"openai":     newOpenAI,    // filled by 04-03a Task 4 (openai.go)
}
```

Then add the thin alias near the other `new<Provider>` aliases at the bottom of the file:

```go
// newElevenLabs is implemented in elevenlabs.go as newElevenLabsImpl;
// this thin alias keeps the Registry map literal stable.
func newElevenLabs(ctx context.Context, cfg ProviderConfig) (Provider, error) {
	return newElevenLabsImpl(ctx, cfg)
}
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `make test-unit-pkg PKG=./internal/providers/...`
Expected: PASS — all `TestElevenLabs_*` green, no regressions in the other provider tests.

- [ ] **Step 7: Lint the touched package**

Run: `make qa-lint-changed`
Expected: PASS (no `goconst`/`gofmt`/`errcheck` findings on the new file). If `goconst` flags the `"elevenlabs"` literal in the test, that is acceptable in `_test.go` (mirror the `//nolint:goconst` used in `kubeai_test.go` line 39 only if the linter actually flags it).

- [ ] **Step 8: Commit**

```bash
git add internal/providers/elevenlabs.go internal/providers/elevenlabs_test.go \
        internal/providers/registry.go internal/providers/util.go
git commit -m "feat(providers): add ElevenLabs model-discovery provider"
```

---

## Task 2: CRD enum + CEL validation + godoc

**Files:**
- Modify: `api/litellm/v1alpha1/modeldiscovery_types.go` (enum, CEL rule, comments)
- Generated (no hand edit): `config/crd/bases/*litellmmodeldiscoveries*.yaml`, `deploy/helm/alitellm-operator/crds/*`, `docs/api-reference/`

**Interfaces:**
- Consumes: nothing new (marker-only change).
- Produces: admission now accepts `spec.type: elevenlabs` and enforces `credentialsSecretRef` required + `region`/`baseUrl` forbidden for that type.

- [ ] **Step 1: Add `elevenlabs` to the Enum marker**

In `api/litellm/v1alpha1/modeldiscovery_types.go`, on the `Type` field (currently line 38), change:

```go
	// +kubebuilder:validation:Enum=anthropic;bedrock;gemini;kubeai;openai
```

to:

```go
	// +kubebuilder:validation:Enum=anthropic;bedrock;elevenlabs;gemini;kubeai;openai
```

- [ ] **Step 2: Add the CEL XValidation rule**

In the marker block above the `LiteLLMModelDiscovery` struct (currently lines 514-520), add a rule for elevenlabs immediately after the `bedrock` rule (line 515). It mirrors the anthropic/gemini shape (requires `credentialsSecretRef`, forbids `region` and `baseUrl`):

```go
// +kubebuilder:validation:XValidation:rule="self.spec.type != 'elevenlabs' || (has(self.spec.credentialsSecretRef) && !has(self.spec.region) && !has(self.spec.baseUrl))",message="elevenlabs requires spec.credentialsSecretRef and forbids spec.region/spec.baseUrl"
```

- [ ] **Step 3: Update the godoc matrices**

Three comment sites in the same file must stay in sync (documentation hygiene rule). Make these edits:

(a) The provider field matrix near the top (currently lines 21-25) — add an `elevenlabs` line after `bedrock`:

```go
//	bedrock — requires region; forbids baseUrl; credentialsSecretRef optional.
//	elevenlabs — requires credentialsSecretRef; forbids region, baseUrl.
//	gemini — requires credentialsSecretRef; forbids region, baseUrl.
```

(b) The MDISC-01 enum-set comment (currently line 27):

```go
// MDISC-01 enforces spec.type ∈ {anthropic, bedrock, elevenlabs, gemini, kubeai, openai}
```

(c) The required-Secret-keys comment block in the `CredentialsSecretRef` godoc (currently lines 89-93) — add the elevenlabs key after bedrock:

```go
	// anthropic: ANTHROPIC_API_KEY
	// bedrock: AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY (AWS_SESSION_TOKEN optional)
	// elevenlabs: ELEVENLABS_API_KEY
	// gemini: GEMINI_API_KEY (or GOOGLE_API_KEY)
	// openai: OPENAI_API_KEY
```

(d) The MDISC-22 comment block on the `LiteLLMModelDiscovery` struct (currently lines 539-543) — add the elevenlabs key after bedrock:

```go
//	anthropic: ANTHROPIC_API_KEY
//	bedrock: AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY (AWS_SESSION_TOKEN optional)
//	elevenlabs: ELEVENLABS_API_KEY
//	gemini: GEMINI_API_KEY (or GOOGLE_API_KEY per provider docs)
//	openai: OPENAI_API_KEY
```

Also update the one-line provider parentheticals in the struct/type godoc that enumerate providers (lines 12-13 "anthropic, bedrock, gemini, kubeai, or openai" and lines 524-526) to include `elevenlabs` — e.g. "anthropic, bedrock, elevenlabs, gemini, kubeai, or openai".

- [ ] **Step 4: Regenerate manifests + ref docs**

Run: `make gen-manifests`
Then: `make gen-crd-ref-docs`
Expected: `config/crd/bases/*litellmmodeldiscoveries*.yaml`, the Helm `crds/` copy, and `docs/api-reference/` now list `elevenlabs` in the enum and carry the new CEL rule. Verify the diff is enum/CEL-only:

Run: `git status --short config/ deploy/helm docs/api-reference`
Expected: only the modeldiscovery CRD + api-reference files changed.

- [ ] **Step 5: Build + unit test (sanity that markers compile)**

Run: `make test-unit`
Expected: PASS (the api package compiles; no CEL syntax error surfaces at codegen).

- [ ] **Step 6: Commit**

```bash
git add api/litellm/v1alpha1/modeldiscovery_types.go config/ deploy/helm docs/api-reference
git commit -m "feat(crd): allow elevenlabs ModelDiscovery type with credentialsSecretRef"
```

---

## Task 3: Controller credential resolution

**Files:**
- Modify: `internal/controller/modeldiscovery_controller.go` (const + credential `case`)
- Test: `internal/controller/modeldiscovery_controller_test.go` (envtest — see Step 1)

**Interfaces:**
- Consumes: `r.resolveStringKey(ctx, namespace, ref, key)` → `(value string, missing bool, err error)`; `providers.RegisterTestProvider(t, "elevenlabs", p)` test seam; `providers.Lookup`.
- Produces: a reconcile path where `spec.type: elevenlabs` resolves `ELEVENLABS_API_KEY` from `credentialsSecretRef` and sets `cfg.APIKey` before dispatch; `providerTypeElevenLabs` const.

- [ ] **Step 1: Write the failing envtest**

Add to `internal/controller/modeldiscovery_controller_test.go`. This test uses the `RegisterTestProvider` seam (so no live HTTP) and asserts a `spec.type: elevenlabs` CR with a valid Secret reaches `Ready=Synced` and generates a child. Mirror the existing per-provider envtest in that file (find an existing `*ElevenLabs*`-shaped analog such as the gemini/openai discovery test and copy its structure). The unique assertions are: `spec.type: elevenlabs`, a Secret with key `ELEVENLABS_API_KEY`, and a generated child whose `spec.params.model` starts `elevenlabs/`.

```go
func TestModelDiscovery_ElevenLabs_GeneratesChildren(t *testing.T) {
	ctx := context.Background()
	ns := mustCreateNamespace(t) // existing helper; if the suite uses a fixed ns, follow that pattern instead

	// Stub the provider to return two ElevenLabs models without HTTP.
	providers.RegisterTestProvider(t, "elevenlabs", &stubProvider{
		typeLabel: "elevenlabs",
		candidates: []providers.Candidate{
			{ID: "eleven_multilingual_v2", DisplayName: "Eleven Multilingual v2"},
			{ID: "scribe_v1", DisplayName: "Scribe v1"},
		},
	})

	// Secret with the fixed elevenlabs key.
	mustCreateSecret(t, ctx, ns, "elevenlabs-credentials", map[string][]byte{
		"ELEVENLABS_API_KEY": []byte("xi-test-key"),
	})

	md := &litellmv1alpha1.LiteLLMModelDiscovery{
		ObjectMeta: metav1.ObjectMeta{Name: "elevenlabs", Namespace: ns},
		Spec: litellmv1alpha1.ModelDiscoverySpec{
			Type:                 "elevenlabs",
			CredentialsSecretRef: &litellmv1alpha1.SecretObjectRef{Name: "elevenlabs-credentials"},
			Refresh:              litellmv1alpha1.ModelDiscoveryRefresh{Interval: metav1.Duration{Duration: time.Minute}},
		},
	}
	mustCreate(t, ctx, md)

	// Reconcile until Ready=Synced, then assert children + their model param.
	waitForDiscoveryReady(t, ctx, ns, "elevenlabs")
	child := mustGetModel(t, ctx, ns, "elevenlabs.eleven-multilingual-v2")
	if !strings.Contains(string(child.Spec.Params.Raw), `"elevenlabs/eleven_multilingual_v2"`) {
		t.Fatalf("child params missing elevenlabs/ model: %s", child.Spec.Params.Raw)
	}
}
```

> NOTE for the implementer: the helper names above (`mustCreateNamespace`, `mustCreateSecret`, `mustCreate`, `waitForDiscoveryReady`, `mustGetModel`, `stubProvider`) are placeholders for whatever the existing `modeldiscovery_controller_test.go` already provides. Open that file FIRST, find the analogous gemini or openai discovery test, and reuse its exact helpers and `stubProvider` (or `RegisterTestProvider` usage). Do NOT invent new helpers if equivalents exist. The child name `elevenlabs.eleven-multilingual-v2` assumes the default-prefix + DNS-1123 normalization (`_` → `-`); confirm against the normalization the existing tests assert.

- [ ] **Step 2: Run the test to verify it fails**

Run: `make test-envtest-pkg PKG=./internal/controller/... FOCUS=TestModelDiscovery_ElevenLabs_GeneratesChildren`
Expected: FAIL — the credential switch hits the `default` arm (`unknown provider type "elevenlabs"`) → `Ready=False, reason=InvalidConfig`, so the CR never reaches Synced.

- [ ] **Step 3: Add the `providerTypeElevenLabs` const**

In `internal/controller/modeldiscovery_controller.go`, in the const block (currently lines 105-109), add after `providerTypeBedrock`:

```go
	providerTypeAnthropic  = "anthropic"
	providerTypeGemini     = "gemini"
	providerTypeOpenAI     = "openai"
	providerTypeBedrock    = "bedrock"
	providerTypeElevenLabs = "elevenlabs"
	providerTypeKubeAI     = "kubeai"
```

- [ ] **Step 4: Add the credential-resolution case**

In the `switch md.Spec.Type` block (currently line 400), add a `case` after the `providerTypeOpenAI` arm (line 433) — identical shape to anthropic/gemini/openai but with the elevenlabs key:

```go
	case providerTypeElevenLabs:
		key, missing, err := r.resolveStringKey(ctx, md.Namespace, md.Spec.CredentialsSecretRef, "ELEVENLABS_API_KEY")
		if err != nil && !missing {
			return ctrl.Result{}, err // transient → controller-runtime backoff
		}
		if missing {
			res := r.writeReadyAndSource(ctx, &md, reasonSecretNotFound, err.Error())
			res.RequeueAfter = connection.DefaultRequeueOnRejectedAfter
			return res, nil
		}
		cfg.APIKey = key
```

> No change to `buildChildModel` or the `litellmProvider` mapping (lines 532-537): elevenlabs maps verbatim to `elevenlabs/<id>` and needs no typed-field overlay (no region, no api_base — baseUrl is forbidden and defaulted in the provider).

- [ ] **Step 5: Run the test to verify it passes**

Run: `make test-envtest-pkg PKG=./internal/controller/... FOCUS=TestModelDiscovery_ElevenLabs_GeneratesChildren`
Expected: PASS — CR reaches `Ready=Synced`; child `elevenlabs.eleven-multilingual-v2` exists with `spec.params.model: elevenlabs/eleven_multilingual_v2`.

- [ ] **Step 6: Run the full controller envtest (no regressions)**

Run: `make test-envtest-pkg PKG=./internal/controller/...`
Expected: PASS (or, if the resource-starved host flakes at suite setup per CLAUDE.md, confirm the focused test passes and defer the full sweep to CI's Envtest job).

- [ ] **Step 7: Lint**

Run: `make qa-lint-changed`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/controller/modeldiscovery_controller.go internal/controller/modeldiscovery_controller_test.go
git commit -m "feat(controller): resolve ELEVENLABS_API_KEY for elevenlabs ModelDiscovery"
```

---

## Task 4: Provider interface godoc

**Files:**
- Modify: `internal/providers/interface.go`

**Interfaces:**
- Consumes/Produces: comment-only; keeps the `Provider.Type()` enum list and `ProviderConfig` per-provider doc in sync.

- [ ] **Step 1: Update `Type()` enum list**

In `internal/providers/interface.go`, the `Type` method doc (currently lines 30-31) reads:

```go
	// Type returns the spec.type enum literal:
	// "anthropic"|"bedrock"|"gemini"|"kubeai"|"openai".
```

Change the literal list to include elevenlabs:

```go
	// Type returns the spec.type enum literal:
	// "anthropic"|"bedrock"|"elevenlabs"|"gemini"|"kubeai"|"openai".
```

- [ ] **Step 2: Add elevenlabs to the constructor-responsibilities doc**

In the `ProviderConfig` godoc (currently lines 52-62), add a line after the `gemini` bullet:

```go
	// - elevenlabs: requires APIKey, HTTPClient. BaseURL CEL-forbidden in
	//   production (test-only override via SetTestBaseURL / cfg.BaseURL).
```

- [ ] **Step 3: Verify it still builds**

Run: `make test-unit-pkg PKG=./internal/providers/...`
Expected: PASS (comment-only change; tests already green from Task 1).

- [ ] **Step 4: Commit**

```bash
git add internal/providers/interface.go
git commit -m "docs(providers): list elevenlabs in Provider interface godoc"
```

---

## Task 5: Sample CR + user-guide docs

**Files:**
- Create: `config/samples/modeldiscovery-elevenlabs.yaml`
- Modify: `config/samples/kustomization.yaml`
- Modify: `docs/user-guide/model-discovery.md`

**Interfaces:**
- Consumes: the elevenlabs type shipped in Tasks 1-3.
- Produces: a runnable sample + user-facing documentation.

- [ ] **Step 1: Create the sample CR**

Create `config/samples/modeldiscovery-elevenlabs.yaml` (mirrors `modeldiscovery-gemini.yaml`'s header style):

```yaml
# ModelDiscovery sample: ElevenLabs (audio — TTS / STT)
#
# CEL rules exercised:
#   - spec.type=elevenlabs matches the Enum constraint
#   - credentialsSecretRef is REQUIRED for elevenlabs
#   - region is FORBIDDEN for elevenlabs
#   - baseUrl is FORBIDDEN for elevenlabs (hosted SaaS, single endpoint)
#   - refresh.interval >= 1m (MDISC-05 floor)
#
# ElevenLabs /v1/models returns TTS, STT, and voice-conversion models all
# mixed together. filters.include narrows to the v2/v3 TTS + scribe STT
# models below; drop the filter to surface every model the account exposes.
#
# Required Secret: elevenlabs-credentials with key:
#   ELEVENLABS_API_KEY
#
# Reconciler call: GET https://api.elevenlabs.io/v1/models
# with the `xi-api-key: $ELEVENLABS_API_KEY` header.
#
# NOTE: the LiteLLM proxy must have STORE_MODEL_IN_DB=True or every child
# model's POST /model/new 500s (see CLAUDE.md common failure modes).
apiVersion: litellm.ackstorm.ai/v1alpha1
kind: LiteLLMModelDiscovery
metadata:
  labels:
    app.kubernetes.io/name: alitellm-operator
    app.kubernetes.io/managed-by: kustomize
  name: elevenlabs-models
  namespace: default
spec:
  type: elevenlabs
  credentialsSecretRef:
    name: elevenlabs-credentials
  secrets:
    - { as: ELEVENLABS_API_KEY, secretRef: { name: elevenlabs-credentials, key: ELEVENLABS_API_KEY } }
  params:
    api_key: "{{ELEVENLABS_API_KEY}}"
  filters:
    include:
      - "^eleven_.*_v2$"
      - "^eleven_v3$"
      - "^scribe_v1$"
  refresh:
    interval: 10m
```

> The `secrets[]` + `params.api_key: "{{ELEVENLABS_API_KEY}}"` pair propagates an inference-time key to each child Model (the discovery-time `credentialsSecretRef` is operator-side ONLY per MDISC-15 — it is never reused for inference). This mirrors the anthropic sample in `docs/user-guide/model-discovery.md` lines 75-79.

- [ ] **Step 2: Register the sample in kustomization**

In `config/samples/kustomization.yaml`, add the new file to the Phase 4 provider sample list (after `modeldiscovery-bedrock.yaml`):

```yaml
- modeldiscovery-anthropic.yaml
- modeldiscovery-bedrock.yaml
- modeldiscovery-elevenlabs.yaml
- modeldiscovery-gemini.yaml
- modeldiscovery-kubeai.yaml
- modeldiscovery-openai.yaml
```

- [ ] **Step 3: Add the provider-matrix row to the user guide**

In `docs/user-guide/model-discovery.md`:

(a) Update the `spec.type` enum row (currently line 13):

```markdown
| `spec.type`                 | yes             | Enum: `anthropic`, `bedrock`, `elevenlabs`, `gemini`, `kubeai`, `openai`.              |
```

(b) Add a row to the Provider field matrix (after the `bedrock` row, currently line 30):

```markdown
| `elevenlabs`| `credentialsSecretRef`      | `region`, `baseUrl`           | `ELEVENLABS_API_KEY`                                                      |
```

- [ ] **Step 4: Add a worked example section to the user guide**

In `docs/user-guide/model-discovery.md`, after the OpenAI-compatible third-parties section (around line 149), add:

```markdown
## ElevenLabs — audio (TTS / STT)

ElevenLabs is a hosted SaaS audio provider. Like `anthropic`/`gemini` it
requires `credentialsSecretRef` and forbids `region`/`baseUrl` (single
public endpoint). The reconciler calls `GET https://api.elevenlabs.io/v1/models`
with the `xi-api-key` header and generates children with
`spec.params.model: "elevenlabs/<raw-id>"`.

```yaml
apiVersion: litellm.ackstorm.ai/v1alpha1
kind: LiteLLMModelDiscovery
metadata:
  name: elevenlabs
spec:
  type: elevenlabs
  prefix: elevenlabs
  credentialsSecretRef: { name: elevenlabs-credentials }   # key: ELEVENLABS_API_KEY
  secrets:
    - { as: ELEVENLABS_API_KEY, secretRef: { name: elevenlabs-credentials, key: ELEVENLABS_API_KEY } }
  params:
    api_key: "{{ELEVENLABS_API_KEY}}"
  filters:
    include: [ "^eleven_.*_v2$", "^eleven_v3$", "^scribe_v1$" ]
  refresh:
    interval: 10m
```

`/v1/models` returns TTS, STT, and voice-conversion models mixed together;
use `filters.include` to narrow. The discovery-time `credentialsSecretRef`
key is operator-side only — the inference-time key for each child flows via
`secrets[]` + `params.api_key` (MDISC-15 separation). The LiteLLM proxy must
run with `STORE_MODEL_IN_DB=True` or each child's `POST /model/new` 500s.
```

- [ ] **Step 5: Build the docs site to catch broken markup**

Run: `make docs-build`
Expected: site builds with no broken-link / missing-page errors for the edited page.

- [ ] **Step 6: Commit**

```bash
git add config/samples/modeldiscovery-elevenlabs.yaml config/samples/kustomization.yaml \
        docs/user-guide/model-discovery.md
git commit -m "docs: add ElevenLabs ModelDiscovery sample and user-guide section"
```

---

## Task 6: Final verification gate

**Files:** none (verification only).

- [ ] **Step 1: Full unit + provider + controller suite**

Run: `make test-unit`
Expected: PASS.

Run: `make test-envtest-pkg PKG=./internal/controller/...`
Expected: PASS (or focused elevenlabs test green + defer full sweep to CI per the host-starvation caveat in CLAUDE.md).

- [ ] **Step 2: Full lint sweep**

Run: `make qa-lint`
Expected: PASS.

- [ ] **Step 3: Confirm generated artifacts are in sync**

Run: `make gen-manifests gen-crd-ref-docs`
Then: `git status --short`
Expected: clean tree (no uncommitted regen drift). If anything changed, the codegen was not committed in Task 2 — amend/commit it.

- [ ] **Step 4: Dry-run the pre-push gate (optional sanity)**

Run: `make pre-push`
Expected: PASS — SPDX headers present on the new `.go` files, `go mod tidy` clean (no new deps), secret scanners clean (the canary keys are synthetic and confined to `_test.go` / samples; if gitleaks flags `xi-canary-...` add it to `.gitleaks.toml`), lint + unit green.

> Per CLAUDE.md, do NOT run `make pre-push` immediately before `git push` once `make hooks` is installed — the hook fires it automatically. Step 4 is a dry-run only.

- [ ] **Step 5: E2E scope note (no action unless requested)**

ElevenLabs discovery hits an external SaaS, so it is intentionally NOT wired into the kind e2e suite (`test/e2e/cluster/`) — the provider is covered by `internal/providers` httptest unit tests and the controller envtest via `RegisterTestProvider`. If e2e coverage is later required, add a mock `/v1/models` backend under `test/e2e/cluster/03-mocks` and a standing `elevenlabs` Discovery in `04-hydration` (out of scope for this plan).

---

## Self-Review

**1. Spec coverage**

- Discovery provider for elevenlabs → Task 1 (provider) + Task 3 (controller wiring). ✓
- CRD accepts `elevenlabs` with correct required/forbidden fields → Task 2 (enum + CEL). ✓
- Standalone `LiteLLMModel` "support" → already works via pass-through (`spec.params.model: elevenlabs/...`); documented in the Task 5 user-guide note (verbatim `elevenlabs/<id>` mapping confirmed at controller line 532 — no code needed). ✓
- "All models, use spec.filters" decision → provider returns every `model_id`; capability flags ignored (Task 1 Step 4 comment + Task 5 sample uses `filters.include`). ✓
- Docs/example scope decision → Task 5. ✓
- Documentation-hygiene rule (same-commit CRD/docs) → Tasks 2 & 5 regenerate + edit docs alongside code. ✓

**2. Placeholder scan**

- Every code step shows complete code. The one unavoidable "follow the existing helper" instruction is the envtest in Task 3 Step 1, where the existing per-provider test helpers MUST be reused rather than invented — flagged explicitly with a NOTE, not left as a silent TODO. Acceptable because inventing parallel helpers would violate the repo's DRY/surgical-change rules.

**3. Type consistency**

- `newElevenLabs` / `newElevenLabsImpl` / `elevenlabsProvider` / `providerTypeElevenLabs` used identically across Tasks 1, 3, 4. ✓
- Controller const `providerTypeElevenLabs = "elevenlabs"` (Task 3) is distinct from the providers-package const of the same name (Task 1) — intentional per the existing pattern (controller re-declares wire labels locally, see controller lines 102-109 comment). ✓
- Secret key string `"ELEVENLABS_API_KEY"` identical in CRD godoc (Task 2), controller case (Task 3), sample (Task 5). ✓
- Base URL `https://api.elevenlabs.io/v1` + `List` appends `/models` → `/v1/models`, matching the documented endpoint. ✓
