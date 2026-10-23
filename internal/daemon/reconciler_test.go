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
	"sigs.k8s.io/controller-runtime/pkg/event"

	bootcv1alpha1 "github.com/bootc-dev/bootc-operator/api/v1alpha1"
	"github.com/bootc-dev/bootc-operator/internal/bootc"
	testutil "github.com/bootc-dev/bootc-operator/test/util"
)

const (
	pollInterval = 200 * time.Millisecond
	pollTimeout  = 10 * time.Second

	stageErrMsg       = "stage failed: pull error"
	bootcStatusErrMsg = "bootc status failed"
)

func TestReconcilePopulatesStatus(t *testing.T) {
	g := NewWithT(t)
	g.SetDefaultEventuallyTimeout(pollTimeout)
	g.SetDefaultEventuallyPollingInterval(pollInterval)
	ctx := context.Background()

	fake := newTestEnv()

	v1 := "v1"
	v2 := "v2"
	v3 := "v3"
	fake.status = newBootcStatus(testutil.DigestA)
	fake.status.Status.Booted.Image.Version = &v1
	fake.status.Status.Staged = &bootc.BootEntry{
		Image: &bootc.ImageStatus{
			Image: bootc.ImageReference{
				Image:     testutil.ImageTaggedRef,
				Transport: "registry",
			},
			ImageDigest:  testutil.DigestB,
			Version:      &v2,
			Architecture: "amd64",
		},
		SoftRebootCapable: true,
	}
	fake.status.Status.Rollback = &bootc.BootEntry{
		Image: &bootc.ImageStatus{
			Image: bootc.ImageReference{
				Image:     testutil.ImageTaggedRef,
				Transport: "registry",
			},
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

	g.Eventually(func() (bootcv1alpha1.BootcNodeStatus, error) {
		var got bootcv1alpha1.BootcNode
		err := k8sClient.Get(ctx, client.ObjectKeyFromObject(bn), &got)
		return got.Status, err
	}).Should(And(
		HaveField("Booted", And(
			Not(BeNil()),
			HaveField("Image", testutil.ImageTaggedRef),
			HaveField("ImageDigest", testutil.DigestA),
			HaveField("Version", v1),
			HaveField("Architecture", "amd64"),
		)),
		HaveField("Staged", And(
			Not(BeNil()),
			HaveField("ImageDigest", testutil.DigestB),
			HaveField("Version", v2),
			HaveField("SoftRebootCapable", BeTrue()),
		)),
		HaveField("Rollback", And(
			Not(BeNil()),
			HaveField("ImageDigest", testutil.DigestC),
			HaveField("Version", v3),
		)),
		HaveField("Conditions", And(
			ContainElement(And(
				HaveField("Type", bootcv1alpha1.NodeIdle),
				HaveField("Status", metav1.ConditionTrue),
				HaveField("Reason", bootcv1alpha1.NodeReasonIdle),
			)),
			ContainElement(And(
				HaveField("Type", bootcv1alpha1.NodeDegraded),
				HaveField("Status", metav1.ConditionFalse),
				HaveField("Reason", bootcv1alpha1.NodeReasonHealthy),
			)),
		)),
	))
}

func TestReconcileBootcStatusError(t *testing.T) {
	g := NewWithT(t)
	g.SetDefaultEventuallyTimeout(pollTimeout)
	g.SetDefaultEventuallyPollingInterval(pollInterval)
	ctx := context.Background()

	fake := newTestEnv()
	fake.setStatusErr(errors.New(bootcStatusErrMsg))

	bn := testutil.NewNode(testNodeName, testutil.ImageDigestRefA)
	g.Expect(k8sClient.Create(ctx, bn)).To(Succeed())
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, bn)
	})

	g.Eventually(func() ([]metav1.Condition, error) {
		var got bootcv1alpha1.BootcNode
		err := k8sClient.Get(ctx, client.ObjectKeyFromObject(bn), &got)
		return got.Status.Conditions, err
	}).Should(ContainElement(And(
		HaveField("Type", bootcv1alpha1.NodeDegraded),
		HaveField("Status", metav1.ConditionTrue),
		HaveField("Reason", bootcv1alpha1.NodeReasonError),
		HaveField(
			"Message",
			Equal(
				fmt.Sprintf("populating bootc fields: getting bootc status: %s", bootcStatusErrMsg),
			),
		),
	)))
}

func TestStagingTriggered(t *testing.T) {
	g := NewWithT(t)
	g.SetDefaultEventuallyTimeout(pollTimeout)
	g.SetDefaultEventuallyPollingInterval(pollInterval)
	ctx := context.Background()

	fake := newTestEnv()
	fake.status = newBootcStatus(testutil.DigestA)

	bn := testutil.NewNode(testNodeName, testutil.ImageDigestRefB)
	g.Expect(k8sClient.Create(ctx, bn)).To(Succeed())
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, bn)
	})

	g.Eventually(func() (bootcv1alpha1.BootcNodeStatus, error) {
		var got bootcv1alpha1.BootcNode
		err := k8sClient.Get(ctx, client.ObjectKeyFromObject(bn), &got)
		return got.Status, err
	}).Should(And(
		HaveField("Staged", And(
			Not(BeNil()),
			HaveField("ImageDigest", testutil.DigestB),
		)),
		HaveField("Conditions", And(
			ContainElement(And(
				HaveField("Type", bootcv1alpha1.NodeIdle),
				HaveField("Status", metav1.ConditionFalse),
				HaveField("Reason", bootcv1alpha1.NodeReasonStaged),
			)),
			ContainElement(And(
				HaveField("Type", bootcv1alpha1.NodeDegraded),
				HaveField("Status", metav1.ConditionFalse),
			)),
		)),
	))

	g.Expect(fake.getStageImg()).To(Equal(testutil.ImageDigestRefB))
	g.Expect(fake.getRebooted()).To(BeFalse())
}

func TestStagingError(t *testing.T) {
	g := NewWithT(t)
	g.SetDefaultEventuallyTimeout(pollTimeout)
	g.SetDefaultEventuallyPollingInterval(pollInterval)
	ctx := context.Background()

	fake := newTestEnv()
	fake.status = newBootcStatus(testutil.DigestA)
	fake.setStageErr(errors.New(stageErrMsg))

	bn := testutil.NewNode(testNodeName, testutil.ImageDigestRefB)
	g.Expect(k8sClient.Create(ctx, bn)).To(Succeed())
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, bn)
	})

	g.Eventually(func() ([]metav1.Condition, error) {
		var got bootcv1alpha1.BootcNode
		err := k8sClient.Get(ctx, client.ObjectKeyFromObject(bn), &got)
		return got.Status.Conditions, err
	}).Should(And(
		ContainElement(And(
			HaveField("Type", bootcv1alpha1.NodeIdle),
			HaveField("Status", metav1.ConditionTrue),
			HaveField("Reason", bootcv1alpha1.NodeReasonIdle),
		)),
		ContainElement(And(
			HaveField("Type", bootcv1alpha1.NodeDegraded),
			HaveField("Status", metav1.ConditionTrue),
			HaveField("Reason", bootcv1alpha1.NodeReasonError),
			HaveField("Message", Equal(fmt.Sprintf("bootc stage failed: %s", stageErrMsg))),
		)),
	))
}

func TestAlreadyStaged(t *testing.T) {
	g := NewWithT(t)
	g.SetDefaultEventuallyTimeout(pollTimeout)
	g.SetDefaultEventuallyPollingInterval(pollInterval)
	ctx := context.Background()

	fake := newTestEnv()
	fake.status = newBootcStatus(testutil.DigestA)
	fake.status.Status.Staged = newBootEntry(testutil.ImageDigestRefB, testutil.DigestB)

	bn := testutil.NewNode(testNodeName, testutil.ImageDigestRefB)
	g.Expect(k8sClient.Create(ctx, bn)).To(Succeed())
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, bn)
	})

	g.Eventually(func() ([]metav1.Condition, error) {
		var got bootcv1alpha1.BootcNode
		err := k8sClient.Get(ctx, client.ObjectKeyFromObject(bn), &got)
		return got.Status.Conditions, err
	}).Should(ContainElement(And(
		HaveField("Type", bootcv1alpha1.NodeIdle),
		HaveField("Status", metav1.ConditionFalse),
		HaveField("Reason", bootcv1alpha1.NodeReasonStaged),
	)))

	g.Expect(fake.getStageImg()).To(BeEmpty())
}

func TestRebootingSet(t *testing.T) {
	g := NewWithT(t)
	g.SetDefaultEventuallyTimeout(pollTimeout)
	g.SetDefaultEventuallyPollingInterval(pollInterval)
	ctx := context.Background()

	fake := newTestEnv()
	fake.status = newBootcStatus(testutil.DigestA)
	fake.status.Status.Staged = newBootEntry(testutil.ImageDigestRefB, testutil.DigestB)

	bn := testutil.NewNode(
		testNodeName,
		testutil.ImageDigestRefB,
		testutil.WithDesiredImageState(bootcv1alpha1.DesiredImageStateBooted),
	)
	g.Expect(k8sClient.Create(ctx, bn)).To(Succeed())
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, bn)
	})

	g.Eventually(func() ([]metav1.Condition, error) {
		var got bootcv1alpha1.BootcNode
		err := k8sClient.Get(ctx, client.ObjectKeyFromObject(bn), &got)
		return got.Status.Conditions, err
	}).Should(ContainElement(And(
		HaveField("Type", bootcv1alpha1.NodeIdle),
		HaveField("Status", metav1.ConditionFalse),
		HaveField("Reason", bootcv1alpha1.NodeReasonRebooting),
	)))

	g.Expect(fake.getRebooted()).To(BeTrue())
}

func TestSoftReboot(t *testing.T) {
	g := NewWithT(t)
	g.SetDefaultEventuallyTimeout(pollTimeout)
	g.SetDefaultEventuallyPollingInterval(pollInterval)
	ctx := context.Background()

	fake := newTestEnv()
	fake.status = newBootcStatus(testutil.DigestA)
	fake.status.Status.Staged = newBootEntry(testutil.ImageDigestRefB, testutil.DigestB)

	bn := testutil.NewNode(
		testNodeName,
		testutil.ImageDigestRefB,
		testutil.WithDesiredImageState(bootcv1alpha1.DesiredImageStateBooted),
		testutil.WithNodeRebootPolicy(bootcv1alpha1.RebootPolicyAllowSoftReboot),
	)
	g.Expect(k8sClient.Create(ctx, bn)).To(Succeed())
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, bn)
	})

	g.Eventually(func() ([]metav1.Condition, error) {
		var got bootcv1alpha1.BootcNode
		err := k8sClient.Get(ctx, client.ObjectKeyFromObject(bn), &got)
		return got.Status.Conditions, err
	}).Should(ContainElement(And(
		HaveField("Type", bootcv1alpha1.NodeIdle),
		HaveField("Status", metav1.ConditionFalse),
		HaveField("Reason", bootcv1alpha1.NodeReasonRebooting),
	)))

	g.Expect(fake.getRebooted()).To(BeFalse(), "should not use systemctl reboot")
	g.Expect(fake.getApplied()).To(BeTrue(), "should use ApplyUpdate")
	g.Expect(fake.getAppliedSoft()).To(BeTrue(), "should pass softReboot=true")
}

func TestRebootOnlyPolicy(t *testing.T) {
	g := NewWithT(t)
	g.SetDefaultEventuallyTimeout(pollTimeout)
	g.SetDefaultEventuallyPollingInterval(pollInterval)
	ctx := context.Background()

	fake := newTestEnv()
	fake.status = newBootcStatus(testutil.DigestA)
	fake.status.Status.Staged = newBootEntry(testutil.ImageDigestRefB, testutil.DigestB)

	bn := testutil.NewNode(
		testNodeName,
		testutil.ImageDigestRefB,
		testutil.WithDesiredImageState(bootcv1alpha1.DesiredImageStateBooted),
		testutil.WithNodeRebootPolicy(bootcv1alpha1.RebootPolicyRebootOnly),
	)
	g.Expect(k8sClient.Create(ctx, bn)).To(Succeed())
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, bn)
	})

	g.Eventually(func() ([]metav1.Condition, error) {
		var got bootcv1alpha1.BootcNode
		err := k8sClient.Get(ctx, client.ObjectKeyFromObject(bn), &got)
		return got.Status.Conditions, err
	}).Should(ContainElement(And(
		HaveField("Type", bootcv1alpha1.NodeIdle),
		HaveField("Status", metav1.ConditionFalse),
		HaveField("Reason", bootcv1alpha1.NodeReasonRebooting),
	)))

	g.Expect(fake.getRebooted()).To(BeTrue(), "should use systemctl reboot")
	g.Expect(fake.getApplied()).To(BeFalse(), "should not use ApplyUpdate")
}

func TestRollback(t *testing.T) {
	g := NewWithT(t)
	g.SetDefaultEventuallyTimeout(pollTimeout)
	g.SetDefaultEventuallyPollingInterval(pollInterval)
	ctx := context.Background()

	fake := newTestEnv()
	fake.status = newBootcStatus(testutil.DigestA)
	fake.status.Status.Staged = newBootEntry(testutil.ImageDigestRefB, testutil.DigestB)

	bn := testutil.NewNode(testNodeName, testutil.ImageDigestRefC)
	g.Expect(k8sClient.Create(ctx, bn)).To(Succeed())
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, bn)
	})

	g.Eventually(func() (bootcv1alpha1.BootcNodeStatus, error) {
		var got bootcv1alpha1.BootcNode
		err := k8sClient.Get(ctx, client.ObjectKeyFromObject(bn), &got)
		return got.Status, err
	}).Should(And(
		HaveField("Conditions", ContainElement(And(
			HaveField("Type", bootcv1alpha1.NodeIdle),
			HaveField("Status", metav1.ConditionFalse),
			HaveField("Reason", bootcv1alpha1.NodeReasonStaged),
		))),
		HaveField("Staged", And(
			Not(BeNil()),
			HaveField("ImageDigest", testutil.DigestC),
		)),
	))

	g.Expect(fake.getRebooted()).To(BeFalse())
}

// TestStaleCacheSpuriousRestage demonstrates that a stale StatusWatcher
// cache can cause the reconciler to spuriously re-stage an image that is
// already booted. This reproduces the mid-rollout bug where a node reboots
// into v2 but the cached bootc status still shows the pre-reboot state,
// causing the daemon to stage v2 again.
func TestStaleCacheSpuriousRestage(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	// Post-reboot reality: node is booted on image C, nothing staged.
	fake := &fakeExecutor{}
	fake.status = newBootcStatus(testutil.DigestC)

	// Stale cache: still believes the node is booted on image A.
	staleStatus := newBootcStatus(testutil.DigestA)
	watcher := &StatusWatcher{
		Events:   make(chan event.GenericEvent, 1),
		NodeName: testNodeName,
		Executor: fake,
	}
	watcher.cached = &staleStatus
	watcher.started = true

	r := &BootcNodeReconciler{
		NodeName:      testNodeName,
		Executor:      fake,
		StatusWatcher: watcher,
		stageDone:     make(chan event.GenericEvent, 1),
	}
	t.Cleanup(func() {
		r.inflight.mu.Lock()
		if r.inflight.cancel != nil {
			r.inflight.cancel()
		}
		r.inflight.mu.Unlock()
	})

	// Desired image is C, which matches what's actually booted.
	bn := testutil.NewNode(testNodeName, testutil.ImageDigestRefC)

	res, err := r.reconcileBootcNode(ctx, bn)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.needsReboot).To(BeFalse(), "should not trigger reboot")

	// populateBootcFields fills bn.Status from the watcher cache.
	// With a fresh cache this would show DigestC (correct); with stale
	// cache it shows DigestA, causing the mismatch.
	g.Expect(bn.Status.Booted.ImageDigest).To(Equal(testutil.DigestC),
		"stale cache returned wrong booted digest")

	// After reconcileBootcNode, acquire() sets inflight.image synchronously
	// before the stage goroutine starts. A non-empty value means a spurious
	// stage was triggered.
	g.Expect(r.inflight.isInFlight(testutil.ImageDigestRefC)).To(BeFalse(),
		"stale cache caused spurious re-stage of already-booted image")
}

func TestCancelInflightStage(t *testing.T) {
	g := NewWithT(t)
	g.SetDefaultEventuallyTimeout(pollTimeout)
	g.SetDefaultEventuallyPollingInterval(pollInterval)
	ctx := context.Background()

	fake := newTestEnv()
	fake.status = newBootcStatus(testutil.DigestA)

	firstBlock := make(chan struct{})
	fake.setStageHook(func() {
		<-firstBlock
	})

	bn := testutil.NewNode(testNodeName, testutil.ImageDigestRefB)
	g.Expect(k8sClient.Create(ctx, bn)).To(Succeed())
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, bn)
	})

	g.Eventually(func() string {
		return fake.getStageImg()
	}).Should(Equal(testutil.ImageDigestRefB))

	fake.setStageHook(nil)
	close(firstBlock)

	g.Eventually(func() error {
		var latest bootcv1alpha1.BootcNode
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(bn), &latest); err != nil {
			return err
		}
		latest.Spec.DesiredImage = testutil.ImageDigestRefC
		return k8sClient.Update(ctx, &latest)
	}).Should(Succeed())

	g.Eventually(func() string {
		return fake.getStageImg()
	}).Should(Equal(testutil.ImageDigestRefC))
}
