// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"fmt"
	"math"
	"slices"
	"strings"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	bootcv1alpha1 "github.com/bootc-dev/bootc-operator/api/v1alpha1"
)

// syncPoolStatus populates the pool's status counts, digest tracking,
// and UpToDate condition from the current rollout state.
func syncPoolStatus(pool *bootcv1alpha1.BootcNodePool, rs *rolloutState) {
	if pool == nil || rs == nil {
		return
	}
	pool.Status.ObservedGeneration = pool.Generation
	pool.Status.NodeCount = clampCount(rs.nodeCount())
	pool.Status.UpdatedCount = clampCount(len(rs.upToDate))
	pool.Status.UpdatingCount = clampCount(
		len(rs.pending) + len(rs.staging) + len(rs.staged) + len(rs.rebooting),
	)
	pool.Status.DegradedCount = clampCount(len(rs.degraded))

	if pool.Status.NodeCount == pool.Status.UpdatedCount {
		pool.Status.DeployedDigest = pool.Status.TargetDigest
	}
	pool.Status.UpdateAvailable = pool.Status.TargetDigest != pool.Status.DeployedDigest

	syncUpToDateCondition(pool, rs)
}

// clampCount converts a count to int32 for the status API, saturating to
// [0, math.MaxInt32] to match the schema instead of wrapping.
func clampCount(n int) int32 {
	if n < 0 {
		return 0
	}
	if n > math.MaxInt32 {
		return math.MaxInt32
	}
	return int32(n)
}

func syncUpToDateCondition(pool *bootcv1alpha1.BootcNodePool, rs *rolloutState) {
	if pool == nil || rs == nil {
		return
	}
	switch {
	case pool.Spec.Rollout != nil && pool.Spec.Rollout.Paused && pool.Status.NodeCount != pool.Status.UpdatedCount:
		apimeta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
			Type:    bootcv1alpha1.PoolUpToDate,
			Status:  metav1.ConditionFalse,
			Reason:  bootcv1alpha1.PoolPaused,
			Message: rolloutBreakdown(pool, rs),
		})
	case pool.Status.NodeCount != pool.Status.UpdatedCount && allSlotsPDBBlocked(rs):
		apimeta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
			Type:    bootcv1alpha1.PoolUpToDate,
			Status:  metav1.ConditionFalse,
			Reason:  bootcv1alpha1.PoolPDBBlocked,
			Message: rolloutBreakdown(pool, rs),
		})
	case pool.Status.NodeCount != pool.Status.UpdatedCount:
		apimeta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
			Type:    bootcv1alpha1.PoolUpToDate,
			Status:  metav1.ConditionFalse,
			Reason:  bootcv1alpha1.PoolRolloutInProgress,
			Message: rolloutBreakdown(pool, rs),
		})
	default:
		apimeta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
			Type:   bootcv1alpha1.PoolUpToDate,
			Status: metav1.ConditionTrue,
			Reason: bootcv1alpha1.PoolAllUpdated,
		})
	}
}

// allSlotsPDBBlocked reports whether every occupied reboot slot is held by a
// stalled drain blocked by a PodDisruptionBudget, and the rollout has consumed
// all of its disruption budget.
func allSlotsPDBBlocked(rs *rolloutState) bool {
	if rs.maxUnavailable <= 0 ||
		rs.occupiedSlots == 0 ||
		rs.occupiedSlots < rs.maxUnavailable ||
		len(rs.pdbBlocked) == 0 {
		return false
	}

	slotted := 0
	for _, bn := range rs.staged {
		if !metav1.HasAnnotation(bn.ObjectMeta, bootcv1alpha1.AnnotationInRebootSlot) {
			continue
		}
		slotted++
		if _, ok := rs.pdbBlocked[bn.Name]; !ok {
			return false
		}
	}
	return slotted > 0 && slotted == rs.occupiedSlots
}

func rolloutBreakdown(pool *bootcv1alpha1.BootcNodePool, rs *rolloutState) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d/%d updated", len(rs.upToDate), rs.nodeCount())

	type bucket struct {
		name  string
		count int
	}
	buckets := []bucket{
		{"pending", len(rs.pending)},
		{"staging", len(rs.staging)},
		{"staged", len(rs.staged)},
		{"rebooting", len(rs.rebooting)},
		{"degraded", len(rs.degraded)},
	}

	b.WriteString("; ")
	for i, bk := range buckets {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%d %s", bk.count, bk.name)
	}

	if len(rs.pdbBlocked) > 0 {
		nodeNames := make([]string, 0, len(rs.pdbBlocked))
		for nodeName := range rs.pdbBlocked {
			nodeNames = append(nodeNames, nodeName)
		}
		slices.Sort(nodeNames)
		for _, nodeName := range nodeNames {
			fmt.Fprintf(&b, "; %s blocked by %s",
				nodeName, pdbBlocksDescription(rs.pdbBlocked[nodeName]))
		}
	}
	return b.String()
}
