// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"context"
	"reflect"
	"sort"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
)

func a2aAgent(ns, name, agentID string, deleting bool) *litellmv1alpha1.LiteLLMA2AAgent {
	a := &litellmv1alpha1.LiteLLMA2AAgent{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
	a.Status.LastRendered.AgentID = agentID
	if deleting {
		now := metav1.Now()
		a.DeletionTimestamp = &now
		a.Finalizers = []string{"test/hold"} // fake client refuses a deletionTimestamp without finalizers
	}
	return a
}

func TestA2AProvider_ListsRegisteredAgentsInNamespace(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := litellmv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		a2aAgent("ns", "finops", "id-1", false),
		a2aAgent("ns", "triage", "id-2", false),
		a2aAgent("ns", "pending", "", false),    // not registered yet
		a2aAgent("ns", "leaving", "id-3", true), // being deleted
		a2aAgent("other", "elsewhere", "id-4", false),
	).Build()

	p, err := newA2A(context.Background(), ProviderConfig{Type: "a2a", Reader: c, Namespace: "ns"})
	if err != nil {
		t.Fatal(err)
	}
	cands, err := p.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(cands))
	for _, cd := range cands {
		ids = append(ids, cd.ID)
	}
	sort.Strings(ids)
	if want := []string{"finops", "triage"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
}

func TestA2AProvider_RequiresReaderAndNamespace(t *testing.T) {
	if _, err := newA2A(context.Background(), ProviderConfig{Type: "a2a", Namespace: "ns"}); err == nil {
		t.Error("nil Reader must be rejected")
	}
	var r client.Reader = fake.NewClientBuilder().Build()
	if _, err := newA2A(context.Background(), ProviderConfig{Type: "a2a", Reader: r}); err == nil {
		t.Error("empty Namespace must be rejected")
	}
}
