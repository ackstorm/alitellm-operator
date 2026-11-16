// SPDX-License-Identifier: MIT

package litellm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"net/url"
)

// Virtual keys. Every call except the /key/generate body names a key by
// HashKey(value): a raw key in a URL is written to HTTP access logs. Shapes
// measured in references/litellm-key-api.md.

// HashKey returns the sha256 hex LiteLLM stores as a key's token.
func HashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// KeyInfo is the `info` object of GET /key/info.
type KeyInfo struct {
	KeyAlias       string         `json:"key_alias"`
	Models         []string       `json:"models"`
	MaxBudget      *float64       `json:"max_budget"`
	BudgetDuration *string        `json:"budget_duration"`
	RPMLimit       *int64         `json:"rpm_limit"`
	TPMLimit       *int64         `json:"tpm_limit"`
	Metadata       map[string]any `json:"metadata"`
	Status         string         `json:"status"`
}

// GenerateKey registers a key. body carries the raw value under "key".
func (c *Client) GenerateKey(ctx context.Context, body map[string]any) error {
	_, err := c.makeRequest(ctx, "POST", "/key/generate", body)
	return err
}

// GetKeyInfo returns ErrNotFound for an unknown key and for a deleted one,
// which LiteLLM can serve from its deleted-keys table with status "deleted".
func (c *Client) GetKeyInfo(ctx context.Context, hash string) (*KeyInfo, error) {
	raw, err := c.makeRequest(ctx, "GET", "/key/info?key="+url.QueryEscape(hash), nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		Info *KeyInfo `json:"info"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("litellm: decode GET /key/info: %w", err)
	}
	if out.Info == nil || out.Info.Status == "deleted" {
		return nil, ErrNotFound
	}
	return out.Info, nil
}

// UpdateKey sets fields on a key. A nil value is sent as JSON null and clears
// the field.
func (c *Client) UpdateKey(ctx context.Context, hash string, fields map[string]any) error {
	body := maps.Clone(fields)
	body["key"] = hash
	_, err := c.makeRequest(ctx, "POST", "/key/update", body)
	return err
}

// DeleteKeys deletes keys by hash. LiteLLM answers 404 when none exists.
func (c *Client) DeleteKeys(ctx context.Context, hashes []string) error {
	_, err := c.makeRequest(ctx, "POST", "/key/delete", map[string]any{"keys": hashes})
	return err
}

// ListKeyHashesByAlias returns the hashes of the keys holding alias. LiteLLM
// keeps aliases unique, so this is zero or one entry.
func (c *Client) ListKeyHashesByAlias(ctx context.Context, alias string) ([]string, error) {
	raw, err := c.makeRequest(ctx, "GET", "/key/list?key_alias="+url.QueryEscape(alias)+"&size=100", nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		Keys []string `json:"keys"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("litellm: decode GET /key/list: %w", err)
	}
	return out.Keys, nil
}
