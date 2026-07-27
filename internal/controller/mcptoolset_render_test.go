// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
	"github.com/ackstorm/alitellm-operator/internal/litellm"
)

func TestRenderToolsetTools_FlattensInDeclarationOrder(t *testing.T) {
	from := []litellmv1alpha1.MCPToolsetServerTools{
		{Server: "hindsight", Tools: []string{"web_search", "fetch_page"}},
		{Server: "confluence", Tools: []string{"search_pages"}},
	}
	got := renderToolsetTools(from, func(name string) string { return name })
	want := []litellm.MCPToolsetTool{
		{ServerID: "hindsight", ToolName: "web_search"},
		{ServerID: "hindsight", ToolName: "fetch_page"},
		{ServerID: "confluence", ToolName: "search_pages"},
	}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("tools[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

// The resolver translates a CR name to its LiteLLM server_id. A name with no
// matching CR must pass through VERBATIM — never dropped, never an error.
func TestRenderToolsetTools_UsesResolverAndFallsBackVerbatim(t *testing.T) {
	resolve := func(name string) string {
		if name == "hindsight" {
			return "hindsight-uuid-1234"
		}
		return name // unresolvable → verbatim
	}
	from := []litellmv1alpha1.MCPToolsetServerTools{
		{Server: "hindsight", Tools: []string{"web_search"}},
		{Server: "6d071d99-39d2-44f9-8182-8917827b7c45", Tools: []string{"raw_uuid_tool"}},
	}
	got := renderToolsetTools(from, resolve)
	if got[0].ServerID != "hindsight-uuid-1234" {
		t.Errorf("resolved server = %q, want hindsight-uuid-1234", got[0].ServerID)
	}
	if got[1].ServerID != "6d071d99-39d2-44f9-8182-8917827b7c45" {
		t.Errorf("unresolvable server = %q, want the verbatim string "+
			"(a raw UUID must survive untouched — no sanitization)", got[1].ServerID)
	}
}

// Duplicate pairs would otherwise make the rendered hash order-dependent on
// user sloppiness and produce redundant LiteLLM entries.
func TestRenderToolsetTools_DedupesPairsFirstWins(t *testing.T) {
	from := []litellmv1alpha1.MCPToolsetServerTools{
		{Server: "a", Tools: []string{"t1", "t2", "t1"}},
		{Server: "a", Tools: []string{"t2"}},
	}
	got := renderToolsetTools(from, func(n string) string { return n })
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2 (deduped), got %v", len(got), got)
	}
	if got[0].ToolName != "t1" || got[1].ToolName != "t2" {
		t.Errorf("order/content = %v, want [a/t1 a/t2] (first occurrence wins)", got)
	}
}

// ALWAYS-EMIT: never nil, so the JSON body carries `tools: []`.
func TestRenderToolsetTools_NeverNil(t *testing.T) {
	if got := renderToolsetTools(nil, func(n string) string { return n }); got == nil {
		t.Fatal("renderToolsetTools(nil) must return a non-nil empty slice, not nil")
	}
	empty := []litellmv1alpha1.MCPToolsetServerTools{{Server: "a", Tools: nil}}
	if got := renderToolsetTools(empty, func(n string) string { return n }); got == nil || len(got) != 0 {
		t.Errorf("server with no tools contributes no pairs and must not be nil, got %v", got)
	}
}

// An empty tool name is skipped rather than sent — LiteLLM would accept it
// and it can only ever be inert.
func TestRenderToolsetTools_SkipsEmptyToolNames(t *testing.T) {
	from := []litellmv1alpha1.MCPToolsetServerTools{
		{Server: "a", Tools: []string{"", "t1"}},
	}
	got := renderToolsetTools(from, func(n string) string { return n })
	if len(got) != 1 || got[0].ToolName != "t1" {
		t.Errorf("got %v, want only [a/t1]", got)
	}
}

func TestServerIDResolver(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := litellmv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	registered := &litellmv1alpha1.LiteLLMMCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "hindsight", Namespace: "default"},
	}
	registered.Status.LastRendered.ServerID = "hindsight-server-id"

	// A CR that exists but has not been reconciled yet: empty ServerID.
	unsynced := &litellmv1alpha1.LiteLLMMCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "pending", Namespace: "default"},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(registered, unsynced).Build()

	resolve := serverIDResolver(context.Background(), c, "default")

	if got := resolve("hindsight"); got != "hindsight-server-id" {
		t.Errorf("resolve(hindsight) = %q, want hindsight-server-id", got)
	}
	// No CR → verbatim. This is the adopted-server / raw-UUID path.
	if got := resolve("6d071d99-39d2-44f9-8182-8917827b7c45"); got != "6d071d99-39d2-44f9-8182-8917827b7c45" {
		t.Errorf("resolve(uuid) = %q, want the verbatim uuid", got)
	}
	// CR exists but unsynced (empty ServerID) → verbatim, NOT empty string.
	// Sending "" would produce a pair LiteLLM stores with a blank server.
	if got := resolve("pending"); got != "pending" {
		t.Errorf("resolve(pending) = %q, want verbatim %q — an unsynced CR must "+
			"never resolve to the empty string", got, "pending")
	}
}
