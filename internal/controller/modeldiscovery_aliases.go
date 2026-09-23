// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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
