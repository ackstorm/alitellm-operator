// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"maps"
	"regexp"
	"slices"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
)

// aliasChunkSize mirrors LiteLLMModelAliasSpec.Aliases MaxItems=128.
const aliasChunkSize = 128

// buildDiscoveryAliases renders the LiteLLMModelAlias CRs for
// spec.aliases: each rule yields `<prefix><child><suffix> → <child>` for every
// generated child its Include patterns match (implicit "^" anchor, like
// spec.filters; empty = all). A duplicate alias name across rules goes to the
// later rule — the alias CRD rejects duplicate names within one CR. Entries
// are sorted by name so chunk membership is stable, 128 per CR named
// `<discovery>-aliases-<n>`. No rules or no entries → nil (the sync step then
// deletes any previously generated CRs). An invalid pattern is returned as an
// error so the Discovery reports it instead of silently emitting nothing.
//
// The ownerRef deliberately leaves BlockOwnerDeletion unset: the
// Discovery's own finalizer already blocks GC (FIX3 HIGH-1), and the alias
// CRs need no drain — GC removes them once the Discovery is gone and the
// alias finalizer rewrites the map.
func buildDiscoveryAliases(md *litellmv1alpha1.LiteLLMModelDiscovery, generated []string, namespace string) ([]*litellmv1alpha1.LiteLLMModelAlias, error) {
	byName := map[string]string{}
	for i, rule := range md.Spec.Aliases {
		res := make([]*regexp.Regexp, len(rule.Include))
		for j, pat := range rule.Include {
			re, err := regexp.Compile("^" + pat)
			if err != nil {
				return nil, fmt.Errorf("spec.aliases[%d].include[%d] %q: %w", i, j, pat, err)
			}
			res[j] = re
		}
		for _, child := range generated {
			if len(res) > 0 && !slices.ContainsFunc(res, func(re *regexp.Regexp) bool { return re.MatchString(child) }) {
				continue
			}
			byName[rule.Prefix+child+rule.Suffix] = child
		}
	}
	names := slices.Sorted(maps.Keys(byName))

	yes := true
	var out []*litellmv1alpha1.LiteLLMModelAlias
	for i := 0; i < len(names); i += aliasChunkSize {
		chunk := names[i:min(i+aliasChunkSize, len(names))]
		entries := make([]litellmv1alpha1.ModelAliasEntry, len(chunk))
		for j, name := range chunk {
			entries[j] = litellmv1alpha1.ModelAliasEntry{Name: name, Value: byName[name]}
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
	return out, nil
}

// syncDiscoveryAliases SSA-applies the desired alias CRs and deletes the
// ones this Discovery controls that are no longer desired (rules removed,
// fewer chunks). It refuses to take over an alias CR it does not control —
// ForceOwnership would otherwise silently overwrite a hand-written CR that
// happens to share the `<discovery>-aliases-<n>` name.
func (r *ModelDiscoveryReconciler) syncDiscoveryAliases(ctx context.Context, md *litellmv1alpha1.LiteLLMModelDiscovery, generated []string) error {
	desired, err := buildDiscoveryAliases(md, generated, r.Namespace)
	if err != nil {
		return err
	}
	keep := make(map[string]struct{}, len(desired))
	for _, a := range desired {
		keep[a.Name] = struct{}{}
		var existing litellmv1alpha1.LiteLLMModelAlias
		err = r.Get(ctx, client.ObjectKeyFromObject(a), &existing)
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
