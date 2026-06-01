// SPDX-License-Identifier: Apache-2.0

// modeldiscovery_controller.go implements the Pipeline B reconciler for
// `LiteLLMModelDiscovery` CRs (spec §6.3 / §7.1 / §7.5). The reconciler resolves
// the per-CR credentials from `spec.credentialsSecretRef`, dispatches to
// the provider via `internal/providers.Registry[spec.type]`, applies the
// `internal/filters.Apply` pipeline, renders one child `LiteLLMModel` per kept
// candidate via Server-Side Apply (D-06), and returns
// `ctrl.Result{RequeueAfter: spec.refresh.interval}` (D-08 — the second
// of the two periodic-kind exceptions to the REL-02 grep gate, alongside
// Phase 2's LiteLLMConnection probe).
//
// Pipeline B contract (MDISC-27):
// - Discovery NEVER imports `internal/litellm`, `internal/connection`,
// `internal/substitution`. It never calls LiteLLM and is not gated on
// `LiteLLMConnection/default`.
// - Discovery propagates `spec.params` + `spec.info` + `spec.secrets[]`
// verbatim into every generated child (MDISC-23). The only typed-field
// overlay is `spec.params.model = "<litellm-provider>/<raw-id>"` (and
// `spec.params.aws_region_name = spec.region` for Bedrock).
// - Credentials from `spec.credentialsSecretRef` are used ONLY for the
// provider-side discovery call. They are NEVER copied into the child's
// spec/metadata. The post-render canary
// `TestModelDiscovery_AC_S1_NoCredentialLeak` enforces this
// (MDISC-15 / AC-S1).
//
// Atomic refresh snapshot (D-09): a transient `Provider.List` failure
// (auth or unreachable) writes status and returns `(ctrl.Result{}, err)`
// for controller-runtime backoff — Discovery does NOT enumerate owned
// children, does NOT diff, does NOT delete anything when the source is
// unreachable. Existing children stay untouched.
//
// Plan boundaries:
// - (THIS file) lays the state machine with a normalization
// STUB at the child-naming step (lowercase + dot-join — no
// DNS-1123 validation, no Bedrock `:` → `-`). replaces
// that step with the full 5-step pipeline + `SkippedCandidates`
// (InvalidDiscoveredName) and cascade-vanish detection.
// - replaces the `AlreadyExists` handling with K8s-native
// conflict resolution (`ExplicitModelExists` / `Conflict`)
// and adoption.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	awsv2 "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
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
	"github.com/ackstorm/alitellm-operator/internal/filters"
	"github.com/ackstorm/alitellm-operator/internal/metrics"
	"github.com/ackstorm/alitellm-operator/internal/normalize"
	"github.com/ackstorm/alitellm-operator/internal/providers"
)

// Discovery-side identifiers (CONTEXT.md "Claude's Discretion" — finalizer
// name mirrors Phase 3's `models.litellm.ackstorm.ai/finalizer`).
const (
	// modelDiscoveryFinalizer is the Discovery finalizer name. Per
	// MDISC-27 + spec §7.5, this finalizer issues NO LiteLLM call — its
	// only work is waiting for owned children to drain via
	// `blockOwnerDeletion=true` cascade, then removing itself. Each
	// child LiteLLMModel's own finalizer issues POST /model/delete (see
	// model_controller.go:147-217).
	modelDiscoveryFinalizer = "modeldiscoveries.litellm.ackstorm.ai/finalizer"

	// generatedByLabel is the label every generated child LiteLLMModel carries
	// (MDISC-24). Value is the parent LiteLLMModelDiscovery's metadata.name.
	// Used as the label-selector key for child enumeration.
	generatedByLabel = "litellm.ackstorm.ai/generated-by"

	// modelDiscoveryKind is the metric label for LiteLLMModelDiscovery CRs.
	modelDiscoveryKind = "LiteLLMModelDiscovery"

	// Provider type discriminators — match internal/providers wire labels.
	// Locally re-declared (rather than imported) to avoid coupling controller
	// switch statements to providers package internals.
	providerTypeAnthropic = "anthropic"
	providerTypeGemini    = "gemini"
	providerTypeOpenAI    = "openai"
	providerTypeBedrock   = "bedrock"
	providerTypeKubeAI    = "kubeai"

	// fieldOwner is the SSA field manager identity used by Discovery on
	// every child LiteLLMModel write (D-06). Per the T-04-04-S1 mitigation in
	// the plan's threat register, this name is globally unique to
	// Discovery — Phase 3's Model controller uses regular Update on
	// `status` (different field set) and does NOT register an SSA field
	// owner, so contention with Discovery's owned fields is structurally
	// impossible.
	fieldOwner = "litellm-modeldiscovery"

	// CredentialsSecretRefField is the field-indexer path registered in
	// cmd/main.go (and suite_test.go for envtest) for reverse-mapping a
	// Secret rotation event back to the ModelDiscoveries that reference
	// it (MDISC-21 — 30s rotation propagation). Mirrors the Phase 3 D-06
	// pattern from model_controller.go:66.
	CredentialsSecretRefField = ".spec.credentialsSecretRef.name" // #nosec G101 -- field-selector JSONPath, not a credential
)

// IndexModelDiscoveryCredentialsSecretRef is the field-indexer function
// for `CredentialsSecretRefField`. Returns the Secret name referenced by
// `spec.credentialsSecretRef.name`, or nil for ModelDiscoveries that
// have no `credentialsSecretRef` (kubeai always; bedrock when relying
// on the default AWS credential chain — IRSA / env / EC2 instance
// profile / EKS Pod Identity per D-05).
//
// Exported so cmd/main.go and suite_test.go can pass it to
// `mgr.GetFieldIndexer.IndexField`. Mirrors `IndexModelSecretRefs` at
// model_controller.go:71-81 but operates on a single optional scalar
// field rather than iterating a slice.
func IndexModelDiscoveryCredentialsSecretRef(o client.Object) []string {
	md, ok := o.(*litellmv1alpha1.LiteLLMModelDiscovery)
	if !ok {
		return nil
	}
	if md.Spec.CredentialsSecretRef == nil || md.Spec.CredentialsSecretRef.Name == "" {
		return nil
	}
	return []string{md.Spec.CredentialsSecretRef.Name}
}

// +kubebuilder:rbac:groups=litellm.ackstorm.ai,resources=litellmmodeldiscoveries,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=litellm.ackstorm.ai,resources=litellmmodeldiscoveries/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=litellm.ackstorm.ai,resources=litellmmodeldiscoveries/finalizers,verbs=update
// +kubebuilder:rbac:groups=litellm.ackstorm.ai,resources=litellmmodels,verbs=get;list;watch;create;update;patch;delete

// ModelDiscoveryReconciler reconciles LiteLLMModelDiscovery CRs per spec §6.3 +
// §7.1 + CONTEXT.md D-01.D-10. The reconciler is periodic
// (`RequeueAfter: spec.refresh.interval`, MDISC-05 floor 1m enforced at
// admission) and event-driven: it `Watches` `spec.credentialsSecretRef`
// (MDISC-21 — Secret rotation propagation within 30s) and `Owns` its
// generated children (cascade-delete + adoption hooks).
//
// Pipeline B contract (MDISC-27): the reconciler does NOT import
// `internal/litellm`, `internal/connection`, or `internal/substitution`.
// It never calls LiteLLM and is not gated on `LiteLLMConnection/default`.
//
// State machine (per-reconcile, 13 steps):
//
//	Step 1: Fetch the CR. NotFound → return nil.
//	Step 2a: DeletionTimestamp set + finalizer → cascade wait (no LiteLLM
//	 call; each child's own finalizer drains LiteLLM). Remove
//	 finalizer + Update.
//	Step 2b: Finalizer absent → AddFinalizer + Update + return.
//	Step 3: (Lazy filter compile — happens in Step 7 via filters.Apply.)
//	Step 4: Resolve credentials from spec.credentialsSecretRef per
//	 spec.type. Missing Secret/key → Ready=False, reason=SecretNotFound.
//	Step 5: Build ProviderConfig + dispatch via providers.Registry. Bad
//	 config → Ready=False, reason=InvalidConfig.
//	Step 6: Call provider.List. Auth error → SourceReachable=False,
//	 reason=AuthFailed + return err. Other error → reason=Unreachable
//	 + return err. NEVER enumerate/delete children on error (D-09
//	 atomic refresh snapshot).
//	Step 7: Apply filters.Apply. UpstreamInvalidError → Ready=False,
//	 reason=UpstreamInvalid (return nil — deterministic, no backoff).
//	 InvalidConfigError → Ready=False, reason=InvalidConfig (return nil).
//	Step 8: Per kept candidate, derive child name via the 5-step
//	 pipeline + DNS-1123 validation.
//	Step 9: Render desired LiteLLMModel object (TypeMeta + ObjectMeta with
//	 ownerRef/label/finalizer + Spec with overlay-merged Params).
//	Step 10: SSA-apply via client.Patch(ctx, ., client.Apply,
//	 client.FieldOwner(fieldOwner), client.ForceOwnership).
//	 Classify outcome per OBS-04: error result label on
//	 apierrors.IsServerTimeout/IsTooManyRequests/IsServiceUnavailable/IsConflict.
//	 AlreadyExists DEFERRED to (treated as ChildCRWriteFailed here).
//	Step 11: Update status (generatedChildren, generatedCount, discoveredCount,
//	 failedCandidates, lastRefreshAt, conditions, observedGeneration).
//	Step 12: Increment DiscoveryGeneratedCount (gauge) +
//	 DiscoveryRefreshTotal{result=success}.
//	Step 13: Return ctrl.Result{RequeueAfter: spec.refresh.interval}.
//
// Anti-patterns avoided (CONTEXT.md "Anti-Patterns"):
// - NO connection cache field (MDISC-27).
// - NO LiteLLM call from any path (MDISC-27).
// - NO branching on spec.type for provider dispatch outside the
// credential-resolution step (D-01 — providers.Registry is the ONLY
// dispatch path).
// - NO drift-corrected counter increments (those belong to the child
// LiteLLMModel controller — model_controller.go:171, 197).
// - NO global LiteLLM enumeration (OWN-01 — vanish detection is
// K8s-native via label selector, lands in).
type ModelDiscoveryReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	HTTPClient *http.Client
	Recorder   record.EventRecorder
	Namespace  string
	Log        logr.Logger
	// BootEvents (FIX2.txt H-2) — optional BootSweeper channel. nil-safe.
	BootEvents <-chan event.GenericEvent

	// cascadeDrainLog tracks per-CR drain progress so the
	// "cascade-delete: waiting for children to drain" line is emitted
	// at INFO only when the remaining count changes or the wait
	// exceeds modelDiscoveryCascadeDrainDeadline. All other reconciles
	// log at V(2). FIX3.txt LOW-3 — prevents one line every 5s during
	// a hung delete.
	cascadeDrainLogMu sync.Mutex
	cascadeDrainLog   map[string]modelDiscoveryCascadeDrainState
}

// modelDiscoveryCascadeDrainState carries per-CR throttle state for the
// "waiting for children to drain" log line. See cascadeDrainLog.
type modelDiscoveryCascadeDrainState struct {
	lastRemaining int
	startedAt     time.Time
	lastWarnAt    time.Time
}

// modelDiscoveryCascadeDrainDeadline is the elapsed-time threshold
// after which the drain-wait log line is escalated to WARN with a
// hint to check finalizer state on the children.
const modelDiscoveryCascadeDrainDeadline = 5 * time.Minute

// logCascadeDrain emits the "cascade-delete: waiting for children to
// drain" line with FIX3.txt LOW-3 throttling: INFO only when the
// remaining count changes from the last observation OR when the wait
// has exceeded modelDiscoveryCascadeDrainDeadline (then WARN). All
// other reconciles log at V(2). Per-CR state lives in
// r.cascadeDrainLog.
func (r *ModelDiscoveryReconciler) logCascadeDrain(_ context.Context, logger logr.Logger, name string, remaining int) (overdue bool) {
	r.cascadeDrainLogMu.Lock()
	defer r.cascadeDrainLogMu.Unlock()
	if r.cascadeDrainLog == nil {
		r.cascadeDrainLog = map[string]modelDiscoveryCascadeDrainState{}
	}
	now := time.Now()
	prev, ok := r.cascadeDrainLog[name]
	if !ok {
		prev = modelDiscoveryCascadeDrainState{lastRemaining: -1, startedAt: now}
	}
	changed := prev.lastRemaining != remaining
	overdue = now.Sub(prev.startedAt) >= modelDiscoveryCascadeDrainDeadline &&
		now.Sub(prev.lastWarnAt) >= modelDiscoveryCascadeDrainDeadline
	switch {
	case overdue:
		logger.Info("cascade-delete: still draining past deadline; check finalizer state on children",
			"remaining", remaining,
			"elapsed", now.Sub(prev.startedAt).Round(time.Second).String())
		prev.lastWarnAt = now
		metrics.CascadeDrainOverdueTotal.WithLabelValues(modelDiscoveryKind).Inc()
	case changed:
		logger.Info("cascade-delete: waiting for children to drain", "remaining", remaining)
	default:
		logger.V(2).Info("cascade-delete: waiting for children to drain", "remaining", remaining)
	}
	prev.lastRemaining = remaining
	r.cascadeDrainLog[name] = prev
	return overdue
}

// forgetCascadeDrain clears the per-CR drain-log throttle state after
// the parent's finalizer is removed (drain complete). Prevents
// monotonic growth of r.cascadeDrainLog over the operator's lifetime.
func (r *ModelDiscoveryReconciler) forgetCascadeDrain(name string) {
	r.cascadeDrainLogMu.Lock()
	defer r.cascadeDrainLogMu.Unlock()
	delete(r.cascadeDrainLog, name)
}

// Reconcile implements the 13-step state machine. See package doc above.
//
//nolint:gocyclo // Linear state machine — splitting into helpers obscures the §6.3 mapping.
func (r *ModelDiscoveryReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("modeldiscovery", req.NamespacedName)

	// ─── Step 1: Fetch the CR ──────────────────────────────────────────────
	var md litellmv1alpha1.LiteLLMModelDiscovery
	if err := r.Get(ctx, req.NamespacedName, &md); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// ─── Step 2a: Deletion path — cascade-delete drain wait (MDISC-28) ─────
	// Discovery's finalizer issues NO LiteLLM call (MDISC-27 + PATTERNS.md
	// line 1023 anti-pattern). The K8s garbage collector cascade-deletes
	// owned children via blockOwnerDeletion=true; each child LiteLLMModel's own
	// finalizer issues POST /model/delete against LiteLLM (Phase 3
	// model_controller.go:147-217). The reconciler MUST wait for all
	// owned children to drain before removing its own finalizer — without
	// this drain, the parent would disappear while the children's
	// finalizers are still running, and K8s GC would orphan-leak the
	// children's POST /model/delete work.
	if !md.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&md, modelDiscoveryFinalizer) {
			// List owned children via the same label selector vanish
			// detection uses (litellm.ackstorm.ai/generated-by=<this>).
			// If any remain, requeue and wait for K8s GC to finish.
			var owned litellmv1alpha1.LiteLLMModelList
			if err := r.List(ctx, &owned,
				client.InNamespace(r.Namespace),
				client.MatchingLabels{generatedByLabel: md.Name},
			); err != nil {
				return ctrl.Result{}, err
			}
			if len(owned.Items) > 0 {
				// FIX3.txt HIGH-1: K8s GC cannot propagate the parent
				// delete to children while the parent's finalizer is
				// pending (deadlock: GC waits for finalizer, finalizer
				// waits for GC). The reconciler MUST issue an explicit
				// Delete against every child that has not yet entered
				// its own deletion path. Children already being deleted
				// (DeletionTimestamp set) are skipped — their own
				// finalizer drives the LiteLLM DELETE.
				for i := range owned.Items {
					child := &owned.Items[i]
					if !child.DeletionTimestamp.IsZero() {
						continue
					}
					if err := r.Delete(ctx, child); err != nil && !apierrors.IsNotFound(err) {
						return ctrl.Result{}, err
					}
				}
				if r.logCascadeDrain(ctx, logger, md.Name, len(owned.Items)) && r.Recorder != nil {
					r.Recorder.Eventf(&md, corev1.EventTypeWarning, "CascadeDrainOverdue",
						"cascade-delete still draining %d child Model(s) past deadline; check finalizer state on the children",
						len(owned.Items))
				}
				return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
			}
			// All children drained. Discovery's finalizer issues NO
			// LiteLLM call — just remove the finalizer.
			// OBS-03: drop the cr_status_age_seconds label before the CR is gone (T-07-01-01).
			metrics.CRStatusAgeTracker.Forget(modelDiscoveryKind, md.Name)
			// FIX3.txt LOW-3: drop drain-log throttle state.
			r.forgetCascadeDrain(md.Name)
			controllerutil.RemoveFinalizer(&md, modelDiscoveryFinalizer)
			if err := r.Update(ctx, &md); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// ─── Step 2b: Finalizer add path ───────────────────────────────────────
	if !controllerutil.ContainsFinalizer(&md, modelDiscoveryFinalizer) {
		controllerutil.AddFinalizer(&md, modelDiscoveryFinalizer)
		if err := r.Update(ctx, &md); err != nil {
			return ctrl.Result{}, err
		}
		// FIX10 (v0.4.4): explicit requeue — discoverySpecChanged
		// predicate filters metadata-only Update events. See
		// mcpserverdiscovery_controller.go finalizer-add note.
		return ctrl.Result{Requeue: true}, nil
	}

	// ─── Step 4: Resolve credentials per spec.type ─────────────────────────
	// The `switch md.Spec.Type` here is NOT a D-01 violation: D-01 forbids
	// per-type branching for PROVIDER dispatch outside the registry; the
	// per-type Secret keys (ANTHROPIC_API_KEY, AWS_ACCESS_KEY_ID, .) are
	// fixed by spec §6.3 line 721-737 normative and force a small switch
	// HERE. The actual provider dispatch below uses providers.Registry.
	// M-SEC1: deny cloud-metadata / loopback / link-local baseUrl before any
	// outbound call. Terminal config error — no backoff storm; a CR edit
	// re-enqueues. Denylist only (private + *.svc still reachable by design;
	// see providers.ValidateBaseURL).
	if md.Spec.BaseURL != "" {
		if err := providers.ValidateBaseURL(md.Spec.BaseURL); err != nil {
			metrics.DiscoveryRefreshTotal.WithLabelValues(modelDiscoveryKind, md.Spec.Type, "error").Inc()
			return r.writeReady(ctx, &md, metav1.ConditionFalse, "InvalidConfig", err.Error()), nil
		}
	}

	cfg := providers.ProviderConfig{
		Type:       md.Spec.Type,
		BaseURL:    md.Spec.BaseURL,
		Region:     md.Spec.Region,
		HTTPClient: r.HTTPClient,
	}
	switch md.Spec.Type {
	case providerTypeAnthropic:
		key, err := r.resolveStringKey(ctx, md.Namespace, md.Spec.CredentialsSecretRef, "ANTHROPIC_API_KEY")
		if err != nil {
			res := r.writeReady(ctx, &md, metav1.ConditionFalse, reasonSecretNotFound, err.Error())
			res.RequeueAfter = connection.DefaultRequeueOnRejectedAfter
			return res, nil
		}
		cfg.APIKey = key
	case providerTypeGemini:
		key, err := r.resolveStringKey(ctx, md.Namespace, md.Spec.CredentialsSecretRef, "GEMINI_API_KEY")
		if err != nil {
			res := r.writeReady(ctx, &md, metav1.ConditionFalse, reasonSecretNotFound, err.Error())
			res.RequeueAfter = connection.DefaultRequeueOnRejectedAfter
			return res, nil
		}
		cfg.APIKey = key
	case providerTypeOpenAI:
		key, err := r.resolveStringKey(ctx, md.Namespace, md.Spec.CredentialsSecretRef, "OPENAI_API_KEY")
		if err != nil {
			res := r.writeReady(ctx, &md, metav1.ConditionFalse, reasonSecretNotFound, err.Error())
			res.RequeueAfter = connection.DefaultRequeueOnRejectedAfter
			return res, nil
		}
		cfg.APIKey = key
	case providerTypeBedrock:
		// Bedrock without credentialsSecretRef → leave AWSCreds nil so the
		// SDK falls through to the default credential chain per D-05.
		if md.Spec.CredentialsSecretRef != nil && md.Spec.CredentialsSecretRef.Name != "" {
			creds, err := r.resolveAWSCredentials(ctx, md.Namespace, md.Spec.CredentialsSecretRef)
			if err != nil {
				res := r.writeReady(ctx, &md, metav1.ConditionFalse, reasonSecretNotFound, err.Error())
				res.RequeueAfter = connection.DefaultRequeueOnRejectedAfter
				return res, nil
			}
			cfg.AWSCreds = creds
		}
	case providerTypeKubeAI:
		// kubeai has no credentialsSecretRef per spec §6.3 line 792 (CEL-forbidden).
	default:
		// Should be impossible — CRD CEL enforces enum at admission. Defensive.
		return r.writeReady(ctx, &md, metav1.ConditionFalse, "InvalidConfig",
			fmt.Sprintf("unknown provider type %q", md.Spec.Type)), nil
	}

	// ─── Step 5: Dispatch provider constructor via the registry (D-01) ─────
	constructor, ok := providers.Lookup(md.Spec.Type)
	if !ok {
		// Same defensive arm as the default above — admission CEL should
		// have caught this. Surface as InvalidConfig.
		return r.writeReady(ctx, &md, metav1.ConditionFalse, "InvalidConfig",
			fmt.Sprintf("no provider constructor registered for type %q", md.Spec.Type)), nil
	}
	provider, err := constructor(ctx, cfg)
	if err != nil {
		// Constructor errors are operator-side spec-shape issues (missing
		// APIKey for anthropic, missing BaseURL for kubeai, .). CEL
		// should have caught most; this is the defensive backstop.
		return r.writeReady(ctx, &md, metav1.ConditionFalse, "InvalidConfig", err.Error()), nil
	}

	// ─── Step 6: Provider.List with D-09 atomic refresh snapshot ───────────
	candidates, listErr := provider.List(ctx)
	if listErr != nil {
		// Classify and write status; DO NOT enumerate/delete children
		// (D-09 atomic refresh snapshot). Return err so controller-runtime
		// backoff retries; cadence resumes from RequeueAfter on the first
		// successful reconcile after recovery.
		var authErr *providers.ProviderAuthError
		if errors.As(listErr, &authErr) {
			r.writeBothConditions(ctx, &md,
				metav1.ConditionFalse, "SourceUnreachable", listErr.Error(),
				metav1.ConditionFalse, "AuthFailed", listErr.Error())
		} else {
			r.writeBothConditions(ctx, &md,
				metav1.ConditionFalse, "SourceUnreachable", listErr.Error(),
				metav1.ConditionFalse, "Unreachable", listErr.Error())
		}
		metrics.DiscoveryRefreshTotal.WithLabelValues(modelDiscoveryKind, md.Spec.Type, "error").Inc()
		logger.V(1).Info("provider.List failed; returning err for backoff (D-09 atomic refresh snapshot)",
			"type", md.Spec.Type, "error", listErr)
		return ctrl.Result{}, listErr
	}

	// ─── Step 7: Apply filters.Apply (lazy regex compile) ──────────────────
	candidateIDs := make([]string, 0, len(candidates))
	for _, c := range candidates {
		candidateIDs = append(candidateIDs, c.ID)
	}
	kept, filterErr := filters.Apply(candidateIDs, md.Spec.Filters)
	if filterErr != nil {
		var upstreamInvalid *filters.UpstreamInvalidError
		var invalidConfig *filters.InvalidConfigError
		switch {
		case errors.As(filterErr, &upstreamInvalid):
			return r.writeReady(ctx, &md, metav1.ConditionFalse, "UpstreamInvalid", upstreamInvalid.Error()), nil
		case errors.As(filterErr, &invalidConfig):
			return r.writeReady(ctx, &md, metav1.ConditionFalse, "InvalidConfig", invalidConfig.Error()), nil
		default:
			// Unknown error type from filters.Apply — should be impossible.
			// Treat as a controller bug; return err for backoff.
			return ctrl.Result{}, filterErr
		}
	}

	// ─── Step 8: Derive child names ─────────────
	// 5-step normalization (internal/normalize) + DNS-1123 validation.
	// Names that fail DNS-1123 land in status.skippedCandidates[reason=
	// InvalidDiscoveredName] with message `<rawID> -> <fullName>` per
	// spec §6.3 line 762 (MDISC-11). The K8s-name form is for the child
	// CR's metadata.name ONLY — the raw provider ID is preserved verbatim
	// in child.spec.params.model via buildChildModel (MDISC-10).
	prefix := md.Spec.Prefix
	if prefix == "" {
		prefix = strings.ToLower(md.Spec.Type)
	}
	litellmProvider := md.Spec.Type
	if litellmProvider == "kubeai" {
		// Per spec §6.3 line 792: kubeai's litellm-provider mapping is
		// "hosted_vllm" (not "kubeai"). All other types map verbatim.
		litellmProvider = "hosted_vllm"
	}

	// ─── Steps 8.5: Enumerate existing children + adoption recognition (04-06) ────
	// Single label-selector list reused for BOTH adoption-recognition AND
	// vanish detection (Step 11) — avoids a double API round-trip per
	// reconcile. The list catches the post-render snapshot of every child
	// LiteLLMModel that ever carried the generated-by=<this> label (including
	// adopted children whose controller ownerRef has been stripped — the
	// label stays even when the ownerRef goes).
	//
	// Per spec §6.3 line 798-808 (adoption + release semantics):
	// - A user stripping the controller ownerRef on a generated child is
	// the spec-defined adoption mechanism (MDISC-25). The Discovery
	// reconciler MUST recognize the strip on the very next reconcile
	// and STOP managing the child: no SSA re-apply (don't claw back
	// ownership), no vanish-delete (don't destroy user-adopted intent).
	// - The label persists across ownerRef strips (the user PATCHing the
	// ownerRef field does not touch labels). The label is therefore
	// the right enumeration key for "children this Discovery ever
	// wrote, alive or adopted".
	//
	// The classification:
	// ownedByThisDiscovery == true → keep managing (SSA-applied + vanish-eligible)
	// ownedByThisDiscovery == false → adopted (skipped + EXCLUDED from SSA + EXCLUDED from vanish)
	var existingChildren litellmv1alpha1.LiteLLMModelList
	if err := r.List(ctx, &existingChildren,
		client.InNamespace(r.Namespace),
		client.MatchingLabels{generatedByLabel: md.Name},
	); err != nil {
		return ctrl.Result{}, fmt.Errorf("list label-matched children for adoption + vanish: %w", err)
	}

	adoptedNames := make(map[string]struct{})
	skipped := make([]litellmv1alpha1.SkippedCandidate, 0, len(existingChildren.Items))
	generated := make([]string, 0, len(existingChildren.Items))
	var failed []litellmv1alpha1.FailedCandidate

	for i := range existingChildren.Items {
		child := &existingChildren.Items[i]
		if ownedByThisDiscovery(child, &md) {
			continue
		}
		// Ownership stripped by user (or never set — adoption recognized).
		// Record as ExplicitModelExists per spec §6.3 line 800 and exclude
		// from the SSA + vanish sets. ownedBy is the child's own name —
		// the adopted child is now "explicit".
		adoptedNames[child.Name] = struct{}{}
		skipped = append(skipped, litellmv1alpha1.SkippedCandidate{
			Name:    child.Name,
			Reason:  "ExplicitModelExists",
			OwnedBy: child.Name,
			Message: "ownerRef stripped; child adopted by user",
		})
		logger.V(1).Info("adoption recognized: child no longer owned by this Discovery",
			"child", child.Name)
	}

	// ─── Steps 9 + 10: Render + SSA-apply child Models ─────────────────────
	for _, rawID := range kept {
		// 5-step pipeline (MDISC-10) → DNS-1123 subdomain validation
		// on the FULL `<prefix>.<normalized>` name (MDISC-11). The
		// validator catches: empty result from Normalize, total > 253
		// chars, invalid chars, leading/trailing non-alnum. Rejected
		// names DO NOT cause a child CR write — they land in
		// status.skippedCandidates[].
		normalized := normalize.Normalize(rawID)
		childName := prefix + "." + normalized
		// Adoption short-circuit (04-06): if this candidate's name belongs
		// to a child whose ownerRef the user already stripped, the candidate
		// is recorded as ExplicitModelExists above (in the adoption-recognition
		// scan) and MUST NOT trigger a fresh SSA write here. Skipping the
		// SSA loop preserves the spec's "Discovery NEVER mutates an
		// adopted child" invariant.
		if _, adopted := adoptedNames[childName]; adopted {
			continue
		}
		if err := normalize.DNS1123Subdomain(childName); err != nil {
			// spec §6.3 line 762 message format: `<original-id> -> <full-name>`
			skipped = append(skipped, litellmv1alpha1.SkippedCandidate{
				Name:    childName,
				Reason:  "InvalidDiscoveredName",
				Message: rawID + " -> " + childName,
			})
			logger.V(1).Info("skipping candidate (InvalidDiscoveredName)",
				"rawID", rawID, "childName", childName, "error", err)
			continue
		}

		child, buildErr := buildChildModel(&md, childName, rawID, litellmProvider, r.Namespace)
		if buildErr != nil {
			// Build-side errors are deterministic — surface as ChildCRWriteFailed.
			metrics.ChildCRWritesTotal.WithLabelValues(modelDiscoveryKind, "create", "error").Inc()
			metrics.DiscoveryFailedTotal.WithLabelValues(modelDiscoveryKind, "ChildCRWriteFailed").Inc()
			failed = append(failed, litellmv1alpha1.FailedCandidate{
				Name:    childName,
				Reason:  "ChildCRWriteFailed",
				Message: buildErr.Error(),
			})
			continue
		}

		// OBS-04 action classification: we cannot trivially split create
		// vs update without an extra Get; use status.generatedChildren
		// as the source of truth for "have we written this name before
		// from this Discovery?". On the first reconcile, no name is in
		// the list, so action="create"; subsequent reconciles see the
		// name and report action="update".
		wasNew := !slices.Contains(md.Status.GeneratedChildren, childName)
		action := "update"
		if wasNew {
			action = "create"
		}

		// K8s-native conflict pre-check (04-06): SSA + ForceOwnership
		// with the same field-manager name across two Discoveries does
		// NOT trigger AlreadyExists — the apiserver simply merges (the
		// field-manager identity is the same from K8s's point of view).
		// To honor spec §6.3 line 798-808's classification table, we Get
		// the colliding object BEFORE attempting Patch:
		// - No existing object → Patch as normal (happy path).
		// - Owned by us (UID) → Patch (idempotent re-apply).
		// - Owned by another MD → Conflict skip (MDISC-13).
		// - No controller owner → ExplicitModelExists skip (MDISC-14).
		// The extra Get is cheap (single named lookup, served from the
		// controller-runtime cache after the first reconcile) and is the
		// only reliable way to detect cross-Discovery collisions when SSA
		// is the write path. AlreadyExists from Patch is still handled
		// below as a defensive backstop (race window between Get and Patch).
		classifiedSkip, retryable, classifyErr := r.classifyAlreadyExists(ctx, childName, &md)
		if classifyErr != nil {
			// Get failed with a non-NotFound apiserver error → ChildCRWriteFailed.
			metrics.ChildCRWritesTotal.WithLabelValues(modelDiscoveryKind, action, "error").Inc()
			metrics.DiscoveryFailedTotal.WithLabelValues(modelDiscoveryKind, "ChildCRWriteFailed").Inc()
			failed = append(failed, litellmv1alpha1.FailedCandidate{
				Name:    childName,
				Reason:  "ChildCRWriteFailed",
				Message: sanitizeError(classifyErr),
			})
			continue
		}
		// retryable == true means: NotFound (no existing object — proceed to
		// Patch happy path) OR owned-by-this-Discovery (idempotent re-apply).
		// classifiedSkip != nil means: collision with a non-us owner → record
		// the skip and SKIP the Patch entirely.
		if classifiedSkip != nil {
			metrics.ChildCRWritesTotal.WithLabelValues(modelDiscoveryKind, action, "conflict").Inc()
			skipped = append(skipped, *classifiedSkip)
			logger.V(1).Info("pre-Patch K8s-native conflict classified",
				"child", childName, "reason", classifiedSkip.Reason,
				"ownedBy", classifiedSkip.OwnedBy)
			continue
		}
		_ = retryable // retryable here means "safe to Patch" — variable used for documentation.

		applyErr := r.Patch(ctx, child, client.Apply,
			client.FieldOwner(fieldOwner),
			client.ForceOwnership)
		if applyErr != nil {
			// K8s-native conflict resolution (04-06): when SSA's CREATE
			// returns AlreadyExists, we Get the colliding object and
			// classify per spec §6.3 line 798-808:
			//
			// - No controller ownerRef → ExplicitModelExists
			// (user-authored LiteLLMModel)
			// - Controller ownerRef points at a → Conflict
			// DIFFERENT LiteLLMModelDiscovery (cross-Discovery race)
			// - Controller ownerRef points at → retryable transient
			// THIS Discovery (UID match) (SSA's ForceOwnership
			// should have won — log
			// and let next reconcile
			// retry)
			// - Get returns NotFound (raced delete) → retryable transient
			//
			// AlreadyExists is the dominant production path here because
			// SSA + ForceOwnership normally overwrites contended fields
			// transparently — pure AlreadyExists fires only when the
			// existing object's controller ownerRef belongs to a
			// DIFFERENT controller (T-04-04-S1: cross-controller field
			// ownership is intentionally non-overrideable by ForceOwnership).
			if apierrors.IsAlreadyExists(applyErr) {
				classifiedSkip, retryable, classifyErr := r.classifyAlreadyExists(ctx, childName, &md)
				if classifyErr != nil {
					// Get itself failed for a non-NotFound reason — surface
					// as ChildCRWriteFailed (apiserver issue).
					metrics.ChildCRWritesTotal.WithLabelValues(modelDiscoveryKind, action, "error").Inc()
					metrics.DiscoveryFailedTotal.WithLabelValues(modelDiscoveryKind, "ChildCRWriteFailed").Inc()
					failed = append(failed, litellmv1alpha1.FailedCandidate{
						Name:    childName,
						Reason:  "ChildCRWriteFailed",
						Message: sanitizeError(classifyErr),
					})
					continue
				}
				if retryable {
					// Either Get returned NotFound (the AlreadyExists raced
					// with a Delete) or the existing child is in fact owned
					// by THIS Discovery (a transient apiserver/cache race
					// where SSA's ForceOwnership should have won). Both
					// resolve on the next reconcile. Result label: count
					// as a conflict so OBS-04 surfaces the contention.
					metrics.ChildCRWritesTotal.WithLabelValues(modelDiscoveryKind, action, "conflict").Inc()
					logger.V(1).Info("AlreadyExists retry-soon",
						"child", childName, "reason", "transient race")
					continue
				}
				if classifiedSkip != nil {
					// ExplicitModelExists or Conflict — the child
					// is NOT ours to write. Record in skipped[] (per
					// spec §6.3 line 870 enum) and move on. No metric for
					// SSA success/error: the Patch never landed.
					metrics.ChildCRWritesTotal.WithLabelValues(modelDiscoveryKind, action, "conflict").Inc()
					skipped = append(skipped, *classifiedSkip)
					logger.V(1).Info("SSA AlreadyExists classified",
						"child", childName, "reason", classifiedSkip.Reason,
						"ownedBy", classifiedSkip.OwnedBy)
					continue
				}
				// Defensive: should be unreachable — classifyAlreadyExists
				// returns (nil, false, nil) in no documented path. Fall
				// through to ChildCRWriteFailed.
			}
			// OBS-04 result=error classification per CONTEXT.md
			// `<specifics>` line 283: server-side transient OR conflict
			// → error. Other apierrors (NotFound on a Patch—admission
			// bug; Forbidden—RBAC bug) flow here too and are surfaced
			// as ChildCRWriteFailed with the apiserver's error string.
			_ = transientApierror(applyErr) // documented seam — the result label is "error" for ALL non-AlreadyExists apply errors
			metrics.ChildCRWritesTotal.WithLabelValues(modelDiscoveryKind, action, "error").Inc()
			metrics.DiscoveryFailedTotal.WithLabelValues(modelDiscoveryKind, "ChildCRWriteFailed").Inc()
			failed = append(failed, litellmv1alpha1.FailedCandidate{
				Name:    childName,
				Reason:  "ChildCRWriteFailed",
				Message: sanitizeError(applyErr),
			})
			continue
		}
		metrics.ChildCRWritesTotal.WithLabelValues(modelDiscoveryKind, action, "success").Inc()
		generated = append(generated, childName)
	}

	sort.Strings(generated)
	sort.Slice(failed, func(i, j int) bool { return failed[i].Name < failed[j].Name })
	sort.Slice(skipped, func(i, j int) bool { return skipped[i].Name < skipped[j].Name })

	// ─── Step 11: Vanish detection (D-09 atomic refresh) ───────────────────
	// Per CONTEXT.md D-09 line 94-101: this step runs ONLY on the
	// post-provider-success path. The reconciler reaches here ONLY when
	// provider.List succeeded AND filter resolution succeeded — any
	// earlier error path returned via `return ctrl.Result{}, listErr` (or
	// the deterministic `return r.writeReady(.), nil`) above. Thus
	// existing children stay UNTOUCHED on transient failures by construction.
	//
	// Vanish-detection contract (MDISC-20 + PATTERNS.md line 1025):
	// - Enumerate owned children via label selector
	// `litellm.ackstorm.ai/generated-by=<this>` (NOT a LiteLLM GET).
	// - T-04-05-T1 mitigation: verify ownerReferences[0].UID == md.UID
	// before issuing Delete. A user-stripped ownerRef (adoption path
	// coming in) means the child is no longer Discovery's
	// to delete — skip it conservatively.
	// - Children NOT in `desiredSet` (post-skip generated + skipped
	// names) get Delete'd. Skipped names are STILL claimed by this
	// Discovery — they shouldn't be vanished just because the
	// reconciler couldn't successfully render them.
	// - The deleted child's own finalizer issues POST /model/delete on
	// LiteLLM (model_controller.go:171/197) AND increments the drift
	// counter (domain=model, action=delete_vanished). Discovery NEVER
	// touches that counter (PATTERNS.md line 1037 + the acceptance-grep
	// fence — this file deliberately contains zero references to the
	// §10 drift counter name).
	desiredSet := make(map[string]struct{}, len(generated)+len(skipped))
	for _, n := range generated {
		desiredSet[n] = struct{}{}
	}
	for _, s := range skipped {
		desiredSet[s.Name] = struct{}{}
	}

	// Reuse `existingChildren` listed in Step 8.5 above (single
	// label-selector API call per reconcile). Adoption-recognition already
	// recorded the adopted-set in `adoptedNames` and added them to
	// `skipped[]`; the vanish loop MUST exclude adopted children from
	// Delete so user-adopted intent is preserved (MDISC-25 + spec §6.3
	// line 801).
	for i := range existingChildren.Items {
		child := &existingChildren.Items[i]
		if _, keep := desiredSet[child.Name]; keep {
			continue
		}
		if _, adopted := adoptedNames[child.Name]; adopted {
			// Don't vanish-delete an adopted child — the user owns it now.
			continue
		}
		// T-04-05-T1 defense: verify the child is genuinely owned by THIS
		// Discovery before deleting. A user-mutated ownerRef (adoption
		// path) is already caught by the adoptedNames map above, but the
		// defensive UID match is retained as a belt-and-suspenders guard
		// against label-only collisions (a malicious user could plant the
		// label on an unrelated CR — UID check rejects that case).
		if !ownedByDiscovery(child, md.UID) {
			logger.V(1).Info("skipping vanish-delete: child no longer owned by this Discovery (ownerRef stripped)",
				"child", child.Name)
			continue
		}
		if err := r.Delete(ctx, child); err != nil && !apierrors.IsNotFound(err) {
			metrics.ChildCRWritesTotal.WithLabelValues(modelDiscoveryKind, "delete", "error").Inc()
			logger.Error(err, "delete vanished child", "child", child.Name)
			continue
		}
		metrics.ChildCRWritesTotal.WithLabelValues(modelDiscoveryKind, "delete", "success").Inc()
		logger.V(1).Info("vanish-deleted child", "child", child.Name)
	}

	// ─── Step 12: Update status ────────────────────────────────────────────
	now := metav1.NewTime(time.Now())
	md.Status.GeneratedChildren = generated
	// int32 casts are safe — kept-set sizes are bounded by provider list
	// length (Anthropic ≤30, Gemini ≤100, OpenAI ≤200, Bedrock ≤200,
	// kubeai/vLLM ≤50), all well under 2^31.
	md.Status.GeneratedCount = int32(len(generated)) //nolint:gosec // see comment above
	md.Status.DiscoveredCount = int32(len(kept))     //nolint:gosec // see comment above
	md.Status.SkippedCandidates = skipped            // MDISC-11 InvalidDiscoveredName entries
	md.Status.FailedCandidates = failed
	md.Status.LastRefreshAt = &now
	md.Status.ObservedGeneration = md.Generation

	// Invariant per spec §6.3 line 875:
	// discoveredCount == generatedCount + len(skippedCandidates) + len(failedCandidates)
	// Locked in code (log-on-violation, no panic in prod). Tests assert
	// the same invariant by inspecting status.
	if want := len(generated) + len(skipped) + len(failed); int(md.Status.DiscoveredCount) != want {
		logger.Error(nil, "invariant violation: discoveredCount != generatedCount + skipped + failed",
			"DiscoveredCount", md.Status.DiscoveredCount,
			"GeneratedCount", md.Status.GeneratedCount,
			"SkippedCandidates", len(skipped),
			"FailedCandidates", len(failed),
			"computed", want)
	}

	// Set both Ready and SourceReachable conditions to success values.
	readyStatus := metav1.ConditionTrue
	readyReason := reasonSynced
	readyMsg := fmt.Sprintf("%d/%d children generated", len(generated), len(kept))
	if len(failed) > 0 {
		// Partial-failure: Discovery is not fully Ready, but the
		// SourceReachable condition is still True (we did reach the
		// provider and got a list — the K8s-side writes are what failed).
		readyStatus = metav1.ConditionFalse
		readyReason = "ChildCRWriteFailed"
		readyMsg = fmt.Sprintf("%d/%d children failed to write to apiserver", len(failed), len(kept))
	}
	if err := r.writeBothConditionsObj(ctx, &md,
		readyStatus, readyReason, readyMsg,
		metav1.ConditionTrue, "Ok", ""); err != nil {
		logStatusUpdateErr(logger, err)
		if apierrors.IsConflict(err) {
			// Conflict (RV bump, CR deleted, UID precondition) — informer
			// re-enqueues with fresh state; suppress controller-runtime's
			// ERROR "Reconciler error" log + backoff for this error class.
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// ─── Step 12: Metrics on success ───────────────────────────────────────
	metrics.DiscoveryGeneratedCount.WithLabelValues(modelDiscoveryKind, md.Spec.Type).Set(float64(len(generated)))
	metrics.DiscoveryRefreshTotal.WithLabelValues(modelDiscoveryKind, md.Spec.Type, "success").Inc()
	metrics.ReconcileTotal.WithLabelValues(modelDiscoveryKind, "success").Inc()
	metrics.CRStatusAgeTracker.RecordSuccess(modelDiscoveryKind, md.Name)

	// ─── Step 13: Return RequeueAfter (D-08 — the second of two REL-02 exceptions) ─
	logger.V(1).Info("reconciled",
		"type", md.Spec.Type,
		"discovered", md.Status.DiscoveredCount,
		"generated", md.Status.GeneratedCount,
		"failed", len(failed),
		"requeueAfter", md.Spec.Refresh.Interval.Duration)
	return ctrl.Result{RequeueAfter: md.Spec.Refresh.Interval.Duration}, nil
}

// transientApierror classifies the OBS-04 result label per CONTEXT.md
// `<specifics>` line 283. Currently the reconciler emits result="error"
// for ALL apply failures (including AlreadyExists, which will
// promote to K8s-native conflict resolution). The classifier is retained
// here as documentation of the future split:
//
//	transient apierrors → result=error (transient/retryable)
//	non-transient → result=error (deterministic — write-blocked)
//
// Both cases currently bucket under "error". introduces a
// "conflict" label value (or reuses "error" with an additional reason
// field on FailedCandidate) — TBD by that plan's design.
func transientApierror(err error) bool {
	return apierrors.IsServerTimeout(err) ||
		apierrors.IsTooManyRequests(err) ||
		apierrors.IsServiceUnavailable(err) ||
		apierrors.IsConflict(err)
}

// buildChildModel constructs the desired child LiteLLMModel object that will be
// applied via SSA. The returned object carries:
// - TypeMeta (required for SSA — apiserver rejects SSA patches
// without apiVersion+kind on the object).
// - ObjectMeta.{Name, Namespace, Labels, OwnerReferences, Finalizers}
// per MDISC-24.
// - Spec.Params built via the "empty-safe overlay" pattern: if
// md.Spec.Params is absent/empty, start with an empty map; else
// decode the user's bag; then overlay the operator's typed fields
// (model = "<litellm-provider>/<rawID>", optionally aws_region_name
// for Bedrock); re-marshal.
// - Spec.Info / Spec.Secrets propagated VERBATIM (MDISC-23).
//
// MDISC-15 / AC-S1: the function does NOT take any credential parameter.
// Credentials live in the reconciler's local cfg variable; they are
// passed to providers.Registry only, never to this builder.
func buildChildModel(
	md *litellmv1alpha1.LiteLLMModelDiscovery,
	childName, rawID, litellmProvider, namespace string,
) (*litellmv1alpha1.LiteLLMModel, error) {
	// Step 1: empty-safe overlay base. The dual guard
	// (`md.Spec.Params.Raw == nil || len(md.Spec.Params.Raw) == 0`) covers
	// two distinct K8s RawExtension states: omitted in YAML (Raw == nil)
	// and `params: {}` literal (Raw is a non-nil empty byte slice). Both
	// must short-circuit to the empty map; without the guard, json.Unmarshal
	// on a nil slice errors with "unexpected end of JSON input".
	paramsMap := map[string]any{}
	if len(md.Spec.Params.Raw) != 0 {
		if err := json.Unmarshal(md.Spec.Params.Raw, &paramsMap); err != nil {
			return nil, fmt.Errorf("decode spec.params: %w", err)
		}
	}

	// Step 2: typed-field overlay (D-07).
	paramsMap["model"] = litellmProvider + "/" + rawID
	if md.Spec.Type == providerTypeBedrock {
		paramsMap["aws_region_name"] = md.Spec.Region
	}
	// FIX.txt H-2 (2026-05-22): kubeai requires api_base on every child so
	// the LiteLLM proxy can route inference requests (hosted_vllm/<id>).
	// Parallel to the bedrock spec.region → aws_region_name overlay above.
	// Diverges from bedrock's overlay-wins precedence on purpose:
	// user-supplied params.api_base is a legitimate routing override (e.g.
	// pointing at a test sidecar), whereas bedrock's region is identity-
	// bearing for the AWS API. Presence-check makes the precedence visible
	// in the diff.
	if md.Spec.Type == providerTypeKubeAI {
		if _, userSet := paramsMap["api_base"]; !userSet {
			paramsMap["api_base"] = md.Spec.BaseURL
		}
	}

	// Step 3: re-marshal.
	paramsBytes, err := json.Marshal(paramsMap)
	if err != nil {
		return nil, fmt.Errorf("re-encode child spec.params: %w", err)
	}

	// Empty-safe Info propagation (Rule 1 — fix discovered during 04-05
	// envtest authoring). The LiteLLMModel CRD requires spec.info to be a JSON
	// object (`type: object` + `x-kubernetes-preserve-unknown-fields:
	// true` per Phase 3 model_types.go). When LiteLLMModelDiscovery's spec.info
	// is absent or empty, propagating `md.Spec.Info` verbatim emits
	// `null`, which the apiserver REJECTS with `spec.info in body must
	// be of type object`. Substitute an empty JSON object `{}` so the
	// child SSA write succeeds; downstream Phase 3 reconciler treats
	// empty info identically to absent (the same empty-safe overlay
	// pattern applied to spec.params above).
	infoRaw := md.Spec.Info
	if len(infoRaw.Raw) == 0 {
		infoRaw = runtime.RawExtension{Raw: []byte(`{}`)}
	}

	// blockOwnerDeletion is a *bool field; the helper here keeps the
	// caller's intent readable. Both fields default-false in K8s, and
	// MDISC-24 explicitly requires both true.
	yes := true

	return &litellmv1alpha1.LiteLLMModel{
		TypeMeta: metav1.TypeMeta{
			APIVersion: litellmv1alpha1.GroupVersion.String(),
			Kind:       "LiteLLMModel",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      childName,
			Namespace: namespace,
			Labels: map[string]string{
				generatedByLabel: md.Name,
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         litellmv1alpha1.GroupVersion.String(),
					Kind:               modelDiscoveryKind,
					Name:               md.Name,
					UID:                md.UID,
					Controller:         &yes,
					BlockOwnerDeletion: &yes,
				},
			},
			Finalizers: []string{modelFinalizer},
		},
		Spec: litellmv1alpha1.ModelSpec{
			Params:  runtime.RawExtension{Raw: paramsBytes},
			Info:    infoRaw,                                                               // verbatim, empty-safe — MDISC-23
			Secrets: append([]litellmv1alpha1.SecretSubstitution(nil), md.Spec.Secrets...), // copy-slice for safety
		},
	}, nil
}

// resolveStringKey reads `<namespace>/<ref.Name>` and returns the value
// of the named Secret key as a string. Returns a wrapped error formatted
// per spec §6.3 / MDISC-22 ("<ns>/<name>:<KEY> not found") so the
// caller's writeReady can surface it verbatim in status.
//
// Empty / absent CredentialsSecretRef: returns an error — the caller's
// switch arm only reaches this helper when the type requires a
// credentialsSecretRef (anthropic/gemini/openai); the CEL admission rule
// guarantees presence. Defensive nil-check here matches the spec
// language anyway.
func (r *ModelDiscoveryReconciler) resolveStringKey(
	ctx context.Context, namespace string, ref *litellmv1alpha1.SecretObjectRef, key string,
) (string, error) {
	if ref == nil || ref.Name == "" {
		return "", fmt.Errorf("%s/<nil>:%s not found", namespace, key)
	}
	var secret corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: ref.Name}, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return "", fmt.Errorf("%s/%s:%s not found", namespace, ref.Name, key)
		}
		return "", err
	}
	val, ok := secret.Data[key]
	if !ok {
		return "", fmt.Errorf("%s/%s:%s not found", namespace, ref.Name, key)
	}
	return string(val), nil
}

// resolveAWSCredentials reads AWS static credentials from the named Secret.
// Required keys: AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY. Optional: AWS_SESSION_TOKEN.
// Missing required keys surface MDISC-22 style messages.
func (r *ModelDiscoveryReconciler) resolveAWSCredentials(
	ctx context.Context, namespace string, ref *litellmv1alpha1.SecretObjectRef,
) (*awsv2.Credentials, error) {
	if ref == nil || ref.Name == "" {
		return nil, nil //nolint:nilnil // intentional: bedrock-no-secret falls through to default chain
	}
	var secret corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: ref.Name}, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("%s/%s:AWS_ACCESS_KEY_ID not found", namespace, ref.Name)
		}
		return nil, err
	}
	accessKey, ok := secret.Data["AWS_ACCESS_KEY_ID"]
	if !ok {
		return nil, fmt.Errorf("%s/%s:AWS_ACCESS_KEY_ID not found", namespace, ref.Name)
	}
	secretKey, ok := secret.Data["AWS_SECRET_ACCESS_KEY"]
	if !ok {
		return nil, fmt.Errorf("%s/%s:AWS_SECRET_ACCESS_KEY not found", namespace, ref.Name)
	}
	creds := &awsv2.Credentials{
		AccessKeyID:     string(accessKey),
		SecretAccessKey: string(secretKey),
		Source:          "alitellm-operator:credentialsSecretRef",
	}
	if sessionToken, ok := secret.Data["AWS_SESSION_TOKEN"]; ok {
		creds.SessionToken = string(sessionToken)
		creds.CanExpire = false
	}
	return creds, nil
}

// writeReady sets only the Ready condition (no SourceReachable touch)
// and writes status. Returns a controller-runtime Result (zero on
// success). The caller passes the return value through unchanged.
//
// §9.1: message is the caller's responsibility — this helper does NOT
// redact. Callers MUST ensure no credential material reaches `message`
// (provider errors are already sanitized via *ProviderAuthError +
// *UpstreamInvalidError + *InvalidConfigError/04-03).
//
//nolint:unparam // status is always ConditionFalse today and result is always zero ctrl.Result; signature mirrors A2A/Model writeStatus helpers and preserves call-site symmetry for the future success branch wiring.
func (r *ModelDiscoveryReconciler) writeReady(
	ctx context.Context, md *litellmv1alpha1.LiteLLMModelDiscovery,
	status metav1.ConditionStatus, reason, message string,
) ctrl.Result {
	apimeta.SetStatusCondition(&md.Status.Conditions, metav1.Condition{
		Type:               conditionTypeReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: md.Generation,
		LastTransitionTime: metav1.Now(),
	})
	md.Status.ObservedGeneration = md.Generation
	if err := r.Status().Update(ctx, md); err != nil {
		logStatusUpdateErr(log.FromContext(ctx), err, "reason", reason)
	}
	metrics.LitellmOperatorReconcileTotal.WithLabelValues(
		modelDiscoveryKind, md.Namespace, metrics.ReasonToReconcileResult(reason),
	).Inc()
	return ctrl.Result{}
}

// writeBothConditions sets both Ready and SourceReachable conditions
// without persisting status. Used by the error path in step 6 where the
// reconciler wants to set both conditions and STILL return err for
// controller-runtime backoff — persisting status is best-effort there.
func (r *ModelDiscoveryReconciler) writeBothConditions(
	ctx context.Context, md *litellmv1alpha1.LiteLLMModelDiscovery,
	readyStatus metav1.ConditionStatus, readyReason, readyMessage string,
	sourceStatus metav1.ConditionStatus, sourceReason, sourceMessage string,
) {
	// Uses Update (not Patch + MergeFrom): callers mutate counters and
	// child lists on md.Status before this call. A MergeFrom orig captured
	// here would already include the mutation and the patch body would
	// drop those fields.
	apimeta.SetStatusCondition(&md.Status.Conditions, metav1.Condition{
		Type:               conditionTypeReady,
		Status:             readyStatus,
		Reason:             readyReason,
		Message:            readyMessage,
		ObservedGeneration: md.Generation,
		LastTransitionTime: metav1.Now(),
	})
	apimeta.SetStatusCondition(&md.Status.Conditions, metav1.Condition{
		Type:               "SourceReachable",
		Status:             sourceStatus,
		Reason:             sourceReason,
		Message:            sourceMessage,
		ObservedGeneration: md.Generation,
		LastTransitionTime: metav1.Now(),
	})
	md.Status.ObservedGeneration = md.Generation
	if err := r.Status().Update(ctx, md); err != nil {
		log.FromContext(ctx).V(1).Info("status update failed (best-effort path)", "error", err)
	}
}

// writeBothConditionsObj is the same as writeBothConditions but returns
// the Status.Update error so the success-path caller can surface it.
func (r *ModelDiscoveryReconciler) writeBothConditionsObj(
	ctx context.Context, md *litellmv1alpha1.LiteLLMModelDiscovery,
	readyStatus metav1.ConditionStatus, readyReason, readyMessage string,
	sourceStatus metav1.ConditionStatus, sourceReason, sourceMessage string,
) error {
	// Uses Update (not Patch + MergeFrom): same rationale as writeBothConditions.
	// Also fixes the pre-existing parity gap with the MCP equivalent by setting
	// ObservedGeneration here.
	apimeta.SetStatusCondition(&md.Status.Conditions, metav1.Condition{
		Type:               conditionTypeReady,
		Status:             readyStatus,
		Reason:             readyReason,
		Message:            readyMessage,
		ObservedGeneration: md.Generation,
		LastTransitionTime: metav1.Now(),
	})
	apimeta.SetStatusCondition(&md.Status.Conditions, metav1.Condition{
		Type:               "SourceReachable",
		Status:             sourceStatus,
		Reason:             sourceReason,
		Message:            sourceMessage,
		ObservedGeneration: md.Generation,
		LastTransitionTime: metav1.Now(),
	})
	md.Status.ObservedGeneration = md.Generation
	return r.Status().Update(ctx, md)
}

// classifyAlreadyExists handles the K8s-native conflict resolution path
// per spec §6.3 line 798-808 (MDISC-13 / MDISC-14). When SSA's Patch
// returns IsAlreadyExists, the colliding object is fetched and bucketed
// into one of three outcomes:
//
//	(skip=ExplicitModelExists, retryable=false, err=nil)
//	 The existing child has NO controller ownerRef (or no
//	 ownerReferences at all). This is the user-authored LiteLLMModel case
//	 from MDISC-14 — Discovery records a skip with ownedBy=<name>
//	 (the user's LiteLLMModel has no parent to point at).
//
//	(skip=Conflict, retryable=false, err=nil)
//	 The existing child's controller ownerRef points at a DIFFERENT
//	 LiteLLMModelDiscovery (different UID, or different Kind entirely — a
//	 third-party controller could plant a LiteLLMModel). Loser records a
//	 skip with ownedBy=<Kind>/<Name>/<UID>. The winning Discovery's
//	 reconcile is unaffected (its child is intact).
//
//	(skip=nil, retryable=true, err=nil)
//	 Either the Get returned NotFound (the AlreadyExists raced with a
//	 Delete) OR the existing child's controller ownerRef points at
//	 THIS Discovery (a transient apiserver/cache race; SSA's
//	 ForceOwnership should have won). Caller continues to the next
//	 candidate; the next reconcile retries.
//
//	(skip=nil, retryable=false, err=<get-err>)
//	 The Get failed with a non-NotFound apiserver error. Caller
//	 surfaces as ChildCRWriteFailed.
//
// Spec §6.3 line 799: "Discovery NEVER mutates a child whose controller
// ownerRef points elsewhere — it can only OBSERVE the conflict and record
// it in status.skippedCandidates[]". This function implements that
// observation-only contract: it issues a single Get and emits a typed
// classification; the caller never re-attempts Patch on a classified
// collision.
//
// T-04-06-T2 mitigation: the Get-then-classify is not atomic (a TOCTOU
// race could mis-classify) — but the OUTCOME is only a status field;
// no destructive action follows a misclassification, and the next
// reconcile re-classifies from fresh state.
func (r *ModelDiscoveryReconciler) classifyAlreadyExists(
	ctx context.Context, childName string, parent *litellmv1alpha1.LiteLLMModelDiscovery,
) (*litellmv1alpha1.SkippedCandidate, bool, error) {
	var existing litellmv1alpha1.LiteLLMModel
	if err := r.Get(ctx, client.ObjectKey{Name: childName, Namespace: r.Namespace}, &existing); err != nil {
		if apierrors.IsNotFound(err) {
			// AlreadyExists raced with a Delete (very rare; possible if
			// a concurrent vanish on a sibling Discovery deleted the
			// colliding child between Patch's pre-check and our Get).
			// Retry on next reconcile.
			return nil, true, nil
		}
		return nil, false, err
	}
	// Locate the controller ownerRef, if any.
	var ctrlRef *metav1.OwnerReference
	for i := range existing.OwnerReferences {
		ref := &existing.OwnerReferences[i]
		if ref.Controller != nil && *ref.Controller {
			ctrlRef = ref
			break
		}
	}
	if ctrlRef == nil {
		// No controller ownerRef → user-authored LiteLLMModel (MDISC-14).
		// OwnedBy is the child's own name — the "owner" is the user.
		return &litellmv1alpha1.SkippedCandidate{
			Name:    childName,
			Reason:  "ExplicitModelExists",
			OwnedBy: existing.Name,
		}, false, nil
	}
	if ctrlRef.Kind == modelDiscoveryKind && ctrlRef.UID == parent.UID {
		// Should not happen — SSA with ForceOwnership should have won.
		// Treat as transient and retry. (If this fires consistently, the
		// SSA field-manager identity may have drifted — investigate.)
		return nil, true, nil
	}
	// Different controller (different LiteLLMModelDiscovery UID, or
	// different Kind entirely). MDISC-13: classify as `Conflict` per
	// ADR-0001 and name the winner. OwnedBy is "<Kind>/<Name>/<UID>"
	// for fully-qualified identification (Kind is included because a
	// third-party controller could have planted the LiteLLMModel —
	// Kind!=LiteLLMModelDiscovery is itself a useful diagnostic signal).
	//
	// Alpha-last-wins ownership transfer between Discoveries is
	// intentionally NOT applied here — it requires a get-then-update
	// path to replace metadata.ownerReferences across field managers
	// and is deferred to a follow-up PR.
	return &litellmv1alpha1.SkippedCandidate{
		Name:    childName,
		Reason:  "Conflict",
		OwnedBy: ctrlRef.Kind + "/" + ctrlRef.Name + "/" + string(ctrlRef.UID),
	}, false, nil
}

// ownedByThisDiscovery reports whether `child`'s controller ownerRef
// (if any) points at `parent` by Kind=LiteLLMModelDiscovery AND UID match. UID
// is forgery-resistant (apiserver-assigned, immutable, opaque); a Name
// match alone would be vulnerable to delete-and-recreate replacement.
//
// Used for adoption recognition (04-06): a child with the generated-by
// label but WITHOUT this Discovery's controller ownerRef has been
// adopted by the user (the user stripped the ownerRef entry via
// `kubectl patch --type=json -p='[{"op":"remove","path":"/metadata/ownerReferences"}]'`
// or equivalent) and MUST NOT be re-claimed by Discovery.
//
// Returns true ONLY when a controller=true ownerRef matches both Kind
// and UID. A non-controller ownerRef (Controller==nil or false) is
// treated as "not owned" — the controller ownerRef is the only entry
// that confers ownership semantics under K8s GC.
func ownedByThisDiscovery(child *litellmv1alpha1.LiteLLMModel, parent *litellmv1alpha1.LiteLLMModelDiscovery) bool {
	for i := range child.OwnerReferences {
		ref := &child.OwnerReferences[i]
		if ref.Controller != nil && *ref.Controller &&
			ref.Kind == modelDiscoveryKind && ref.UID == parent.UID {
			return true
		}
	}
	return false
}

// sanitizeError is the defense-in-depth seam for status-message redaction.
// The K8s apiserver does NOT echo credential material in its error
// responses (it never sees the upstream provider's API key — only the
// operator code resolves Secrets), so for v1alpha1 this helper simply
// returns the apiserver's error string verbatim. The seam is reserved
// for future surfaces where credential fragments could in principle
// reach status (e.g. a future provider that proxies through the
// apiserver as a webhook).
//
// Mirrors the Bedrock-side `sanitizeAWSError` pattern from —
// same shape, different scope. Kept here as a single chokepoint so a
// future redaction policy (regex strip of `sk-.`, `AKIA.`, etc.)
// applies uniformly to every status surface that calls it.
//
// Calling pattern: every err string that lands in
// FailedCandidate.Message or SkippedCandidate.Message MUST go through
// this helper (the linter / canary test enforces this in CI — see
// `TestModelDiscovery_AC_S1_NoCredentialLeak` planned in Phase 4
// validation).
func sanitizeError(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// ownedByDiscovery is the T-04-05-T1 defense for vanish detection:
// returns true iff `child` has an ownerReference back at this
// LiteLLMModelDiscovery (matched by UID, which is forgery-resistant — Names
// are user-mutable but UIDs are apiserver-assigned and immutable).
//
// A child with a stripped ownerRef (the user adopted it manually,
// turning it into an "explicit" LiteLLMModel from Discovery's perspective)
// MUST NOT be vanished by Discovery — vanish-delete on such a child
// would silently destroy user-authored intent. expands
// this into full ExplicitModelExists + adoption-recognition handling;
// Just conservatively skips delete in this case.
//
// Returns true if ANY ownerReference matches md.UID (Phase 4's writes
// always set controller=true on the FIRST entry, so this is robust
// even if some future feature appends a second entry).
func ownedByDiscovery(child *litellmv1alpha1.LiteLLMModel, mdUID types.UID) bool {
	for _, ref := range child.OwnerReferences {
		if ref.UID == mdUID {
			return true
		}
	}
	return false
}

// secretToModelDiscoveries maps a Secret update event to the set of
// LiteLLMModelDiscovery CRs that reference it via `.spec.credentialsSecretRef.name`
// (MDISC-21 — 30s rotation propagation). Uses the field indexer
// registered in cmd/main.go / suite_test.go. Mirrors `secretToModels`
// at model_controller.go:596-612 modulo the CRD list type.
func (r *ModelDiscoveryReconciler) secretToModelDiscoveries(ctx context.Context, obj client.Object) []reconcile.Request {
	return secretToRequests(ctx, r.Client, r.Log, &litellmv1alpha1.LiteLLMModelDiscoveryList{}, obj.GetNamespace(), obj.GetName(), CredentialsSecretRefField, "secretToModelDiscoveries")
}

// SetupWithManager registers the ModelDiscoveryReconciler with
// controller-runtime.
//
// Watches:
// - For(&LiteLLMModelDiscovery{}) — primary watch.
// - Watches(&Secret{}, secretToModelDiscoveries) — MDISC-21 rotation
// propagation via the field indexer registered in cmd/main.go.
// - Owns(&LiteLLMModel{}) — child LiteLLMModel events drive sub-interval Discovery
// reconciles (cascade-delete + adoption hooks).
//
// Named("modeldiscovery") — controller registry name.
func (r *ModelDiscoveryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	b := ctrl.NewControllerManagedBy(mgr).
		For(&litellmv1alpha1.LiteLLMModelDiscovery{},
			builder.WithPredicates(discoverySpecChanged())).
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(r.secretToModelDiscoveries),
		).
		Owns(&litellmv1alpha1.LiteLLMModel{}, builder.WithPredicates(ownedChildSpecChanged())).
		WithOptions(transientBackoffOptions()).
		Named("modeldiscovery")
	if src := BootEventsSource(r.BootEvents); src != nil {
		b = b.WatchesRawSource(src)
	}
	return b.Complete(r)
}
