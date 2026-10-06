// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"flag"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	bootcv1alpha1 "github.com/bootc-dev/bootc-operator/api/v1alpha1"
	operatorconfig "github.com/bootc-dev/bootc-operator/internal/config"
)

// Omitted fields must remain absent so CLI values can supply the fallback.
func TestBootcOperatorConfigDefaults(t *testing.T) {
	for _, tc := range []struct {
		name, spec        string
		wantInsecure      *bool
		wantTag, wantPoll *int32
	}{
		{"empty spec", `{}`, nil, nil, nil},
		{"partial controller", `{"controller":{"allowInsecureRegistry":true}}`, ptr.To(true), nil, nil},
		{"partial daemon", `{"daemon":{"statusPollPeriodSeconds":17}}`, nil, nil, ptr.To(int32(17))},
		{"explicit false", `{"controller":{"allowInsecureRegistry":false,"tagResolutionPeriodSeconds":10}}`, ptr.To(false), ptr.To(int32(10)), nil},
		{"null sections", `{"controller":null,"daemon":null}`, nil, nil, nil},
		{"null fields", `{"controller":{"allowInsecureRegistry":null,"tagResolutionPeriodSeconds":null},"daemon":{"statusPollPeriodSeconds":null}}`, nil, nil, nil},
		{"minimum periods", `{"controller":{"tagResolutionPeriodSeconds":1},"daemon":{"statusPollPeriodSeconds":1}}`, nil, ptr.To(int32(1)), ptr.To(int32(1))},
		{"maximum periods", `{"controller":{"tagResolutionPeriodSeconds":2147483647},"daemon":{"statusPollPeriodSeconds":2147483647}}`, nil, ptr.To(int32(2147483647)), ptr.To(int32(2147483647))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			ctx := context.Background()
			object := operatorConfigObject(t, "cluster", tc.spec)
			g.Expect(k8sClient.Create(ctx, object)).To(Succeed())
			t.Cleanup(
				func() { g.Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, object))).To(Succeed()) },
			)
			var got bootcv1alpha1.BootcOperatorConfig
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(object), &got)).To(Succeed())
			g.Expect(got.Spec).To(Equal(bootcv1alpha1.BootcOperatorConfigSpec{
				Controller: &bootcv1alpha1.OperatorControllerConfig{
					AllowInsecureRegistry:      tc.wantInsecure,
					TagResolutionPeriodSeconds: tc.wantTag,
				},
				Daemon: &bootcv1alpha1.OperatorDaemonConfig{
					StatusPollPeriodSeconds: tc.wantPoll,
				},
			}))
		})
	}
}

func TestBootcOperatorConfigValidation(t *testing.T) {
	for _, tc := range []struct {
		name       string
		spec       string
		field      string
		badRequest bool
	}{
		{"missing spec", "", "spec", false},
		{"null spec", `null`, "spec", false},
		{"zero tag period", `{"controller":{"tagResolutionPeriodSeconds":0}}`, "spec.controller.tagResolutionPeriodSeconds", false},
		{"negative tag period", `{"controller":{"tagResolutionPeriodSeconds":-1}}`, "spec.controller.tagResolutionPeriodSeconds", false},
		{"zero poll period", `{"daemon":{"statusPollPeriodSeconds":0}}`, "spec.daemon.statusPollPeriodSeconds", false},
		{"negative poll period", `{"daemon":{"statusPollPeriodSeconds":-1}}`, "spec.daemon.statusPollPeriodSeconds", false},
		{"overflowing tag period", `{"controller":{"tagResolutionPeriodSeconds":2147483648}}`, "spec.controller.tagResolutionPeriodSeconds", false},
		{"overflowing poll period", `{"daemon":{"statusPollPeriodSeconds":2147483648}}`, "spec.daemon.statusPollPeriodSeconds", false},
		{"string period", `{"daemon":{"statusPollPeriodSeconds":"10s"}}`, "spec.daemon.statusPollPeriodSeconds", false},
		{"fractional period", `{"controller":{"tagResolutionPeriodSeconds":1.5}}`, "spec.controller.tagResolutionPeriodSeconds", false},
		{"string boolean", `{"controller":{"allowInsecureRegistry":"true"}}`, "spec.controller.allowInsecureRegistry", false},
		{"namespace list type", `{"controller":{"pullSecretNamespaces":"team-a"}}`, "spec.controller.pullSecretNamespaces", false},
		{"namespace item type", `{"controller":{"pullSecretNamespaces":[1]}}`, "spec.controller.pullSecretNamespaces", false},
		{"empty namespace", `{"controller":{"pullSecretNamespaces":[""]}}`, "spec.controller.pullSecretNamespaces", false},
		{"uppercase namespace", `{"controller":{"pullSecretNamespaces":["Team-A"]}}`, "spec.controller.pullSecretNamespaces", false},
		{"namespace whitespace", `{"controller":{"pullSecretNamespaces":[" team-a"]}}`, "spec.controller.pullSecretNamespaces", false},
		{"namespace comma", `{"controller":{"pullSecretNamespaces":["team-a,team-b"]}}`, "spec.controller.pullSecretNamespaces", false},
		{"namespace too long", `{"controller":{"pullSecretNamespaces":["` + strings.Repeat("a", 64) + `"]}}`, "spec.controller.pullSecretNamespaces", false},
		{"duplicate namespaces", `{"controller":{"pullSecretNamespaces":["team-a","team-a"]}}`, "spec.controller.pullSecretNamespaces", false},
		{"unknown field", `{"controller":{"tagResolutionPeriodSecond":10}}`, "spec.controller.tagResolutionPeriodSecond", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			object := operatorConfigObject(t, "cluster", tc.spec)
			err := k8sClient.Create(context.Background(), object,
				client.DryRunAll, client.FieldValidation(metav1.FieldValidationStrict))
			if tc.badRequest {
				g.Expect(err).To(MatchError(apierrors.IsBadRequest, "IsBadRequest"))
			} else {
				g.Expect(err).To(MatchError(apierrors.IsInvalid, "IsInvalid"))
			}
			g.Expect(err.Error()).To(ContainSubstring(tc.field))
		})
	}
}

func TestBootcOperatorConfigRoundTrip(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	loaded, err := operatorconfig.Load(ctx, testEnv.Config, k8sClient.Scheme())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(loaded).To(BeNil(), "an absent instance permits startup defaults")
	config := &bootcv1alpha1.BootcOperatorConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "custom-config"},
		Spec: bootcv1alpha1.BootcOperatorConfigSpec{
			Controller: &bootcv1alpha1.OperatorControllerConfig{
				AllowInsecureRegistry:      ptr.To(true),
				TagResolutionPeriodSeconds: ptr.To(int32(10)),
				PullSecretNamespaces:       []string{"team-a", strings.Repeat("a", 63)},
			},
			Daemon: &bootcv1alpha1.OperatorDaemonConfig{
				StatusPollPeriodSeconds: ptr.To(int32(17)),
			},
		},
	}
	wantSpec := *config.Spec.DeepCopy()
	g.Expect(k8sClient.Create(ctx, config)).To(Succeed())
	t.Cleanup(func() {
		g.Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, config))).To(Succeed())
	})
	got, err := operatorconfig.Load(ctx, testEnv.Config, k8sClient.Scheme())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(got).NotTo(BeNil())
	g.Expect(got.Name).To(Equal("custom-config"))
	g.Expect(got.Spec).To(Equal(wantSpec))

	startup := got
	updated := got.DeepCopy()
	updated.Spec.Controller.AllowInsecureRegistry = ptr.To(false)
	updated.Spec.Controller.PullSecretNamespaces = []string{"team-b"}
	updated.Spec.Daemon.StatusPollPeriodSeconds = ptr.To(int32(23))
	wantSpec = *updated.Spec.DeepCopy()
	g.Expect(k8sClient.Update(ctx, updated)).To(Succeed())
	got, err = operatorconfig.Load(ctx, testEnv.Config, k8sClient.Scheme())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(got).NotTo(BeNil())
	g.Expect(got.Spec).To(Equal(wantSpec))
	g.Expect(got.UID).To(Equal(config.UID))
	g.Expect(got.Generation).To(Equal(config.Generation + 1))
	g.Expect(startup.Spec).
		To(Equal(config.Spec), "previously loaded configuration remains a snapshot")

	g.Expect(k8sClient.Delete(ctx, got)).To(Succeed())
	loaded, err = operatorconfig.Load(ctx, testEnv.Config, k8sClient.Scheme())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(loaded).To(BeNil(), "deleting the instance restores defaults on the next load")
}

// Typed writes must preserve an explicitly empty list, which overrides CLI
// namespaces. Explicit values matching built-in defaults also override CLI values.
func TestBootcOperatorConfigEmptyNamespaces(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	cfg := &bootcv1alpha1.BootcOperatorConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec: bootcv1alpha1.BootcOperatorConfigSpec{
			Controller: &bootcv1alpha1.OperatorControllerConfig{
				PullSecretNamespaces:       []string{},
				TagResolutionPeriodSeconds: ptr.To(int32(300)),
			},
		},
	}
	g.Expect(k8sClient.Create(ctx, cfg)).To(Succeed())
	t.Cleanup(func() { g.Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, cfg))).To(Succeed()) })
	for _, update := range []bool{false, true} {
		if update {
			cfg.Labels = map[string]string{"test": "updated"}
			g.Expect(k8sClient.Update(ctx, cfg)).To(Succeed())
		}
		g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(cfg), cfg)).To(Succeed())
		g.Expect(cfg.Spec.Controller.PullSecretNamespaces).To(Equal([]string{}))
		loaded, err := operatorconfig.Load(ctx, testEnv.Config, k8sClient.Scheme())
		g.Expect(err).NotTo(HaveOccurred())
		fs := flag.NewFlagSet(t.Name(), flag.ContinueOnError)
		namespaces := fs.String(operatorconfig.PullSecretNamespacesFlag, "", "")
		interval := fs.Duration(operatorconfig.TagResolutionIntervalFlag, 5*time.Minute, "")
		g.Expect(fs.Parse([]string{"--pull-secret-namespaces=team-a", "--tag-resolution-interval=10s"})).
			To(Succeed())
		g.Expect(operatorconfig.ApplyToFlags(fs, loaded)).To(Succeed())
		g.Expect(*namespaces).To(BeEmpty())
		g.Expect(*interval).
			To(Equal(5*time.Minute), "explicit default-valued fields override CLI")
	}
	cfg.Spec.Controller.PullSecretNamespaces = nil
	cfg.Spec.Controller.TagResolutionPeriodSeconds = nil
	g.Expect(k8sClient.Update(ctx, cfg)).To(Succeed())
	loaded, err := operatorconfig.Load(ctx, testEnv.Config, k8sClient.Scheme())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(loaded.Spec.Controller.PullSecretNamespaces).To(BeNil())
	g.Expect(loaded.Spec.Controller.TagResolutionPeriodSeconds).To(BeNil())
}

func TestBootcOperatorConfigMultipleInstances(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	for _, name := range []string{"operator-one", "operator-two"} {
		object := operatorConfigObject(t, name, `{}`)
		g.Expect(k8sClient.Create(ctx, object)).To(Succeed(), "the API allows arbitrary names")
		t.Cleanup(func() {
			g.Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, object))).To(Succeed())
		})
	}

	loaded, err := operatorconfig.Load(ctx, testEnv.Config, k8sClient.Scheme())
	g.Expect(loaded).To(BeNil())
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(And(
		ContainSubstring("operator-one"),
		ContainSubstring("operator-two"),
	))
}

func TestBootcOperatorConfigMissingCRD(t *testing.T) {
	g := NewWithT(t)
	// A separate API server keeps the missing-CRD case isolated from the suite.
	env := &envtest.Environment{}
	restConfig, err := env.Start()
	g.Expect(err).NotTo(HaveOccurred())
	t.Cleanup(func() { g.Expect(env.Stop()).To(Succeed()) })

	loaded, err := operatorconfig.Load(context.Background(), restConfig, k8sClient.Scheme())
	g.Expect(err).To(MatchError(apierrors.IsNotFound, "IsNotFound"))
	g.Expect(err.Error()).To(ContainSubstring("ensure the bootcoperatorconfigs CRD is installed"))
	g.Expect(loaded).To(BeNil())
}

// Use unstructured objects to exercise missing, null, and invalid fields that
// typed Go objects cannot represent on the wire.
func operatorConfigObject(t *testing.T, name, spec string) *unstructured.Unstructured {
	t.Helper()
	object := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": bootcv1alpha1.GroupVersion.String(),
		"kind":       "BootcOperatorConfig",
		"metadata":   map[string]any{"name": name},
	}}
	if spec != "" {
		var value any
		NewWithT(t).Expect(json.Unmarshal([]byte(spec), &value)).To(Succeed())
		object.Object["spec"] = value
	}
	return object
}
