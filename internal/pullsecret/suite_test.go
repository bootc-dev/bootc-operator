// SPDX-License-Identifier: Apache-2.0

package pullsecret

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	bootcv1alpha1 "github.com/bootc-dev/bootc-operator/api/v1alpha1"
	testutil "github.com/bootc-dev/bootc-operator/test/util"
)

const (
	pollInterval = 200 * time.Millisecond
	pollTimeout  = 10 * time.Second
)

var k8sClient client.Client

func TestMain(m *testing.M) {
	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))

	if err := bootcv1alpha1.AddToScheme(scheme.Scheme); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to add scheme: %v\n", err)
		os.Exit(1)
	}

	testEnv := &envtest.Environment{
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

	// Mirror the manager the operator runs: the Secret informer is
	// restricted to the allowed pull secret namespaces.
	pullSecretNamespaces := []string{testutil.OperatorNamespaceName}
	secretNamespaces := make(map[string]cache.Config, len(pullSecretNamespaces))
	for _, ns := range pullSecretNamespaces {
		secretNamespaces[ns] = cache.Config{}
	}
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme.Scheme,
		Metrics: metricsserver.Options{
			BindAddress: "0", // disable metrics server in tests
		},
		Cache: cache.Options{
			ByObject: map[client.Object]cache.ByObject{
				&corev1.Secret{}: {Namespaces: secretNamespaces},
			},
		},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create manager: %v\n", err)
		os.Exit(1)
	}

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testutil.OperatorNamespaceName}}
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create operator namespace: %v\n", err)
		os.Exit(1)
	}

	if err := IndexPoolPullSecretRef(context.Background(), mgr.GetFieldIndexer()); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to index pools by pull secret: %v\n", err)
		os.Exit(1)
	}

	if err := (&PullSecretAggregator{
		Client:               mgr.GetClient(),
		Namespace:            testutil.OperatorNamespaceName,
		PullSecretNamespaces: pullSecretNamespaces,
	}).SetupWithManager(mgr); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to setup pull secret aggregator: %v\n", err)
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

	// Stop the manager; this implicitly tests that it can shut down cleanly.
	mgrCancel()
	<-mgrDone

	if err := testEnv.Stop(); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to stop envtest: %v\n", err)
	}

	os.Exit(code)
}
