// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
)

// resolveSecretMap resolves spec.secrets[] into an as→value map (M-Q1). It is
// the shared core of the five reconcilers (Team, Model, MCPServer, A2AAgent,
// GuardRail) that previously each carried a byte-identical copy of this loop.
//
// Returns:
//   - (map, "", nil)      — all secrets resolved.
//   - (nil, missMsg, nil) — a referenced Secret OR key is missing; missMsg is
//     the exact "<ns>/<name>:<key> not found" string the callers surface as
//     reason=SecretNotFound (a soft, requeue-on-rejected condition).
//   - (nil, "", err)      — a transient Get error; the caller returns it for
//     controller-runtime backoff.
//
// The writeStatus / metrics / requeue tail stays typed at each call site.
func resolveSecretMap(ctx context.Context, c client.Client, ns string, secrets []litellmv1alpha1.SecretSubstitution) (map[string]string, string, error) {
	out := make(map[string]string, len(secrets))
	for _, entry := range secrets {
		var secret corev1.Secret
		key := types.NamespacedName{Namespace: ns, Name: entry.SecretRef.Name}
		if err := c.Get(ctx, key, &secret); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, ns + "/" + entry.SecretRef.Name + ":" + entry.SecretRef.Key + " not found", nil
			}
			return nil, "", err
		}
		val, ok := secret.Data[entry.SecretRef.Key]
		if !ok {
			return nil, ns + "/" + entry.SecretRef.Name + ":" + entry.SecretRef.Key + " not found", nil
		}
		out[entry.As] = string(val)
	}
	return out, "", nil
}
