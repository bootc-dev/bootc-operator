// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	sigsyaml "sigs.k8s.io/yaml"

	bootcv1alpha1 "github.com/bootc-dev/bootc-operator/api/v1alpha1"
	operatorconfig "github.com/bootc-dev/bootc-operator/internal/config"
)

func TestBootcOperatorConfigDefaults(t *testing.T) {
	const (
		tagDefault  = bootcv1alpha1.DefaultTagResolutionPeriodSeconds
		pollDefault = bootcv1alpha1.DefaultStatusPollPeriodSeconds
	)
	for _, tc := range []struct {
		name         string
		spec         string
		wantInsecure bool
		wantTag      int32
		wantPoll     int32
	}{
		{"empty spec", `{}`, false, tagDefault, pollDefault},
		{"partial controller", `{"controller":{"allowInsecureRegistry":true}}`, true, tagDefault, pollDefault},
		{"partial daemon", `{"daemon":{"statusPollPeriodSeconds":17}}`, false, tagDefault, 17},
		{"explicit false", `{"controller":{"allowInsecureRegistry":false,"tagResolutionPeriodSeconds":10}}`, false, 10, pollDefault},
		{"null sections", `{"controller":null,"daemon":null}`, false, tagDefault, pollDefault},
		{"null fields", `{"controller":{"allowInsecureRegistry":null,"tagResolutionPeriodSeconds":null},"daemon":{"statusPollPeriodSeconds":null}}`, false, tagDefault, pollDefault},
		{"minimum periods", `{"controller":{"tagResolutionPeriodSeconds":1},"daemon":{"statusPollPeriodSeconds":1}}`, false, 1, 1},
		{"maximum periods", `{"controller":{"tagResolutionPeriodSeconds":2147483647},"daemon":{"statusPollPeriodSeconds":2147483647}}`, false, 2147483647, 2147483647},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			ctx := context.Background()
			object := operatorConfigObject(t, "cluster", tc.spec)
			g.Expect(k8sClient.Create(ctx, object)).To(Succeed())
			t.Cleanup(func() {
				g.Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, object))).To(Succeed())
			})

			var got bootcv1alpha1.BootcOperatorConfig
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(object), &got)).To(Succeed())
			g.Expect(got.Spec).To(Equal(bootcv1alpha1.BootcOperatorConfigSpec{
				Controller: &bootcv1alpha1.OperatorControllerConfig{
					AllowInsecureRegistry:      ptr.To(tc.wantInsecure),
					TagResolutionPeriodSeconds: ptr.To(tc.wantTag),
				},
				Daemon: &bootcv1alpha1.OperatorDaemonConfig{
					StatusPollPeriodSeconds: ptr.To(tc.wantPoll),
				},
			}))
		})
	}
}

func TestBootcOperatorConfigReleaseExample(t *testing.T) {
	g := NewWithT(t)
	data, err := os.ReadFile("../../config/samples/bootc_v1alpha1_bootcoperatorconfig.yaml")
	g.Expect(err).NotTo(HaveOccurred())

	var example bootcv1alpha1.BootcOperatorConfig
	g.Expect(sigsyaml.UnmarshalStrict(data, &example)).To(Succeed())
	g.Expect(example.APIVersion).To(Equal(bootcv1alpha1.GroupVersion.String()))
	g.Expect(example.Kind).To(Equal("BootcOperatorConfig"))
	g.Expect(example.Name).To(Equal("cluster"))
	g.Expect(example.Spec).To(Equal(bootcv1alpha1.BootcOperatorConfigSpec{
		Controller: &bootcv1alpha1.OperatorControllerConfig{
			AllowInsecureRegistry:      ptr.To(false),
			TagResolutionPeriodSeconds: ptr.To(bootcv1alpha1.DefaultTagResolutionPeriodSeconds),
		},
		Daemon: &bootcv1alpha1.OperatorDaemonConfig{
			StatusPollPeriodSeconds: ptr.To(bootcv1alpha1.DefaultStatusPollPeriodSeconds),
		},
	}))
	g.Expect(k8sClient.Create(context.Background(), &example,
		client.DryRunAll, client.FieldValidation(metav1.FieldValidationStrict))).To(Succeed())
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
