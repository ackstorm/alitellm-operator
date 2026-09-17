// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"encoding/json"
	"testing"

	"github.com/ackstorm/alitellm-operator/internal/litellm"
)

const catalogTestTarget = "gemini.gemini-pro-latest"

// chatRow is a /model/info row for a fully-capable chat model, mirroring the
// shape LiteLLM actually returns (numbers decode as float64, per-token costs).
func chatRow() litellm.ModelInfoResponse {
	return litellm.ModelInfoResponse{
		ModelName: catalogTestTarget,
		ModelInfo: litellm.ModelInfo{Extra: map[string]any{
			"mode":                        "chat",
			"supports_vision":             true,
			"supports_pdf_input":          true,
			"supports_function_calling":   true,
			"supports_reasoning":          true,
			"supports_audio_input":        nil, // LiteLLM nulls unknown capabilities
			"max_input_tokens":            float64(1048576),
			"max_output_tokens":           float64(65535),
			"input_cost_per_token":        1.25e-06,
			"output_cost_per_token":       1e-05,
			"cache_read_input_token_cost": 1.25e-07,
		}},
	}
}

func decodeCatalog(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("catalog is not valid JSON: %v", err)
	}
	prov, ok := doc[catalogProviderID].(map[string]any)
	if !ok {
		t.Fatalf("catalog has no %q provider: %s", catalogProviderID, raw)
	}
	models, ok := prov["models"].(map[string]any)
	if !ok {
		t.Fatalf("provider has no models object: %s", raw)
	}
	return models
}

func TestRenderOpenCodeCatalogMapsCapabilities(t *testing.T) {
	desired := map[string]string{"ackstorm.smart": catalogTestTarget}
	raw, err := RenderOpenCodeCatalog(desired, []litellm.ModelInfoResponse{chatRow()},
		CatalogConfig{APIBase: "https://api.example.com/v1"})
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	models := decodeCatalog(t, raw)
	m, ok := models["ackstorm.smart"].(map[string]any)
	if !ok {
		t.Fatalf("alias missing from catalog: %v", models)
	}
	if m["attachment"] != true || m["tool_call"] != true || m["reasoning"] != true {
		t.Errorf("capability flags wrong: %v", m)
	}
	// A null capability must NOT become a modality — audio is null here.
	inputs := m["modalities"].(map[string]any)["input"].([]any)
	want := []any{"text", "image", "pdf"}
	if len(inputs) != len(want) {
		t.Fatalf("modalities.input = %v, want %v", inputs, want)
	}
	for i := range want {
		if inputs[i] != want[i] {
			t.Errorf("modalities.input = %v, want %v", inputs, want)
			break
		}
	}
	// LiteLLM prices per token; models.dev expects per million.
	cost := m["cost"].(map[string]any)
	if cost["input"] != 1.25 || cost["output"] != 10.0 || cost["cache_read"] != 0.125 {
		t.Errorf("cost not converted to per-million: %v", cost)
	}
	lim := m["limit"].(map[string]any)
	if lim["context"] != float64(1048576) || lim["output"] != float64(65535) {
		t.Errorf("limit = %v", lim)
	}
}

func TestRenderOpenCodeCatalogProviderFields(t *testing.T) {
	raw, err := RenderOpenCodeCatalog(map[string]string{"a": catalogTestTarget},
		[]litellm.ModelInfoResponse{chatRow()},
		CatalogConfig{APIBase: "https://api.example.com/v1"})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	prov := doc[catalogProviderID].(map[string]any)
	// These three are what let a user delete their hand-written provider
	// block from opencode.json entirely.
	if prov["api"] != "https://api.example.com/v1" {
		t.Errorf("api = %v", prov["api"])
	}
	if prov["npm"] != "@ai-sdk/openai-compatible" {
		t.Errorf("npm = %v", prov["npm"])
	}
	if env := prov["env"].([]any); len(env) != 1 || env[0] != "LITELLM_API_KEY" {
		t.Errorf("env = %v", env)
	}
}

func TestRenderOpenCodeCatalogDropsNonChatAndUnresolvable(t *testing.T) {
	desired := map[string]string{
		"ackstorm.smart":  catalogTestTarget,
		"ackstorm.tts":    "openai.gpt-4o-mini-tts", // mode audio_speech
		"ackstorm.embed":  "gemini.embedding-001",   // mode embedding
		"ackstorm.router": "ackstorm.router-target", // routers report no mode
		"ackstorm.ghost":  "gemini.does-not-exist",  // absent from /model/info
	}
	rows := []litellm.ModelInfoResponse{
		chatRow(),
		{ModelName: "openai.gpt-4o-mini-tts", ModelInfo: litellm.ModelInfo{
			Extra: map[string]any{"mode": "audio_speech"}}},
		{ModelName: "gemini.embedding-001", ModelInfo: litellm.ModelInfo{
			Extra: map[string]any{"mode": "embedding"}}},
		{ModelName: "ackstorm.router-target", ModelInfo: litellm.ModelInfo{
			Extra: map[string]any{}}},
	}

	raw, err := RenderOpenCodeCatalog(desired, rows, CatalogConfig{APIBase: "x"})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	models := decodeCatalog(t, raw)
	if len(models) != 1 {
		t.Fatalf("models = %v, want only ackstorm.smart", models)
	}
	if _, ok := models["ackstorm.smart"]; !ok {
		t.Errorf("chat alias dropped: %v", models)
	}
}

func TestRenderOpenCodeCatalogFallsBackOnMissingLimits(t *testing.T) {
	rows := []litellm.ModelInfoResponse{{
		ModelName: catalogTestTarget,
		ModelInfo: litellm.ModelInfo{Extra: map[string]any{"mode": "chat"}},
	}}
	raw, err := RenderOpenCodeCatalog(map[string]string{"a": catalogTestTarget}, rows,
		CatalogConfig{APIBase: "x"})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	m := decodeCatalog(t, raw)["a"].(map[string]any)
	lim := m["limit"].(map[string]any)
	// OpenCode needs a number; LiteLLM omits limits for models it has no
	// cost-map entry for.
	if lim["context"] != float64(catalogDefaultContext) || lim["output"] != float64(catalogDefaultOutput) {
		t.Errorf("limit = %v, want the fallbacks", lim)
	}
	if m["attachment"] != false || m["tool_call"] != false {
		t.Errorf("absent capabilities must render false, got %v", m)
	}
}

func TestRenderOpenCodeCatalogEmptyIsValid(t *testing.T) {
	raw, err := RenderOpenCodeCatalog(map[string]string{}, nil, CatalogConfig{APIBase: "x"})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if models := decodeCatalog(t, raw); len(models) != 0 {
		t.Errorf("want an empty models object, got %v", models)
	}
}

func TestCatalogConfigFromEnv(t *testing.T) {
	for _, tc := range []struct {
		name, cm, api string
		want          CatalogConfig
		wantErr       bool
	}{
		{name: "disabled when unset"},
		{
			name: "ns/name splits",
			cm:   "alitellm-auth/opencode-catalog", api: "https://api.example.com/v1",
			want: CatalogConfig{
				ConfigMapNamespace: "alitellm-auth",
				ConfigMapName:      "opencode-catalog",
				APIBase:            "https://api.example.com/v1",
			},
		},
		{name: "bare name rejected", cm: "opencode-catalog", api: "x", wantErr: true},
		{name: "empty namespace rejected", cm: "/opencode-catalog", api: "x", wantErr: true},
		{name: "too many segments rejected", cm: "a/b/c", api: "x", wantErr: true},
		{name: "enabled without api base rejected", cm: "ns/name", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(catalogEnvConfigMap, tc.cm)
			t.Setenv(catalogEnvAPIBase, tc.api)
			got, err := CatalogConfigFromEnv()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
			if got.Enabled() != (tc.want.ConfigMapName != "") {
				t.Errorf("Enabled() = %v for %+v", got.Enabled(), got)
			}
		})
	}
}
