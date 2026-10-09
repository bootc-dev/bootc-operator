// SPDX-License-Identifier: Apache-2.0

package config

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	bootcv1alpha1 "github.com/bootc-dev/bootc-operator/api/v1alpha1"
)

// Bound the bootstrap read without imposing a timeout on the manager's watches.
const startupTimeout = 10 * time.Second

// Load lists configurations directly from the API before caches start. An empty
// list permits defaults; installation, permission, and network failures do not.
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
	// This client only lists our known resource, so API discovery is unnecessary.
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
		return nil, fmt.Errorf("create client for BootcOperatorConfig: %w", err)
	}
	var configs bootcv1alpha1.BootcOperatorConfigList
	if err := c.List(ctx, &configs); err != nil {
		return nil, fmt.Errorf(
			"list BootcOperatorConfig: ensure the bootcoperatorconfigs CRD is installed, "+
				"the API server is reachable, and this service account can list bootcoperatorconfigs: %w",
			err,
		)
	}
	if len(configs.Items) == 0 {
		return nil, nil
	}
	if len(configs.Items) > 1 {
		names := make([]string, 0, len(configs.Items))
		for _, cfg := range configs.Items {
			names = append(names, cfg.Name)
		}
		sort.Strings(names)
		return nil, fmt.Errorf("found multiple BootcOperatorConfig resources (%s); keep only one",
			strings.Join(names, ", "))
	}
	cfg := &configs.Items[0]
	if err := validate(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func validate(cfg *bootcv1alpha1.BootcOperatorConfig) error {
	if cfg.Name == "" {
		return fmt.Errorf("BootcOperatorConfig list returned an object without a name")
	}
	if c := cfg.Spec.Controller; c != nil && c.TagResolutionPeriodSeconds != nil &&
		*c.TagResolutionPeriodSeconds < 1 {
		return fmt.Errorf(
			"BootcOperatorConfig/%s spec.controller.tagResolutionPeriodSeconds must be at least 1",
			cfg.Name,
		)
	}
	if d := cfg.Spec.Daemon; d != nil && d.StatusPollPeriodSeconds != nil &&
		*d.StatusPollPeriodSeconds < 1 {
		return fmt.Errorf(
			"BootcOperatorConfig/%s spec.daemon.statusPollPeriodSeconds must be at least 1",
			cfg.Name,
		)
	}
	return nil
}
