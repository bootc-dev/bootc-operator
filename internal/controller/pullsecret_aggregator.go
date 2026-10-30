// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	bootcv1alpha1 "github.com/bootc-dev/bootc-operator/api/v1alpha1"
	"github.com/bootc-dev/bootc-operator/internal/pullsecret"
)

// PullSecretAggregator maintains the aggregated pull secret mounted into the
// daemon: one key per source secret referenced by a pool, holding that
// secret's .dockerconfigjson. It is the Secret's only writer and recomputes
// it from scratch on every reconcile.
type PullSecretAggregator struct {
	client.Client

	// Namespace is the operator namespace, which holds the aggregated
	// Secret.
	Namespace string

	// PullSecretNamespaces is the allowlist of namespaces pools may
	// reference pull secrets from.
	PullSecretNamespaces []string
}

// +kubebuilder:rbac:groups="",namespace=system,resources=secrets,verbs=get;list;watch;create
// +kubebuilder:rbac:groups="",namespace=system,resources=secrets,resourceNames=bootc-pull-secrets,verbs=update;patch

// SetupWithManager sets up the aggregator with the Manager. All events map to
// the single aggregated Secret.
func (r *PullSecretAggregator) SetupWithManager(mgr ctrl.Manager) error {
	enqueue := handler.EnqueueRequestsFromMapFunc(
		func(context.Context, client.Object) []reconcile.Request {
			return []reconcile.Request{{NamespacedName: r.secretKey()}}
		},
	)
	// Reconcile once at startup, so the Secret exists even when no pool or
	// source secret generates an event.
	startup := make(chan event.GenericEvent, 1)
	startup <- event.GenericEvent{Object: &corev1.Secret{}}
	return ctrl.NewControllerManagedBy(mgr).
		Named("pullsecretaggregator").
		WatchesRawSource(source.Channel(startup, enqueue)).
		Watches(&bootcv1alpha1.BootcNodePool{}, enqueue,
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&corev1.Secret{}, enqueue,
			builder.WithPredicates(r.secretPredicate())).
		Complete(r)
}

func (r *PullSecretAggregator) secretKey() types.NamespacedName {
	return types.NamespacedName{Namespace: r.Namespace, Name: pullsecret.SecretName}
}

// secretPredicate accepts source secrets (dockerconfigjson) and the
// aggregated Secret itself, so it is repaired if deleted or edited.
func (r *PullSecretAggregator) secretPredicate() predicate.Predicate {
	return predicate.NewPredicateFuncs(func(obj client.Object) bool {
		secret, ok := obj.(*corev1.Secret)
		if !ok {
			return false
		}
		return secret.Type == corev1.SecretTypeDockerConfigJson ||
			client.ObjectKeyFromObject(secret) == r.secretKey()
	})
}

// Reconcile creates or updates the aggregated Secret to match the source
// secrets currently referenced by pools.
func (r *PullSecretAggregator) Reconcile(
	ctx context.Context,
	_ ctrl.Request,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	desired, err := r.desiredData(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}

	var secret corev1.Secret
	err = r.Get(ctx, r.secretKey(), &secret)
	if apierrors.IsNotFound(err) {
		secret = corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      pullsecret.SecretName,
				Namespace: r.Namespace,
			},
			Type: corev1.SecretTypeOpaque,
			Data: desired,
		}
		if err := r.Create(ctx, &secret); err != nil {
			return requeueIfStale(ctx, fmt.Errorf("creating aggregated pull secret: %w", err))
		}
		log.Info("Created aggregated pull secret", "keys", len(desired))
		return ctrl.Result{}, nil
	}
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("fetching aggregated pull secret: %w", err)
	}

	if maps.EqualFunc(secret.Data, desired, bytes.Equal) {
		return ctrl.Result{}, nil
	}
	secret.Data = desired
	if err := r.Update(ctx, &secret); err != nil {
		return requeueIfStale(ctx, fmt.Errorf("updating aggregated pull secret: %w", err))
	}
	log.Info("Updated aggregated pull secret", "keys", len(desired))
	return ctrl.Result{}, nil
}

// requeueIfStale requeues without reporting an error when a write failed
// because the cache was behind (typically right after the aggregator's own
// write); the retry recomputes the Secret from scratch.
func requeueIfStale(ctx context.Context, err error) (ctrl.Result, error) {
	if apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err) {
		logf.FromContext(ctx).
			V(1).
			Info("Aggregated pull secret changed, retrying", "error", err.Error())
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	return ctrl.Result{}, err
}

// desiredData returns the aggregated Secret data for all pools. References
// the pool reconciler reports as SecretError (namespace not allowed, name
// too long, secret missing or without .dockerconfigjson) are skipped.
func (r *PullSecretAggregator) desiredData(ctx context.Context) (map[string][]byte, error) {
	var pools bootcv1alpha1.BootcNodePoolList
	if err := r.List(ctx, &pools); err != nil {
		return nil, fmt.Errorf("listing BootcNodePools: %w", err)
	}

	desired := map[string][]byte{}
	for i := range pools.Items {
		ref := pools.Items[i].Spec.PullSecretRef
		if ref == nil || !slices.Contains(r.PullSecretNamespaces, ref.Namespace) {
			continue
		}
		key, err := pullsecret.Key(ref.Namespace, ref.Name)
		if err != nil {
			continue
		}
		var secret corev1.Secret
		err = r.Get(ctx, types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}, &secret)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("fetching pull secret %s/%s: %w", ref.Namespace, ref.Name, err)
		}
		if data, ok := secret.Data[corev1.DockerConfigJsonKey]; ok {
			desired[key] = data
		}
	}
	return desired, nil
}
