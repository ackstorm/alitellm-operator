// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
)

// TestModelDiscovery_MetadataBaseURL_InvalidConfig is the M-SEC1 guard: a
// ModelDiscovery whose spec.baseUrl points at the cloud-metadata address is
// rejected at reconcile time with Ready=False reason=InvalidConfig, before
// any outbound provider.List call. The reconciler returns immediately after
// providers.ValidateBaseURL fails, so no HTTP request is issued.
func TestModelDiscovery_MetadataBaseURL_InvalidConfig(t *testing.T) {
	ctx := context.Background()
	mdName := "md-metadata-deny"
	ensureNoModelDiscovery(t, ctx, mdName)
	t.Cleanup(func() { ensureNoModelDiscovery(t, context.Background(), mdName) })

	// kubeai requires baseUrl (no credentials secret needed). Override the
	// sample's in-cluster URL with the cloud-metadata address.
	md := modeldiscoverySampleCR(mdName, "kubeai")
	md.Spec.BaseURL = "http://169.254.169.254/latest/meta-data/"
	if err := k8sClient.Create(ctx, md); err != nil {
		t.Fatalf("create ModelDiscovery: %v", err)
	}

	got := pollDiscoveryStatusReady(t, ctx, mdName, "InvalidConfig", 30*time.Second)
	ready := apimeta.FindStatusCondition(got.Status.Conditions, conditionTypeReady)
	if ready == nil || ready.Reason != "InvalidConfig" {
		t.Fatalf("want Ready=False reason=InvalidConfig; got %+v", got.Status.Conditions)
	}
}
