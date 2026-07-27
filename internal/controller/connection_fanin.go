// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
)

// fanInNamespace resolves the namespace to fan a Connection event into
// across two trigger paths:
//
//   - The Watches() informer path delivers a *LiteLLMConnection as obj —
//     use the object's namespace so a future cross-namespace operator
//     deployment still routes correctly.
//   - The ConnectionRebuiltSource raw-source path delivers a nil obj
//     (the Cache emits a content-free GenericEvent) — fall back to the
//     reconciler's configured operator namespace so the same mapper
//     serves both paths.
//
// fallback MUST be the reconciler's r.Namespace.
//
// Callers MUST log + drop on an empty return (WR-04): a misconfigured
// Namespace combined with the raw-source path would otherwise silently
// drop every Ready emit, reproducing CR-01-style stuck-Ready=False
// symptoms with no operator-side signal.
func fanInNamespace(obj client.Object, fallback string) string {
	if conn, ok := obj.(*litellmv1alpha1.LiteLLMConnection); ok && conn != nil {
		return conn.Namespace
	}
	return fallback
}

// logEmptyFanInNamespace records a V(1) diagnostic when a Connection
// fan-in mapper resolves to an empty namespace. Centralized so all six
// mappers share the same observable symptom; never logs at higher
// verbosity because the empty-namespace path is normally caused by an
// operator misconfiguration the platform owner should catch in CI.
func logEmptyFanInNamespace(ctx context.Context, kind string) {
	log.FromContext(ctx).V(1).Info(
		"connection fan-in mapper: empty namespace; dropping rebuilt enqueue",
		"kind", kind,
		"hint", "set --watch-namespace / WATCH_NAMESPACE on the operator",
	)
}

// connectionFanIn maps a LiteLLMConnection trigger (either the
// Watches/connectionReadyTransition path with a *LiteLLMConnection as obj,
// or the ConnectionRebuiltSource raw-source path with a nil obj per
// fanInNamespace) to reconcile requests for every CR of the target kind in
// the resolved namespace (M-Q2). It is the shared core of the five
// connectionTo<Kind> mappers, which were byte-identical modulo the *List
// type and the kind label.
//
// Together with the false→true predicate AND the cache-rebuilt source,
// this fans-in the Connection-Ready recovery signal so dependent CRs
// re-reconcile within a single event window instead of waiting on their own
// backoff queue (FIX.txt M-3b; issue #44 cache-population race close).
func connectionFanIn(ctx context.Context, c client.Client, obj client.Object, list client.ObjectList, defaultNS, kindLabel string) []reconcile.Request {
	ns := fanInNamespace(obj, defaultNS)
	if ns == "" {
		logEmptyFanInNamespace(ctx, kindLabel)
		return nil
	}
	if err := c.List(ctx, list, client.InNamespace(ns)); err != nil {
		log.FromContext(ctx).V(1).Info("connection fan-in: List failed, dropping re-enqueue",
			"kind", kindLabel, "namespace", ns, "err", err.Error())
		return nil
	}
	objs, err := apimeta.ExtractList(list)
	if err != nil {
		log.FromContext(ctx).V(1).Info("connection fan-in: ExtractList failed, dropping re-enqueue",
			"kind", kindLabel, "err", err.Error())
		return nil
	}
	out := make([]reconcile.Request, 0, len(objs))
	for _, o := range objs {
		if co, ok := o.(client.Object); ok {
			out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(co)})
		}
	}
	return out
}

func (r *MCPServerReconciler) connectionToMCPServers(ctx context.Context, obj client.Object) []reconcile.Request {
	return connectionFanIn(ctx, r.Client, obj, &litellmv1alpha1.LiteLLMMCPServerList{}, r.Namespace, "LiteLLMMCPServer")
}

func (r *ModelReconciler) connectionToModels(ctx context.Context, obj client.Object) []reconcile.Request {
	return connectionFanIn(ctx, r.Client, obj, &litellmv1alpha1.LiteLLMModelList{}, r.Namespace, "LiteLLMModel")
}

func (r *A2AAgentReconciler) connectionToA2AAgents(ctx context.Context, obj client.Object) []reconcile.Request {
	return connectionFanIn(ctx, r.Client, obj, &litellmv1alpha1.LiteLLMA2AAgentList{}, r.Namespace, "LiteLLMA2AAgent")
}

func (r *TeamReconciler) connectionToTeams(ctx context.Context, obj client.Object) []reconcile.Request {
	return connectionFanIn(ctx, r.Client, obj, &litellmv1alpha1.LiteLLMTeamList{}, r.Namespace, "LiteLLMTeam")
}

func (r *MCPToolsetReconciler) connectionToMCPToolsets(ctx context.Context, obj client.Object) []reconcile.Request {
	return connectionFanIn(ctx, r.Client, obj, &litellmv1alpha1.LiteLLMMCPToolsetList{}, r.Namespace, "LiteLLMMCPToolset")
}

// ConnectionRebuiltSource returns a controller-runtime source that
// drains a Cache.Subscribe() channel and, for each event, invokes
// mapper(ctx, nil) and enqueues the returned requests. Closes the
// boot-time race described in issue #44:
//
//	t0: Connection-watch CreateFunc fires CreateFunc on the initial
//	    cache list with Ready=true. Mapper enqueues all child CRs.
//	t0+δ: Child reconcile reads Cache.Snapshot()==zero because the
//	      Connection reconciler has not yet called Rebuild in this
//	      process. Writes Ready=False/LiteLLMUnavailable.
//	t0+probe: Connection reconciler probes; Cache.Rebuild is called
//	          with Ready=true. The predicate-based watch stays silent
//	          (Ready=True→Ready=True is not a transition), but THIS
//	          source fires its rebuilt event, mapper re-enqueues all
//	          child CRs, and the next reconcile reads a populated
//	          snapshot and writes Ready=True.
//
// Pass a nil channel to skip wiring — useful for tests that wire a
// FakeConnectionCache (the fake does not implement Subscribe).
//
// Passing a non-nil ch with a nil mapper is a programmer error: the
// subscriber would drain the channel forever and never enqueue
// anything, reproducing the very stuck-on-boot symptom #44 was filed
// for but with no operator-side signal. Panic at setup time so the
// boot phase fails loudly instead of degrading silently (WR-03).
func ConnectionRebuiltSource(ch <-chan event.GenericEvent, mapper handler.MapFunc) source.Source {
	if ch == nil {
		return nil
	}
	if mapper == nil {
		panic("ConnectionRebuiltSource: mapper is nil but ch is non-nil — programmer error")
	}
	return source.TypedFunc[reconcile.Request](
		func(ctx context.Context, q workqueue.TypedRateLimitingInterface[reconcile.Request]) error {
			go func() {
				for {
					select {
					case <-ctx.Done():
						return
					case _, ok := <-ch:
						if !ok {
							return
						}
						for _, req := range mapper(ctx, nil) {
							q.Add(req)
						}
					}
				}
			}()
			return nil
		},
	)
}
