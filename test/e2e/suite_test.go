//go:build e2e

// SPDX-License-Identifier: Apache-2.0

package e2e_test

import (
	"context"
	"os"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
)

var (
	ctx     context.Context
	cancel  context.CancelFunc
	cs      *kubernetes.Clientset
	timeout = 60 * time.Second
)

// silence unused-var warning until specs land in Phase 9
var _ = timeout

func TestTier2(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Tier 2 Suite")
}

var _ = BeforeSuite(func() {
	ctx, cancel = context.WithCancel(context.Background())

	// Resolve kubeconfig from $KUBECONFIG (devtools wrapper sets it to
	// /workspace/.gocache/kube/config). Fall back to controller-runtime's
	// in-cluster / default loader.
	cfg, err := clientcmd.BuildConfigFromFlags("", os.Getenv("KUBECONFIG"))
	if err != nil {
		cfg, err = ctrl.GetConfig()
		Expect(err).NotTo(HaveOccurred())
	}
	cs, err = kubernetes.NewForConfig(cfg)
	Expect(err).NotTo(HaveOccurred())

	_, err = cs.CoreV1().Namespaces().Get(ctx, "default", metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred(),
		"default namespace missing — did you run `make cluster-up`?")
})

var _ = AfterSuite(func() { cancel() })
