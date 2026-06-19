// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	bootcv1alpha1 "github.com/bootc-dev/bootc-operator/api/v1alpha1"
)

const testNodeName = "test-node"

var (
	testEnv    *envtest.Environment
	k8sClient  client.Client
	fake       *fakeExecutor
	reconciler *BootcNodeReconciler
)

// resetDaemon resets all volatile state on the shared reconciler and
// fake executor so each test starts from a clean slate.
func resetDaemon() {
	fake.reset()
	reconciler.rebootIssued = false
	reconciler.inflight.reset()
}

func TestMain(m *testing.M) {
	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))

	if err := bootcv1alpha1.AddToScheme(scheme.Scheme); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to add scheme: %v\n", err)
		os.Exit(1)
	}

	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}

	cfg, err := testEnv.Start()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to start envtest: %v\n", err)
		os.Exit(1)
	}

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create client: %v\n", err)
		os.Exit(1)
	}

	fake = &fakeExecutor{}

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme.Scheme,
		Metrics: metricsserver.Options{
			BindAddress: "0",
		},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create manager: %v\n", err)
		os.Exit(1)
	}

	reconciler = &BootcNodeReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		NodeName: testNodeName,
		Executor: fake,
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to setup reconciler: %v\n", err)
		os.Exit(1)
	}

	mgrCtx, mgrCancel := context.WithCancel(context.Background())

	mgrDone := make(chan struct{})
	go func() {
		defer close(mgrDone)
		if err := mgr.Start(mgrCtx); err != nil {
			fmt.Fprintf(os.Stderr, "Manager exited with error: %v\n", err)
			os.Exit(1)
		}
	}()

	code := m.Run()

	mgrCancel()
	<-mgrDone

	if err := testEnv.Stop(); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to stop envtest: %v\n", err)
	}

	os.Exit(code)
}
