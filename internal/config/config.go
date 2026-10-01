// SPDX-License-Identifier: Apache-2.0

package config

import (
	"flag"
	"time"

	"github.com/go-logr/logr"

	bootcv1alpha1 "github.com/bootc-dev/bootc-operator/api/v1alpha1"
)

// ControllerOptions holds the controller's effective startup configuration.
type ControllerOptions struct {
	AllowInsecureRegistry bool
	TagResolutionInterval time.Duration
}

// DaemonOptions holds the daemon's effective startup configuration.
type DaemonOptions struct {
	PollInterval time.Duration
}

// ExplicitFlags snapshots flags supplied on the command line, including false
// boolean values. Call it immediately after parsing, before applying configuration.
func ExplicitFlags(fs *flag.FlagSet) map[string]bool {
	explicit := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
	return explicit
}

// ResolveController overlays CR fields on the parsed CLI defaults unless a flag
// was explicitly supplied. cfg must have been validated by Load. CLI durations
// retain their precision and semantics.
func ResolveController(
	cfg *bootcv1alpha1.BootcOperatorConfig,
	cli ControllerOptions,
	explicit map[string]bool,
) ControllerOptions {
	if cfg != nil && cfg.Spec.Controller != nil {
		spec := cfg.Spec.Controller
		if spec.AllowInsecureRegistry != nil && !explicit["allow-insecure-registry"] {
			cli.AllowInsecureRegistry = *spec.AllowInsecureRegistry
		}
		if spec.TagResolutionPeriodSeconds != nil && !explicit["tag-resolution-interval"] {
			cli.TagResolutionInterval = time.Duration(
				*spec.TagResolutionPeriodSeconds,
			) * time.Second
		}
	}
	return cli
}

// ResolveDaemon overlays CR fields on the parsed CLI defaults unless a flag was
// explicitly supplied. cfg must have been validated by Load.
func ResolveDaemon(
	cfg *bootcv1alpha1.BootcOperatorConfig,
	cli DaemonOptions,
	explicit map[string]bool,
) DaemonOptions {
	if cfg != nil && cfg.Spec.Daemon != nil {
		if seconds := cfg.Spec.Daemon.StatusPollPeriodSeconds; seconds != nil &&
			!explicit["bootc-poll-interval"] {
			cli.PollInterval = time.Duration(*seconds) * time.Second
		}
	}
	return cli
}

// LogSource records the selected resource and CLI overrides without logging
// credentials or bootstrap flags. Supported flags are supplied by the component.
func LogSource(
	log logr.Logger,
	cfg *bootcv1alpha1.BootcOperatorConfig,
	explicit map[string]bool,
	flags ...string,
) {
	overrides := make([]string, 0, len(flags))
	for _, name := range flags {
		if explicit[name] {
			overrides = append(overrides, name)
		}
	}
	if cfg == nil {
		log.Info("Loaded operator configuration", "source", "defaults", "cliOverrides", overrides)
		return
	}
	log.Info("Loaded operator configuration", "source", "BootcOperatorConfig/"+cfg.Name,
		"uid", cfg.UID, "generation", cfg.Generation, "cliOverrides", overrides)
}
