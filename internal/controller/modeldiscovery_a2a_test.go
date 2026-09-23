// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
)

// TestModelDiscovery_A2A_PublishesRegisteredAgents: a registered agent
// becomes <prefix>.<name> with params.model a2a1/<name>; an excluded one does
// not; creating a new agent after the Discovery adds its model without a
// refresh tick.
func TestModelDiscovery_A2A_PublishesRegisteredAgents(t *testing.T) {
	ctx := context.Background()
	const mdName = "a2a-disc"
	agents := []string{"a2a-disc-one", "a2a-disc-skip", "a2a-disc-late"}
	resetMockA2A()
	ensureNoModelDiscovery(t, ctx, mdName)
	for _, n := range agents {
		ensureNoA2AAgent(t, ctx, n)
	}
	cleanupConn := setupReadyConnectionA2A(t, ctx)
	t.Cleanup(func() {
		ensureNoModelDiscovery(t, context.Background(), mdName)
		for _, n := range agents {
			ensureNoA2AAgent(t, context.Background(), n)
		}
		cleanupConn()
	})

	for _, n := range agents[:2] {
		if err := k8sClient.Create(ctx, a2aSampleCR(n)); err != nil {
			t.Fatalf("create agent %s: %v", n, err)
		}
	}
	md := &litellmv1alpha1.LiteLLMModelDiscovery{
		ObjectMeta: metav1.ObjectMeta{Name: mdName, Namespace: WatchNamespace},
		Spec: litellmv1alpha1.ModelDiscoverySpec{
			Type:    "a2a",
			Prefix:  "agent",
			Filters: &litellmv1alpha1.ModelDiscoveryFilters{Exclude: []string{".*-skip"}},
			Refresh: litellmv1alpha1.ModelDiscoveryRefresh{Interval: metav1.Duration{Duration: time.Hour}},
		},
	}
	if err := k8sClient.Create(ctx, md); err != nil {
		t.Fatalf("create discovery: %v", err)
	}

	child := waitForA2AModel(t, ctx, "agent.a2a-disc-one")
	var params map[string]any
	if err := json.Unmarshal(child.Spec.Params.Raw, &params); err != nil {
		t.Fatal(err)
	}
	if params["model"] != "a2a1/a2a-disc-one" {
		t.Errorf("params.model = %v, want a2a1/a2a-disc-one", params["model"])
	}
	var skipped litellmv1alpha1.LiteLLMModel
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: "agent.a2a-disc-skip", Namespace: WatchNamespace}, &skipped); err == nil {
		t.Error("agent.a2a-disc-skip exists; the exclude filter should have dropped it")
	}

	// Refresh is 1h: the late agent must arrive through the A2AAgent watch.
	if err := k8sClient.Create(ctx, a2aSampleCR(agents[2])); err != nil {
		t.Fatal(err)
	}
	waitForA2AModel(t, ctx, "agent.a2a-disc-late")
}

// TestModelDiscovery_A2A_CELForbidsUpstreamFields: a2a has no upstream, so
// baseUrl is rejected at admission.
func TestModelDiscovery_A2A_CELForbidsUpstreamFields(t *testing.T) {
	md := &litellmv1alpha1.LiteLLMModelDiscovery{
		ObjectMeta: metav1.ObjectMeta{Name: "a2a-cel", Namespace: WatchNamespace},
		Spec: litellmv1alpha1.ModelDiscoverySpec{
			Type:    "a2a",
			BaseURL: "https://example.com",
			Refresh: litellmv1alpha1.ModelDiscoveryRefresh{Interval: metav1.Duration{Duration: time.Hour}},
		},
	}
	err := k8sClient.Create(context.Background(), md)
	if err == nil {
		_ = k8sClient.Delete(context.Background(), md)
		t.Fatal("create succeeded; want CEL rejection")
	}
	if !strings.Contains(err.Error(), "a2a forbids") {
		t.Fatalf("err = %v, want a2a forbids", err)
	}
}

func waitForA2AModel(t *testing.T, ctx context.Context, name string) *litellmv1alpha1.LiteLLMModel {
	t.Helper()
	var m litellmv1alpha1.LiteLLMModel
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if k8sClient.Get(ctx, client.ObjectKey{Name: name, Namespace: WatchNamespace}, &m) == nil {
			return &m
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("model %s not generated within 30s", name)
	return nil
}
