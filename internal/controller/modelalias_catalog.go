// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/ackstorm/alitellm-operator/internal/litellm"
)

// OpenCode reads its model catalog from <OPENCODE_MODELS_URL>/api.json using
// the models.dev schema. That schema is models.dev's, not an industry
// standard, and OpenCode is its only significant consumer — hence the
// hardcoded shape here rather than a pluggable format abstraction.
//
// The catalog REPLACES the upstream catalog on the client rather than merging
// into it, so a client pointed at this file sees only our provider. That is
// the intended behaviour for a proxy-only setup.

const (
	// catalogProviderID is the provider key in the emitted catalog, and the
	// prefix OpenCode shows in its model picker.
	catalogProviderID = "ackstorm"

	// catalogConfigMapKey is the ConfigMap data key. It is also the filename
	// the serving side exposes, because OpenCode fetches <base>/api.json.
	catalogConfigMapKey = "api.json"

	// catalogEnvConfigMap addresses the ConfigMap the catalog is written to,
	// as "<namespace>/<name>". Empty disables catalog generation entirely.
	catalogEnvConfigMap = "OPENCODE_CATALOG_CONFIGMAP"

	// catalogEnvAPIBase is the PUBLIC LiteLLM base URL clients reach. The
	// operator cannot derive it — its own endpoint is the in-cluster Service.
	catalogEnvAPIBase = "OPENCODE_CATALOG_API_BASE"

	// catalogEnvFilters optionally configures JSON include/exclude patterns
	// to filter which alias names are rendered into the catalog.
	catalogEnvFilters = "OPENCODE_CATALOG_FILTERS"

	// costPerMillion converts LiteLLM's per-token prices to the
	// per-million-token prices models.dev expects.
	costPerMillion = 1e6

	// Fallbacks for a target whose model_info carries no limits. LiteLLM
	// omits them for models absent from its cost map; OpenCode needs a number.
	catalogDefaultContext = 128000
	catalogDefaultOutput  = 8192
)

// catalogModalities maps a LiteLLM model_info capability flag to the
// models.dev input modality it implies. "text" is always present.
var catalogModalities = []struct {
	flag     string
	modality string
}{
	{"supports_vision", "image"},
	{"supports_audio_input", "audio"},
	{"supports_pdf_input", "pdf"},
	{"supports_video_input", "video"},
}

// CatalogFilters specifies include/exclude regex patterns for alias names.
type CatalogFilters struct {
	Include []string `json:"include,omitempty"`
	Exclude []string `json:"exclude,omitempty"`
}

// CatalogConfig is the operator-level OpenCode catalog configuration.
type CatalogConfig struct {
	// ConfigMapNamespace / ConfigMapName address the ConfigMap the rendered
	// catalog is written to. This is normally the namespace of the service
	// that mounts and serves it, NOT the operator's own — a ConfigMap can
	// only be mounted by a pod in its own namespace.
	ConfigMapNamespace string
	ConfigMapName      string
	// APIBase is the public LiteLLM base URL, e.g. https://api.example.com/v1.
	APIBase string
	// Filters optionally narrows which aliases are rendered into the catalog.
	Filters *CatalogFilters
}

// Enabled reports whether catalog generation is configured.
func (c CatalogConfig) Enabled() bool { return c.ConfigMapName != "" }

// CatalogConfigFromEnv reads the catalog configuration. An unset
// OPENCODE_CATALOG_CONFIGMAP disables the feature and is not an error; a
// malformed or half-configured one IS, because the operator would otherwise
// silently produce nothing while someone waits for a file to appear.
func CatalogConfigFromEnv() (CatalogConfig, error) {
	raw := strings.TrimSpace(os.Getenv(catalogEnvConfigMap))
	if raw == "" {
		return CatalogConfig{}, nil
	}
	ns, name, ok := strings.Cut(raw, "/")
	if !ok || ns == "" || name == "" || strings.Contains(name, "/") {
		return CatalogConfig{}, fmt.Errorf(
			"%s must be \"<namespace>/<name>\", got %q", catalogEnvConfigMap, raw)
	}
	apiBase := strings.TrimSpace(os.Getenv(catalogEnvAPIBase))
	if apiBase == "" {
		return CatalogConfig{}, fmt.Errorf(
			"%s is required when %s is set", catalogEnvAPIBase, catalogEnvConfigMap)
	}

	var filters *CatalogFilters
	if rawFilters := strings.TrimSpace(os.Getenv(catalogEnvFilters)); rawFilters != "" {
		var f CatalogFilters
		if err := json.Unmarshal([]byte(rawFilters), &f); err != nil {
			return CatalogConfig{}, fmt.Errorf("%s must be valid JSON: %w", catalogEnvFilters, err)
		}
		filters = &f
	}

	return CatalogConfig{
		ConfigMapNamespace: ns,
		ConfigMapName:      name,
		APIBase:            apiBase,
		Filters:            filters,
	}, nil
}

// filterAlias returns true if alias matches include/exclude filters.
// If filters is nil or empty, all aliases pass.
func (f *CatalogFilters) filterAlias(alias string) (bool, error) {
	if f == nil {
		return true, nil
	}
	if len(f.Include) > 0 {
		matched := false
		for _, pat := range f.Include {
			re, err := regexp.Compile(pat)
			if err != nil {
				return false, fmt.Errorf("catalog filter include %q: %w", pat, err)
			}
			if re.MatchString(alias) {
				matched = true
				break
			}
		}
		if !matched {
			return false, nil
		}
	}
	for _, pat := range f.Exclude {
		re, err := regexp.Compile(pat)
		if err != nil {
			return false, fmt.Errorf("catalog filter exclude %q: %w", pat, err)
		}
		if re.MatchString(alias) {
			return false, nil
		}
	}
	return true, nil
}

// RenderOpenCodeCatalog renders the aggregate alias map into an OpenCode
// catalog document.
//
// desired is ModelAliasAggregate.Desired (alias name → target model_name).
// rows is the full GET /model/info set. Each alias is resolved to its TARGET
// row — never to a row named after the alias itself — so rendering does not
// depend on LiteLLM having reloaded the model_group_alias map the reconciler
// just wrote. LiteLLM does expand aliases into rows of their own, but only
// after that reload, which is a race we simply do not need to enter.
//
// An alias is dropped when its target is absent from rows, or when the target
// is not a chat model: OpenCode only drives chat completions, so embedding,
// tts, stt and image_generation entries would be dead rows in its picker.
// Router models (auto_router/*) report no mode and drop out the same way.
func RenderOpenCodeCatalog(
	desired map[string]string,
	rows []litellm.ModelInfoResponse,
	cfg CatalogConfig,
) ([]byte, error) {
	byName := make(map[string]litellm.ModelInfo, len(rows))
	for _, row := range rows {
		byName[row.ModelName] = row.ModelInfo
	}

	models := map[string]any{}
	for alias, target := range desired {
		if cfg.Filters != nil {
			pass, err := cfg.Filters.filterAlias(alias)
			if err != nil {
				return nil, err
			}
			if !pass {
				continue
			}
		}
		info, ok := byName[target]
		if !ok {
			continue
		}
		if catalogString(info.Extra, "mode") != "chat" {
			continue
		}
		models[alias] = renderCatalogModel(alias, info)
	}

	doc := map[string]any{
		catalogProviderID: map[string]any{
			"id":     catalogProviderID,
			"name":   "ACKSTORM",
			"npm":    "@ai-sdk/openai-compatible",
			"api":    cfg.APIBase,
			"env":    []string{},
			"models": models,
		},
	}
	return json.MarshalIndent(doc, "", "  ")
}

func renderCatalogModel(alias string, info litellm.ModelInfo) map[string]any {
	inputs := []string{"text"}
	for _, m := range catalogModalities {
		if catalogBool(info.Extra, m.flag) {
			inputs = append(inputs, m.modality)
		}
	}
	return map[string]any{
		"id":   alias,
		"name": alias,
		// attachment is what actually unlocks file/image attach in the
		// OpenCode TUI; the modalities list only says which types it takes.
		"attachment":  catalogBool(info.Extra, "supports_vision"),
		"reasoning":   catalogBool(info.Extra, "supports_reasoning"),
		"tool_call":   catalogBool(info.Extra, "supports_function_calling"),
		"temperature": true,
		"modalities":  map[string]any{"input": inputs, "output": []string{"text"}},
		"limit": map[string]any{
			"context": catalogInt(info.Extra, "max_input_tokens", catalogDefaultContext),
			"output":  catalogInt(info.Extra, "max_output_tokens", catalogDefaultOutput),
		},
		"cost": map[string]any{
			"input":      catalogNumber(info.Extra, "input_cost_per_token") * costPerMillion,
			"output":     catalogNumber(info.Extra, "output_cost_per_token") * costPerMillion,
			"cache_read": catalogNumber(info.Extra, "cache_read_input_token_cost") * costPerMillion,
		},
	}
}

// The four accessors below read ModelInfo.Extra, which is decoded JSON: every
// key may be absent, null, or the wrong type (LiteLLM nulls out capabilities
// it does not know). Each returns a zero value rather than erroring — a
// missing capability is "not supported", not a failure to render.

func catalogString(extra map[string]any, key string) string {
	s, _ := extra[key].(string)
	return s
}

func catalogBool(extra map[string]any, key string) bool {
	b, _ := extra[key].(bool)
	return b
}

func catalogNumber(extra map[string]any, key string) float64 {
	f, _ := extra[key].(float64)
	return f
}

func catalogInt(extra map[string]any, key string, fallback int) int {
	f, ok := extra[key].(float64)
	if !ok || f <= 0 {
		return fallback
	}
	return int(f)
}

// writeCatalogConfigMap renders the catalog and upserts it into the
// configured ConfigMap.
//
// It writes through CatalogWriter, NOT r.Client: the manager's cache is
// scoped to WATCH_NAMESPACE, and CreateOrUpdate issues a Get that would go
// through that cache and fail with "unknown namespace for the cache" for a
// ConfigMap in the serving namespace. That is a client-side error no RBAC
// grant can fix, so this one object needs an uncached client.
func (r *ModelAliasReconciler) writeCatalogConfigMap(
	ctx context.Context,
	agg ModelAliasAggregate,
	cli *litellm.Client,
) error {
	rows, err := cli.ListModelInfo(ctx)
	if err != nil {
		return fmt.Errorf("list model info: %w", err)
	}
	rendered, err := RenderOpenCodeCatalog(agg.Desired, rows, r.Catalog)
	if err != nil {
		return fmt.Errorf("render catalog: %w", err)
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name:      r.Catalog.ConfigMapName,
		Namespace: r.Catalog.ConfigMapNamespace,
	}}
	_, err = controllerutil.CreateOrUpdate(ctx, r.CatalogWriter, cm, func() error {
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data[catalogConfigMapKey] = string(rendered)
		return nil
	})
	if err != nil {
		return fmt.Errorf("upsert ConfigMap %s/%s: %w",
			r.Catalog.ConfigMapNamespace, r.Catalog.ConfigMapName, err)
	}
	return nil
}
