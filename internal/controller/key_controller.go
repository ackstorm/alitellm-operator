// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
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
	keyFinalizer = "keys.litellm.ackstorm.ai/finalizer"
	keyKind      = "LiteLLMKey"

	// keyManagedBy marks the keys and Secrets this operator created. It
	// carries no version so it still matches after an upgrade.
	keyManagedBy     = "alitellm-operator"
	keyMetaManagedBy = "managed-by"
	keyMetaOwnerUID  = "owner-uid"

	keySecretManagedByLabel = "app.kubernetes.io/managed-by" // #nosec G101 -- label name
	// keySecretOwnerUIDAnnotation stands in for an ownerReference, which
	// cannot cross namespaces.
	keySecretOwnerUIDAnnotation = "litellm.ackstorm.ai/owner-uid" // #nosec G101 -- annotation name

	reasonSecretAccessDenied = "SecretAccessDenied" // #nosec G101 -- condition reason
	reasonSecretNotOwned     = "SecretNotOwned"     // #nosec G101 -- condition reason
	reasonKeyAliasConflict   = "Conflict"
)

var errKeyAliasConflict = errors.New("key alias is held by a key the operator did not create")

// +kubebuilder:rbac:groups=litellm.ackstorm.ai,resources=litellmkeys,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=litellm.ackstorm.ai,resources=litellmkeys/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=litellm.ackstorm.ai,resources=litellmkeys/finalizers,verbs=update

// KeyReconciler projects LiteLLMKey onto a LiteLLM virtual key and writes the
// key value to a Secret in spec.secretRef.namespace. Every reconcile reads
// /key/info, so there is no steady-state short-circuit.
type KeyReconciler struct {
	client.Client
	Scheme            *runtime.Scheme
	Cache             connection.ConnectionCache
	Recorder          record.EventRecorder
	Namespace         string
	ConnectionRebuilt <-chan event.GenericEvent
	// Secrets must not be the manager's cached client: the cache covers only
	// WATCH_NAMESPACE, and a read that lags a create would look like a
	// deleted Secret and rotate a live key.
	Secrets client.Client
}

func (r *KeyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("key", req.NamespacedName)
	var k litellmv1alpha1.LiteLLMKey
	if err := r.Get(ctx, req.NamespacedName, &k); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !k.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.finalize(ctx, logger, &k)
	}
	if !controllerutil.ContainsFinalizer(&k, keyFinalizer) {
		controllerutil.AddFinalizer(&k, keyFinalizer)
		return ctrl.Result{}, r.Update(ctx, &k)
	}

	snap := r.Cache.Snapshot()
	if !snap.Usable() {
		reason := snap.Reason
		if reason == "" {
			reason = reasonConnecting
		}
		return ctrl.Result{}, r.setReady(ctx, &k, metav1.ConditionFalse, reasonLiteLLMUnavailable,
			fmt.Sprintf("LiteLLMConnection/default not Ready (reason: %s)", reason), "")
	}

	// The Secret is written before LiteLLM is called: a crash in between
	// leaves a value LiteLLM does not know, which the 404 arm below repairs.
	value, reason, message, err := r.ensureSecret(ctx, &k)
	if err != nil {
		return ctrl.Result{}, err
	}
	if reason != "" {
		return ctrl.Result{}, r.setReady(ctx, &k, metav1.ConditionFalse, reason, message, "")
	}

	hash := litellm.HashKey(value)
	body := renderKeyBody(&k.Spec)
	got, err := snap.Client.GetKeyInfo(ctx, hash)
	switch {
	case litellm.IsNotFound(err):
		err = r.generate(ctx, snap.Client, &k, value, body)
	case err == nil && !keyInSync(&k.Spec, got):
		err = snap.Client.UpdateKey(ctx, hash, body)
	}
	if errors.Is(err, errKeyAliasConflict) {
		return ctrl.Result{}, r.setReady(ctx, &k, metav1.ConditionFalse, reasonKeyAliasConflict,
			fmt.Sprintf("LiteLLM key alias %q is held by a key this operator did not create", k.Name), "")
	}
	if err != nil {
		return classifyMutationError(ctx, logger, err, "/key", keyKind,
			func(c context.Context, s metav1.ConditionStatus, reason, msg string) error {
				return r.writeStatus(c, &k, s, reason, msg, "")
			},
			r.Cache.InvalidateOn401, snap.NormalizedRequeueOnRejectedAfter)
	}
	return ctrl.Result{}, r.setReady(ctx, &k, metav1.ConditionTrue, reasonSynced, "key applied", keyRenderedHash(body))
}

// ensureSecret returns the key value, creating the Secret when it is absent.
// A non-empty reason means the Secret cannot be used and nothing was changed.
func (r *KeyReconciler) ensureSecret(ctx context.Context, k *litellmv1alpha1.LiteLLMKey) (value, reason, message string, err error) {
	ref := k.Spec.SecretRef
	denied := fmt.Sprintf("the operator may not access Secrets in namespace %q; add it to the Helm value keySecretNamespaces", ref.Namespace)
	var s corev1.Secret
	err = r.Secrets.Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, &s)
	switch {
	case apierrors.IsForbidden(err):
		return "", reasonSecretAccessDenied, denied, nil
	case err == nil:
		if s.Annotations[keySecretOwnerUIDAnnotation] != string(k.UID) {
			return "", reasonSecretNotOwned,
				fmt.Sprintf("Secret %s/%s exists and was not created for this LiteLLMKey", ref.Namespace, ref.Name), nil
		}
		if v := s.Data[ref.Key]; len(v) > 0 {
			return string(v), "", "", nil
		}
		if err := r.Secrets.Delete(ctx, &s); err != nil && !apierrors.IsNotFound(err) {
			return "", "", "", err
		}
	case !apierrors.IsNotFound(err):
		return "", "", "", err
	}

	value = "sk-" + rand.Text()
	err = r.Secrets.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   ref.Namespace,
			Name:        ref.Name,
			Labels:      map[string]string{keySecretManagedByLabel: keyManagedBy},
			Annotations: map[string]string{keySecretOwnerUIDAnnotation: string(k.UID)},
		},
		Data: map[string][]byte{ref.Key: []byte(value)},
	})
	if apierrors.IsForbidden(err) {
		return "", reasonSecretAccessDenied, denied, nil
	}
	if err != nil {
		return "", "", "", err
	}
	return value, "", "", nil
}

// generate registers the key. On a 400 it looks for a holder of the alias: a
// key this operator created under an earlier value is replaced, anything else
// is a conflict. A 400 with no holder is returned as it came.
func (r *KeyReconciler) generate(ctx context.Context, c *litellm.Client, k *litellmv1alpha1.LiteLLMKey, value string, body map[string]any) error {
	gen := make(map[string]any, len(body)+3)
	for f, v := range body {
		gen[f] = v
	}
	gen["key"] = value
	gen["key_alias"] = k.Name
	gen["metadata"] = map[string]any{keyMetaManagedBy: keyManagedBy, keyMetaOwnerUID: string(k.UID)}

	err := c.GenerateKey(ctx, gen)
	if rejectedStatus(err) != http.StatusBadRequest {
		return err
	}
	held, derr := deleteManagedKeys(ctx, c, k.Name)
	if derr != nil {
		return derr
	}
	if held == 0 {
		return err
	}
	return c.GenerateKey(ctx, gen)
}

// deleteManagedKeys deletes the keys holding alias and reports how many it
// found. It returns errKeyAliasConflict, deleting nothing, when a holder was
// not created by this operator.
func deleteManagedKeys(ctx context.Context, c *litellm.Client, alias string) (int, error) {
	hashes, err := c.ListKeyHashesByAlias(ctx, alias)
	if err != nil {
		return 0, err
	}
	for _, h := range hashes {
		info, err := c.GetKeyInfo(ctx, h)
		if litellm.IsNotFound(err) {
			continue
		}
		if err != nil {
			return 0, err
		}
		if info.Metadata[keyMetaManagedBy] != keyManagedBy {
			return len(hashes), errKeyAliasConflict
		}
	}
	if len(hashes) == 0 {
		return 0, nil
	}
	if err := c.DeleteKeys(ctx, hashes); err != nil && !litellm.IsNotFound(err) {
		return 0, err
	}
	return len(hashes), nil
}

// finalize deletes the LiteLLM key, then the Secret. The key is found by
// alias, so this works when the Secret is already gone.
func (r *KeyReconciler) finalize(ctx context.Context, logger logr.Logger, k *litellmv1alpha1.LiteLLMKey) error {
	if !controllerutil.ContainsFinalizer(k, keyFinalizer) {
		return nil
	}
	policy := deletionpolicy.Resolve(k, k.Spec.DeletionPolicy)
	onAckMissing := newAckMissingFn(r.Recorder, k, keyKind, k.Namespace, k.Name, policy)
	snap := r.Cache.Snapshot()
	if !snap.Usable() {
		if err := onAckMissing(ackUnavailable(snap)); err != nil {
			return err
		}
	} else if _, err := deleteManagedKeys(ctx, snap.Client, k.Name); err != nil && !errors.Is(err, errKeyAliasConflict) {
		var auth401 *litellm.Auth401Error
		switch {
		case errors.As(err, &auth401):
			// No cache invalidation on the deletion path: the probe owns key health.
			if gerr := onAckMissing("401 on /key/delete", false); gerr != nil {
				return gerr
			}
		case is4xxStatus(err):
			logger.Info("deletion: deterministic 4xx on /key/delete; ack-missing", "error", err.Error())
			if gerr := onAckMissing("4xx on /key/delete: "+err.Error(), true); gerr != nil {
				return gerr
			}
		default:
			return err
		}
	}
	if err := r.deleteOwnedSecret(ctx, k); err != nil {
		return err
	}
	metrics.DeletionBlocked.Forget(keyKind, k.Namespace, k.Name)
	controllerutil.RemoveFinalizer(k, keyFinalizer)
	return r.Update(ctx, k)
}

// deleteOwnedSecret removes the Secret this CR created. A Secret that is
// gone, unreadable or owned by something else is left alone.
func (r *KeyReconciler) deleteOwnedSecret(ctx context.Context, k *litellmv1alpha1.LiteLLMKey) error {
	ref := k.Spec.SecretRef
	var s corev1.Secret
	err := r.Secrets.Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, &s)
	if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if s.Annotations[keySecretOwnerUIDAnnotation] != string(k.UID) {
		return nil
	}
	err = r.Secrets.Delete(ctx, &s, client.Preconditions{UID: &s.UID})
	if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) {
		return nil
	}
	return err
}

// renderKeyBody builds the fields the operator manages on the key. Unset
// budget and limit fields are nil so they reach LiteLLM as JSON null and
// clear a value removed from the spec.
func renderKeyBody(s *litellmv1alpha1.KeySpec) map[string]any {
	body := map[string]any{
		"models":          slices.Sorted(slices.Values(s.Models)),
		"max_budget":      nil,
		"budget_duration": nil,
		"rpm_limit":       nil,
		"tpm_limit":       nil,
	}
	if b := s.Budget; b != nil {
		if b.Limit != nil {
			body["max_budget"] = *b.Limit
		}
		if b.Period != "" {
			body["budget_duration"] = b.Period
		}
	}
	if rl := s.RateLimits; rl != nil {
		if rl.RPM != nil {
			body["rpm_limit"] = *rl.RPM
		}
		if rl.TPM != nil {
			body["tpm_limit"] = *rl.TPM
		}
	}
	return body
}

func keyRenderedHash(body map[string]any) string {
	b, _ := json.Marshal(body) // encoding/json sorts map keys
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func keyInSync(s *litellmv1alpha1.KeySpec, got *litellm.KeyInfo) bool {
	b, rl := s.Budget, s.RateLimits
	if b == nil {
		b = &litellmv1alpha1.BudgetSpec{}
	}
	if rl == nil {
		rl = &litellmv1alpha1.RateLimitsSpec{}
	}
	return slices.Equal(slices.Sorted(slices.Values(s.Models)), slices.Sorted(slices.Values(got.Models))) &&
		floatPtrEqual(b.Limit, got.MaxBudget) && b.Period == derefString(got.BudgetDuration) &&
		keyLimitEqual(rl.RPM, got.RPMLimit) && keyLimitEqual(rl.TPM, got.TPMLimit)
}

func keyLimitEqual(want *int32, got *int64) bool {
	return (want == nil && got == nil) || (want != nil && got != nil && int64(*want) == *got)
}

// setReady writes status only when it would change, so a steady-state relist
// tick costs one GET and no status write. hash is set on the Synced write.
func (r *KeyReconciler) setReady(ctx context.Context, k *litellmv1alpha1.LiteLLMKey, status metav1.ConditionStatus, reason, message, hash string) error {
	if c := apimeta.FindStatusCondition(k.Status.Conditions, conditionTypeReady); c != nil &&
		c.Status == status && c.Reason == reason && c.Message == message &&
		k.Status.ObservedGeneration == k.Generation &&
		(hash == "" || (k.Status.LastRendered != nil && k.Status.LastRendered.Hash == hash)) {
		return nil
	}
	if err := r.writeStatus(ctx, k, status, reason, message, hash); err != nil && !apierrors.IsConflict(err) {
		return err
	}
	return nil
}

func (r *KeyReconciler) writeStatus(ctx context.Context, k *litellmv1alpha1.LiteLLMKey, status metav1.ConditionStatus, reason, message, hash string) error {
	cond := buildReadyCondition(k.Generation, status, reason, message)
	gen, alias, now := k.Generation, k.Name, metav1.Now()
	var fresh litellmv1alpha1.LiteLLMKey
	err := writeStatusWithRetry(ctx, r.Client, k, &fresh, func(f *litellmv1alpha1.LiteLLMKey) {
		apimeta.SetStatusCondition(&f.Status.Conditions, cond)
		f.Status.ObservedGeneration = gen
		if hash != "" {
			f.Status.LastRendered = &litellmv1alpha1.KeyLastRenderedStatus{Hash: hash, KeyAlias: alias, At: &now}
		}
	})
	if err == nil {
		k.Status = fresh.Status
		k.ResourceVersion = fresh.ResourceVersion
	}
	recordReconcileMetric(keyKind, k.Namespace, reason)
	return err
}

func (r *KeyReconciler) connectionToKeys(ctx context.Context, obj client.Object) []reconcile.Request {
	return connectionFanIn(ctx, r.Client, obj, &litellmv1alpha1.LiteLLMKeyList{}, r.Namespace, keyKind)
}

func (r *KeyReconciler) SetupWithManager(mgr ctrl.Manager, safetyRelistCh chan reconcile.Request) error {
	b := ctrl.NewControllerManagedBy(mgr).
		For(&litellmv1alpha1.LiteLLMKey{}).
		Watches(
			&litellmv1alpha1.LiteLLMConnection{},
			handler.EnqueueRequestsFromMapFunc(r.connectionToKeys),
			builder.WithPredicates(connectionReadyTransition()),
		).
		WithOptions(transientBackoffOptions()).
		Named("key")
	if src := ConnectionRebuiltSource(r.ConnectionRebuilt, r.connectionToKeys); src != nil {
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

func ListKeyRequests(ctx context.Context, c client.Client, namespace string) ([]reconcile.Request, error) {
	var list litellmv1alpha1.LiteLLMKeyList
	if err := c.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	reqs := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
	}
	return reqs, nil
}
