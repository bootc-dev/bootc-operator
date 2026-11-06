// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	bootcv1alpha1 "github.com/bootc-dev/bootc-operator/api/v1alpha1"
)

const (
	// pdbMultipleMatchHint explains that a pod selected by more than one
	// PodDisruptionBudget is a configuration error.
	pdbMultipleMatchHint = "misconfiguration: pod is selected by multiple PodDisruptionBudgets"

	// pdbNeverAllowsEvictionHint explains a budget that cannot allow an
	// eviction even when every pod is healthy.
	pdbNeverAllowsEvictionHint = "never allows eviction (desiredHealthy >= expectedPods)"
)

// pdbRef identifies a PodDisruptionBudget.
type pdbRef struct {
	Namespace string
	Name      string
}

func (r pdbRef) String() string {
	return r.Namespace + "/" + r.Name
}

// pdbBlock describes why a pod cannot be evicted through a
// PodDisruptionBudget. PDBs holds one budget normally, or every matching
// budget when a pod is selected by more than one.
type pdbBlock struct {
	PDBs []pdbRef
	Hint string
}

// key returns a stable identifier for the blocking PDB set.
func (b pdbBlock) key() string {
	refs := make([]string, len(b.PDBs))
	for i, ref := range b.PDBs {
		refs[i] = ref.String()
	}
	slices.Sort(refs)
	return strings.Join(refs, ",")
}

// describe renders the block for an event note.
func (b pdbBlock) describe() string {
	refs := make([]string, len(b.PDBs))
	for i, ref := range b.PDBs {
		refs[i] = ref.String()
	}
	slices.Sort(refs)

	noun := "PodDisruptionBudget"
	if len(refs) > 1 {
		noun = "PodDisruptionBudgets"
	}
	return fmt.Sprintf("%s %s: %s", noun, strings.Join(refs, ", "), b.Hint)
}

// pdbCanIgnorePod reports whether eviction bypasses PodDisruptionBudgets for
// this pod. The eviction API ignores budgets for pending, terminal, and
// already-terminating pods.
func pdbCanIgnorePod(pod *corev1.Pod) bool {
	switch pod.Status.Phase {
	case corev1.PodPending, corev1.PodSucceeded, corev1.PodFailed:
		return true
	}
	return pod.DeletionTimestamp != nil
}

// podReady reports whether the pod's Ready condition is true.
func podReady(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

// unhealthyPodEvictableByPDB reports whether the pod may be evicted despite a
// zero disruption budget because it is unhealthy and the budget's unhealthy-pod
// eviction policy allows it.
func unhealthyPodEvictableByPDB(pod *corev1.Pod, pdb *policyv1.PodDisruptionBudget) bool {
	if podReady(pod) {
		return false
	}

	policy := policyv1.IfHealthyBudget
	if pdb.Spec.UnhealthyPodEvictionPolicy != nil {
		policy = *pdb.Spec.UnhealthyPodEvictionPolicy
	}

	switch policy {
	case policyv1.AlwaysAllow:
		return true
	case policyv1.IfHealthyBudget:
		return pdb.Status.DesiredHealthy > 0 &&
			pdb.Status.CurrentHealthy >= pdb.Status.DesiredHealthy
	default:
		return false
	}
}

// pdbBlockHint explains why a zero-budget PDB cannot make progress.
func pdbBlockHint(pdb *policyv1.PodDisruptionBudget) string {
	if pdb.Status.CurrentHealthy < pdb.Status.ExpectedPods {
		notReady := pdb.Status.ExpectedPods - pdb.Status.CurrentHealthy
		return fmt.Sprintf("%d pod(s) not ready", notReady)
	}
	return pdbNeverAllowsEvictionHint
}

// pdbBlockForPod returns why pod cannot be evicted because of pdbs, or nil if
// no PDB blocks it.
func pdbBlockForPod(pod *corev1.Pod, pdbs []policyv1.PodDisruptionBudget) (*pdbBlock, error) {
	if pdbCanIgnorePod(pod) {
		return nil, nil
	}

	var matched []policyv1.PodDisruptionBudget
	for i := range pdbs {
		selector, err := metav1.LabelSelectorAsSelector(pdbs[i].Spec.Selector)
		if err != nil {
			return nil, fmt.Errorf(
				"invalid selector on PodDisruptionBudget %s/%s: %w",
				pdbs[i].Namespace,
				pdbs[i].Name,
				err,
			)
		}
		if selector.Matches(labels.Set(pod.Labels)) {
			matched = append(matched, pdbs[i])
		}
	}

	switch {
	case len(matched) >= 2:
		refs := make([]pdbRef, len(matched))
		for i := range matched {
			refs[i] = pdbRef{Namespace: matched[i].Namespace, Name: matched[i].Name}
		}
		return &pdbBlock{PDBs: refs, Hint: pdbMultipleMatchHint}, nil
	case len(matched) == 1:
		pdb := &matched[0]
		if pdb.Status.DisruptionsAllowed != 0 {
			return nil, nil
		}
		if unhealthyPodEvictableByPDB(pod, pdb) {
			return nil, nil
		}
		return &pdbBlock{
			PDBs: []pdbRef{{Namespace: pdb.Namespace, Name: pdb.Name}},
			Hint: pdbBlockHint(pdb),
		}, nil
	default:
		return nil, nil
	}
}

// pdbBlocksForPods returns the unique PDB blocks for all pods.
func pdbBlocksForPods(
	pods []corev1.Pod,
	pdbsByNamespace map[string][]policyv1.PodDisruptionBudget,
) ([]pdbBlock, error) {
	var blocks []pdbBlock
	seen := map[string]bool{}

	for i := range pods {
		block, err := pdbBlockForPod(&pods[i], pdbsByNamespace[pods[i].Namespace])
		if err != nil {
			return nil, err
		}
		if block == nil || seen[block.key()] {
			continue
		}
		seen[block.key()] = true
		blocks = append(blocks, *block)
	}

	slices.SortFunc(blocks, func(a, b pdbBlock) int {
		return strings.Compare(a.key(), b.key())
	})
	return blocks, nil
}

// pdbBlocksKey returns a stable key for a set of blocks so callers can detect
// when the blocking PDBs change.
func pdbBlocksKey(blocks []pdbBlock) string {
	keys := make([]string, len(blocks))
	for i := range blocks {
		keys[i] = blocks[i].key()
	}
	slices.Sort(keys)
	return strings.Join(keys, ";")
}

// pdbBlocksDescription names the PDBs in the blocks for status messages.
func pdbBlocksDescription(blocks []pdbBlock) string {
	seen := map[string]bool{}
	var refs []string
	for _, block := range blocks {
		for _, ref := range block.PDBs {
			key := ref.String()
			if seen[key] {
				continue
			}
			seen[key] = true
			refs = append(refs, key)
		}
	}
	slices.Sort(refs)

	noun := "PDB"
	if len(refs) > 1 {
		noun = "PDBs"
	}
	return noun + " " + strings.Join(refs, ", ")
}

// findPDBBlocksForNode lists the pods the drainer would evict from nodeName and
// reports which PodDisruptionBudgets block them.
func (r *BootcNodePoolReconciler) findPDBBlocksForNode(
	ctx context.Context,
	pool *bootcv1alpha1.BootcNodePool,
	nodeName string,
) ([]pdbBlock, error) {
	log := logf.FromContext(ctx)

	helper := r.newDrainHelper(ctx, nodeName, resolveDrainTimeout(pool))
	podList, errs := helper.GetPodsForDeletion(nodeName)
	for _, err := range errs {
		log.V(1).Info(
			"Ignoring pod-list warning while checking PodDisruptionBudgets",
			"node", nodeName,
			"error", err,
		)
	}
	if podList == nil {
		return nil, fmt.Errorf("listing pods to drain from node %s failed", nodeName)
	}

	var pods []corev1.Pod
	namespaces := map[string]bool{}
	for _, pod := range podList.Pods() {
		if pod.DeletionTimestamp != nil {
			continue
		}
		pods = append(pods, pod)
		namespaces[pod.Namespace] = true
	}
	if len(pods) == 0 {
		return nil, nil
	}

	namespaceNames := make([]string, 0, len(namespaces))
	for namespace := range namespaces {
		namespaceNames = append(namespaceNames, namespace)
	}
	slices.Sort(namespaceNames)

	pdbsByNamespace := make(map[string][]policyv1.PodDisruptionBudget, len(namespaceNames))
	for _, namespace := range namespaceNames {
		pdbList, err := r.KubeClient.PolicyV1().PodDisruptionBudgets(namespace).List(
			ctx,
			metav1.ListOptions{},
		)
		if err != nil {
			return nil, fmt.Errorf(
				"listing PodDisruptionBudgets in namespace %s: %w",
				namespace,
				err,
			)
		}
		pdbsByNamespace[namespace] = pdbList.Items
	}

	return pdbBlocksForPods(pods, pdbsByNamespace)
}
