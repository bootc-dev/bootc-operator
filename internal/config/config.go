// SPDX-License-Identifier: Apache-2.0

package config

import (
	"flag"
	"fmt"
	"strconv"
	"time"

	"github.com/go-logr/logr"

	bootcv1alpha1 "github.com/bootc-dev/bootc-operator/api/v1alpha1"
)

// ExplicitFlags snapshots flags supplied on the command line, including false
// boolean values. Call it immediately after parsing, before applying configuration.
func ExplicitFlags(fs *flag.FlagSet) map[string]bool {
	explicit := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
	return explicit
}

// ApplyToFlags applies fields supported by this process to parsed flag values.
// It leaves explicitly supplied flags untouched, including false and fractional
// durations. Setting Flag.Value directly does not mark CR values as CLI flags.
// cfg must have been validated by Load.
func ApplyToFlags(fs *flag.FlagSet, cfg *bootcv1alpha1.BootcOperatorConfig) error {
	if cfg == nil {
		return nil
	}
	explicit := ExplicitFlags(fs)
	set := func(name, value string) error {
		f := fs.Lookup(name)
		if f == nil || explicit[name] {
			return nil // The other process may not register this flag.
		}
		if err := f.Value.Set(value); err != nil {
			return fmt.Errorf("apply BootcOperatorConfig/%s to --%s: %w", cfg.Name, name, err)
		}
		return nil
	}
	if c := cfg.Spec.Controller; c != nil {
		if c.AllowInsecureRegistry != nil {
			if err := set(
				"allow-insecure-registry",
				strconv.FormatBool(*c.AllowInsecureRegistry),
			); err != nil {
				return err
			}
		}
		if c.TagResolutionPeriodSeconds != nil {
			period := time.Duration(*c.TagResolutionPeriodSeconds) * time.Second
			if err := set("tag-resolution-interval", period.String()); err != nil {
				return err
			}
		}
	}
	if d := cfg.Spec.Daemon; d != nil && d.StatusPollPeriodSeconds != nil {
		period := time.Duration(*d.StatusPollPeriodSeconds) * time.Second
		if err := set("bootc-poll-interval", period.String()); err != nil {
			return err
		}
	}
	return nil
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
