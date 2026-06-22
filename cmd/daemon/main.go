// SPDX-License-Identifier: Apache-2.0

package main

import (
	"flag"
	"fmt"
	"os"

	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	bootcv1alpha1 "github.com/bootc-dev/bootc-operator/api/v1alpha1"
	"github.com/bootc-dev/bootc-operator/internal/bootc"
	"github.com/bootc-dev/bootc-operator/internal/daemon"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(bootcv1alpha1.AddToScheme(scheme))
}

func main() {
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	nodeName := os.Getenv("NODE_NAME")
	if nodeName == "" {
		setupLog.Error(fmt.Errorf("NODE_NAME not set"), "NODE_NAME environment variable is required")
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		// Only cache the BootcNode object for this node to avoid unnecessary watches.
		Cache: cache.Options{
			ByObject: map[client.Object]cache.ByObject{
				&bootcv1alpha1.BootcNode{}: {
					Field: fields.OneTermEqualSelector("metadata.name", nodeName),
				},
			},
		},
	})
	if err != nil {
		setupLog.Error(err, "Failed to start manager")
		os.Exit(1)
	}

	if err := (&daemon.BootcNodeReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		NodeName: nodeName,
		Executor: bootc.NewHostExecutor(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "bootcnode")
		os.Exit(1)
	}

	setupLog.Info("Starting daemon", "node", nodeName)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "Failed to run daemon")
		os.Exit(1)
	}
}
