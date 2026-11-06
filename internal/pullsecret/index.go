// SPDX-License-Identifier: Apache-2.0

package pullsecret

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	bootcv1alpha1 "github.com/bootc-dev/bootc-operator/api/v1alpha1"
)

// PoolRefIndex is the field index on BootcNodePool that maps a pull secret
// to the pools referencing it. Use it with client.MatchingFields and
// PoolRefIndexValue on a cache-backed client.
const PoolRefIndex = "spec.pullSecretRef"

// PoolRefIndexValue returns the PoolRefIndex value for the secret
// namespace/name.
func PoolRefIndexValue(namespace, name string) string {
	return types.NamespacedName{Namespace: namespace, Name: name}.String()
}

// PoolRefIndexFunc extracts the PoolRefIndex values of a BootcNodePool.
func PoolRefIndexFunc(obj client.Object) []string {
	pool, ok := obj.(*bootcv1alpha1.BootcNodePool)
	if !ok || pool.Spec.PullSecretRef == nil {
		return nil
	}
	ref := pool.Spec.PullSecretRef
	return []string{PoolRefIndexValue(ref.Namespace, ref.Name)}
}

// IndexPoolPullSecretRef registers PoolRefIndex with the manager's cache. A
// manager accepts the registration once, so call it once per manager, before
// starting it.
func IndexPoolPullSecretRef(ctx context.Context, indexer client.FieldIndexer) error {
	err := indexer.IndexField(ctx, &bootcv1alpha1.BootcNodePool{}, PoolRefIndex, PoolRefIndexFunc)
	if err != nil {
		return fmt.Errorf("indexing BootcNodePool %s: %w", PoolRefIndex, err)
	}
	return nil
}
