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
	_ "crypto/sha256" // register SHA-256 for digest validation
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/distribution/reference"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	bootcv1alpha1 "github.com/jlebon/bootc-operator/api/v1alpha1"
)

// drainStatus tracks an in-progress drain goroutine for a single node.
type drainStatus struct {
	result    chan error         //nolint:unused // receives nil on success, error on failure; closed after send
	cancel    context.CancelFunc //nolint:unused // to abort on targetDigest change or node removal
	startTime time.Time          //nolint:unused // for stall detection
	isStalled bool               //nolint:unused // set once after stall threshold; triggers event emission
}

// BootcNodePoolReconciler reconciles a BootcNodePool object
type BootcNodePoolReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder

	// drainCh receives events from drain goroutines to re-enqueue the
	// owning pool after a drain completes.
	drainCh chan event.GenericEvent

	// drains tracks in-progress drain goroutines keyed by node name.
	// Protected by drainsMu. The mutex is not necessary today since
	// MaxConcurrentReconciles defaults to 1, but would be needed if
	// concurrent reconciles are enabled in the future.
	drains   map[string]*drainStatus //nolint:unused
	drainsMu sync.Mutex              //nolint:unused
}

// +kubebuilder:rbac:groups=node.bootc.dev,resources=bootcnodepools,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=node.bootc.dev,resources=bootcnodepools/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=node.bootc.dev,resources=bootcnodepools/finalizers,verbs=update
// +kubebuilder:rbac:groups=node.bootc.dev,resources=bootcnodes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=node.bootc.dev,resources=bootcnodes/status,verbs=get
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// SetupWithManager sets up the controller with the Manager.
func (r *BootcNodePoolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Recorder = mgr.GetEventRecorder("bootc-operator")
	r.drainCh = make(chan event.GenericEvent, 100)
	r.drains = make(map[string]*drainStatus)

	return ctrl.NewControllerManagedBy(mgr).
		For(&bootcv1alpha1.BootcNodePool{}).
		Owns(&bootcv1alpha1.BootcNode{}).
		Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(r.mapNodeToPoolRequests), builder.WithPredicates(nodePredicates())).
		WatchesRawSource(source.Channel(r.drainCh, &handler.EnqueueRequestForObject{})).
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

// nodeSelectorMatchesNode evaluates whether a node's labels match a
// LabelSelector.
func nodeSelectorMatchesNode(sel *metav1.LabelSelector, node *corev1.Node) (bool, error) {
	selector, err := metav1.LabelSelectorAsSelector(sel)
	if err != nil {
		return false, err
	}
	return selector.Matches(labels.Set(node.Labels)), nil
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

	// Snapshot status so we can detect changes and write once at the end.
	statusOrig := pool.Status.DeepCopy()

	// Start with conditions in a healthy state; sync functions only set
	// degraded conditions when something is wrong.
	apimeta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
		Type:   bootcv1alpha1.PoolDegraded,
		Status: metav1.ConditionFalse,
		Reason: bootcv1alpha1.PoolOK,
	})

	// Resolve the target digest from the image ref.
	if err := r.resolveTargetDigest(&pool); err != nil {
		if isInvalidSpecError(err) {
			return r.setInvalidSpecCondition(ctx, &pool, err)
		}
		return ctrl.Result{}, fmt.Errorf("resolving target digest: %w", err)
	}

	// Sync pool membership.
	if err := r.syncMembership(ctx, &pool); err != nil {
		if isInvalidSpecError(err) {
			return r.setInvalidSpecCondition(ctx, &pool, err)
		}
		return ctrl.Result{}, fmt.Errorf("syncing membership: %w", err)
	}

	// Write pool status once if anything changed.
	if !reflect.DeepEqual(pool.Status, *statusOrig) {
		if err := r.Status().Update(ctx, &pool); err != nil {
			return ctrl.Result{}, fmt.Errorf("updating pool status: %w", err)
		}
	}

	return ctrl.Result{}, nil
}

// resolveTargetDigest parses the digest from the pool's image ref and
// sets pool.Status.TargetDigest. For digest refs (the only kind
// supported now), the digest is extracted directly. Tag resolution is
// deferred to Milestone 5.
func (r *BootcNodePoolReconciler) resolveTargetDigest(pool *bootcv1alpha1.BootcNodePool) error {
	ref, err := parseImageRef(pool.Spec.Image.Ref)
	if err != nil {
		return &invalidSpecError{fmt.Sprintf("invalid image ref %q: %v", pool.Spec.Image.Ref, err)}
	}
	digested, ok := ref.(reference.Digested)
	if !ok {
		return &invalidSpecError{fmt.Sprintf("image ref %q has no digest (tag resolution not yet supported)", pool.Spec.Image.Ref)}
	}
	pool.Status.TargetDigest = digested.Digest().String()
	return nil
}

// parseImageRef parses an image reference string into a named
// reference, validating the format per OCI conventions. It requires
// fully qualified names (e.g. "quay.io/example/myos:latest"); short
// names like "myos:latest" are rejected.
func parseImageRef(ref string) (reference.Named, error) {
	return reference.ParseNamed(ref)
}

// invalidSpecError indicates a user-provided spec value that the
// controller cannot process. Reconcile surfaces these as
// Degraded/InvalidSpec conditions rather than requeueing with backoff.
type invalidSpecError struct {
	msg string
}

func (e *invalidSpecError) Error() string { return e.msg }

func isInvalidSpecError(err error) bool {
	var e *invalidSpecError
	return errors.As(err, &e)
}

// setInvalidSpecCondition sets Degraded/InvalidSpec on the pool and
// returns (Result, nil) so Reconcile stops without requeueing.
func (r *BootcNodePoolReconciler) setInvalidSpecCondition(ctx context.Context, pool *bootcv1alpha1.BootcNodePool, specErr error) (ctrl.Result, error) {
	apimeta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
		Type:    bootcv1alpha1.PoolDegraded,
		Status:  metav1.ConditionTrue,
		Reason:  bootcv1alpha1.PoolInvalidSpec,
		Message: specErr.Error(),
	})
	if err := r.Status().Update(ctx, pool); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating pool status: %w", err)
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
			// XXX: val should probably be a list of nodes instead of a bool
			// so we can say in the conflict condition msg exactly which nodes
			// are overlapping
			ownedSet[name] = bn
		}
	}

	// Create BootcNodes for new matches and sync spec for existing ones.
	// If a BootcNode already exists for a node but is owned by a different
	// pool, that's a conflict — we skip that node and track the
	// conflicting pool name.
	conflicting := map[string]bool{}
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
				if !apierrors.IsAlreadyExists(err) {
					return fmt.Errorf("creating BootcNode for %s: %w", nodeName, err)
				}
				// BootcNode exists but isn't ours — find the owning pool.
				if existing, ok := allBootcNodes[nodeName]; ok {
					if owner := metav1.GetControllerOf(existing); owner != nil {
						conflicting[owner.Name] = true
					}
				}
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

	// Set or clear the conflict condition based on what we found.
	var conflictingPools []string
	for name := range conflicting {
		conflictingPools = append(conflictingPools, name)
	}
	syncConflictCondition(pool, conflictingPools)

	return nil
}

// listMatchingNodes returns all Nodes whose labels match the pool's
// nodeSelector.
func (r *BootcNodePoolReconciler) listMatchingNodes(ctx context.Context, pool *bootcv1alpha1.BootcNodePool) ([]corev1.Node, error) {
	selector, err := metav1.LabelSelectorAsSelector(pool.Spec.NodeSelector)
	if err != nil {
		return nil, &invalidSpecError{msg: fmt.Sprintf("invalid nodeSelector: %v", err)}
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

// syncBootcNodeSpec updates a BootcNode's spec fields to match the pool.
func (r *BootcNodePoolReconciler) syncBootcNodeSpec(ctx context.Context, pool *bootcv1alpha1.BootcNodePool, bn *bootcv1alpha1.BootcNode) error {
	modified := bn.DeepCopy()
	desiredImage := desiredImageFromPool(pool)
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

// desiredImageFromPool constructs the desiredImage pullspec from the
// pool's image name and resolved targetDigest (e.g.
// "quay.io/example/myos@sha256:abc123").
func desiredImageFromPool(pool *bootcv1alpha1.BootcNodePool) string {
	ref, _ := parseImageRef(pool.Spec.Image.Ref)
	return reference.TrimNamed(ref).String() + "@" + pool.Status.TargetDigest
}

// createBootcNode creates a BootcNode for a node joining the pool and
// labels the node as managed.
func (r *BootcNodePoolReconciler) createBootcNode(ctx context.Context, pool *bootcv1alpha1.BootcNodePool, node *corev1.Node) error {
	bn := &bootcv1alpha1.BootcNode{
		ObjectMeta: metav1.ObjectMeta{
			Name: node.Name,
		},
		Spec: bootcv1alpha1.BootcNodeSpec{
			DesiredImage:      desiredImageFromPool(pool),
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

// syncConflictCondition sets or clears the Degraded condition with
// reason NodeConflict on the pool. It only mutates the in-memory
// object; the caller is responsible for writing status.
func syncConflictCondition(pool *bootcv1alpha1.BootcNodePool, conflictingPools []string) {
	if len(conflictingPools) > 0 {
		apimeta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
			Type:   bootcv1alpha1.PoolDegraded,
			Status: metav1.ConditionTrue,
			Reason: bootcv1alpha1.PoolNodeConflict,
			// Sort so the message is stable across reconciles.
			Message: fmt.Sprintf("Node selector overlaps with pool(s): %s",
				strings.Join(slices.Sorted(slices.Values(conflictingPools)), ", ")),
		})
	}
}
