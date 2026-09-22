// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
)

const (
	// EnvA2AModelProvider names the LiteLLM provider prefix used in the
	// generated child's `litellm_params.model`. It is configurable because
	// the provider that speaks A2A is a deployment-local choice, not an
	// upstream constant: LiteLLM's own built-in `a2a/` prefix exists but
	// (as of 1.99.1) drops the agent's configured auth headers and pins the
	// wire format to A2A 0.3, so deployments front it with their own
	// custom_provider_map entry instead. Making this an env var means
	// switching to a fixed upstream route later costs a Deployment edit,
	// not a new image.
	EnvA2AModelProvider = "A2A_MODEL_PROVIDER"

	// a2aModelProviderFallback is the provider prefix used when
	// EnvA2AModelProvider is unset.
	a2aModelProviderFallback = "a2a1"

	// exposedModelNamePrefix prefixes the generated child's name. The child
	// is `<prefix><agent metadata.name>`, which is also what a client calls,
	// so it is deliberately readable rather than hashed.
	exposedModelNamePrefix = "agent."

	// generatedByAgentLabel marks a LiteLLMModel generated from a
	// LiteLLMA2AAgent, valued with the agent's name. Deliberately NOT
	// `generatedByLabel` (which ModelDiscovery uses): that label drives
	// ModelDiscovery's prune list, and a Discovery CR that happened to share
	// a name with an agent would otherwise adopt — and then delete — a child
	// it never created.
	generatedByAgentLabel = "litellm.ackstorm.ai/generated-by-agent"

	// exposeFieldOwner is the SSA field owner for the generated child.
	exposeFieldOwner = "litellm-a2aagent"
)

// a2aModelProvider returns the configured provider prefix for generated
// children, falling back to a2aModelProviderFallback when unset or blank.
func a2aModelProvider() string {
	if v := strings.TrimSpace(os.Getenv(EnvA2AModelProvider)); v != "" {
		return v
	}
	return a2aModelProviderFallback
}

// exposedModelName is the generated child's name for the given agent.
func exposedModelName(agentName string) string {
	return exposedModelNamePrefix + agentName
}

// reconcileExposedModel brings the generated LiteLLMModel child in line with
// spec.exposeAsModel: applied when the block is present, pruned when it is not.
//
// It is called ONCE, early in Reconcile — before the connection gate and
// before any of the hash/steady-state branching. That placement is deliberate:
// the child is a Kubernetes object, so it needs neither LiteLLM reachability
// nor a completed registration, and calling it from the later branches would
// reintroduce the #102 failure shape, where a CR that short-circuits at the
// steady-state return never reaches the projection and silently never gets a
// child.
//
// A conflict on the SSA apply is returned to the caller for ordinary backoff.
func (r *A2AAgentReconciler) reconcileExposedModel(
	ctx context.Context, a2a *litellmv1alpha1.LiteLLMA2AAgent,
) error {
	name := exposedModelName(a2a.Name)

	if a2a.Spec.ExposeAsModel == nil {
		return r.pruneExposedModel(ctx, a2a, name)
	}

	child, err := buildExposedModel(a2a, name)
	if err != nil {
		return err
	}
	if err := r.Patch(ctx, child, client.Apply,
		client.FieldOwner(exposeFieldOwner), client.ForceOwnership); err != nil {
		return fmt.Errorf("apply generated model %s: %w", name, err)
	}
	return nil
}

// pruneExposedModel deletes a previously-generated child once
// spec.exposeAsModel is removed.
//
// It deletes ONLY a child this agent generated, verified by the
// generatedByAgentLabel plus a controller owner reference naming this exact
// UID. A hand-written LiteLLMModel that happens to be called
// `agent.<something>` is therefore never touched — the label check alone would
// not be enough, since a user can set any label they like.
func (r *A2AAgentReconciler) pruneExposedModel(
	ctx context.Context, a2a *litellmv1alpha1.LiteLLMA2AAgent, name string,
) error {
	var existing litellmv1alpha1.LiteLLMModel
	if err := r.Get(ctx, types.NamespacedName{Namespace: a2a.Namespace, Name: name}, &existing); err != nil {
		if apierrors.IsNotFound(err) {
			return nil // nothing to prune — the common case
		}
		return fmt.Errorf("get generated model %s: %w", name, err)
	}
	if existing.Labels[generatedByAgentLabel] != a2a.Name || !ownedByAgent(&existing, a2a.UID) {
		return nil // not ours; leave it alone
	}
	if err := r.Delete(ctx, &existing); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete generated model %s: %w", name, err)
	}
	return nil
}

// ownedByAgent reports whether obj carries a controller owner reference with
// the given agent UID.
func ownedByAgent(obj *litellmv1alpha1.LiteLLMModel, uid types.UID) bool {
	for _, ref := range obj.OwnerReferences {
		if ref.UID == uid && ref.Controller != nil && *ref.Controller {
			return true
		}
	}
	return false
}

// buildExposedModel renders the generated LiteLLMModel for an agent.
//
// The child intentionally carries NO spec.secrets: the agent's credentials
// authenticate the operator to the agent and are held on the agent
// registration, not on the model row, which only needs to name the route.
func buildExposedModel(
	a2a *litellmv1alpha1.LiteLLMA2AAgent, name string,
) (*litellmv1alpha1.LiteLLMModel, error) {
	params := map[string]any{
		// The segment after the prefix is metadata.name, which is also the
		// agent_name this reconciler registers with LiteLLM, so the model row
		// and the agent registration cannot drift apart.
		"model": a2aModelProvider() + "/" + a2a.Name,
	}
	paramsBytes, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("encode generated model params: %w", err)
	}

	info := map[string]any{"mode": "chat"}
	if desc := agentCardDescription(a2a); desc != "" {
		info["description"] = desc
	}
	if groups := a2a.Spec.ExposeAsModel.AccessGroups; len(groups) > 0 {
		info["access_groups"] = groups
	}
	infoBytes, err := json.Marshal(info)
	if err != nil {
		return nil, fmt.Errorf("encode generated model info: %w", err)
	}

	yes := true
	return &litellmv1alpha1.LiteLLMModel{
		TypeMeta: metav1.TypeMeta{
			APIVersion: litellmv1alpha1.GroupVersion.String(),
			Kind:       "LiteLLMModel",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: a2a.Namespace,
			Labels: map[string]string{
				generatedByAgentLabel: a2a.Name,
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         litellmv1alpha1.GroupVersion.String(),
					Kind:               a2aAgentKind,
					Name:               a2a.Name,
					UID:                a2a.UID,
					Controller:         &yes,
					BlockOwnerDeletion: &yes,
				},
			},
		},
		Spec: litellmv1alpha1.ModelSpec{
			Params: runtime.RawExtension{Raw: paramsBytes},
			Info:   runtime.RawExtension{Raw: infoBytes},
		},
	}, nil
}

// agentCardDescription pulls spec.agentCard.description so the generated model
// carries the same human text as the agent. Returns "" when the card is
// absent, unparseable, or has no description — the description is cosmetic and
// must never fail a reconcile.
func agentCardDescription(a2a *litellmv1alpha1.LiteLLMA2AAgent) string {
	if len(a2a.Spec.AgentCard.Raw) == 0 {
		return ""
	}
	var card map[string]any
	if err := json.Unmarshal(a2a.Spec.AgentCard.Raw, &card); err != nil {
		return ""
	}
	desc, _ := card["description"].(string)
	return desc
}
