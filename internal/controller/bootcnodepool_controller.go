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

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

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
// TODO(user): Modify the Reconcile function to compare the state specified by
// the BootcNodePool object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.23.3/pkg/reconcile
func (r *BootcNodePoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	_ = logf.FromContext(ctx)

	// TODO(user): your logic here

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *BootcNodePoolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&bootcv1alpha1.BootcNodePool{}).
		Named("bootcnodepool").
		Complete(r)
}
