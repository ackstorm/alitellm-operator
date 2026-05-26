// SPDX-License-Identifier: Apache-2.0

// Package deletionpolicy resolves the effective deletion policy for a
// finalized CR, applying the precedence chain:
//
//	annotation override  >  spec.deletionPolicy  >  default (Orphan)
//
// Children owned by a Discovery controller always resolve to Orphan,
// regardless of spec or annotation, so that vanish-detection cannot be
// deadlocked by a stuck child.
package deletionpolicy

import "sigs.k8s.io/controller-runtime/pkg/client"

// Policy is the resolved deletion behavior.
type Policy string

const (
	// Orphan: remove the finalizer when the LiteLLM-side delete cannot
	// be confirmed. Preserves REL-06 anti-storm at the cost of possibly
	// orphaning the LiteLLM entry.
	Orphan Policy = "Orphan"

	// Delete: block finalizer removal until the LiteLLM-side ack
	// succeeds. CR stays in Terminating until LiteLLM confirms the
	// delete (or the user flips the annotation override).
	Delete Policy = "Delete"

	// AnnotationOverride is the per-CR break-glass annotation. Accepts
	// the same values as the spec field; any other value is ignored.
	AnnotationOverride = "litellm.ackstorm.ai/deletion-policy-override"
)

// discoveryOwnerKinds is the closed set of Discovery parent kinds that
// auto-create finalized children. Discovery-owned children must always
// resolve to Orphan so vanish-detection is never blocked.
var discoveryOwnerKinds = map[string]struct{}{
	"LiteLLMModelDiscovery":     {},
	"LiteLLMMCPServerDiscovery": {},
}

// Resolve returns the effective Policy for obj.
//
// Precedence:
//  1. If obj has a controller OwnerReference of a Discovery kind →
//     Orphan (forced).
//  2. If the AnnotationOverride annotation parses to a known policy →
//     that value.
//  3. If specPolicy is a known policy → that value.
//  4. Default Orphan.
func Resolve(obj client.Object, specPolicy string) Policy {
	if isDiscoveryOwned(obj) {
		return Orphan
	}
	if v, ok := obj.GetAnnotations()[AnnotationOverride]; ok {
		if p, ok := parse(v); ok {
			return p
		}
	}
	if p, ok := parse(specPolicy); ok {
		return p
	}
	return Orphan
}

func parse(s string) (Policy, bool) {
	switch Policy(s) {
	case Orphan, Delete:
		return Policy(s), true
	default:
		return "", false
	}
}

func isDiscoveryOwned(obj client.Object) bool {
	for _, ref := range obj.GetOwnerReferences() {
		if ref.Controller == nil || !*ref.Controller {
			continue
		}
		if _, ok := discoveryOwnerKinds[ref.Kind]; ok {
			return true
		}
	}
	return false
}
