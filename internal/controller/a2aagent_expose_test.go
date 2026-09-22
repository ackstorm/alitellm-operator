// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
)

// ensureNoExposedModel deletes a LiteLLMModel and waits for it to be GONE,
// finalizer drained.
//
// Waiting is the point, not politeness. Every model test in this package counts
// `POST /model/new` against the shared mock by method+path only, with no
// model-name filter — so a child of ours still reconciling (or still draining)
// when the next test starts inflates that test's count and fails it. Deleting
// without waiting leaves exactly that race.
func ensureNoExposedModel(t *testing.T, ctx context.Context, name string) {
	t.Helper()
	var m litellmv1alpha1.LiteLLMModel
	err := k8sClient.Get(ctx, types.NamespacedName{Namespace: WatchNamespace, Name: name}, &m)
	if err == nil {
		_ = k8sClient.Delete(ctx, &m)
	} else if !apierrors.IsNotFound(err) {
		t.Logf("ensureNoExposedModel(%s): get failed: %v", name, err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var probe litellmv1alpha1.LiteLLMModel
		if err := k8sClient.Get(ctx,
			types.NamespacedName{Namespace: WatchNamespace, Name: name}, &probe); apierrors.IsNotFound(err) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Logf("ensureNoExposedModel(%s): still present after 30s; later tests may see its mutations", name)
}

// pollExposedModel waits for the generated child to exist and returns it.
func pollExposedModel(t *testing.T, ctx context.Context, name string, timeout time.Duration) *litellmv1alpha1.LiteLLMModel {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		var m litellmv1alpha1.LiteLLMModel
		err := k8sClient.Get(ctx, types.NamespacedName{Namespace: WatchNamespace, Name: name}, &m)
		if err == nil {
			return &m
		}
		last = err
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("generated model %q not found within %s (last error: %v)", name, timeout, last)
	return nil
}

// pollExposedModelGone waits for the generated child to disappear.
func pollExposedModelGone(t *testing.T, ctx context.Context, name string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var m litellmv1alpha1.LiteLLMModel
		err := k8sClient.Get(ctx, types.NamespacedName{Namespace: WatchNamespace, Name: name}, &m)
		if apierrors.IsNotFound(err) {
			return
		}
		// A child with a finalizer would linger with a deletionTimestamp;
		// treat that as pruned too, since the intent is observable.
		if err == nil && m.DeletionTimestamp != nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("generated model %q still present after %s; want pruned", name, timeout)
}

// TestA2AAgent_ExposeAsModel_ProjectsChild covers the happy path: a CR with
// spec.exposeAsModel gets a generated LiteLLMModel whose litellm_params.model
// names the agent, carrying the declared access group, the agent card's
// description, and an owner reference for cascade deletion.
func TestA2AAgent_ExposeAsModel_ProjectsChild(t *testing.T) {
	ctx := context.Background()
	resetMockA2A()
	ensureNoA2AAgent(t, ctx, "expose-projects")
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionA2A(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoA2AAgent(t, context.Background(), "expose-projects")
		ensureNoExposedModel(t, context.Background(), "agent.expose-projects")
	})

	cr := &litellmv1alpha1.LiteLLMA2AAgent{
		ObjectMeta: metav1.ObjectMeta{Name: "expose-projects", Namespace: WatchNamespace},
		Spec: litellmv1alpha1.A2AAgentSpec{
			Endpoint:  "https://agent.example.com/a2a",
			AgentCard: runtime.RawExtension{Raw: []byte(`{"name":"Sample","description":"triages things"}`)},
			ExposeAsModel: &litellmv1alpha1.ExposeAsModel{
				AccessGroups: []string{"a2a"},
			},
		},
	}
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create A2AAgent: %v", err)
	}

	child := pollExposedModel(t, ctx, "agent.expose-projects", 30*time.Second)

	var params map[string]any
	if err := json.Unmarshal(child.Spec.Params.Raw, &params); err != nil {
		t.Fatalf("decode child params: %v", err)
	}
	if got, want := params["model"], "a2a1/expose-projects"; got != want {
		t.Errorf("child litellm_params.model: want %q, got %v", want, got)
	}

	var info map[string]any
	if err := json.Unmarshal(child.Spec.Info.Raw, &info); err != nil {
		t.Fatalf("decode child info: %v", err)
	}
	if got, want := info["description"], "triages things"; got != want {
		t.Errorf("child description: want %q, got %v", want, got)
	}
	groups, _ := info["access_groups"].([]any)
	if len(groups) != 1 || groups[0] != "a2a" {
		t.Errorf("child access_groups: want [a2a], got %v", info["access_groups"])
	}

	if child.Labels[generatedByAgentLabel] != "expose-projects" {
		t.Errorf("child %s label: want %q, got %q",
			generatedByAgentLabel, "expose-projects", child.Labels[generatedByAgentLabel])
	}

	// Owner reference must be a CONTROLLER ref with blockOwnerDeletion, or
	// deleting the agent leaves the model orphaned in LiteLLM.
	var owned bool
	for _, ref := range child.OwnerReferences {
		if ref.Kind == a2aAgentKind && ref.Name == "expose-projects" &&
			ref.Controller != nil && *ref.Controller &&
			ref.BlockOwnerDeletion != nil && *ref.BlockOwnerDeletion {
			owned = true
		}
	}
	if !owned {
		t.Errorf("child owner references: want controller ref to the agent, got %+v", child.OwnerReferences)
	}
}

// TestA2AAgent_ExposeAsModel_AbsentBlockCreatesNothing guards the default:
// an agent without the block must not grow a child. Without this, enabling
// projection by accident would silently publish every agent as a model.
func TestA2AAgent_ExposeAsModel_AbsentBlockCreatesNothing(t *testing.T) {
	ctx := context.Background()
	resetMockA2A()
	ensureNoA2AAgent(t, ctx, "expose-absent")
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionA2A(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoA2AAgent(t, context.Background(), "expose-absent")
	})

	cr := &litellmv1alpha1.LiteLLMA2AAgent{
		ObjectMeta: metav1.ObjectMeta{Name: "expose-absent", Namespace: WatchNamespace},
		Spec: litellmv1alpha1.A2AAgentSpec{
			Endpoint:  "https://agent.example.com/a2a",
			AgentCard: runtime.RawExtension{Raw: []byte(`{"name":"Sample"}`)},
		},
	}
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create A2AAgent: %v", err)
	}

	// Let the agent reach steady state, so "no child" means "never created"
	// rather than "not created yet".
	pollA2AAgentCondition(t, ctx, "expose-absent", reasonSynced, 30*time.Second)

	var m litellmv1alpha1.LiteLLMModel
	err := k8sClient.Get(ctx, types.NamespacedName{Namespace: WatchNamespace, Name: "agent.expose-absent"}, &m)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("want NotFound for agent.expose-absent, got err=%v model=%+v", err, m)
	}
}

// TestA2AAgent_ExposeAsModel_RemovalPrunesChild covers the toggle-off path.
// Owner references only cascade on agent DELETION; removing the block while
// the agent lives has to prune explicitly, or the model lingers and keeps
// advertising a route the user just withdrew.
func TestA2AAgent_ExposeAsModel_RemovalPrunesChild(t *testing.T) {
	ctx := context.Background()
	resetMockA2A()
	ensureNoA2AAgent(t, ctx, "expose-prune")
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionA2A(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoA2AAgent(t, context.Background(), "expose-prune")
		ensureNoExposedModel(t, context.Background(), "agent.expose-prune")
	})

	cr := &litellmv1alpha1.LiteLLMA2AAgent{
		ObjectMeta: metav1.ObjectMeta{Name: "expose-prune", Namespace: WatchNamespace},
		Spec: litellmv1alpha1.A2AAgentSpec{
			Endpoint:      "https://agent.example.com/a2a",
			AgentCard:     runtime.RawExtension{Raw: []byte(`{"name":"Sample"}`)},
			ExposeAsModel: &litellmv1alpha1.ExposeAsModel{AccessGroups: []string{"a2a"}},
		},
	}
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create A2AAgent: %v", err)
	}
	pollExposedModel(t, ctx, "agent.expose-prune", 30*time.Second)

	// Drop the block; the child must be pruned.
	var live litellmv1alpha1.LiteLLMA2AAgent
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: WatchNamespace, Name: "expose-prune"}, &live); err != nil {
		t.Fatalf("get A2AAgent: %v", err)
	}
	live.Spec.ExposeAsModel = nil
	if err := k8sClient.Update(ctx, &live); err != nil {
		t.Fatalf("update A2AAgent: %v", err)
	}

	pollExposedModelGone(t, ctx, "agent.expose-prune", 30*time.Second)
}

// TestA2AAgent_ExposeAsModel_LeavesForeignModelAlone is the safety case for
// prune: a hand-written LiteLLMModel that happens to occupy the generated
// name must survive, because the user owns it. Name collision alone must
// never authorize a delete.
func TestA2AAgent_ExposeAsModel_LeavesForeignModelAlone(t *testing.T) {
	ctx := context.Background()
	resetMockA2A()
	ensureNoA2AAgent(t, ctx, "expose-foreign")
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionA2A(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoA2AAgent(t, context.Background(), "expose-foreign")
		ensureNoExposedModel(t, context.Background(), "agent.expose-foreign")
	})

	// A user-owned model sitting on the name the agent would generate.
	foreign := &litellmv1alpha1.LiteLLMModel{
		ObjectMeta: metav1.ObjectMeta{Name: "agent.expose-foreign", Namespace: WatchNamespace},
		Spec: litellmv1alpha1.ModelSpec{
			Params: runtime.RawExtension{Raw: []byte(`{"model":"openai/gpt-4o-mini"}`)},
			Info:   runtime.RawExtension{Raw: []byte(`{}`)},
		},
	}
	if err := k8sClient.Create(ctx, foreign); err != nil {
		t.Fatalf("create foreign model: %v", err)
	}

	// Agent WITHOUT the block — reconcile takes the prune path over a name
	// that is occupied by someone else's object.
	cr := &litellmv1alpha1.LiteLLMA2AAgent{
		ObjectMeta: metav1.ObjectMeta{Name: "expose-foreign", Namespace: WatchNamespace},
		Spec: litellmv1alpha1.A2AAgentSpec{
			Endpoint:  "https://agent.example.com/a2a",
			AgentCard: runtime.RawExtension{Raw: []byte(`{"name":"Sample"}`)},
		},
	}
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create A2AAgent: %v", err)
	}
	pollA2AAgentCondition(t, ctx, "expose-foreign", reasonSynced, 30*time.Second)

	var still litellmv1alpha1.LiteLLMModel
	if err := k8sClient.Get(ctx,
		types.NamespacedName{Namespace: WatchNamespace, Name: "agent.expose-foreign"}, &still); err != nil {
		t.Fatalf("foreign model was deleted by the prune path: %v", err)
	}
	if still.DeletionTimestamp != nil {
		t.Error("foreign model is terminating; the prune path must not touch a model it did not generate")
	}
}
