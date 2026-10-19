// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
	"github.com/ackstorm/alitellm-operator/internal/connection"
	"github.com/ackstorm/alitellm-operator/internal/controller/deletionpolicy"
	"github.com/ackstorm/alitellm-operator/internal/litellm"
	"github.com/ackstorm/alitellm-operator/internal/metrics"
)

const (
	modelGroupBudgetFinalizer = "modelgroupbudgets.litellm.ackstorm.ai/finalizer"
	modelGroupBudgetKind      = "LiteLLMModelGroupBudget"
	// ModelGroupBudgetGroupIndexField indexes budgets by tag for the
	// one-owner-per-tag rule.
	ModelGroupBudgetGroupIndexField = ".spec.modelGroup"
)

// +kubebuilder:rbac:groups=litellm.ackstorm.ai,resources=litellmmodelgroupbudgets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=litellm.ackstorm.ai,resources=litellmmodelgroupbudgets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=litellm.ackstorm.ai,resources=litellmmodelgroupbudgets/finalizers,verbs=update

// ModelGroupBudgetReconciler projects LiteLLMModelGroupBudget onto
// /access_group/{tag}/budget. GET-compare-PUT, so steady state is one GET
// per relist tick and no status write.
type ModelGroupBudgetReconciler struct {
	client.Client
	Scheme            *runtime.Scheme
	Cache             connection.ConnectionCache
	Recorder          record.EventRecorder
	Namespace         string
	ConnectionRebuilt <-chan event.GenericEvent
}

func IndexModelGroupBudgetGroup(o client.Object) []string {
	b, ok := o.(*litellmv1alpha1.LiteLLMModelGroupBudget)
	if !ok {
		return nil
	}
	return []string{b.Spec.ModelGroup}
}

func (r *ModelGroupBudgetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("modelgroupbudget", req.NamespacedName)
	var b litellmv1alpha1.LiteLLMModelGroupBudget
	if err := r.Get(ctx, req.NamespacedName, &b); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	owner, err := r.isOwner(ctx, &b)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !b.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.finalize(ctx, logger, &b, owner)
	}
	if !controllerutil.ContainsFinalizer(&b, modelGroupBudgetFinalizer) {
		controllerutil.AddFinalizer(&b, modelGroupBudgetFinalizer)
		return ctrl.Result{}, r.Update(ctx, &b)
	}
	if !owner {
		return ctrl.Result{}, r.setReady(ctx, &b, metav1.ConditionFalse, reasonDuplicateModelGroup,
			fmt.Sprintf("modelGroup %q is already budgeted by an older LiteLLMModelGroupBudget", b.Spec.ModelGroup))
	}

	snap := r.Cache.Snapshot()
	if !snap.Usable() {
		reason := snap.Reason
		if reason == "" {
			reason = reasonConnecting
		}
		return ctrl.Result{}, r.setReady(ctx, &b, metav1.ConditionFalse, reasonLiteLLMUnavailable,
			fmt.Sprintf("LiteLLMConnection/default not Ready (reason: %s)", reason))
	}

	group := b.Spec.ModelGroup
	got, err := snap.Client.GetModelGroupBudget(ctx, group)
	if err == nil && !budgetMatches(&b.Spec, got) {
		if budgetNeedsClear(&b.Spec, got) {
			err = snap.Client.DeleteModelGroupBudget(ctx, group)
		}
		if err == nil {
			err = snap.Client.PutModelGroupBudget(ctx, group, budgetRequest(&b.Spec))
		}
	}
	if litellm.IsNotFound(err) {
		// Re-driven by the safety relist once a model carries the tag.
		return ctrl.Result{}, r.setReady(ctx, &b, metav1.ConditionFalse, reasonModelGroupNotFound,
			fmt.Sprintf("no LiteLLM model carries access group %q", group))
	}
	if err != nil {
		return classifyMutationError(ctx, logger, err, "/access_group/{group}/budget", modelGroupBudgetKind,
			func(c context.Context, s metav1.ConditionStatus, reason, msg string) error {
				return r.writeStatus(c, &b, s, reason, msg)
			},
			r.Cache.InvalidateOn401, snap.NormalizedRequeueOnRejectedAfter)
	}
	return ctrl.Result{}, r.setReady(ctx, &b, metav1.ConditionTrue, reasonSynced, "budget applied")
}

// isOwner: oldest creationTimestamp wins, ties broken by name. Deleting CRs
// still count, so the owner keeps ownership while it DELETEs the budget; a
// waiting duplicate takes over at the next relist tick.
func (r *ModelGroupBudgetReconciler) isOwner(ctx context.Context, b *litellmv1alpha1.LiteLLMModelGroupBudget) (bool, error) {
	var peers litellmv1alpha1.LiteLLMModelGroupBudgetList
	if err := r.List(ctx, &peers, client.InNamespace(b.Namespace),
		client.MatchingFields{ModelGroupBudgetGroupIndexField: b.Spec.ModelGroup}); err != nil {
		return false, err
	}
	for i := range peers.Items {
		p := &peers.Items[i]
		if p.CreationTimestamp.Before(&b.CreationTimestamp) ||
			(p.CreationTimestamp.Equal(&b.CreationTimestamp) && p.Name < b.Name) {
			return false, nil
		}
	}
	return true, nil
}

// finalize: only the owner touches LiteLLM; a duplicate drains silently so it
// can never wipe the owner's budget.
func (r *ModelGroupBudgetReconciler) finalize(ctx context.Context, logger logr.Logger, b *litellmv1alpha1.LiteLLMModelGroupBudget, owner bool) error {
	if !controllerutil.ContainsFinalizer(b, modelGroupBudgetFinalizer) {
		return nil
	}
	if owner {
		policy := deletionpolicy.Resolve(b, b.Spec.DeletionPolicy)
		onAckMissing := newAckMissingFn(r.Recorder, b, modelGroupBudgetKind, b.Namespace, b.Name, policy)
		snap := r.Cache.Snapshot()
		if !snap.Usable() {
			if err := onAckMissing(ackUnavailable(snap)); err != nil {
				return err
			}
		} else if err := snap.Client.DeleteModelGroupBudget(ctx, b.Spec.ModelGroup); err != nil {
			var auth401 *litellm.Auth401Error
			switch {
			case errors.As(err, &auth401):
				// No cache invalidation on the deletion path: the probe owns key health.
				if gerr := onAckMissing("401 on DeleteModelGroupBudget", false); gerr != nil {
					return gerr
				}
			case is4xxStatus(err):
				logger.Info("deletion: deterministic 4xx on DeleteModelGroupBudget; ack-missing", "error", err.Error())
				if gerr := onAckMissing("4xx on DeleteModelGroupBudget: "+err.Error(), true); gerr != nil {
					return gerr
				}
			default:
				return err
			}
		}
	}
	metrics.DeletionBlocked.Forget(modelGroupBudgetKind, b.Namespace, b.Name)
	controllerutil.RemoveFinalizer(b, modelGroupBudgetFinalizer)
	return r.Update(ctx, b)
}

func budgetMatches(s *litellmv1alpha1.ModelGroupBudgetSpec, got *litellm.ModelGroupBudget) bool {
	if got == nil {
		return false
	}
	return floatPtrEqual(s.Limit, got.MaxBudget) && floatPtrEqual(s.SoftLimit, got.SoftBudget) &&
		s.Period == derefString(got.BudgetDuration)
}

// budgetNeedsClear: a field set in LiteLLM but unset in spec. PUT omits unset
// fields, so it cannot clear them — DELETE first, then PUT.
func budgetNeedsClear(s *litellmv1alpha1.ModelGroupBudgetSpec, got *litellm.ModelGroupBudget) bool {
	return got != nil && ((s.Limit == nil && got.MaxBudget != nil) ||
		(s.SoftLimit == nil && got.SoftBudget != nil) ||
		(s.Period == "" && derefString(got.BudgetDuration) != ""))
}

func budgetRequest(s *litellmv1alpha1.ModelGroupBudgetSpec) *litellm.ModelGroupBudgetRequest {
	return &litellm.ModelGroupBudgetRequest{MaxBudget: s.Limit, SoftBudget: s.SoftLimit, BudgetDuration: s.Period}
}

func floatPtrEqual(a, b *float64) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// setReady writes the Ready condition only when it would change, so a
// steady-state relist tick costs one GET and no status write. Also heals a
// stale Ready (the #102 rule) because every path ends here.
func (r *ModelGroupBudgetReconciler) setReady(ctx context.Context, b *litellmv1alpha1.LiteLLMModelGroupBudget, status metav1.ConditionStatus, reason, message string) error {
	if c := apimeta.FindStatusCondition(b.Status.Conditions, conditionTypeReady); c != nil &&
		c.Status == status && c.Reason == reason && c.Message == message &&
		b.Status.ObservedGeneration == b.Generation {
		return nil
	}
	if err := r.writeStatus(ctx, b, status, reason, message); err != nil && !apierrors.IsConflict(err) {
		return err
	}
	return nil
}

func (r *ModelGroupBudgetReconciler) writeStatus(ctx context.Context, b *litellmv1alpha1.LiteLLMModelGroupBudget, status metav1.ConditionStatus, reason, message string) error {
	cond := buildReadyCondition(b.Generation, status, reason, message)
	gen := b.Generation
	var fresh litellmv1alpha1.LiteLLMModelGroupBudget
	err := writeStatusWithRetry(ctx, r.Client, b, &fresh, func(f *litellmv1alpha1.LiteLLMModelGroupBudget) {
		apimeta.SetStatusCondition(&f.Status.Conditions, cond)
		f.Status.ObservedGeneration = gen
	})
	if err == nil {
		b.Status = fresh.Status
		b.ResourceVersion = fresh.ResourceVersion
	}
	recordReconcileMetric(modelGroupBudgetKind, b.Namespace, reason)
	return err
}

func (r *ModelGroupBudgetReconciler) connectionToModelGroupBudgets(ctx context.Context, obj client.Object) []reconcile.Request {
	return connectionFanIn(ctx, r.Client, obj, &litellmv1alpha1.LiteLLMModelGroupBudgetList{}, r.Namespace, modelGroupBudgetKind)
}

func (r *ModelGroupBudgetReconciler) SetupWithManager(mgr ctrl.Manager, safetyRelistCh chan reconcile.Request) error {
	b := ctrl.NewControllerManagedBy(mgr).
		For(&litellmv1alpha1.LiteLLMModelGroupBudget{}).
		Watches(
			&litellmv1alpha1.LiteLLMConnection{},
			handler.EnqueueRequestsFromMapFunc(r.connectionToModelGroupBudgets),
			builder.WithPredicates(connectionReadyTransition()),
		).
		WithOptions(transientBackoffOptions()).
		Named("modelgroupbudget")
	if src := ConnectionRebuiltSource(r.ConnectionRebuilt, r.connectionToModelGroupBudgets); src != nil {
		b = b.WatchesRawSource(src)
	}
	if safetyRelistCh != nil {
		b = b.WatchesRawSource(source.TypedFunc[reconcile.Request](
			func(ctx context.Context, q workqueue.TypedRateLimitingInterface[reconcile.Request]) error {
				go func() {
					for {
						select {
						case <-ctx.Done():
							return
						case req, ok := <-safetyRelistCh:
							if !ok {
								return
							}
							q.Add(req)
						}
					}
				}()
				return nil
			},
		))
	}
	return b.Complete(r)
}

func ListModelGroupBudgetRequests(ctx context.Context, c client.Client, namespace string) ([]reconcile.Request, error) {
	var list litellmv1alpha1.LiteLLMModelGroupBudgetList
	if err := c.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	reqs := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
	}
	return reqs, nil
}
