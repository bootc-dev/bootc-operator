// SPDX-License-Identifier: Apache-2.0

package config

import (
	"context"
	"errors"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	bootcv1alpha1 "github.com/bootc-dev/bootc-operator/api/v1alpha1"
)

// Bound the bootstrap read without imposing a timeout on the manager's watches.
const startupTimeout = 10 * time.Second

// Load reads the optional singleton directly from the API before caches start.
// Only an absent instance permits defaults; installation, permission, and network
// failures must not silently change the operator's configuration.
func Load(
	ctx context.Context,
	restConfig *rest.Config,
	scheme *runtime.Scheme,
) (*bootcv1alpha1.BootcOperatorConfig, error) {
	return load(ctx, restConfig, scheme, startupTimeout)
}

func load(
	ctx context.Context,
	restConfig *rest.Config,
	scheme *runtime.Scheme,
	timeout time.Duration,
) (*bootcv1alpha1.BootcOperatorConfig, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	bootstrap := rest.CopyConfig(restConfig)
	bootstrap.Timeout = timeout
	// This client only reads our known singleton, so API discovery is unnecessary.
	gv := bootcv1alpha1.GroupVersion
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{gv})
	mapper.AddSpecific(
		gv.WithKind("BootcOperatorConfig"),
		gv.WithResource(
			"bootcoperatorconfigs",
		),
		gv.WithResource("bootcoperatorconfig"),
		meta.RESTScopeRoot,
	)
	c, err := client.New(bootstrap, client.Options{Scheme: scheme, Mapper: mapper})
	if err != nil {
		return nil, fmt.Errorf("create client for BootcOperatorConfig/cluster: %w", err)
	}
	cfg := &bootcv1alpha1.BootcOperatorConfig{}
	if err := c.Get(
		ctx,
		client.ObjectKey{Name: bootcv1alpha1.BootcOperatorConfigName},
		cfg,
	); err != nil {
		var status apierrors.APIStatus
		if apierrors.IsNotFound(err) && errors.As(err, &status) {
			details := status.Status().Details
			if details != nil && details.Name == bootcv1alpha1.BootcOperatorConfigName &&
				details.Group == bootcv1alpha1.GroupVersion.Group && details.Kind == "bootcoperatorconfigs" {
				// client-go synthesizes matching details for an unstructured HTTP
				// 404 too. Such endpoint failures do not establish object absence.
				if !apierrors.IsUnexpectedServerError(err) {
					return nil, nil
				}
			}
		}
		return nil, fmt.Errorf(
			"load BootcOperatorConfig/cluster: ensure the bootcoperatorconfigs CRD is installed, "+
				"the API server is reachable, and this service account can get bootcoperatorconfigs/cluster: %w",
			err,
		)
	}
	if cfg.Name != bootcv1alpha1.BootcOperatorConfigName {
		return nil, fmt.Errorf(
			"load BootcOperatorConfig/cluster: API returned unexpected object name %q",
			cfg.Name,
		)
	}
	if err := validate(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func validate(cfg *bootcv1alpha1.BootcOperatorConfig) error {
	if c := cfg.Spec.Controller; c != nil && c.TagResolutionPeriodSeconds != nil &&
		*c.TagResolutionPeriodSeconds < 1 {
		return fmt.Errorf(
			"BootcOperatorConfig/cluster spec.controller.tagResolutionPeriodSeconds must be at least 1",
		)
	}
	if d := cfg.Spec.Daemon; d != nil && d.StatusPollPeriodSeconds != nil &&
		*d.StatusPollPeriodSeconds < 1 {
		return fmt.Errorf(
			"BootcOperatorConfig/cluster spec.daemon.statusPollPeriodSeconds must be at least 1",
		)
	}
	return nil
}
