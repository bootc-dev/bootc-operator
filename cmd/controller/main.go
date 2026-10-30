// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	bootcv1alpha1 "github.com/bootc-dev/bootc-operator/api/v1alpha1"
	"github.com/bootc-dev/bootc-operator/internal/config"
	"github.com/bootc-dev/bootc-operator/internal/controller"
	"github.com/bootc-dev/bootc-operator/internal/pullsecret"
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
	var showVersion bool
	var probeAddr string
	var tagResolutionInterval time.Duration
	var allowInsecureRegistry bool
	var pullSecretNamespacesFlag string
	flag.StringVar(
		&probeAddr,
		"health-probe-bind-address",
		":8081",
		"The address the probe endpoint binds to.",
	)
	flag.DurationVar(
		&tagResolutionInterval,
		config.TagResolutionIntervalFlag,
		time.Duration(bootcv1alpha1.DefaultTagResolutionPeriodSeconds)*time.Second,
		"How often to re-resolve tag-based image refs.",
	)
	flag.BoolVar(
		&allowInsecureRegistry,
		config.AllowInsecureRegistryFlag,
		false,
		"Allow falling back to HTTP when resolving tag-based image refs against registries that do not serve TLS.",
	)
	flag.StringVar(
		&pullSecretNamespacesFlag,
		config.PullSecretNamespacesFlag,
		"",
		"Comma-separated extra namespaces that pools may reference pull secrets from. "+
			"The operator namespace is always allowed.",
	)
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&showVersion, "version", false, "Print version and exit.")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	if showVersion {
		fmt.Println(version.String())
		return
	}
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
	operatorNamespace := detectNamespace()
	if operatorNamespace == "" {
		setupLog.Error(nil, "Operator namespace unknown; set POD_NAMESPACE")
		os.Exit(1)
	}
	pullSecretNamespaces, err := controller.ParsePullSecretNamespaces(
		pullSecretNamespacesFlag,
		operatorNamespace,
	)
	if err != nil {
		setupLog.Error(err, "Invalid pull secret namespace configuration")
		os.Exit(1)
	}
	setupLog.Info("Pull secret namespaces", "namespaces", pullSecretNamespaces)

	config.LogSource(
		setupLog,
		operatorConfig,
		explicit,
		config.AllowInsecureRegistryFlag,
		config.TagResolutionIntervalFlag,
		config.PullSecretNamespacesFlag,
	)

	mgr, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme:                 scheme,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "bootc-operator.bootc.dev",
		Cache: cache.Options{
			ByObject: controller.SecretCacheByObject(pullSecretNamespaces),
		},
	})
	if err != nil {
		setupLog.Error(err, "Failed to start manager")
		os.Exit(1)
	}

	if err := pullsecret.IndexPoolPullSecretRef(
		context.Background(),
		mgr.GetFieldIndexer(),
	); err != nil {
		setupLog.Error(err, "Failed to index pools by pull secret")
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
		EventNamespace:        operatorNamespace,
		TagResolver:           &registry.GGCRResolver{AllowInsecure: allowInsecureRegistry},
		TagResolutionInterval: tagResolutionInterval,
		PullSecretNamespaces:  pullSecretNamespaces,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "bootcnodepool")
		os.Exit(1)
	}

	if err := (&pullsecret.PullSecretAggregator{
		Client:               mgr.GetClient(),
		Namespace:            operatorNamespace,
		PullSecretNamespaces: pullSecretNamespaces,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "pullsecretaggregator")
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
