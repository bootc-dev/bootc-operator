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
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	bootcv1alpha1 "github.com/bootc-dev/bootc-operator/api/v1alpha1"
)

const testNodeName = "test-node"

var (
	testEnv        *envtest.Environment
	k8sClient      client.Client
	testReconciler *BootcNodeReconciler
)

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

	fake := &fakeExecutor{}
	testReconciler = &BootcNodeReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		NodeName: testNodeName,
		Executor: fake,
		StatusWatcher: &StatusWatcher{
			Events:   make(chan event.GenericEvent, 1),
			NodeName: testNodeName,
			Executor: fake,
		},
	}
	if err := mgr.Add(testReconciler.StatusWatcher); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to add status watcher: %v\n", err)
		os.Exit(1)
	}
	if err := testReconciler.SetupWithManager(mgr); err != nil {
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

// newTestEnv creates a fresh fakeExecutor and StatusWatcher for a test,
// wiring them into the shared reconciler. Each test gets its own cache.
func newTestEnv() *fakeExecutor {
	fake := &fakeExecutor{}
	testReconciler.Executor = fake
	testReconciler.StatusWatcher = &StatusWatcher{
		Events:   testReconciler.StatusWatcher.Events,
		NodeName: testNodeName,
		Executor: fake,
	}
	return fake
}
