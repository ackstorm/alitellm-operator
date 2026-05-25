// SPDX-License-Identifier: Apache-2.0

package litellm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/go-logr/logr"
)

func TestGetRouterSettings_ParsesCallbacksEndpoint(t *testing.T) {
	body := `{
	  "status": "success",
	  "router_settings": {
	    "routing_strategy": "simple-shuffle",
	    "model_group_alias": {"ackstorm.smart": "gemini.gemini-3-pro-preview"}
	  }
	}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/get/config/callbacks" || r.Method != http.MethodGet {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "sk-test", logr.Discard())
	rs, err := c.GetRouterSettings(context.Background())
	if err != nil {
		t.Fatalf("GetRouterSettings: %v", err)
	}
	if rs.ModelGroupAlias["ackstorm.smart"] != "gemini.gemini-3-pro-preview" {
		t.Fatalf("alias map not parsed: %#v", rs.ModelGroupAlias)
	}
	if rs.Extra["routing_strategy"] != "simple-shuffle" {
		t.Fatalf("extra not preserved: %#v", rs.Extra)
	}
}

func TestUpdateRouterSettings_SendsMergedBody(t *testing.T) {
	want := map[string]any{
		"router_settings": map[string]any{
			"routing_strategy":  "simple-shuffle",
			"model_group_alias": map[string]any{"ackstorm.smart": "gemini.gemini-3-pro-preview"},
		},
	}
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/config/update" || r.Method != http.MethodPost {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		_, _ = w.Write([]byte(`{"status":"success"}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "sk-test", logr.Discard())
	rs := &RouterSettings{
		Extra:           map[string]any{"routing_strategy": "simple-shuffle"},
		ModelGroupAlias: map[string]string{"ackstorm.smart": "gemini.gemini-3-pro-preview"},
	}
	if err := c.UpdateRouterSettings(context.Background(), rs); err != nil {
		t.Fatalf("UpdateRouterSettings: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("body mismatch\nwant=%v\ngot=%v", want, got)
	}
}

func TestUpdateRouterSettings_NilReturnsError(t *testing.T) {
	c := NewClient("http://unused", "sk-test", logr.Discard())
	if err := c.UpdateRouterSettings(context.Background(), nil); err == nil {
		t.Fatalf("expected error on nil RouterSettings")
	}
}
