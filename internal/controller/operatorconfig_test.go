// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"testing"

	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	bootcv1alpha1 "github.com/bootc-dev/bootc-operator/api/v1alpha1"
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
