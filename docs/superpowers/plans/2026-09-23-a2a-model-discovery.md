# A2A Model Discovery Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans (inline) to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `LiteLLMModelDiscovery` with `spec.type: a2a` publishes every registered `LiteLLMA2AAgent` in its namespace as a callable model (`<prefix>.<agent>` → `a2a1/<agent>`), filtered by include/exclude, so agents become models without writing `exposeAsModel` on each one.

**Architecture:** A new provider `a2a` in `internal/providers` whose `List` reads `LiteLLMA2AAgent` CRs (not HTTP, not LiteLLM) and returns one `Candidate` per registered, non-deleting agent (`ID = metadata.name`). The Discovery reconciler passes it a `client.Reader` + namespace, derives the child's `litellm_params.model` provider from `A2A_MODEL_PROVIDER` (same as `exposeAsModel`), and gains a watch on `LiteLLMA2AAgent` so a new agent becomes a model without waiting for `refresh.interval`. Everything else (filters, prefix, info propagation, prune, status) is the existing Discovery pipeline unchanged.

**Tech Stack:** Go, controller-runtime, kubebuilder CEL, envtest.

## Global Constraints

- Source is `LiteLLMA2AAgent` CRs in the Discovery's namespace. Only agents with `status.lastRendered.agentID != ""` and no `deletionTimestamp` are candidates (the `a2a1` handler resolves by registered `agent_name`; an unregistered agent would be a model that 404s).
- `Candidate.ID` = agent `metadata.name` (== LiteLLM `agent_name`). Child `params.model` = `<A2A_MODEL_PROVIDER, default a2a1>/<metadata.name>`.
- CEL: `type: a2a` forbids `credentialsSecretRef`, `region`, `baseUrl`; `litellmProvider` stays openai-only.
- Default prefix is `a2a` (lowercased type, existing rule). Gitops sets `prefix: agent` to keep the current `agent.<name>` names.
- `exposeAsModel` keeps working; it is documented as superseded, not removed.
- Out of scope: per-agent `model_info.description` from the agent card (Discovery `info` is CR-level). Add only if asked.

---

### Task 1: `a2a` provider + reconciler wiring + CRD

**Files:**
- Create: `internal/providers/a2a.go`, `internal/providers/a2a_test.go`
- Modify: `internal/providers/interface.go` (`ProviderConfig`: `Reader`, `Namespace`; `Type()` doc enum)
- Modify: `internal/providers/registry.go` (row `"a2a": newA2A`)
- Modify: `api/litellm/v1alpha1/modeldiscovery_types.go` (enum + CEL rule + doc)
- Modify: `internal/controller/modeldiscovery_controller.go` (const, Step 4 case, provider derivation, watch + mapper, RBAC marker)
- Test: `internal/controller/modeldiscovery_a2a_test.go`

**Interfaces:**
- Produces: `providers.ProviderConfig.Reader client.Reader`, `providers.ProviderConfig.Namespace string`; `func newA2A(ctx context.Context, cfg ProviderConfig) (Provider, error)`; `func (r *ModelDiscoveryReconciler) agentToA2ADiscoveries(ctx context.Context, obj client.Object) []reconcile.Request`.

- [ ] **Step 1: Failing provider unit test** — `internal/providers/a2a_test.go`:

```go
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
	objs := []client.Object{
		a2aAgent("ns", "finops", "id-1", false),
		a2aAgent("ns", "triage", "id-2", false),
		a2aAgent("ns", "pending", "", false),   // not registered yet
		a2aAgent("ns", "leaving", "id-3", true), // being deleted
		a2aAgent("other", "elsewhere", "id-4", false),
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).WithStatusSubresource(&litellmv1alpha1.LiteLLMA2AAgent{}).Build()
	// WithObjects drops status on objects with a status subresource; set it explicitly.
	for _, o := range objs {
		a := o.(*litellmv1alpha1.LiteLLMA2AAgent)
		id := a.Status.LastRendered.AgentID
		var got litellmv1alpha1.LiteLLMA2AAgent
		if err := c.Get(context.Background(), client.ObjectKeyFromObject(a), &got); err != nil {
			t.Fatal(err)
		}
		got.Status.LastRendered.AgentID = id
		if err := c.Status().Update(context.Background(), &got); err != nil {
			t.Fatal(err)
		}
	}

	p, err := newA2A(context.Background(), ProviderConfig{Type: "a2a", Reader: c, Namespace: "ns"})
	if err != nil {
		t.Fatal(err)
	}
	cands, err := p.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
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
	if _, err := newA2A(context.Background(), ProviderConfig{Type: "a2a", Reader: fake.NewClientBuilder().Build()}); err == nil {
		t.Error("empty Namespace must be rejected")
	}
}
```

Run: `make test-unit-pkg PKG=./internal/providers/... ; echo "EXIT=$?"` → compile failure (`newA2A`, `Reader` undefined).

- [ ] **Step 2: Implement the provider**

`internal/providers/interface.go`, `ProviderConfig` — append:
```go
	// Reader and Namespace are used only by the a2a provider, which lists
	// LiteLLMA2AAgent CRs instead of calling an upstream. The reconciler sets
	// them for spec.type=a2a; every HTTP provider ignores them.
	Reader    client.Reader
	Namespace string
```
(import `sigs.k8s.io/controller-runtime/pkg/client`; add `"a2a"` to the `Type()` doc enum.)

`internal/providers/a2a.go`:
```go
// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"context"
	"errors"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
)

// a2aProvider "discovers" the A2A agents this operator has registered: one
// candidate per LiteLLMA2AAgent in the namespace. It reads the cluster, not
// LiteLLM (Discovery never calls LiteLLM, MDISC-27) and not the agents
// themselves. Only REGISTERED agents qualify — the a2a1 handler resolves a
// model to a registered agent_name, so an unregistered one would be a model
// that 404s — and agents being deleted drop out so their model is pruned.
type a2aProvider struct {
	reader    client.Reader
	namespace string
}

func newA2A(_ context.Context, cfg ProviderConfig) (Provider, error) {
	if cfg.Reader == nil {
		return nil, errors.New("a2a: nil Reader")
	}
	if cfg.Namespace == "" {
		return nil, errors.New("a2a: empty Namespace")
	}
	return &a2aProvider{reader: cfg.Reader, namespace: cfg.Namespace}, nil
}

func (p *a2aProvider) Type() string { return "a2a" }

func (p *a2aProvider) List(ctx context.Context) ([]Candidate, error) {
	var agents litellmv1alpha1.LiteLLMA2AAgentList
	if err := p.reader.List(ctx, &agents, client.InNamespace(p.namespace)); err != nil {
		return nil, fmt.Errorf("a2a: list LiteLLMA2AAgent: %w", err)
	}
	out := make([]Candidate, 0, len(agents.Items))
	for i := range agents.Items {
		a := &agents.Items[i]
		if a.Status.LastRendered.AgentID == "" || !a.DeletionTimestamp.IsZero() {
			continue
		}
		out = append(out, Candidate{ID: a.Name, DisplayName: a.Name})
	}
	return out, nil
}
```
`internal/providers/registry.go`: add `"a2a": newA2A,` (alphabetical, first row).

Run: `make test-unit-pkg PKG=./internal/providers/... ; echo "EXIT=$?"` → PASS.

- [ ] **Step 3: CRD — enum + CEL**

`api/litellm/v1alpha1/modeldiscovery_types.go`:
- `+kubebuilder:validation:Enum=a2a;anthropic;bedrock;elevenlabs;gemini;kubeai;openai`
- Add next to the other per-type rules:
```go
// +kubebuilder:validation:XValidation:rule="self.spec.type != 'a2a' || (!has(self.spec.credentialsSecretRef) && !has(self.spec.region) && !has(self.spec.baseUrl))",message="a2a forbids spec.credentialsSecretRef/spec.region/spec.baseUrl"
```
- Extend the `Type` doc and the "Provider field matrix" comment with the `a2a` row: source = `LiteLLMA2AAgent` CRs in the namespace (registered only), no credentials, child `params.model = <A2A_MODEL_PROVIDER>/<agent>`, default prefix `a2a`.
- Update the "seven CR-level XValidation rules" count comment to eight.

Run: `make gen-manifests gen-code; echo "EXIT=$?"` → `EXIT=0`.

- [ ] **Step 4: Reconciler wiring**

`internal/controller/modeldiscovery_controller.go`:
- Constant next to `providerTypeKubeAI`: `providerTypeA2A = "a2a"`.
- Step 4 credential switch, new case:
```go
	case providerTypeA2A:
		// No credentials: the provider lists LiteLLMA2AAgent CRs (CEL forbids
		// credentialsSecretRef/region/baseUrl for this type).
		cfg.Reader = r.Client
		cfg.Namespace = md.Namespace
```
- Provider derivation (after the kubeai → hosted_vllm block, before the `LitellmProvider` override):
```go
	if litellmProvider == providerTypeA2A {
		// Same provider exposeAsModel stamps, so a discovered agent and an
		// exposeAsModel child route identically (a2aagent_expose.go).
		litellmProvider = a2aModelProvider()
	}
```
- RBAC marker: `// +kubebuilder:rbac:groups=litellm.ackstorm.ai,resources=litellma2aagents,verbs=get;list;watch`
- Watch + mapper so a newly registered (or removed) agent re-drives the Discovery immediately:
```go
// agentToA2ADiscoveries enqueues every a2a-type Discovery in the agent's
// namespace. Any agent change can add or drop a candidate (registration sets
// status.lastRendered.agentID; deletion sets deletionTimestamp), so all of
// them, not just ones whose filters match.
func (r *ModelDiscoveryReconciler) agentToA2ADiscoveries(ctx context.Context, obj client.Object) []reconcile.Request {
	var mds litellmv1alpha1.LiteLLMModelDiscoveryList
	if err := r.List(ctx, &mds, client.InNamespace(obj.GetNamespace())); err != nil {
		log.FromContext(ctx).Error(err, "list model discoveries for a2a agent fan-in")
		return nil
	}
	var reqs []reconcile.Request
	for i := range mds.Items {
		if mds.Items[i].Spec.Type == providerTypeA2A {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&mds.Items[i])})
		}
	}
	return reqs
}
```
In `SetupWithManager`, after the Secret watch:
```go
		Watches(
			&litellmv1alpha1.LiteLLMA2AAgent{},
			handler.EnqueueRequestsFromMapFunc(r.agentToA2ADiscoveries),
		).
```
and add the line to the `Watches:` doc comment. Check the reconciler does not short-circuit an enqueue that arrives before `refresh.interval` elapsed (`grep -n "LastRefreshAt\|RequeueAfter" internal/controller/modeldiscovery_controller.go`). If it skips the list when the interval has not elapsed, bypass that gate for `providerTypeA2A` (listing CRs is free) and say so in a comment.

- [ ] **Step 5: Envtest** — `internal/controller/modeldiscovery_a2a_test.go`:
```go
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
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
	resetMockA2A()
	ensureNoModelDiscovery(t, ctx, mdName)
	for _, n := range []string{"a2a-disc-one", "a2a-disc-skip", "a2a-disc-late"} {
		ensureNoA2AAgent(t, ctx, n)
	}
	cleanupConn := setupReadyConnectionA2A(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoModelDiscovery(t, context.Background(), mdName)
		for _, n := range []string{"a2a-disc-one", "a2a-disc-skip", "a2a-disc-late"} {
			ensureNoA2AAgent(t, context.Background(), n)
		}
	})

	for _, n := range []string{"a2a-disc-one", "a2a-disc-skip"} {
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

	child := waitForModel(t, ctx, "agent.a2a-disc-one")
	var params map[string]any
	if err := json.Unmarshal(child.Spec.Params.Raw, &params); err != nil {
		t.Fatal(err)
	}
	if params["model"] != "a2a1/a2a-disc-one" {
		t.Errorf("params.model = %v, want a2a1/a2a-disc-one", params["model"])
	}
	assertNoModel(t, ctx, "agent.a2a-disc-skip")

	// Refresh is 1h: the late agent must arrive through the A2AAgent watch.
	if err := k8sClient.Create(ctx, a2aSampleCR("a2a-disc-late")); err != nil {
		t.Fatal(err)
	}
	waitForModel(t, ctx, "agent.a2a-disc-late")
}

func waitForModel(t *testing.T, ctx context.Context, name string) *litellmv1alpha1.LiteLLMModel {
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

func assertNoModel(t *testing.T, ctx context.Context, name string) {
	t.Helper()
	var m litellmv1alpha1.LiteLLMModel
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: name, Namespace: WatchNamespace}, &m); err == nil {
		t.Errorf("model %s exists; it should have been filtered out", name)
	}
}
```
(If `waitForModel` / `assertNoModel` names collide with existing helpers, reuse those instead.)

Plus a CEL admission check in the same file: creating `type: a2a` with `baseUrl` set must fail with `a2a forbids`.

Run: `make test-envtest-pkg PKG=./internal/controller/... FOCUS='ModelDiscovery' TIMEOUT=15m ; echo "EXIT=$?"` → PASS.

- [ ] **Step 6: Docs + commit**

- `docs/user-guide/` model-discovery page: `a2a` row + example:
```yaml
apiVersion: litellm.ackstorm.ai/v1alpha1
kind: LiteLLMModelDiscovery
metadata:
  name: a2a-agents
spec:
  type: a2a
  prefix: agent
  filters:
    exclude: ["internal-.*"]
  info:
    mode: chat
    access_groups: ["a2a"]
  refresh:
    interval: 15m
```
- `docs/user-guide/a2a-agent.md`: `exposeAsModel` section — note `type: a2a` discovery as the bulk alternative; do not use both for the same agent (same child name → `ExplicitModelExists` skip).
- API reference regenerate/edit as in the previous plan; `make helm-sync` (pre-push fails on chart CRD drift otherwise); CHANGELOG "Added: ModelDiscovery type a2a".

```bash
make helm-sync gen-manifests gen-code; echo "EXIT=$?"
make qa-lint-changed; echo "EXIT=$?"
make test-unit > "$LOG" 2>&1; echo "EXIT=$?"; grep -E -- '--- FAIL|^FAIL' "$LOG"
git add api internal config deploy docs CHANGELOG.md
git commit -m "feat(modeldiscovery): add a2a type sourcing LiteLLMA2AAgent CRs"
```

---

### Task 2: Gitops migration (after the operator release is in prod)

**Files (in `/workspace/private/ackstorm/nglz-genai/gitops-genai-blueprint`):**
- Create: the Discovery CR next to the other discoveries (`grep -rln "kind: LiteLLMModelDiscovery" workloads`) — same file/dir convention.
- Modify: `workloads/config/a2aagents.yaml` — delete each `exposeAsModel:` block and its comment.

- [ ] **Step 1:** Add
```yaml
apiVersion: litellm.ackstorm.ai/v1alpha1
kind: LiteLLMModelDiscovery
metadata:
  name: a2a-agents
  namespace: ${namespace}
spec:
  type: a2a
  prefix: agent            # keeps the existing agent.<name> model names
  info:
    mode: chat
    access_groups: ["a2a"] # same model tag as today (granted by team-dream)
  refresh:
    interval: 15m
```
- [ ] **Step 2:** Remove `exposeAsModel` from the five agents in the SAME commit. Ordering note: if the Discovery reconciles before the agent reconciler prunes the old `agent.*` children, those names are skipped as `ExplicitModelExists`; the prune fires an agent event → the new watch re-drives the Discovery, so it converges within seconds. Verify rather than assume.
- [ ] **Step 3:** `kustomize build workloads/config > /dev/null; echo "EXIT=$?"`; commit `feat(a2a): publish agents as models via a2a discovery`.
- [ ] **Step 4 (after push, user go-ahead):** `kubectl get llmd -n ackstorm a2a-agents` → `Ready=True`, `generatedChildren` = 5 `agent.*`; `/v1/models` with a user key still lists the five `agent.*`.
