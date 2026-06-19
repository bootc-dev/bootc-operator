// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	bootcv1alpha1 "github.com/jlebon/bootc-operator/api/v1alpha1"
	"github.com/jlebon/bootc-operator/internal/bootc"
	testutil "github.com/jlebon/bootc-operator/test/util"
)

const (
	pollInterval = 200 * time.Millisecond
	pollTimeout  = 10 * time.Second

	bootcStatusErrMsg = "bootc status failed"
)

func TestReconcilePopulatesStatus(t *testing.T) {
	g := NewWithT(t)
	g.SetDefaultEventuallyTimeout(pollTimeout)
	g.SetDefaultEventuallyPollingInterval(pollInterval)
	ctx := context.Background()

	v1 := "v1"
	v2 := "v2"
	v3 := "v3"
	fake.status = newBootcStatus(testutil.DigestA)
	fake.status.Status.Booted.Image.Version = &v1
	fake.status.Status.Staged = &bootc.BootEntry{
		Image: &bootc.ImageStatus{
			Image:        bootc.ImageReference{Image: testutil.ImageTaggedRef, Transport: "registry"},
			ImageDigest:  testutil.DigestB,
			Version:      &v2,
			Architecture: "amd64",
		},
		SoftRebootCapable: true,
	}
	fake.status.Status.Rollback = &bootc.BootEntry{
		Image: &bootc.ImageStatus{
			Image:        bootc.ImageReference{Image: testutil.ImageTaggedRef, Transport: "registry"},
			ImageDigest:  testutil.DigestC,
			Version:      &v3,
			Architecture: "amd64",
		},
	}

	bn := testutil.NewNode(testNodeName, testutil.ImageDigestRefA)
	g.Expect(k8sClient.Create(ctx, bn)).To(Succeed())
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, bn)
	})

	g.Eventually(func(g Gomega) {
		var got bootcv1alpha1.BootcNode
		g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(bn), &got)).To(Succeed())

		g.Expect(got.Status.Booted).NotTo(BeNil())
		g.Expect(got.Status.Booted.Image).To(Equal(testutil.ImageTaggedRef))
		g.Expect(got.Status.Booted.ImageDigest).To(Equal(testutil.DigestA))
		g.Expect(got.Status.Booted.Version).To(Equal(v1))
		g.Expect(got.Status.Booted.Architecture).To(Equal("amd64"))

		g.Expect(got.Status.Staged).NotTo(BeNil())
		g.Expect(got.Status.Staged.ImageDigest).To(Equal(testutil.DigestB))
		g.Expect(got.Status.Staged.Version).To(Equal(v2))
		g.Expect(got.Status.Staged.SoftRebootCapable).To(BeTrue())

		g.Expect(got.Status.Rollback).NotTo(BeNil())
		g.Expect(got.Status.Rollback.ImageDigest).To(Equal(testutil.DigestC))
		g.Expect(got.Status.Rollback.Version).To(Equal(v3))

		g.Expect(got.Status.Conditions).To(ContainElement(And(
			HaveField("Type", bootcv1alpha1.NodeIdle),
			HaveField("Status", metav1.ConditionTrue),
			HaveField("Reason", bootcv1alpha1.NodeReasonIdle),
		)))
		g.Expect(got.Status.Conditions).To(ContainElement(And(
			HaveField("Type", bootcv1alpha1.NodeDegraded),
			HaveField("Status", metav1.ConditionFalse),
			HaveField("Reason", bootcv1alpha1.NodeReasonHealthy),
		)))
	}).Should(Succeed())
}

func TestReconcileBootcStatusError(t *testing.T) {
	g := NewWithT(t)
	g.SetDefaultEventuallyTimeout(pollTimeout)
	g.SetDefaultEventuallyPollingInterval(pollInterval)
	ctx := context.Background()

	fake.reset()
	fake.setStatusErr(errors.New(bootcStatusErrMsg))

	bn := testutil.NewNode(testNodeName, testutil.ImageDigestRefA)
	g.Expect(k8sClient.Create(ctx, bn)).To(Succeed())
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, bn)
	})

	g.Eventually(func(g Gomega) {
		var got bootcv1alpha1.BootcNode
		g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(bn), &got)).To(Succeed())
		g.Expect(got.Status.Conditions).To(ContainElement(And(
			HaveField("Type", bootcv1alpha1.NodeDegraded),
			HaveField("Status", metav1.ConditionTrue),
			HaveField("Reason", bootcv1alpha1.NodeReasonError),
			HaveField("Message", Equal(fmt.Sprintf("failed to get bootc status: getting bootc status: %s", bootcStatusErrMsg))),
		)))
	}).Should(Succeed())
}
