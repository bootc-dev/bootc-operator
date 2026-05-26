// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	bootcv1alpha1 "github.com/jlebon/bootc-operator/api/v1alpha1"
	"github.com/jlebon/bootc-operator/internal/bootc"
)

// BootcNodeReconciler reconciles the BootcNode for the node this daemon
// runs on. It reads bootc status from the host and writes it into the
// BootcNode's status subresource.
type BootcNodeReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	NodeName string
	Executor bootc.Executor
}

func (r *BootcNodeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&bootcv1alpha1.BootcNode{}).
		Named("bootcnode").
		Complete(r)
}

func (r *BootcNodeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithValues("node", r.NodeName)

	if req.Name != r.NodeName {
		return ctrl.Result{}, nil
	}

	var bn bootcv1alpha1.BootcNode
	if err := r.Get(ctx, req.NamespacedName, &bn); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("BootcNode not found, waiting for controller to create it")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fetching BootcNode: %w", err)
	}

	patch := client.MergeFrom(bn.DeepCopy())

	if err := r.populateStatus(ctx, &bn); err != nil {
		log.Error(err, "Failed to populate bootc status")
	}

	if err := r.Status().Patch(ctx, &bn, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("patching BootcNode status: %w", err)
	}

	log.Info("Patched BootcNode status from bootc")
	return ctrl.Result{}, nil
}

func (r *BootcNodeReconciler) populateStatus(ctx context.Context, bn *bootcv1alpha1.BootcNode) error {
	data, err := r.Executor.Status(ctx)
	if err != nil {
		apimeta.SetStatusCondition(&bn.Status.Conditions, metav1.Condition{
			Type:               bootcv1alpha1.NodeDegraded,
			Status:             metav1.ConditionTrue,
			Reason:             bootcv1alpha1.NodeReasonError,
			Message:            fmt.Sprintf("failed to get bootc status: %v", err),
			ObservedGeneration: bn.Generation,
		})
		return fmt.Errorf("getting bootc status: %w", err)
	}

	status, err := bootc.ParseStatus(data)
	if err != nil {
		apimeta.SetStatusCondition(&bn.Status.Conditions, metav1.Condition{
			Type:               bootcv1alpha1.NodeDegraded,
			Status:             metav1.ConditionTrue,
			Reason:             bootcv1alpha1.NodeReasonError,
			Message:            fmt.Sprintf("failed to parse bootc status: %v", err),
			ObservedGeneration: bn.Generation,
		})
		return fmt.Errorf("parsing bootc status: %w", err)
	}

	bn.Status.ObservedGeneration = bn.Generation
	bn.Status.Booted = convertBootEntry(status.Status.Booted)
	bn.Status.Staged = convertBootEntry(status.Status.Staged)
	bn.Status.Rollback = convertBootEntry(status.Status.Rollback)

	apimeta.SetStatusCondition(&bn.Status.Conditions, metav1.Condition{
		Type:               bootcv1alpha1.NodeIdle,
		Status:             metav1.ConditionTrue,
		Reason:             bootcv1alpha1.NodeReasonIdle,
		ObservedGeneration: bn.Generation,
	})
	apimeta.SetStatusCondition(&bn.Status.Conditions, metav1.Condition{
		Type:               bootcv1alpha1.NodeDegraded,
		Status:             metav1.ConditionFalse,
		Reason:             bootcv1alpha1.NodeReasonHealthy,
		ObservedGeneration: bn.Generation,
	})

	return nil
}

func convertBootEntry(entry *bootc.BootEntry) *bootcv1alpha1.ImageInfo {
	if entry == nil || entry.Image == nil {
		return nil
	}
	img := entry.Image

	info := &bootcv1alpha1.ImageInfo{
		Image:             img.Image.Image,
		ImageDigest:       img.ImageDigest,
		Architecture:      img.Architecture,
		Incompatible:      entry.Incompatible,
		SoftRebootCapable: entry.SoftRebootCapable,
	}

	if img.Version != nil {
		info.Version = *img.Version
	}

	if img.Timestamp != nil {
		t := metav1.NewTime(*img.Timestamp)
		info.Timestamp = &t
	}

	return info
}
