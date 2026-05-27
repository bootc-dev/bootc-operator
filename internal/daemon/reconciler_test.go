// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"fmt"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	bootcv1alpha1 "github.com/jlebon/bootc-operator/api/v1alpha1"
	testutil "github.com/jlebon/bootc-operator/test/util"
)

const (
	pollInterval = 200 * time.Millisecond
	pollTimeout  = 10 * time.Second

	testImageRef = testutil.ImageDigestRefA

	bootcStatusFull = `{
  "apiVersion": "org.containers.bootc/v1alpha1",
  "kind": "BootcHost",
  "spec": {
    "image": {"image": "quay.io/example/myos:latest", "transport": "registry"},
    "bootOrder": "default"
  },
  "status": {
    "booted": {
      "image": {
        "image": {"image": "quay.io/example/myos:latest", "transport": "registry"},
        "imageDigest": "` + testutil.DigestA + `",
        "version": "1.0",
        "architecture": "amd64"
      },
      "incompatible": false,
      "pinned": false,
      "softRebootCapable": false,
      "downloadOnly": false
    },
    "staged": {
      "image": {
        "image": {"image": "quay.io/example/myos:latest", "transport": "registry"},
        "imageDigest": "` + testutil.DigestB + `",
        "version": "2.0",
        "architecture": "amd64"
      },
      "incompatible": false,
      "pinned": false,
      "softRebootCapable": true,
      "downloadOnly": false
    },
    "rollback": {
      "image": {
        "image": {"image": "quay.io/example/myos:latest", "transport": "registry"},
        "imageDigest": "` + testutil.DigestC + `",
        "version": "0.9",
        "architecture": "amd64"
      },
      "incompatible": false,
      "pinned": false,
      "softRebootCapable": false,
      "downloadOnly": false
    },
    "rollbackQueued": false
  }
}`
)

func TestReconcilePopulatesStatus(t *testing.T) {
	g := NewWithT(t)
	g.SetDefaultEventuallyTimeout(pollTimeout)
	g.SetDefaultEventuallyPollingInterval(pollInterval)
	ctx := context.Background()

	fake.set([]byte(bootcStatusFull), nil)

	bn := testutil.NewNode(testNodeName, testImageRef)
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
		g.Expect(got.Status.Booted.Version).To(Equal("1.0"))
		g.Expect(got.Status.Booted.Architecture).To(Equal("amd64"))

		g.Expect(got.Status.Staged).NotTo(BeNil())
		g.Expect(got.Status.Staged.ImageDigest).To(Equal(testutil.DigestB))
		g.Expect(got.Status.Staged.Version).To(Equal("2.0"))
		g.Expect(got.Status.Staged.SoftRebootCapable).To(BeTrue())

		g.Expect(got.Status.Rollback).NotTo(BeNil())
		g.Expect(got.Status.Rollback.ImageDigest).To(Equal(testutil.DigestC))
		g.Expect(got.Status.Rollback.Version).To(Equal("0.9"))

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

	fake.set(nil, fmt.Errorf("bootc status failed"))

	bn := testutil.NewNode(testNodeName, testImageRef)
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
			HaveField("Message", ContainSubstring("bootc status")),
		)))
	}).Should(Succeed())
}

func TestReconcileInvalidJSON(t *testing.T) {
	g := NewWithT(t)
	g.SetDefaultEventuallyTimeout(pollTimeout)
	g.SetDefaultEventuallyPollingInterval(pollInterval)
	ctx := context.Background()

	fake.set([]byte(`{invalid json`), nil)

	bn := testutil.NewNode(testNodeName, testImageRef)
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
			HaveField("Message", ContainSubstring("parse")),
		)))
	}).Should(Succeed())
}
