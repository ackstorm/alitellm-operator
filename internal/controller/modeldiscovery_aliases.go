// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
)

// aliasChunkSize mirrors LiteLLMModelAliasSpec.Aliases MaxItems=128.
const aliasChunkSize = 128

// buildDiscoveryAliases renders the LiteLLMModelAlias CRs for
// spec.aliasSuffix: one entry `<child><suffix> → <child>` per generated
// child, 128 entries per CR named `<discovery>-aliases-<n>`. generated must
// be sorted so chunk membership is stable. Empty suffix or no children →
// nil (the sync step then deletes any previously generated CRs).
//
// The ownerRef deliberately leaves BlockOwnerDeletion unset: the
// Discovery's own finalizer already blocks GC (FIX3 HIGH-1), and the alias
// CRs need no drain — GC removes them once the Discovery is gone and the
// alias finalizer rewrites the map.
func buildDiscoveryAliases(md *litellmv1alpha1.LiteLLMModelDiscovery, generated []string, namespace string) []*litellmv1alpha1.LiteLLMModelAlias {
	if md.Spec.AliasSuffix == "" {
		return nil
	}
	yes := true
	var out []*litellmv1alpha1.LiteLLMModelAlias
	for i := 0; i < len(generated); i += aliasChunkSize {
		chunk := generated[i:min(i+aliasChunkSize, len(generated))]
		entries := make([]litellmv1alpha1.ModelAliasEntry, len(chunk))
		for j, child := range chunk {
			entries[j] = litellmv1alpha1.ModelAliasEntry{Name: child + md.Spec.AliasSuffix, Value: child}
		}
		out = append(out, &litellmv1alpha1.LiteLLMModelAlias{
			TypeMeta: metav1.TypeMeta{
				APIVersion: litellmv1alpha1.GroupVersion.String(),
				Kind:       "LiteLLMModelAlias",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("%s-aliases-%d", md.Name, len(out)),
				Namespace: namespace,
				Labels:    map[string]string{generatedByLabel: md.Name},
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: litellmv1alpha1.GroupVersion.String(),
					Kind:       modelDiscoveryKind,
					Name:       md.Name,
					UID:        md.UID,
					Controller: &yes,
				}},
			},
			Spec: litellmv1alpha1.LiteLLMModelAliasSpec{Aliases: entries},
		})
	}
	return out
}

// syncDiscoveryAliases SSA-applies the desired alias CRs and deletes the
// ones this Discovery controls that are no longer desired (suffix cleared,
// fewer chunks). It refuses to take over an alias CR it does not control —
// ForceOwnership would otherwise silently overwrite a hand-written CR that
// happens to share the `<discovery>-aliases-<n>` name.
func (r *ModelDiscoveryReconciler) syncDiscoveryAliases(ctx context.Context, md *litellmv1alpha1.LiteLLMModelDiscovery, generated []string) error {
	desired := buildDiscoveryAliases(md, generated, r.Namespace)
	keep := make(map[string]struct{}, len(desired))
	for _, a := range desired {
		keep[a.Name] = struct{}{}
		var existing litellmv1alpha1.LiteLLMModelAlias
		err := r.Get(ctx, client.ObjectKeyFromObject(a), &existing)
		switch {
		case err == nil && !metav1.IsControlledBy(&existing, md):
			return fmt.Errorf("LiteLLMModelAlias %s exists and is not owned by this discovery", a.Name)
		case err != nil && !apierrors.IsNotFound(err):
			return fmt.Errorf("get LiteLLMModelAlias %s: %w", a.Name, err)
		}
		if err := r.Patch(ctx, a, client.Apply, client.FieldOwner(fieldOwner), client.ForceOwnership); err != nil {
			return fmt.Errorf("apply LiteLLMModelAlias %s: %w", a.Name, err)
		}
	}

	var owned litellmv1alpha1.LiteLLMModelAliasList
	if err := r.List(ctx, &owned,
		client.InNamespace(r.Namespace),
		client.MatchingLabels{generatedByLabel: md.Name},
	); err != nil {
		return fmt.Errorf("list generated LiteLLMModelAliases: %w", err)
	}
	for i := range owned.Items {
		a := &owned.Items[i]
		if _, ok := keep[a.Name]; ok || !metav1.IsControlledBy(a, md) {
			continue
		}
		if err := r.Delete(ctx, a); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete LiteLLMModelAlias %s: %w", a.Name, err)
		}
	}
	return nil
}
