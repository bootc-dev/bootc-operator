// SPDX-License-Identifier: Apache-2.0

package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	bootcv1alpha1 "github.com/bootc-dev/bootc-operator/api/v1alpha1"
	"github.com/bootc-dev/bootc-operator/internal/bootc"
	"github.com/bootc-dev/bootc-operator/internal/config"
	"github.com/bootc-dev/bootc-operator/internal/daemon"
	"github.com/bootc-dev/bootc-operator/internal/version"
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
	var options config.DaemonOptions
	flag.DurationVar(
		&options.PollInterval,
		"bootc-poll-interval",
		time.Duration(bootcv1alpha1.DefaultStatusPollPeriodSeconds)*time.Second,
		"Interval for polling bootc status as a fallback to fsnotify",
	)

	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	explicit := config.ExplicitFlags(flag.CommandLine)

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	setupLog.Info(
		"starting",
		"component",
		"daemon",
		"version",
		version.Version,
		"commit",
		version.GitCommit,
	)

	nodeName := os.Getenv("NODE_NAME")
	if nodeName == "" {
		setupLog.Error(
			fmt.Errorf("NODE_NAME not set"),
			"NODE_NAME environment variable is required",
		)
		os.Exit(1)
	}

	ctx := ctrl.SetupSignalHandler()
	restConfig := ctrl.GetConfigOrDie()
	operatorConfig, err := config.Load(ctx, restConfig, scheme)
	if err != nil {
		setupLog.Error(err, "Failed to load operator configuration")
		os.Exit(1)
	}
	options = config.ResolveDaemon(operatorConfig, options, explicit)
	config.LogSource(setupLog, operatorConfig, explicit, "bootc-poll-interval")

	mgr, err := ctrl.NewManager(restConfig, ctrl.Options{
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

	executor := bootc.NewHostExecutor()

	watcher := &daemon.StatusWatcher{
		PollInterval:  options.PollInterval,
		OstreePath:    daemon.DefaultOstreePath,
		ComposefsPath: daemon.DefaultComposefsPath,
		Events:        make(chan event.GenericEvent, 1),
		NodeName:      nodeName,
		Executor:      executor,
	}
	if err := mgr.Add(watcher); err != nil {
		setupLog.Error(err, "Failed to add status watcher")
		os.Exit(1)
	}

	if err := (&daemon.BootcNodeReconciler{
		Client:        mgr.GetClient(),
		Scheme:        mgr.GetScheme(),
		NodeName:      nodeName,
		HostRoot:      "/proc/1/root",
		Executor:      executor,
		StatusWatcher: watcher,
		APIReader:     mgr.GetAPIReader(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "bootcnode")
		os.Exit(1)
	}

	setupLog.Info("Starting daemon", "node", nodeName, "pollInterval", options.PollInterval)
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "Failed to run daemon")
		os.Exit(1)
	}
}
