// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
)

// connectionToMCPServers maps a LiteLLMConnection event (filtered by
// connectionReadyTransition) to reconcile requests for every
// LiteLLMMCPServer in the same namespace. Together with the
// false→true predicate, this fans-in the Connection-Ready recovery
// signal so dependent CRs re-reconcile within a single event window
// instead of waiting on their own backoff queue (FIX.txt M-3b).
func (r *MCPServerReconciler) connectionToMCPServers(ctx context.Context, obj client.Object) []reconcile.Request {
	conn, ok := obj.(*litellmv1alpha1.LiteLLMConnection)
	if !ok {
		return nil
	}
	var list litellmv1alpha1.LiteLLMMCPServerList
	if err := r.List(ctx, &list, client.InNamespace(conn.Namespace)); err != nil {
		return nil
	}
	out := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{
			Name: list.Items[i].Name, Namespace: list.Items[i].Namespace,
		}})
	}
	return out
}

// connectionToModels — see connectionToMCPServers for the contract.
func (r *ModelReconciler) connectionToModels(ctx context.Context, obj client.Object) []reconcile.Request {
	conn, ok := obj.(*litellmv1alpha1.LiteLLMConnection)
	if !ok {
		return nil
	}
	var list litellmv1alpha1.LiteLLMModelList
	if err := r.List(ctx, &list, client.InNamespace(conn.Namespace)); err != nil {
		return nil
	}
	out := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{
			Name: list.Items[i].Name, Namespace: list.Items[i].Namespace,
		}})
	}
	return out
}

// connectionToA2AAgents — see connectionToMCPServers for the contract.
func (r *A2AAgentReconciler) connectionToA2AAgents(ctx context.Context, obj client.Object) []reconcile.Request {
	conn, ok := obj.(*litellmv1alpha1.LiteLLMConnection)
	if !ok {
		return nil
	}
	var list litellmv1alpha1.LiteLLMA2AAgentList
	if err := r.List(ctx, &list, client.InNamespace(conn.Namespace)); err != nil {
		return nil
	}
	out := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{
			Name: list.Items[i].Name, Namespace: list.Items[i].Namespace,
		}})
	}
	return out
}

// connectionToTeams — see connectionToMCPServers for the contract.
func (r *TeamReconciler) connectionToTeams(ctx context.Context, obj client.Object) []reconcile.Request {
	conn, ok := obj.(*litellmv1alpha1.LiteLLMConnection)
	if !ok {
		return nil
	}
	var list litellmv1alpha1.LiteLLMTeamList
	if err := r.List(ctx, &list, client.InNamespace(conn.Namespace)); err != nil {
		return nil
	}
	out := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{
			Name: list.Items[i].Name, Namespace: list.Items[i].Namespace,
		}})
	}
	return out
}
