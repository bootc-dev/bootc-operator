// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	bootcv1alpha1 "github.com/jlebon/bootc-operator/api/v1alpha1"
)

// BootcNodeReconciler reconciles the BootcNode for the node this daemon
// runs on. It reads bootc status from the host and writes it into the
// BootcNode's status subresource.
type BootcNodeReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	NodeName string
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

	log.Info("Reconciling BootcNode")

	return ctrl.Result{}, nil
}
