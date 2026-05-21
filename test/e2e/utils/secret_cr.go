//go:build e2e

// SPDX-License-Identifier: Apache-2.0

package utils

import (
	"context"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

// CreateSecretAndCR applies the Secret first, waits for it to be readable
// via Get, then applies the CR. Enforces spec §8.2 conv #1 — without this
// ordering, reconcilers fire once against a missing Secret and emit a
// transient SecretNotFound event that flickers connection_ready 0→1.
//
// cr is expected to be an *unstructured.Unstructured carrying its own GVR
// (or one resolvable via the dynamic client). Pass the corresponding
// schema.GroupVersionResource via crGVR.
func CreateSecretAndCR(
	ctx context.Context,
	cs *kubernetes.Clientset,
	dyn dynamic.Interface,
	secret *corev1.Secret,
	cr *unstructured.Unstructured,
	crGVR schema.GroupVersionResource,
) error {
	if secret == nil {
		return errors.New("secret must not be nil")
	}
	if cr == nil {
		return errors.New("cr must not be nil")
	}

	// 1. Apply Secret.
	if _, err := cs.CoreV1().Secrets(secret.Namespace).Create(ctx, secret, metav1.CreateOptions{}); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create secret: %w", err)
		}
	}

	// 2. Confirm Secret is gettable before applying the CR.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := cs.CoreV1().Secrets(secret.Namespace).Get(ctx, secret.Name, metav1.GetOptions{}); err == nil {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("secret %s/%s not visible within 5s", secret.Namespace, secret.Name)
		}
		time.Sleep(100 * time.Millisecond)
	}

	// 3. Apply CR via dynamic client.
	if _, err := dyn.Resource(crGVR).Namespace(cr.GetNamespace()).
		Create(ctx, cr, metav1.CreateOptions{}); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create cr: %w", err)
		}
	}
	return nil
}
