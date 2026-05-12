/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	bootcv1alpha1 "github.com/jlebon/bootc-operator/api/v1alpha1"
	testutil "github.com/jlebon/bootc-operator/test/util"
)

// pollInterval and pollTimeout control how long tests wait for the
// async reconciler to act.
const (
	pollInterval = 200 * time.Millisecond
	pollTimeout  = 10 * time.Second
)

// waitFor polls until condFn returns true or the timeout expires.
func waitFor(t *testing.T, condFn func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(pollTimeout)
	for {
		if condFn() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for: %s", msg)
		}
		time.Sleep(pollInterval)
	}
}

// TestMembershipCreatesBootcNodes verifies that creating a pool and
// matching nodes causes BootcNodes to be created with the correct
// ownerReference, desiredImage, and that nodes are labeled
// bootc.dev/managed. It also verifies that removing a node's matching
// label causes cleanup (BootcNode deleted, managed label removed).
func TestMembershipCreatesBootcNodes(t *testing.T) {
	ctx := context.Background()

	// Create two worker nodes.
	node1 := testutil.NewK8sNode("mem-worker-1", testutil.WorkerLabels())
	node2 := testutil.NewK8sNode("mem-worker-2", testutil.WorkerLabels())
	for _, n := range []*corev1.Node{node1, node2} {
		if err := k8sClient.Create(ctx, n); err != nil {
			t.Fatalf("Failed to create node %s: %v", n.Name, err)
		}
		t.Cleanup(func() {
			_ = k8sClient.Delete(ctx, n)
		})
	}

	// Create a pool selecting workers.
	pool := testutil.NewPool("mem-workers", testImageDigestRefA, testutil.WithWorkerSelector())
	if err := k8sClient.Create(ctx, pool); err != nil {
		t.Fatalf("Failed to create pool: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, pool)
	})

	// Wait for BootcNodes to appear and verify their properties.
	for _, nodeName := range []string{"mem-worker-1", "mem-worker-2"} {
		name := nodeName
		var bn bootcv1alpha1.BootcNode
		waitFor(t, func() bool {
			return k8sClient.Get(ctx, client.ObjectKey{Name: name}, &bn) == nil
		}, "BootcNode "+name+" to be created")

		// Check ownerReference.
		owner := metav1.GetControllerOf(&bn)
		if owner == nil {
			t.Errorf("BootcNode %s has no controller owner", name)
		} else if owner.Name != pool.Name {
			t.Errorf("BootcNode %s owner = %s, want %s", name, owner.Name, pool.Name)
		}

		// Check desiredImage.
		if bn.Spec.DesiredImage != testImageDigestRefA {
			t.Errorf("BootcNode %s desiredImage = %s, want %s", name, bn.Spec.DesiredImage, testImageDigestRefA)
		}

		// Check desiredImageState.
		if bn.Spec.DesiredImageState != bootcv1alpha1.DesiredImageStateStaged {
			t.Errorf("BootcNode %s desiredImageState = %s, want Staged", name, bn.Spec.DesiredImageState)
		}
	}

	// Verify nodes are labeled bootc.dev/managed.
	for _, nodeName := range []string{"mem-worker-1", "mem-worker-2"} {
		name := nodeName
		waitFor(t, func() bool {
			var n corev1.Node
			if err := k8sClient.Get(ctx, client.ObjectKey{Name: name}, &n); err != nil {
				return false
			}
			_, ok := n.Labels[bootcv1alpha1.LabelManaged]
			return ok
		}, "node "+name+" to be labeled managed")
	}

	// Remove the worker label from mem-worker-1 and verify cleanup.
	var fresh corev1.Node
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: "mem-worker-1"}, &fresh); err != nil {
		t.Fatalf("Failed to get node: %v", err)
	}
	patch := client.StrategicMergeFrom(fresh.DeepCopy())
	delete(fresh.Labels, "node-role.kubernetes.io/worker")
	if err := k8sClient.Patch(ctx, &fresh, patch); err != nil {
		t.Fatalf("Failed to patch node labels: %v", err)
	}

	// Wait for BootcNode to be deleted.
	waitFor(t, func() bool {
		var bn bootcv1alpha1.BootcNode
		err := k8sClient.Get(ctx, client.ObjectKey{Name: "mem-worker-1"}, &bn)
		return apierrors.IsNotFound(err)
	}, "BootcNode mem-worker-1 to be deleted")

	// Verify managed label is removed.
	waitFor(t, func() bool {
		var n corev1.Node
		if err := k8sClient.Get(ctx, client.ObjectKey{Name: "mem-worker-1"}, &n); err != nil {
			return false
		}
		_, hasLabel := n.Labels[bootcv1alpha1.LabelManaged]
		return !hasLabel
	}, "managed label to be removed from mem-worker-1")
}

// TestMembershipSyncsDesiredImage verifies that changing the pool's
// image ref updates desiredImage on all owned BootcNodes and resets
// desiredImageState to Staged.
func TestMembershipSyncsDesiredImage(t *testing.T) {
	ctx := context.Background()

	node := testutil.NewK8sNode("mem-sync-1", testutil.WorkerLabels())
	if err := k8sClient.Create(ctx, node); err != nil {
		t.Fatalf("Failed to create node: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, node)
	})

	pool := testutil.NewPool("mem-sync-pool", testImageDigestRefA, testutil.WithWorkerSelector())
	if err := k8sClient.Create(ctx, pool); err != nil {
		t.Fatalf("Failed to create pool: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, pool)
	})

	// Wait for BootcNode with image A.
	waitFor(t, func() bool {
		var bn bootcv1alpha1.BootcNode
		if err := k8sClient.Get(ctx, client.ObjectKey{Name: node.Name}, &bn); err != nil {
			return false
		}
		return bn.Spec.DesiredImage == testImageDigestRefA
	}, "BootcNode to have image A")

	// Update pool image to B.
	var freshPool bootcv1alpha1.BootcNodePool
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: pool.Name}, &freshPool); err != nil {
		t.Fatalf("Failed to get pool: %v", err)
	}
	freshPool.Spec.Image.Ref = testImageDigestRefB
	if err := k8sClient.Update(ctx, &freshPool); err != nil {
		t.Fatalf("Failed to update pool: %v", err)
	}

	// Wait for BootcNode to be updated with image B.
	waitFor(t, func() bool {
		var bn bootcv1alpha1.BootcNode
		if err := k8sClient.Get(ctx, client.ObjectKey{Name: node.Name}, &bn); err != nil {
			return false
		}
		return bn.Spec.DesiredImage == testImageDigestRefB &&
			bn.Spec.DesiredImageState == bootcv1alpha1.DesiredImageStateStaged
	}, "BootcNode to have image B with state Staged")
}

// TestMembershipConflictDetection verifies that when a node matches two
// pools, the conflicting pool is marked Degraded with reason
// NodeConflict for the contested node, but non-contested nodes in
// that pool are still handled.
func TestMembershipConflictDetection(t *testing.T) {
	ctx := context.Background()

	// node1: pool1 only, node2: pool2 only, node3: both (contested).
	node1 := testutil.NewK8sNode("mem-conflict-1", map[string]string{"pool1": "true"})
	node2 := testutil.NewK8sNode("mem-conflict-2", map[string]string{"pool2": "true"})
	node3 := testutil.NewK8sNode("mem-conflict-3", map[string]string{"pool1": "true", "pool2": "true"})
	for _, n := range []*corev1.Node{node1, node2, node3} {
		if err := k8sClient.Create(ctx, n); err != nil {
			t.Fatalf("Failed to create node %s: %v", n.Name, err)
		}
		t.Cleanup(func() {
			_ = k8sClient.Delete(ctx, n)
		})
	}

	// Create first pool selecting pool1=true — matches node1 and node3.
	pool1 := testutil.NewPool("mem-conflict-pool1", testImageDigestRefA,
		testutil.WithNodeSelector(map[string]string{"pool1": "true"}))
	if err := k8sClient.Create(ctx, pool1); err != nil {
		t.Fatalf("Failed to create pool1: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, pool1)
	})

	// Wait for pool1 to claim node1 and node3.
	for _, name := range []string{node1.Name, node3.Name} {
		n := name
		waitFor(t, func() bool {
			var bn bootcv1alpha1.BootcNode
			return k8sClient.Get(ctx, client.ObjectKey{Name: n}, &bn) == nil
		}, "BootcNode "+n+" to be created by pool1")
	}

	// Create second pool selecting pool2=true — matches node2 and node3,
	// but node3 is already owned by pool1 (conflict).
	pool2 := testutil.NewPool("mem-conflict-pool2", testImageDigestRefB,
		testutil.WithNodeSelector(map[string]string{"pool2": "true"}))
	if err := k8sClient.Create(ctx, pool2); err != nil {
		t.Fatalf("Failed to create pool2: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, pool2)
	})

	// Wait for pool2 to be marked Degraded/NodeConflict.
	waitFor(t, func() bool {
		var p bootcv1alpha1.BootcNodePool
		if err := k8sClient.Get(ctx, client.ObjectKey{Name: pool2.Name}, &p); err != nil {
			return false
		}
		cond := apimeta.FindStatusCondition(p.Status.Conditions, bootcv1alpha1.PoolDegraded)
		return cond != nil &&
			cond.Status == metav1.ConditionTrue &&
			cond.Reason == bootcv1alpha1.PoolNodeConflict &&
			strings.Contains(cond.Message, pool1.Name)
	}, "pool2 to be Degraded/NodeConflict")

	// Verify pool1 is not degraded.
	var p1 bootcv1alpha1.BootcNodePool
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: pool1.Name}, &p1); err != nil {
		t.Fatalf("Failed to get pool1: %v", err)
	}
	cond := apimeta.FindStatusCondition(p1.Status.Conditions, bootcv1alpha1.PoolDegraded)
	if cond != nil && cond.Status == metav1.ConditionTrue {
		t.Errorf("pool1 should not be degraded, but got: %s/%s: %s", cond.Reason, cond.Status, cond.Message)
	}

	// Verify non-contested nodes are still handled: node1 by pool1,
	// node2 by pool2.
	for _, tc := range []struct {
		nodeName string
		poolName string
	}{
		{node1.Name, pool1.Name},
		{node2.Name, pool2.Name},
	} {
		tc := tc
		waitFor(t, func() bool {
			var bn bootcv1alpha1.BootcNode
			if err := k8sClient.Get(ctx, client.ObjectKey{Name: tc.nodeName}, &bn); err != nil {
				return false
			}
			o := metav1.GetControllerOf(&bn)
			return o != nil && o.Name == tc.poolName
		}, "BootcNode "+tc.nodeName+" to be owned by "+tc.poolName)
	}
}
