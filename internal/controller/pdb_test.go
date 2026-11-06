// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	bootcv1alpha1 "github.com/bootc-dev/bootc-operator/api/v1alpha1"
	testutil "github.com/bootc-dev/bootc-operator/test/util"
)

func testPDBPod(labels map[string]string, ready, terminating bool) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod",
			Namespace: "default",
			Labels:    labels,
		},
	}
	if terminating {
		now := metav1.Now()
		pod.DeletionTimestamp = &now
	}
	pod.Status.Phase = corev1.PodRunning
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	pod.Status.Conditions = []corev1.PodCondition{{
		Type:   corev1.PodReady,
		Status: status,
	}}
	return pod
}

func testPDB(
	name string,
	selector *metav1.LabelSelector,
	allowed, current, expected, desired int32,
	policy *policyv1.UnhealthyPodEvictionPolicyType,
) policyv1.PodDisruptionBudget {
	return policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: policyv1.PodDisruptionBudgetSpec{
			Selector:                   selector,
			UnhealthyPodEvictionPolicy: policy,
		},
		Status: policyv1.PodDisruptionBudgetStatus{
			DisruptionsAllowed: allowed,
			CurrentHealthy:     current,
			ExpectedPods:       expected,
			DesiredHealthy:     desired,
		},
	}
}

func TestPDBBlockForPod(t *testing.T) {
	matching := &metav1.LabelSelector{MatchLabels: map[string]string{"app": "demo"}}
	empty := &metav1.LabelSelector{}
	other := &metav1.LabelSelector{MatchLabels: map[string]string{"app": "other"}}
	alwaysAllow := policyv1.AlwaysAllow
	ifHealthyBudget := policyv1.IfHealthyBudget

	tests := []struct {
		name     string
		pod      *corev1.Pod
		pdbs     []policyv1.PodDisruptionBudget
		wantPDBs []string
		wantHint string
	}{
		{
			name: "zero budget blocks a ready pod",
			pod:  testPDBPod(map[string]string{"app": "demo"}, true, false),
			pdbs: []policyv1.PodDisruptionBudget{
				testPDB("demo", matching, 0, 1, 1, 1, nil),
			},
			wantPDBs: []string{"default/demo"},
			wantHint: pdbNeverAllowsEvictionHint,
		},
		{
			name: "available disruption does not block",
			pod:  testPDBPod(map[string]string{"app": "demo"}, true, false),
			pdbs: []policyv1.PodDisruptionBudget{
				testPDB("demo", matching, 1, 1, 1, 1, nil),
			},
		},
		{
			name: "terminating pod is ignored",
			pod:  testPDBPod(map[string]string{"app": "demo"}, true, true),
			pdbs: []policyv1.PodDisruptionBudget{
				testPDB("demo", matching, 0, 1, 1, 1, nil),
			},
		},
		{
			name: "pending pod is ignored",
			pod: func() *corev1.Pod {
				pod := testPDBPod(map[string]string{"app": "demo"}, false, false)
				pod.Status.Phase = corev1.PodPending
				return pod
			}(),
			pdbs: []policyv1.PodDisruptionBudget{
				testPDB("demo", matching, 0, 0, 1, 1, nil),
			},
		},
		{
			name: "succeeded pod is ignored",
			pod: func() *corev1.Pod {
				pod := testPDBPod(map[string]string{"app": "demo"}, false, false)
				pod.Status.Phase = corev1.PodSucceeded
				return pod
			}(),
			pdbs: []policyv1.PodDisruptionBudget{
				testPDB("demo", matching, 0, 0, 1, 1, nil),
			},
		},
		{
			name: "empty selector matches every pod in the namespace",
			pod:  testPDBPod(map[string]string{"app": "demo"}, true, false),
			pdbs: []policyv1.PodDisruptionBudget{
				testPDB("everything", empty, 0, 1, 1, 1, nil),
			},
			wantPDBs: []string{"default/everything"},
			wantHint: pdbNeverAllowsEvictionHint,
		},
		{
			name: "non-matching selector does not block",
			pod:  testPDBPod(map[string]string{"app": "demo"}, true, false),
			pdbs: []policyv1.PodDisruptionBudget{
				testPDB("other", other, 0, 1, 1, 1, nil),
			},
		},
		{
			name: "AlwaysAllow evicts an unhealthy pod",
			pod:  testPDBPod(map[string]string{"app": "demo"}, false, false),
			pdbs: []policyv1.PodDisruptionBudget{
				testPDB("demo", matching, 0, 0, 2, 2, &alwaysAllow),
			},
		},
		{
			name: "AlwaysAllow does not evict a healthy pod",
			pod:  testPDBPod(map[string]string{"app": "demo"}, true, false),
			pdbs: []policyv1.PodDisruptionBudget{
				testPDB("demo", matching, 0, 1, 1, 1, &alwaysAllow),
			},
			wantPDBs: []string{"default/demo"},
			wantHint: pdbNeverAllowsEvictionHint,
		},
		{
			name: "IfHealthyBudget evicts an unhealthy pod when healthy",
			pod:  testPDBPod(map[string]string{"app": "demo"}, false, false),
			pdbs: []policyv1.PodDisruptionBudget{
				testPDB("demo", matching, 0, 1, 3, 1, &ifHealthyBudget),
			},
		},
		{
			name: "IfHealthyBudget blocks an unhealthy pod without budget",
			pod:  testPDBPod(map[string]string{"app": "demo"}, false, false),
			pdbs: []policyv1.PodDisruptionBudget{
				testPDB("demo", matching, 0, 1, 3, 2, &ifHealthyBudget),
			},
			wantPDBs: []string{"default/demo"},
			wantHint: "2 pod(s) not ready",
		},
		{
			name: "IfHealthyBudget requires a nonzero desired healthy",
			pod:  testPDBPod(map[string]string{"app": "demo"}, false, false),
			pdbs: []policyv1.PodDisruptionBudget{
				testPDB("demo", matching, 0, 0, 1, 0, &ifHealthyBudget),
			},
			wantPDBs: []string{"default/demo"},
			wantHint: "1 pod(s) not ready",
		},
		{
			name: "default policy behaves as IfHealthyBudget",
			pod:  testPDBPod(map[string]string{"app": "demo"}, false, false),
			pdbs: []policyv1.PodDisruptionBudget{
				testPDB("demo", matching, 0, 1, 3, 2, nil),
			},
			wantPDBs: []string{"default/demo"},
			wantHint: "2 pod(s) not ready",
		},
		{
			name: "multiple matching PDBs are a misconfiguration",
			pod:  testPDBPod(map[string]string{"app": "demo"}, true, false),
			pdbs: []policyv1.PodDisruptionBudget{
				testPDB("first", matching, 1, 1, 1, 1, nil),
				testPDB("second", matching, 1, 1, 1, 1, nil),
			},
			wantPDBs: []string{"default/first", "default/second"},
			wantHint: pdbMultipleMatchHint,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			block, err := pdbBlockForPod(tt.pod, tt.pdbs)
			g.Expect(err).NotTo(HaveOccurred())

			if tt.wantPDBs == nil {
				g.Expect(block).To(BeNil())
				return
			}

			g.Expect(block).NotTo(BeNil())
			g.Expect(block.Hint).To(Equal(tt.wantHint))
			names := make([]string, len(block.PDBs))
			for i, ref := range block.PDBs {
				names[i] = ref.String()
			}
			g.Expect(names).To(ConsistOf(tt.wantPDBs))
		})
	}
}

func TestDrainBlockedByPDBNote(t *testing.T) {
	tests := []struct {
		name   string
		blocks []pdbBlock
		want   string
	}{
		{
			name: "single PDB",
			blocks: []pdbBlock{{
				PDBs: []pdbRef{{Namespace: "default", Name: "web"}},
				Hint: "2 pod(s) not ready",
			}},
			want: "Drain blocked by PodDisruptionBudget default/web: 2 pod(s) not ready",
		},
		{
			name: "multiple PDBs",
			blocks: []pdbBlock{{
				PDBs: []pdbRef{
					{Namespace: "default", Name: "web"},
					{Namespace: "default", Name: "api"},
				},
				Hint: pdbMultipleMatchHint,
			}},
			want: "Drain blocked by PodDisruptionBudgets default/api, default/web: " +
				pdbMultipleMatchHint,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			g.Expect(drainBlockedByPDBNote{Blocks: tt.blocks}.Note()).To(Equal(tt.want))
		})
	}
}

func TestRecordDrainStallsPDBBlocked(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	pod := testPDBPod(map[string]string{"app": "demo"}, true, false)
	pod.Name = "blocked-pod"
	pod.Spec.NodeName = "pdb-worker"

	selector := &metav1.LabelSelector{MatchLabels: map[string]string{"app": "demo"}}
	pdb := testPDB("budget-one", selector, 0, 1, 1, 1, nil)

	kubeClient := fake.NewSimpleClientset(pod, &pdb)
	recorder := &capturingEventRecorder{}
	status := &drainStatus{startTime: time.Now().Add(-time.Hour)}
	reconciler := &BootcNodePoolReconciler{
		Recorder:            recorder,
		KubeClient:          kubeClient,
		DrainStallThreshold: time.Millisecond,
		drains:              map[string]*drainStatus{pod.Spec.NodeName: status},
	}
	pool := testutil.NewPool("pdb-pool", testImageDigestRefA, testutil.WithWorkerSelector())
	node := testutil.NewBootcNode(pod.Spec.NodeName, testImageDigestRefA)
	rs := &rolloutState{}
	nodes := map[string]*bootcv1alpha1.BootcNode{node.Name: node}

	nextCheck := reconciler.recordDrainStalls(ctx, pool, rs, nodes)
	g.Expect(nextCheck).To(Equal(drainStallRequeueInterval))
	g.Expect(rs.pdbBlocked).To(HaveKey(node.Name))
	g.Expect(rs.pdbBlocked[node.Name]).To(HaveLen(1))
	g.Expect(recorder.events).To(HaveLen(2))
	g.Expect(recorder.events[0].reason).To(Equal(eventReasonDrainTakingTooLong))
	g.Expect(recorder.events[1].reason).To(Equal(eventReasonDrainBlockedByPDB))
	g.Expect(recorder.events[1].note).To(ContainSubstring("default/budget-one"))

	// The same blocking PDB set does not re-emit the event.
	reconciler.recordDrainStalls(ctx, pool, rs, nodes)
	g.Expect(recorder.events).To(HaveLen(2))

	// Changing the blocking PDB set re-emits the event.
	g.Expect(kubeClient.PolicyV1().PodDisruptionBudgets("default").Delete(
		ctx, pdb.Name, metav1.DeleteOptions{},
	)).To(Succeed())
	replacement := testPDB("budget-two", selector, 0, 1, 1, 1, nil)
	_, err := kubeClient.PolicyV1().PodDisruptionBudgets("default").Create(
		ctx, &replacement, metav1.CreateOptions{},
	)
	g.Expect(err).NotTo(HaveOccurred())

	reconciler.recordDrainStalls(ctx, pool, rs, nodes)
	g.Expect(recorder.events).To(HaveLen(3))
	g.Expect(recorder.events[2].reason).To(Equal(eventReasonDrainBlockedByPDB))
	g.Expect(recorder.events[2].note).To(ContainSubstring("default/budget-two"))
}
