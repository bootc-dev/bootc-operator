// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"fmt"
	"github.com/distribution/reference"
	"reflect"
	"sync"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/source"

	bootcv1alpha1 "github.com/bootc-dev/bootc-operator/api/v1alpha1"
	"github.com/bootc-dev/bootc-operator/internal/bootc"
)

const (
	stageBackoffMin = 5 * time.Second
	// caps exponential backoff at 5m20s
	stageMaxBackoffExponent = 7
)

// stageOp tracks the state of an in-flight bootc stage operation.
type stageOp struct {
	mu sync.Mutex
	// runMu serializes run() calls so that at most one bootc switch
	// process is active at a time, even if the previous one is still
	// exiting after context cancellation.
	runMu   sync.Mutex
	image   string
	cancel  context.CancelFunc
	err     error
	retries int
}

// BootcNodeReconciler reconciles the BootcNode for the node this daemon
// runs on. It reads bootc status from the watcher's cache, detects image
// mismatches, and drives updates via bootc stage.
type BootcNodeReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	NodeName      string
	Executor      bootc.Executor
	StatusWatcher *StatusWatcher

	inflight  stageOp
	stageDone chan event.GenericEvent
	// rebootIssued tracks whether a reboot has been issued so classifyAction
	// can distinguish the Staged→Rebooting.
	rebootIssued bool
}

func (r *BootcNodeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.stageDone = make(chan event.GenericEvent, 1)

	return ctrl.NewControllerManagedBy(mgr).
		For(&bootcv1alpha1.BootcNode{}).
		WatchesRawSource(source.Channel(r.stageDone, &handler.EnqueueRequestForObject{})).
		WatchesRawSource(source.Channel(r.StatusWatcher.Events, &handler.EnqueueRequestForObject{})).
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

	orig := bn.DeepCopy()
	bn.Status.ObservedGeneration = bn.Generation

	apimeta.SetStatusCondition(&bn.Status.Conditions, metav1.Condition{
		Type:               bootcv1alpha1.NodeIdle,
		Status:             metav1.ConditionTrue,
		Reason:             bootcv1alpha1.NodeReasonIdle,
		ObservedGeneration: bn.Generation,
	})

	res, reconcileErr := r.reconcileBootcNode(ctx, &bn)

	if res.degradedMsg != "" {
		apimeta.SetStatusCondition(&bn.Status.Conditions, metav1.Condition{
			Type:               bootcv1alpha1.NodeDegraded,
			Status:             metav1.ConditionTrue,
			Reason:             bootcv1alpha1.NodeReasonError,
			Message:            res.degradedMsg,
			ObservedGeneration: bn.Generation,
		})
	} else {
		apimeta.SetStatusCondition(&bn.Status.Conditions, metav1.Condition{
			Type:               bootcv1alpha1.NodeDegraded,
			Status:             metav1.ConditionFalse,
			Reason:             bootcv1alpha1.NodeReasonHealthy,
			ObservedGeneration: bn.Generation,
		})
	}

	if !reflect.DeepEqual(bn.Status, orig.Status) {
		if patchErr := r.Status().Patch(ctx, &bn, client.MergeFrom(orig)); patchErr != nil {
			return ctrl.Result{}, fmt.Errorf("patching BootcNode status: %w", patchErr)
		}
	}

	// Reboot after the status patch so the Rebooting condition is persisted before the node goes down.
	if res.needsReboot {
		log.Info("Starting reboot")
		if err := r.Executor.Reboot(ctx); err != nil {
			return ctrl.Result{}, fmt.Errorf("reboot: %w", err)
		}
		// Record if the reboot was issued in this way we can transition from Staged to Rebooting
		r.rebootIssued = true
	}

	return res.result, reconcileErr
}

type reconcileResult struct {
	result      ctrl.Result
	degradedMsg string
	needsReboot bool
}

// reconcileBootcNode defines the result of the reconcile of the bootc nodes. It returns the results for the reconcile,
// the degraded message and eventual errors. We distinguish the degraded message from a reconcile error since we want to
// implement an exponential back-off if the staging failed.
func (r *BootcNodeReconciler) reconcileBootcNode(ctx context.Context, bn *bootcv1alpha1.BootcNode) (reconcileResult, error) {
	log := logf.FromContext(ctx).WithValues("node", r.NodeName)

	if err := r.populateBootcFields(ctx, bn); err != nil {
		degradedErr := fmt.Errorf("populating bootc fields: %w", err)
		return reconcileResult{degradedMsg: degradedErr.Error()}, degradedErr
	}

	if bn.Status.Booted == nil {
		degradedErr := fmt.Errorf("bootc status has no booted entry")
		return reconcileResult{degradedMsg: degradedErr.Error()}, degradedErr
	}

	desiredRef, err := reference.ParseNamed(bn.Spec.DesiredImage)
	if err != nil {
		// Image is invalid don't trigger another reconcile iteration
		return reconcileResult{
			degradedMsg: fmt.Sprintf("invalid image ref %q: %v", bn.Spec.DesiredImage, err),
		}, nil
	}
	// The controller always resolves tags to digests at the pool level.
	digested, ok := desiredRef.(reference.Digested)
	if !ok {
		return reconcileResult{
			degradedMsg: fmt.Sprintf("image ref %q has no digest", bn.Spec.DesiredImage),
		}, nil
	}

	// Nothing to do the desired image matches the booted ones.
	// Rest the reconciler to start from a clean state
	if digested.Digest().String() == bn.Status.Booted.ImageDigest {
		r.reset()
		return reconcileResult{}, nil
	}

	stageErr := r.inflight.takeErr()

	if stageErr != nil {
		// Set delay for the requeue. If the error is set, then the requeue delay is ignored. For this reason, in this
		// case we set the degraded message but not the reconcile error.
		return reconcileResult{
			result:      ctrl.Result{RequeueAfter: r.inflight.backoff()},
			degradedMsg: fmt.Sprintf("bootc stage failed: %v", stageErr),
		}, nil
	}

	desiredImage := desiredRef.String()

	action := r.classifyAction(bn, digested, desiredImage)

	res := reconcileResult{}
	var reason string
	switch action {
	case actionStage:
		reason = bootcv1alpha1.NodeReasonStaging
		switchCtx, cancel := context.WithCancel(context.Background())
		r.inflight.acquire(log, desiredImage, cancel)
		log.Info("Starting staging", "image", desiredImage)
		go r.inflight.run(switchCtx, r.NodeName, desiredImage, r.Executor, r.stageDone)

	case actionAwaitStage:
		reason = bootcv1alpha1.NodeReasonStaging

	case actionReboot:
		reason = bootcv1alpha1.NodeReasonRebooting
		res.needsReboot = true

	case actionAwaitReboot:
		reason = bootcv1alpha1.NodeReasonRebooting

	case actionAwaitBooted:
		reason = bootcv1alpha1.NodeReasonStaged
		log.Info("Image staged", "image", desiredImage)
	}

	apimeta.SetStatusCondition(&bn.Status.Conditions, metav1.Condition{
		Type:               bootcv1alpha1.NodeIdle,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		ObservedGeneration: bn.Generation,
	})

	return res, nil
}

func (s *stageOp) takeErr() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := s.err
	s.err = nil
	if err != nil && s.retries < stageMaxBackoffExponent {
		s.retries++
	}
	return err
}

func (s *stageOp) backoff() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return stageBackoffMin << (s.retries - 1)
}

func (s *stageOp) isInFlight(image string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.image == image
}

func (s *stageOp) acquire(log logr.Logger, image string, cancel context.CancelFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cancel != nil {
		log.Info("Cancelling in-flight stage", "old", s.image, "new", image)
		s.cancel()
		s.retries = 0
	}
	s.image = image
	s.cancel = cancel
	s.err = nil
}

func (r *BootcNodeReconciler) reset() {
	r.rebootIssued = false
	r.inflight.mu.Lock()
	defer r.inflight.mu.Unlock()
	r.inflight.retries = 0
}

// run executes bootc stage in a goroutine. The results are delivered via the done channel.
func (s *stageOp) run(ctx context.Context, nodeName, image string, executor bootc.Executor, done chan<- event.GenericEvent) {
	s.runMu.Lock()
	defer s.runMu.Unlock()

	log := logf.FromContext(context.Background()).WithValues("node", nodeName, "image", image)

	// TODO: exec bootc switch async and select on the cancel channel to send SIGINT for graceful shutdown.
	err := executor.Stage(ctx, image)

	s.mu.Lock()
	if ctx.Err() != nil {
		log.Info("Stage cancelled")
		s.mu.Unlock()
		return
	}
	if err != nil {
		log.Error(err, "Stage failed")
		s.err = err
	}
	s.image = ""
	s.cancel = nil
	s.mu.Unlock()

	done <- event.GenericEvent{
		Object: &bootcv1alpha1.BootcNode{
			ObjectMeta: metav1.ObjectMeta{Name: nodeName},
		},
	}
}

func (r *BootcNodeReconciler) populateBootcFields(ctx context.Context, bn *bootcv1alpha1.BootcNode) error {
	status, err := r.StatusWatcher.GetStatus(ctx)
	if err != nil {
		return fmt.Errorf("getting bootc status: %w", err)
	}

	bn.Status.Booted = convertBootEntry(status.Status.Booted)
	bn.Status.Staged = convertBootEntry(status.Status.Staged)
	bn.Status.Rollback = convertBootEntry(status.Status.Rollback)

	return nil
}

// updateAction represents the next step the daemon should take for an
// in-progress update. Classified once, then used to drive the stage.
type updateAction int

const (
	actionStage       updateAction = iota // desired image not yet staged
	actionAwaitStage                      // stage in-flight, waiting for completion
	actionAwaitBooted                     // staged, waiting for reboot approval
	actionReboot                          // staged + approved, issue reboot
	actionAwaitReboot                     // reboot issued, waiting for completion
)

func (r *BootcNodeReconciler) classifyAction(bn *bootcv1alpha1.BootcNode, digested reference.Digested, desiredImage string) updateAction {
	desiredDigest := digested.Digest().String()
	alreadyStaged := bn.Status.Staged != nil && bn.Status.Staged.ImageDigest == desiredDigest
	if !alreadyStaged {
		if r.inflight.isInFlight(desiredImage) {
			return actionAwaitStage
		}
		return actionStage
	}

	if bn.Spec.DesiredImageState != bootcv1alpha1.DesiredImageStateBooted {
		return actionAwaitBooted
	}

	// rebootIssued is volatile: if the daemon restarts it resets to false.
	// That is safe because either the reboot already landed (booted digest
	// matches and we return idle earlier) or it hasn't and we re-issue it.
	if r.rebootIssued {
		return actionAwaitReboot
	}
	return actionReboot
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
