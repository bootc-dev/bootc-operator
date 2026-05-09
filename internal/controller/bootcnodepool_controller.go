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
	"fmt"
	"reflect"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	bootcv1alpha1 "github.com/jlebon/bootc-operator/api/v1alpha1"
)

// BootcNodePoolReconciler reconciles a BootcNodePool object
type BootcNodePoolReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=node.bootc.dev,resources=bootcnodepools,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=node.bootc.dev,resources=bootcnodepools/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=node.bootc.dev,resources=bootcnodepools/finalizers,verbs=update
// +kubebuilder:rbac:groups=node.bootc.dev,resources=bootcnodes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=node.bootc.dev,resources=bootcnodes/status,verbs=get
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;update;patch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *BootcNodePoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithValues("pool", req.Name)

	// Fetch the pool.
	var pool bootcv1alpha1.BootcNodePool
	if err := r.Get(ctx, req.NamespacedName, &pool); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("Pool deleted, nothing to do")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fetching pool: %w", err)
	}

	// Sync pool membership.
	if err := r.syncMembership(ctx, &pool); err != nil {
		return ctrl.Result{}, fmt.Errorf("syncing membership: %w", err)
	}

	return ctrl.Result{}, nil
}

// syncMembership reconciles the set of BootcNodes owned by this pool
// against the set of Nodes matching the pool's nodeSelector.
func (r *BootcNodePoolReconciler) syncMembership(ctx context.Context, pool *bootcv1alpha1.BootcNodePool) error {
	log := logf.FromContext(ctx).WithValues("pool", pool.Name)

	// List all nodes matching the pool's selector.
	matchingNodes, err := r.listMatchingNodes(ctx, pool)
	if err != nil {
		return fmt.Errorf("listing matching nodes: %w", err)
	}
	matchingSet := map[string]*corev1.Node{}
	for i := range matchingNodes {
		matchingSet[matchingNodes[i].Name] = &matchingNodes[i]
	}

	// List all BootcNodes and partition into owned by this pool vs others.
	allBootcNodes, err := r.listAllBootcNodes(ctx)
	if err != nil {
		return fmt.Errorf("listing BootcNodes: %w", err)
	}
	ownedSet := map[string]*bootcv1alpha1.BootcNode{}
	for name, bn := range allBootcNodes {
		if metav1.IsControlledBy(bn, pool) {
			ownedSet[name] = bn
		}
	}

	// Create BootcNodes for new matches and sync spec for existing ones.
	for nodeName, node := range matchingSet {
		if bn, exists := ownedSet[nodeName]; exists {
			// Sync spec fields if needed.
			if err := r.syncBootcNodeSpec(ctx, pool, bn); err != nil {
				return fmt.Errorf("syncing BootcNode spec for %s: %w", nodeName, err)
			}
		} else {
			// New match: create BootcNode and label the node.
			log.Info("Creating BootcNode for new match", "node", nodeName)
			if err := r.createBootcNode(ctx, pool, node); err != nil {
				return fmt.Errorf("creating BootcNode for %s: %w", nodeName, err)
			}
		}
	}

	// Delete BootcNodes for nodes that no longer match.
	for nodeName, bn := range ownedSet {
		if _, stillMatches := matchingSet[nodeName]; !stillMatches {
			log.Info("Removing BootcNode for departed node", "node", nodeName)
			if err := r.removeBootcNode(ctx, bn); err != nil {
				return fmt.Errorf("removing BootcNode for %s: %w", nodeName, err)
			}
		}
	}

	return nil
}

// createBootcNode creates a BootcNode for a node joining the pool and
// labels the node as managed.
func (r *BootcNodePoolReconciler) createBootcNode(ctx context.Context, pool *bootcv1alpha1.BootcNodePool, node *corev1.Node) error {
	bn := &bootcv1alpha1.BootcNode{
		ObjectMeta: metav1.ObjectMeta{
			Name: node.Name,
		},
		Spec: bootcv1alpha1.BootcNodeSpec{
			DesiredImage:      pool.Spec.Image.Ref,
			DesiredImageState: bootcv1alpha1.DesiredImageStateStaged,
		},
	}

	// Copy pull secret ref from pool if set.
	if pool.Spec.PullSecretRef != nil {
		bn.Spec.PullSecretRef = pool.Spec.PullSecretRef.DeepCopy()
	}

	// Set ownerReference so the BootcNode is cleaned up if the pool is
	// deleted and so the Owns() watch routes BootcNode events to this pool.
	if err := controllerutil.SetControllerReference(pool, bn, r.Scheme); err != nil {
		return fmt.Errorf("setting owner reference: %w", err)
	}

	if err := r.Create(ctx, bn); err != nil {
		return fmt.Errorf("creating BootcNode: %w", err)
	}

	// Label the node as managed.
	if err := r.ensureManagedLabel(ctx, node, true); err != nil {
		return fmt.Errorf("labeling node: %w", err)
	}

	return nil
}

// removeBootcNode deletes a BootcNode for a node leaving the pool,
// removes the managed label, and restores prior cordon state.
func (r *BootcNodePoolReconciler) removeBootcNode(ctx context.Context, bn *bootcv1alpha1.BootcNode) error {
	// Try to clean up the node (label + cordon state) before deleting
	// the BootcNode. The node may have been deleted from the cluster.
	var node corev1.Node
	if err := r.Get(ctx, types.NamespacedName{Name: bn.Name}, &node); err == nil {
		// No point in cleaning up the Node object if it's going away anyway...
		if node.DeletionTimestamp == nil {
			if err := r.restoreCordonState(ctx, &node); err != nil {
				return fmt.Errorf("restoring cordon state: %w", err)
			}
			if err := r.ensureManagedLabel(ctx, &node, false); err != nil {
				return fmt.Errorf("removing managed label: %w", err)
			}
		}
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("fetching node %s: %w", bn.Name, err)
	}

	if err := r.Delete(ctx, bn); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting BootcNode: %w", err)
	}

	return nil
}

// syncBootcNodeSpec updates a BootcNode's spec fields to match the pool.
func (r *BootcNodePoolReconciler) syncBootcNodeSpec(ctx context.Context, pool *bootcv1alpha1.BootcNodePool, bn *bootcv1alpha1.BootcNode) error {
	modified := bn.DeepCopy()
	desiredImage := pool.Spec.Image.Ref
	needPatch := false

	if modified.Spec.DesiredImage != desiredImage {
		modified.Spec.DesiredImage = desiredImage
		// desiredImage changed; reset desired state to Staged to revoke any
		// pending reboot approval
		modified.Spec.DesiredImageState = bootcv1alpha1.DesiredImageStateStaged
		needPatch = true
	}

	newPullSecretRef := pool.Spec.PullSecretRef.DeepCopy()
	if !reflect.DeepEqual(modified.Spec.PullSecretRef, newPullSecretRef) {
		modified.Spec.PullSecretRef = newPullSecretRef
		needPatch = true
	}

	if needPatch {
		if err := r.Patch(ctx, modified, client.MergeFrom(bn)); err != nil {
			return fmt.Errorf("patching BootcNode: %w", err)
		}
		*bn = *modified
	}

	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *BootcNodePoolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&bootcv1alpha1.BootcNodePool{}).
		Owns(&bootcv1alpha1.BootcNode{}).
		Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(r.mapNodeToPoolRequests), builder.WithPredicates(nodePredicates())).
		Named("bootcnodepool").
		Complete(r)
}

// mapNodeToPoolRequests maps a Node event to the BootcNodePool(s) that should
// be reconciled. It enqueues two sets: (1) pools whose nodeSelector matches
// the node's current labels, and (2) if a BootcNode exists for this node, the
// pool that owns it. The second set is needed so the owning pool can clean up
// when a node's labels change such that it no longer matches, or when the node
// is deleted entirely.
func (r *BootcNodePoolReconciler) mapNodeToPoolRequests(ctx context.Context, obj client.Object) []reconcile.Request {
	node, ok := obj.(*corev1.Node)
	if !ok {
		return nil
	}
	log := logf.FromContext(ctx).WithValues("node", node.Name)

	var requests []reconcile.Request
	seen := map[types.NamespacedName]bool{}

	// (1) Pools whose selector matches this node's labels.
	var pools bootcv1alpha1.BootcNodePoolList
	if err := r.List(ctx, &pools); err != nil {
		log.Error(err, "Failed to list BootcNodePools in node mapper")
		return nil
	}
	for i := range pools.Items {
		pool := &pools.Items[i]
		matches, err := nodeSelectorMatchesNode(pool.Spec.NodeSelector, node)
		if err != nil {
			log.Error(err, "Failed to evaluate nodeSelector", "pool", pool.Name)
			continue
		}
		if matches {
			key := types.NamespacedName{Name: pool.Name}
			if !seen[key] {
				log.V(1).Info("Node matches pool selector", "pool", pool.Name)
				requests = append(requests, reconcile.Request{NamespacedName: key})
				seen[key] = true
			}
		}
	}

	// (2) Pool that owns the BootcNode for this node (if any).
	var bootcNode bootcv1alpha1.BootcNode
	if err := r.Get(ctx, types.NamespacedName{Name: node.Name}, &bootcNode); err != nil {
		// No BootcNode for this node — nothing else to enqueue.
		return requests
	}
	for _, ref := range bootcNode.OwnerReferences {
		if ref.Kind == "BootcNodePool" {
			key := types.NamespacedName{Name: ref.Name}
			if !seen[key] {
				log.V(1).Info("Node has BootcNode owned by pool", "pool", ref.Name)
				requests = append(requests, reconcile.Request{NamespacedName: key})
				seen[key] = true
			}
		}
	}

	return requests
}

// nodePredicates returns predicates that filter Node events to only those
// relevant to pool membership: label changes, Ready condition changes, and
// spec.unschedulable changes.
func nodePredicates() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return true
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return true
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldNode, ok1 := e.ObjectOld.(*corev1.Node)
			newNode, ok2 := e.ObjectNew.(*corev1.Node)
			if !ok1 || !ok2 {
				return true
			}
			return nodeLabelsChanged(oldNode, newNode) ||
				nodeReadyConditionChanged(oldNode, newNode) ||
				nodeUnschedulableChanged(oldNode, newNode)
		},
		GenericFunc: func(e event.GenericEvent) bool {
			return true
		},
	}
}

// listMatchingNodes returns all Nodes whose labels match the pool's
// nodeSelector.
func (r *BootcNodePoolReconciler) listMatchingNodes(ctx context.Context, pool *bootcv1alpha1.BootcNodePool) ([]corev1.Node, error) {
	selector, err := metav1.LabelSelectorAsSelector(pool.Spec.NodeSelector)
	if err != nil {
		return nil, fmt.Errorf("parsing nodeSelector: %w", err)
	}

	var nodeList corev1.NodeList
	if err := r.List(ctx, &nodeList, client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return nil, fmt.Errorf("listing nodes: %w", err)
	}
	return nodeList.Items, nil
}

// listAllBootcNodes returns all BootcNodes keyed by name.
func (r *BootcNodePoolReconciler) listAllBootcNodes(ctx context.Context) (map[string]*bootcv1alpha1.BootcNode, error) {
	var bnList bootcv1alpha1.BootcNodeList
	if err := r.List(ctx, &bnList); err != nil {
		return nil, fmt.Errorf("listing BootcNodes: %w", err)
	}

	all := make(map[string]*bootcv1alpha1.BootcNode, len(bnList.Items))
	for i := range bnList.Items {
		all[bnList.Items[i].Name] = &bnList.Items[i]
	}
	return all, nil
}

// ensureManagedLabel adds or removes the bootc.dev/managed label on a Node.
func (r *BootcNodePoolReconciler) ensureManagedLabel(ctx context.Context, node *corev1.Node, managed bool) error {
	_, hasLabel := node.Labels[bootcv1alpha1.LabelManaged]
	if managed && hasLabel {
		return nil
	}
	if !managed && !hasLabel {
		return nil
	}

	modified := node.DeepCopy()
	if managed {
		if modified.Labels == nil {
			modified.Labels = map[string]string{}
		}
		modified.Labels[bootcv1alpha1.LabelManaged] = ""
	} else {
		delete(modified.Labels, bootcv1alpha1.LabelManaged)
	}
	if err := r.Patch(ctx, modified, client.StrategicMergeFrom(node)); err != nil {
		return err
	}
	*node = *modified
	return nil
}

// restoreCordonState restores a node's cordon state based on the
// bootc.dev/was-cordoned annotation. If the annotation is "true", the
// node was already cordoned before the operator touched it, so we leave
// it as is. Otherwise we uncordon it. The annotation is removed.
func (r *BootcNodePoolReconciler) restoreCordonState(ctx context.Context, node *corev1.Node) error {
	_, hasAnnotation := node.Annotations[bootcv1alpha1.AnnotationWasCordoned]
	if !hasAnnotation {
		return nil
	}

	modified := node.DeepCopy()
	wasCordoned := modified.Annotations[bootcv1alpha1.AnnotationWasCordoned] == "true"
	if !wasCordoned {
		// Node was not cordoned before we touched it; uncordon it.
		modified.Spec.Unschedulable = false
	}

	delete(modified.Annotations, bootcv1alpha1.AnnotationWasCordoned)
	if err := r.Patch(ctx, modified, client.StrategicMergeFrom(node)); err != nil {
		return err
	}
	*node = *modified
	return nil
}

// nodeSelectorMatchesNode evaluates whether a node's labels match a
// LabelSelector.
func nodeSelectorMatchesNode(sel *metav1.LabelSelector, node *corev1.Node) (bool, error) {
	selector, err := metav1.LabelSelectorAsSelector(sel)
	if err != nil {
		return false, err
	}
	return selector.Matches(labels.Set(node.Labels)), nil
}

func nodeLabelsChanged(oldNode, newNode *corev1.Node) bool {
	return !reflect.DeepEqual(oldNode.Labels, newNode.Labels)
}

func nodeReadyConditionChanged(oldNode, newNode *corev1.Node) bool {
	return nodeReadyStatus(oldNode) != nodeReadyStatus(newNode)
}

func nodeReadyStatus(node *corev1.Node) corev1.ConditionStatus {
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status
		}
	}
	return corev1.ConditionUnknown
}

func nodeUnschedulableChanged(oldNode, newNode *corev1.Node) bool {
	return oldNode.Spec.Unschedulable != newNode.Spec.Unschedulable
}
