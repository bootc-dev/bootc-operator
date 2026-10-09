// SPDX-License-Identifier: Apache-2.0

package config

import (
	"errors"
	"flag"
	"math"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	"k8s.io/utils/ptr"

	bootcv1alpha1 "github.com/bootc-dev/bootc-operator/api/v1alpha1"
)

func TestApplyToFlags(t *testing.T) {
	for _, tt := range []struct {
		name       string
		controller *bootcv1alpha1.OperatorControllerConfig
		daemon     *bootcv1alpha1.OperatorDaemonConfig
		args       []string
		want       bool
		wantTag    time.Duration
		wantPoll   time.Duration
	}{
		{name: "absent instance", wantTag: 5 * time.Minute, wantPoll: 5 * time.Minute},
		{
			name: "empty sections", controller: &bootcv1alpha1.OperatorControllerConfig{},
			daemon: &bootcv1alpha1.OperatorDaemonConfig{}, wantTag: 5 * time.Minute, wantPoll: 5 * time.Minute,
		},
		{
			name: "partial controller", controller: &bootcv1alpha1.OperatorControllerConfig{AllowInsecureRegistry: ptr.To(true)},
			want: true, wantTag: 5 * time.Minute, wantPoll: 5 * time.Minute,
		},
		{
			name:       "configured periods",
			controller: &bootcv1alpha1.OperatorControllerConfig{TagResolutionPeriodSeconds: ptr.To(int32(10))},
			daemon:     &bootcv1alpha1.OperatorDaemonConfig{StatusPollPeriodSeconds: ptr.To(int32(17))},
			wantTag:    10 * time.Second, wantPoll: 17 * time.Second,
		},
		{
			name:       "maximum periods do not overflow",
			controller: &bootcv1alpha1.OperatorControllerConfig{TagResolutionPeriodSeconds: ptr.To(int32(math.MaxInt32))},
			daemon:     &bootcv1alpha1.OperatorDaemonConfig{StatusPollPeriodSeconds: ptr.To(int32(math.MaxInt32))},
			wantTag:    time.Duration(math.MaxInt32) * time.Second,
			wantPoll:   time.Duration(math.MaxInt32) * time.Second,
		},
		{
			name: "explicit false and fractional durations override CR",
			controller: &bootcv1alpha1.OperatorControllerConfig{
				AllowInsecureRegistry: ptr.To(true), TagResolutionPeriodSeconds: ptr.To(int32(10)),
			},
			daemon: &bootcv1alpha1.OperatorDaemonConfig{StatusPollPeriodSeconds: ptr.To(int32(17))},
			args: []string{
				"--allow-insecure-registry=false",
				"--tag-resolution-interval=1500ms",
				"--bootc-poll-interval=500ms",
			},
			wantTag:  1500 * time.Millisecond,
			wantPoll: 500 * time.Millisecond,
		},
		{
			name:       "explicit true overrides false",
			controller: &bootcv1alpha1.OperatorControllerConfig{AllowInsecureRegistry: ptr.To(false)},
			args:       []string{"--allow-insecure-registry"}, want: true,
			wantTag: 5 * time.Minute, wantPoll: 5 * time.Minute,
		},
		{
			name:       "CLI retains zero and negative duration behavior",
			controller: &bootcv1alpha1.OperatorControllerConfig{TagResolutionPeriodSeconds: ptr.To(int32(10))},
			daemon:     &bootcv1alpha1.OperatorDaemonConfig{StatusPollPeriodSeconds: ptr.To(int32(17))},
			args:       []string{"--tag-resolution-interval=0", "--bootc-poll-interval=-1s"},
			wantPoll:   -time.Second,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			fs := flag.NewFlagSet(t.Name(), flag.ContinueOnError)
			var allowInsecure bool
			var tagInterval, pollInterval time.Duration
			fs.BoolVar(&allowInsecure, "allow-insecure-registry", false, "")
			fs.DurationVar(&tagInterval, "tag-resolution-interval", 5*time.Minute, "")
			fs.DurationVar(&pollInterval, "bootc-poll-interval", 5*time.Minute, "")
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
			g.Expect(ApplyToFlags(fs, cfg)).To(Succeed())
			g.Expect(allowInsecure).To(Equal(tt.want))
			g.Expect(tagInterval).To(Equal(tt.wantTag))
			g.Expect(pollInterval).To(Equal(tt.wantPoll))
			g.Expect(ExplicitFlags(fs)).
				To(Equal(explicit), "configuration must not mark flags as explicit")
		})
	}
}

func TestApplyToFlagsSkipsOtherComponent(t *testing.T) {
	g := NewWithT(t)
	fs := flag.NewFlagSet(t.Name(), flag.ContinueOnError)
	var pollInterval time.Duration
	fs.DurationVar(&pollInterval, "bootc-poll-interval", 5*time.Minute, "")
	cfg := &bootcv1alpha1.BootcOperatorConfig{
		Spec: bootcv1alpha1.BootcOperatorConfigSpec{
			Controller: &bootcv1alpha1.OperatorControllerConfig{
				AllowInsecureRegistry: ptr.To(true),
			},
			Daemon: &bootcv1alpha1.OperatorDaemonConfig{
				StatusPollPeriodSeconds: ptr.To(int32(17)),
			},
		},
	}
	g.Expect(ApplyToFlags(fs, cfg)).To(Succeed())
	g.Expect(pollInterval).To(Equal(17 * time.Second))
}

func TestApplyToFlagsReportsSetterError(t *testing.T) {
	g := NewWithT(t)
	fs := flag.NewFlagSet(t.Name(), flag.ContinueOnError)
	fs.Func("allow-insecure-registry", "", func(string) error { return errors.New("rejected") })
	cfg := &bootcv1alpha1.BootcOperatorConfig{
		Spec: bootcv1alpha1.BootcOperatorConfigSpec{
			Controller: &bootcv1alpha1.OperatorControllerConfig{
				AllowInsecureRegistry: ptr.To(true),
			},
		},
	}
	g.Expect(ApplyToFlags(fs, cfg)).
		To(MatchError(ContainSubstring("--allow-insecure-registry: rejected")))
}
