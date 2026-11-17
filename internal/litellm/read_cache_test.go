// SPDX-License-Identifier: MIT

package litellm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
)

// TestReadCache_SharedListAndDropOnWrite — a relist burst's per-model id
// lookups share ONE GET /model/info, and the operator's own write drops
// the cache so the next lookup is live.
func TestReadCache_SharedListAndDropOnWrite(t *testing.T) {
	var gets atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			gets.Add(1)
			_, _ = w.Write([]byte(`{"data":[
				{"model_name":"a","model_info":{"id":"a1"}},
				{"model_name":"b","model_info":{"id":"b1"}},
				{"model_name":"a","model_info":{"id":"a2"}}
			]}`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "sk-test", logr.Discard(), WithReadCacheTTL(time.Minute))
	ctx := context.Background()

	for name, want := range map[string][]string{"a": {"a1", "a2"}, "b": {"b1"}, "missing": nil} {
		got, err := c.GetModelIDsByName(ctx, name)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: got %v, %v; want %v", name, got, err, want)
		}
	}
	if n := gets.Load(); n != 1 {
		t.Fatalf("GETs before write: got %d want 1", n)
	}

	if err := c.DeleteModel(ctx, "a2"); err != nil {
		t.Fatalf("DeleteModel: %v", err)
	}
	if _, err := c.GetModelIDsByName(ctx, "a"); err != nil {
		t.Fatalf("GetModelIDsByName after write: %v", err)
	}
	if n := gets.Load(); n != 2 {
		t.Fatalf("GETs after write: got %d want 2 (write must drop the cache)", n)
	}
}

// TestReadCache_ErrorsNotCached — a failed read is retried upstream, not
// served from the cache.
func TestReadCache_ErrorsNotCached(t *testing.T) {
	var gets atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if gets.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "sk-test", logr.Discard(), WithReadCacheTTL(time.Minute))

	if _, err := c.GetModelIDsByName(context.Background(), "a"); err == nil {
		t.Fatal("first call: want 5xx error")
	}
	if _, err := c.GetModelIDsByName(context.Background(), "a"); err != nil {
		t.Fatalf("second call: want fresh success, got %v", err)
	}
}
