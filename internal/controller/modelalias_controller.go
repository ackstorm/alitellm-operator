// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
	"github.com/ackstorm/alitellm-operator/internal/connection"
)

// ModelAlias-specific reasons. The shared reasonSynced and
// reasonLiteLLMUnavailable are reused from litellmconnection_controller.go.
const (
	reasonAliasPartialConflict = "PartialAliasConflict"
	reasonModelAliasRejected   = "LiteLLMRejected"
)

const (
	modelAliasFinalizer = "modelaliases.litellm.ackstorm.ai/finalizer"

	// ModelAliasSingletonKey is the sentinel reconcile key all
	// LiteLLMModelAlias CR events coalesce onto. Concurrent events from N
	// CRs are deduplicated by the work-queue into a single reconcile pass
	// per debounce window, producing exactly one HTTP write to LiteLLM
	// /config/update per pass (regardless of N).
	ModelAliasSingletonKey = "_modelalias_singleton"

	// modelAliasResyncPeriod is the safety-relist cadence. The controller
	// otherwise reconciles on CR events and LiteLLMConnection Ready
	// transitions; periodic resync defends against missed events and
	// out-of-band LiteLLM state drift.
	modelAliasResyncPeriod = 15 * time.Minute
)

// +kubebuilder:rbac:groups=litellm.ackstorm.ai,resources=litellmmodelaliases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=litellm.ackstorm.ai,resources=litellmmodelaliases/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=litellm.ackstorm.ai,resources=litellmmodelaliases/finalizers,verbs=update

// ModelAliasReconciler aggregates ALL LiteLLMModelAlias CRs into one
// router_settings.model_group_alias map and writes it via the LiteLLM
// /config/update endpoint. All CR events coalesce onto the sentinel
// reconcile key ModelAliasSingletonKey so concurrent edits produce ONE
// HTTP write per debounce window.
type ModelAliasReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	Cache     connection.ConnectionCache
	Recorder  record.EventRecorder
	Namespace string
	Log       logr.Logger
	// ConnectionRebuilt — see GuardRailReconciler.ConnectionRebuilt
	// (issue #44 cache-population race close). nil-safe.
	ConnectionRebuilt <-chan event.GenericEvent
}

// Reconcile implements the ModelAlias aggregate state machine.
//
// Ordering invariant (MALIAS-03): finalizers on CRs being deleted are
// removed ONLY AFTER the LiteLLM POST /config/update succeeds for the
// rebuilt-without-them map. Removing earlier would risk orphan entries in
// LiteLLM if the write fails.
func (r *ModelAliasReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("singleton", req.NamespacedName)

	// Defense in depth (post-2026-05-26 review F3): all alias and
	// connection events must map to the singleton key via the
	// SetupWithManager Watches handlers. Any non-singleton key is
	// either a bug (For default mapper re-introduced) or an external
	// enqueue we don't service.
	if req.Name != ModelAliasSingletonKey {
		logger.V(2).Info("ignoring non-singleton reconcile key", "got", req.Name)
		return ctrl.Result{}, nil
	}

	var list litellmv1alpha1.LiteLLMModelAliasList
	if err := r.List(ctx, &list); err != nil {
		return ctrl.Result{}, fmt.Errorf("list LiteLLMModelAlias: %w", err)
	}

	// Add finalizers to alive CRs that lack one. Deleting CRs keep their
	// finalizer until the post-write strip step below.
	for i := range list.Items {
		item := &list.Items[i]
		if !item.DeletionTimestamp.IsZero() {
			continue
		}
		if controllerutil.ContainsFinalizer(item, modelAliasFinalizer) {
			continue
		}
		controllerutil.AddFinalizer(item, modelAliasFinalizer)
		if err := r.Update(ctx, item); err != nil {
			if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
				return ctrl.Result{Requeue: true}, nil
			}
			return ctrl.Result{}, err
		}
	}

	snap := r.Cache.Snapshot()
	if !snap.Ready {
		msg := fmt.Sprintf("LiteLLMConnection/default not Ready (reason: %s)", snap.Reason)
		return r.broadcastNotReady(ctx, list.Items, reasonLiteLLMUnavailable, msg, logger)
	}

	agg := AggregateModelAliases(filterAliveAliases(list.Items))

	cli := snap.Client
	current, err := cli.GetRouterSettings(ctx)
	if err != nil {
		msg := fmt.Sprintf("GET /get/config/callbacks: %v", err)
		return r.broadcastNotReady(ctx, list.Items, reasonModelAliasRejected, msg, logger)
	}
	current.ModelGroupAlias = agg.Desired
	if err := cli.UpdateRouterSettings(ctx, current); err != nil {
		msg := fmt.Sprintf("POST /config/update: %v", err)
		return r.broadcastNotReady(ctx, list.Items, reasonModelAliasRejected, msg, logger)
	}

	if err := r.writePerCRStatuses(ctx, list.Items, agg, logger); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.stripDeletingFinalizers(ctx, list.Items); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: modelAliasResyncPeriod}, nil
}

// filterAliveAliases drops CRs with DeletionTimestamp set so a pending
// delete is not included in the desired map.
func filterAliveAliases(items []litellmv1alpha1.LiteLLMModelAlias) []litellmv1alpha1.LiteLLMModelAlias {
	out := make([]litellmv1alpha1.LiteLLMModelAlias, 0, len(items))
	for _, it := range items {
		if it.DeletionTimestamp.IsZero() {
			out = append(out, it)
		}
	}
	return out
}

// broadcastNotReady applies a uniform Ready=False condition to every alive
// CR with no per-entry status mutation (because nothing was written).
func (r *ModelAliasReconciler) broadcastNotReady(
	ctx context.Context,
	items []litellmv1alpha1.LiteLLMModelAlias,
	reason, message string,
	logger logr.Logger,
) (ctrl.Result, error) {
	for _, item := range items {
		if !item.DeletionTimestamp.IsZero() {
			continue
		}
		cond := metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			Reason:             reason,
			Message:            message,
			ObservedGeneration: item.Generation,
			LastTransitionTime: metav1.Now(),
		}
		if err := r.applyStatus(ctx, item, cond, nil); err != nil {
			logger.Error(err, "status update on broadcast", "cr", crKey(item))
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

// writePerCRStatuses computes per-entry statuses for every alive CR after
// a successful LiteLLM write and updates the CR status subresource.
// Ready=True iff every entry applied; otherwise Ready=False with reason
// PartialAliasConflict and a message naming the conflicting entries.
func (r *ModelAliasReconciler) writePerCRStatuses(
	ctx context.Context,
	items []litellmv1alpha1.LiteLLMModelAlias,
	agg ModelAliasAggregate,
	logger logr.Logger,
) error {
	for _, item := range items {
		if !item.DeletionTimestamp.IsZero() {
			continue
		}
		rows := agg.ResolveCR(item)

		allApplied := true
		var conflicting []string
		for _, row := range rows {
			if !row.Applied {
				allApplied = false
				conflicting = append(conflicting, fmt.Sprintf("%s→%s", row.Name, row.ConflictsWith))
			}
		}

		var cond metav1.Condition
		if allApplied {
			cond = metav1.Condition{
				Type:               "Ready",
				Status:             metav1.ConditionTrue,
				Reason:             reasonSynced,
				Message:            fmt.Sprintf("%d alias entries applied to LiteLLM", len(rows)),
				ObservedGeneration: item.Generation,
				LastTransitionTime: metav1.Now(),
			}
		} else {
			cond = metav1.Condition{
				Type:               "Ready",
				Status:             metav1.ConditionFalse,
				Reason:             reasonAliasPartialConflict,
				Message:            fmt.Sprintf("%d of %d alias entries lost slot: %v", len(conflicting), len(rows), conflicting),
				ObservedGeneration: item.Generation,
				LastTransitionTime: metav1.Now(),
			}
		}

		if err := r.applyStatus(ctx, item, cond, rows); err != nil {
			logger.Error(err, "status update", "cr", crKey(item))
			return err
		}
	}
	return nil
}

// applyStatus reads the CR fresh, sets the Ready condition + AliasStatuses,
// and writes via the status subresource. Conflict errors are swallowed
// (will retry on the next reconcile).
func (r *ModelAliasReconciler) applyStatus(
	ctx context.Context,
	item litellmv1alpha1.LiteLLMModelAlias,
	cond metav1.Condition,
	rows []litellmv1alpha1.AliasEntryStatus,
) error {
	var fresh litellmv1alpha1.LiteLLMModelAlias
	if err := r.Get(ctx, types.NamespacedName{Name: item.Name, Namespace: item.Namespace}, &fresh); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	meta.SetStatusCondition(&fresh.Status.Conditions, cond)
	fresh.Status.ObservedGeneration = item.Generation
	if rows != nil {
		fresh.Status.AliasStatuses = rows
	}
	if err := r.Status().Update(ctx, &fresh); err != nil {
		if apierrors.IsConflict(err) {
			return nil
		}
		return err
	}
	return nil
}

// stripDeletingFinalizers removes the finalizer from CRs whose
// DeletionTimestamp is set, AFTER the LiteLLM POST /config/update has
// already removed their entries from the merged map. This guarantees
// MALIAS-03 (no orphan entries survive a deletion).
func (r *ModelAliasReconciler) stripDeletingFinalizers(
	ctx context.Context,
	items []litellmv1alpha1.LiteLLMModelAlias,
) error {
	for _, item := range items {
		if item.DeletionTimestamp.IsZero() {
			continue
		}
		if !controllerutil.ContainsFinalizer(&item, modelAliasFinalizer) {
			continue
		}
		controllerutil.RemoveFinalizer(&item, modelAliasFinalizer)
		if err := r.Update(ctx, &item); err != nil && !apierrors.IsConflict(err) && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// SetupWithManager wires the reconciler so all events coalesce onto the
// singleton work-queue key. NO For() registration — the default mapper
// would enqueue the per-object key in addition to the singleton, causing
// two Reconcile invocations per alias event (post-2026-05-26 review F3).
// The first Watches() owns the alias informer; Named() satisfies
// controller-runtime's controller-name requirement.
func (r *ModelAliasReconciler) SetupWithManager(mgr ctrl.Manager) error {
	b := ctrl.NewControllerManagedBy(mgr).
		Named("modelalias").
		Watches(
			&litellmv1alpha1.LiteLLMModelAlias{},
			handler.EnqueueRequestsFromMapFunc(r.mapToSingleton),
		).
		Watches(
			&litellmv1alpha1.LiteLLMConnection{},
			handler.EnqueueRequestsFromMapFunc(r.connectionToSingleton),
			builder.WithPredicates(connectionReadyTransition()),
		)
	if src := ConnectionRebuiltSource(r.ConnectionRebuilt, r.connectionToSingleton); src != nil {
		b = b.WatchesRawSource(src)
	}
	return b.Complete(r)
}

func (r *ModelAliasReconciler) mapToSingleton(_ context.Context, _ client.Object) []reconcile.Request {
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: r.Namespace, Name: ModelAliasSingletonKey}}}
}

func (r *ModelAliasReconciler) connectionToSingleton(_ context.Context, _ client.Object) []reconcile.Request {
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: r.Namespace, Name: ModelAliasSingletonKey}}}
}
