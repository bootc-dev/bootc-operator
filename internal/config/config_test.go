// SPDX-License-Identifier: Apache-2.0

package config

import (
	"flag"
	"math"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	"k8s.io/utils/ptr"

	bootcv1alpha1 "github.com/bootc-dev/bootc-operator/api/v1alpha1"
)

func TestResolve(t *testing.T) {
	defaults := ControllerOptions{TagResolutionInterval: 5 * time.Minute}
	daemonDefaults := DaemonOptions{PollInterval: 5 * time.Minute}
	for _, tt := range []struct {
		name       string
		controller *bootcv1alpha1.OperatorControllerConfig
		daemon     *bootcv1alpha1.OperatorDaemonConfig
		args       []string
		want       ControllerOptions
		wantDaemon DaemonOptions
	}{
		{name: "absent instance", want: defaults, wantDaemon: daemonDefaults},
		{
			name: "empty sections", controller: &bootcv1alpha1.OperatorControllerConfig{},
			daemon: &bootcv1alpha1.OperatorDaemonConfig{}, want: defaults, wantDaemon: daemonDefaults,
		},
		{
			name: "partial controller", controller: &bootcv1alpha1.OperatorControllerConfig{AllowInsecureRegistry: ptr.To(true)},
			want: ControllerOptions{true, 5 * time.Minute}, wantDaemon: daemonDefaults,
		},
		{
			name:       "configured periods",
			controller: &bootcv1alpha1.OperatorControllerConfig{TagResolutionPeriodSeconds: ptr.To(int32(10))},
			daemon:     &bootcv1alpha1.OperatorDaemonConfig{StatusPollPeriodSeconds: ptr.To(int32(17))},
			want:       ControllerOptions{false, 10 * time.Second}, wantDaemon: DaemonOptions{17 * time.Second},
		},
		{
			name:       "maximum periods do not overflow",
			controller: &bootcv1alpha1.OperatorControllerConfig{TagResolutionPeriodSeconds: ptr.To(int32(math.MaxInt32))},
			daemon:     &bootcv1alpha1.OperatorDaemonConfig{StatusPollPeriodSeconds: ptr.To(int32(math.MaxInt32))},
			want:       ControllerOptions{false, time.Duration(math.MaxInt32) * time.Second},
			wantDaemon: DaemonOptions{time.Duration(math.MaxInt32) * time.Second},
		},
		{
			name: "explicit false and fractional durations override CR",
			controller: &bootcv1alpha1.OperatorControllerConfig{
				AllowInsecureRegistry: ptr.To(true), TagResolutionPeriodSeconds: ptr.To(int32(10)),
			},
			daemon: &bootcv1alpha1.OperatorDaemonConfig{StatusPollPeriodSeconds: ptr.To(int32(17))},
			args:   []string{"--allow-insecure-registry=false", "--tag-resolution-interval=1500ms", "--bootc-poll-interval=500ms"},
			want:   ControllerOptions{false, 1500 * time.Millisecond}, wantDaemon: DaemonOptions{500 * time.Millisecond},
		},
		{
			name:       "explicit true overrides false",
			controller: &bootcv1alpha1.OperatorControllerConfig{AllowInsecureRegistry: ptr.To(false)},
			args:       []string{"--allow-insecure-registry"}, want: ControllerOptions{true, 5 * time.Minute}, wantDaemon: daemonDefaults,
		},
		{
			name:       "CLI retains zero and negative duration behavior",
			controller: &bootcv1alpha1.OperatorControllerConfig{TagResolutionPeriodSeconds: ptr.To(int32(10))},
			daemon:     &bootcv1alpha1.OperatorDaemonConfig{StatusPollPeriodSeconds: ptr.To(int32(17))},
			args:       []string{"--tag-resolution-interval=0", "--bootc-poll-interval=-1s"},
			want:       ControllerOptions{}, wantDaemon: DaemonOptions{-time.Second},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			fs := flag.NewFlagSet(t.Name(), flag.ContinueOnError)
			var cli ControllerOptions
			var daemonCLI DaemonOptions
			fs.BoolVar(&cli.AllowInsecureRegistry, "allow-insecure-registry", false, "")
			fs.DurationVar(&cli.TagResolutionInterval, "tag-resolution-interval",
				time.Duration(bootcv1alpha1.DefaultTagResolutionPeriodSeconds)*time.Second, "")
			fs.DurationVar(&daemonCLI.PollInterval, "bootc-poll-interval",
				time.Duration(bootcv1alpha1.DefaultStatusPollPeriodSeconds)*time.Second, "")
			g.Expect(fs.Parse(tt.args)).To(Succeed())
			explicit := ExplicitFlags(fs)
			var cfg *bootcv1alpha1.BootcOperatorConfig
			if tt.controller != nil || tt.daemon != nil {
				cfg = &bootcv1alpha1.BootcOperatorConfig{
					Spec: bootcv1alpha1.BootcOperatorConfigSpec{
						Controller: tt.controller,
						Daemon:     tt.daemon,
					},
				}
			}
			g.Expect(ResolveController(cfg, cli, explicit)).To(Equal(tt.want))
			g.Expect(ResolveDaemon(cfg, daemonCLI, explicit)).To(Equal(tt.wantDaemon))
		})
	}
}
