// SPDX-License-Identifier: Apache-2.0

package main

import (
	"flag"
	"os"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	bootcv1alpha1 "github.com/bootc-dev/bootc-operator/api/v1alpha1"
	"github.com/bootc-dev/bootc-operator/internal/config"
	"github.com/bootc-dev/bootc-operator/internal/controller"
	"github.com/bootc-dev/bootc-operator/internal/registry"
	"github.com/bootc-dev/bootc-operator/internal/version"
)

const namespacePath = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func detectNamespace() string {
	if ns := os.Getenv("POD_NAMESPACE"); ns != "" {
		return ns
	}
	if data, err := os.ReadFile(namespacePath); err == nil {
		return strings.TrimSpace(string(data))
	}
	return ""
}

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(bootcv1alpha1.AddToScheme(scheme))
}

func main() {
	var enableLeaderElection bool
	var probeAddr string
	var tagResolutionInterval time.Duration
	var allowInsecureRegistry bool
	flag.StringVar(
		&probeAddr,
		"health-probe-bind-address",
		":8081",
		"The address the probe endpoint binds to.",
	)
	flag.DurationVar(
		&tagResolutionInterval,
		"tag-resolution-interval",
		time.Duration(bootcv1alpha1.DefaultTagResolutionPeriodSeconds)*time.Second,
		"How often to re-resolve tag-based image refs.",
	)
	flag.BoolVar(
		&allowInsecureRegistry,
		"allow-insecure-registry",
		false,
		"Allow falling back to HTTP when resolving tag-based image refs against registries that do not serve TLS.",
	)
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
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
		"controller",
		"version",
		version.Version,
		"commit",
		version.GitCommit,
	)

	ctx := ctrl.SetupSignalHandler()
	restConfig := ctrl.GetConfigOrDie()
	operatorConfig, err := config.Load(ctx, restConfig, scheme)
	if err != nil {
		setupLog.Error(err, "Failed to load operator configuration")
		os.Exit(1)
	}
	if err := config.ApplyToFlags(flag.CommandLine, operatorConfig); err != nil {
		setupLog.Error(err, "Failed to apply operator configuration")
		os.Exit(1)
	}
	config.LogSource(
		setupLog,
		operatorConfig,
		explicit,
		"allow-insecure-registry",
		"tag-resolution-interval",
	)

	mgr, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme:                 scheme,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "bootc-operator.bootc.dev",
	})
	if err != nil {
		setupLog.Error(err, "Failed to start manager")
		os.Exit(1)
	}

	kubeClient, err := kubernetes.NewForConfig(mgr.GetConfig())
	if err != nil {
		setupLog.Error(err, "Failed to create Kubernetes clientset")
		os.Exit(1)
	}

	if err := (&controller.BootcNodePoolReconciler{
		Client:                mgr.GetClient(),
		Scheme:                mgr.GetScheme(),
		KubeClient:            kubeClient,
		EventNamespace:        detectNamespace(),
		TagResolver:           &registry.GGCRResolver{AllowInsecure: allowInsecureRegistry},
		TagResolutionInterval: tagResolutionInterval,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "bootcnodepool")
		os.Exit(1)
	}

	// +kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("Starting manager")
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "Failed to run manager")
		os.Exit(1)
	}
}
