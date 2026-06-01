// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"

	"github.com/go-logr/logr"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// secretToRequests maps a Secret event to the set of CRs that reference it
// via a field-indexed name (M-Q3). It is the shared core of the six
// per-kind secretTo<Kind> mappers, which were byte-identical modulo the
// *List type, the index-field constant, and the log label. The singleton
// secretToConnection mapper (no index, manual filter) is intentionally NOT
// folded in.
//
// list is a fresh empty *List of the target kind; it is populated in place
// by the List call and read back via apimeta.ExtractList so this helper
// stays type-agnostic.
func secretToRequests(ctx context.Context, c client.Client, log logr.Logger, list client.ObjectList, ns, name, indexField, logLabel string) []reconcile.Request {
	if err := c.List(ctx, list, client.InNamespace(ns), client.MatchingFields{indexField: name}); err != nil {
		log.V(1).Info(logLabel+": list failed; skipping", "error", err)
		return nil
	}
	objs, err := apimeta.ExtractList(list)
	if err != nil {
		log.V(1).Info(logLabel+": extract list failed; skipping", "error", err)
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
