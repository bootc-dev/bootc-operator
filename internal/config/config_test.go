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
		name           string
		controller     *bootcv1alpha1.OperatorControllerConfig
		daemon         *bootcv1alpha1.OperatorDaemonConfig
		args           []string
		want           bool
		wantTag        time.Duration
		wantPoll       time.Duration
		wantNamespaces string
	}{
		{name: "absent instance", wantTag: 5 * time.Minute, wantPoll: 5 * time.Minute},
		{
			name:       "omitted CR fields retain CLI values",
			controller: &bootcv1alpha1.OperatorControllerConfig{},
			daemon:     &bootcv1alpha1.OperatorDaemonConfig{},
			args:       []string{"--allow-insecure-registry", "--tag-resolution-interval=1500ms", "--bootc-poll-interval=500ms", "--pull-secret-namespaces=team-a"},
			want:       true, wantTag: 1500 * time.Millisecond, wantPoll: 500 * time.Millisecond, wantNamespaces: "team-a",
		},
		{
			name:           "configured namespaces",
			controller:     &bootcv1alpha1.OperatorControllerConfig{PullSecretNamespaces: []string{"team-a", "team-b"}},
			wantNamespaces: "team-a,team-b", wantTag: 5 * time.Minute, wantPoll: 5 * time.Minute,
		},
		{
			name:       "empty CR namespaces override CLI",
			args:       []string{"--pull-secret-namespaces=team-b"},
			controller: &bootcv1alpha1.OperatorControllerConfig{PullSecretNamespaces: []string{}},
			wantTag:    5 * time.Minute, wantPoll: 5 * time.Minute,
		},
		{
			name:           "CR namespaces override CLI",
			controller:     &bootcv1alpha1.OperatorControllerConfig{PullSecretNamespaces: []string{"team-a"}},
			args:           []string{"--pull-secret-namespaces=team-b"},
			wantNamespaces: "team-a", wantTag: 5 * time.Minute, wantPoll: 5 * time.Minute,
		},
		{
			name:           "CR namespaces override empty CLI",
			controller:     &bootcv1alpha1.OperatorControllerConfig{PullSecretNamespaces: []string{"team-a"}},
			args:           []string{"--pull-secret-namespaces="},
			wantNamespaces: "team-a",
			wantTag:        5 * time.Minute, wantPoll: 5 * time.Minute,
		},
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
			name: "CR overrides explicit false and fractional durations",
			controller: &bootcv1alpha1.OperatorControllerConfig{
				AllowInsecureRegistry: ptr.To(true), TagResolutionPeriodSeconds: ptr.To(int32(10)),
			},
			daemon: &bootcv1alpha1.OperatorDaemonConfig{StatusPollPeriodSeconds: ptr.To(int32(17))},
			args: []string{
				"--allow-insecure-registry=false",
				"--tag-resolution-interval=1500ms",
				"--bootc-poll-interval=500ms",
			},
			want: true, wantTag: 10 * time.Second, wantPoll: 17 * time.Second,
		},
		{
			name:       "CR false overrides explicit true",
			controller: &bootcv1alpha1.OperatorControllerConfig{AllowInsecureRegistry: ptr.To(false)},
			args:       []string{"--allow-insecure-registry"},
			wantTag:    5 * time.Minute, wantPoll: 5 * time.Minute,
		},
		{
			name:       "omitted CR fields retain zero and negative CLI durations",
			controller: &bootcv1alpha1.OperatorControllerConfig{},
			args:       []string{"--tag-resolution-interval=0", "--bootc-poll-interval=-1s"},
			wantPoll:   -time.Second,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			fs := flag.NewFlagSet(t.Name(), flag.ContinueOnError)
			var allowInsecure bool
			namespaces := fs.String(PullSecretNamespacesFlag, "", "")
			var tagInterval, pollInterval time.Duration
			fs.BoolVar(&allowInsecure, AllowInsecureRegistryFlag, false, "")
			fs.DurationVar(&tagInterval, TagResolutionIntervalFlag, 5*time.Minute, "")
			fs.DurationVar(&pollInterval, BootcPollIntervalFlag, 5*time.Minute, "")
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
			g.Expect(*namespaces).To(Equal(tt.wantNamespaces))
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
	fs.DurationVar(&pollInterval, BootcPollIntervalFlag, 5*time.Minute, "")
	cfg := &bootcv1alpha1.BootcOperatorConfig{
		Spec: bootcv1alpha1.BootcOperatorConfigSpec{
			Controller: &bootcv1alpha1.OperatorControllerConfig{
				AllowInsecureRegistry: ptr.To(true),
				PullSecretNamespaces:  []string{"team-a"},
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
	fs.Func(AllowInsecureRegistryFlag, "", func(string) error { return errors.New("rejected") })
	cfg := &bootcv1alpha1.BootcOperatorConfig{
		Spec: bootcv1alpha1.BootcOperatorConfigSpec{
			Controller: &bootcv1alpha1.OperatorControllerConfig{
				AllowInsecureRegistry: ptr.To(true),
				PullSecretNamespaces:  []string{"team-a"},
			},
		},
	}
	g.Expect(ApplyToFlags(fs, cfg)).
		To(MatchError(ContainSubstring("--allow-insecure-registry: rejected")))
}
