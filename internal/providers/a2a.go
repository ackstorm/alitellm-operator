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
