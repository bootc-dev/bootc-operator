// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	bootcv1alpha1 "github.com/bootc-dev/bootc-operator/api/v1alpha1"
	testutil "github.com/bootc-dev/bootc-operator/test/util"
)

type capturedEvent struct {
	regarding string
	related   string
	eventType string
	reason    string
	action    string
	note      string
}

type capturingEventRecorder struct {
	events []capturedEvent
}

type patchFailingClient struct {
	client.Client
	err error
}

func (c *patchFailingClient) Patch(
	context.Context,
	client.Object,
	client.Patch,
	...client.PatchOption,
) error {
	return c.err
}

type staticTagResolver struct {
	digest string
}

func (r staticTagResolver) Resolve(context.Context, string) (string, error) {
	return r.digest, nil
}

func (r *capturingEventRecorder) Eventf(
	regarding runtime.Object,
	related runtime.Object,
	eventType, reason, action, note string,
	args ...interface{},
) {
	event := capturedEvent{
		eventType: eventType,
		reason:    reason,
		action:    action,
		note:      fmt.Sprintf(note, args...),
	}
	if object, ok := regarding.(metav1.Object); ok {
		event.regarding = object.GetName()
	}
	if object, ok := related.(metav1.Object); ok {
		event.related = object.GetName()
	}
	r.events = append(r.events, event)
}

func TestRecordPoolEvents(t *testing.T) {
	condition := func(conditionType string, status metav1.ConditionStatus, reason, message string) metav1.Condition {
		return metav1.Condition{
			Type:    conditionType,
			Status:  status,
			Reason:  reason,
			Message: message,
		}
	}

	tests := []struct {
		name       string
		imageRef   string
		previous   bootcv1alpha1.BootcNodePoolStatus
		current    bootcv1alpha1.BootcNodePoolStatus
		tagChanged bool
		wantEvents []capturedEvent
	}{
		{
			name:     "moving tag retargets active rollout",
			imageRef: testutil.ImageTaggedRef,
			previous: bootcv1alpha1.BootcNodePoolStatus{
				TargetDigest: testDigestA,
				Conditions: []metav1.Condition{
					condition(
						bootcv1alpha1.PoolUpToDate,
						metav1.ConditionFalse,
						bootcv1alpha1.PoolRolloutInProgress,
						"0/1 updated",
					),
				},
			},
			current: bootcv1alpha1.BootcNodePoolStatus{
				TargetDigest: testDigestB,
				Conditions: []metav1.Condition{
					condition(
						bootcv1alpha1.PoolUpToDate,
						metav1.ConditionFalse,
						bootcv1alpha1.PoolRolloutInProgress,
						"0/1 updated",
					),
				},
			},
			tagChanged: true,
			wantEvents: []capturedEvent{
				{
					regarding: "pool-events",
					eventType: corev1.EventTypeNormal,
					reason:    eventReasonImageUpdateAvailable,
					action:    eventActionResolveImage,
					note: fmt.Sprintf(
						"Image tag %s resolved to new digest %s (previously %s)",
						testutil.ImageTaggedRef,
						testDigestB,
						testDigestA,
					),
				},
				{
					regarding: "pool-events",
					eventType: corev1.EventTypeNormal,
					reason:    eventReasonRolloutStarted,
					action:    eventActionRollout,
					note:      "Rollout started toward digest " + testDigestB,
				},
			},
		},
		{
			name:     "digest retarget starts a new active rollout",
			imageRef: testImageDigestRefB,
			previous: bootcv1alpha1.BootcNodePoolStatus{
				TargetDigest: testDigestA,
				Conditions: []metav1.Condition{
					condition(
						bootcv1alpha1.PoolUpToDate,
						metav1.ConditionFalse,
						bootcv1alpha1.PoolRolloutInProgress,
						"0/1 updated",
					),
				},
			},
			current: bootcv1alpha1.BootcNodePoolStatus{
				TargetDigest: testDigestB,
				Conditions: []metav1.Condition{
					condition(
						bootcv1alpha1.PoolUpToDate,
						metav1.ConditionFalse,
						bootcv1alpha1.PoolRolloutInProgress,
						"0/1 updated",
					),
				},
			},
			wantEvents: []capturedEvent{{
				regarding: "pool-events",
				eventType: corev1.EventTypeNormal,
				reason:    eventReasonRolloutStarted,
				action:    eventActionRollout,
				note:      "Rollout started toward digest " + testDigestB,
			}},
		},
		{
			name:     "rollout starts",
			imageRef: testImageDigestRefB,
			previous: bootcv1alpha1.BootcNodePoolStatus{
				TargetDigest: testDigestB,
				Conditions: []metav1.Condition{
					condition(
						bootcv1alpha1.PoolUpToDate,
						metav1.ConditionTrue,
						bootcv1alpha1.PoolAllUpdated,
						"",
					),
				},
			},
			current: bootcv1alpha1.BootcNodePoolStatus{
				TargetDigest: testDigestB,
				Conditions: []metav1.Condition{
					condition(
						bootcv1alpha1.PoolUpToDate,
						metav1.ConditionFalse,
						bootcv1alpha1.PoolRolloutInProgress,
						"0/1 updated",
					),
				},
			},
			wantEvents: []capturedEvent{{
				regarding: "pool-events",
				eventType: corev1.EventTypeNormal,
				reason:    eventReasonRolloutStarted,
				action:    eventActionRollout,
				note:      "Rollout started toward digest " + testDigestB,
			}},
		},
		{
			name:     "unpausing starts rollout",
			imageRef: testImageDigestRefB,
			previous: bootcv1alpha1.BootcNodePoolStatus{
				TargetDigest: testDigestB,
				Conditions: []metav1.Condition{
					condition(
						bootcv1alpha1.PoolUpToDate,
						metav1.ConditionFalse,
						bootcv1alpha1.PoolPaused,
						"0/1 updated",
					),
				},
			},
			current: bootcv1alpha1.BootcNodePoolStatus{
				TargetDigest: testDigestB,
				Conditions: []metav1.Condition{
					condition(
						bootcv1alpha1.PoolUpToDate,
						metav1.ConditionFalse,
						bootcv1alpha1.PoolRolloutInProgress,
						"0/1 updated",
					),
				},
			},
			wantEvents: []capturedEvent{{
				regarding: "pool-events",
				eventType: corev1.EventTypeNormal,
				reason:    eventReasonRolloutStarted,
				action:    eventActionRollout,
				note:      "Rollout started toward digest " + testDigestB,
			}},
		},
		{
			name:     "rollout completes",
			imageRef: testImageDigestRefB,
			previous: bootcv1alpha1.BootcNodePoolStatus{
				TargetDigest: testDigestB,
				Conditions: []metav1.Condition{
					condition(
						bootcv1alpha1.PoolUpToDate,
						metav1.ConditionFalse,
						bootcv1alpha1.PoolRolloutInProgress,
						"0/1 updated",
					),
				},
			},
			current: bootcv1alpha1.BootcNodePoolStatus{
				TargetDigest: testDigestB,
				Conditions: []metav1.Condition{
					condition(
						bootcv1alpha1.PoolUpToDate,
						metav1.ConditionTrue,
						bootcv1alpha1.PoolAllUpdated,
						"",
					),
				},
			},
			wantEvents: []capturedEvent{{
				regarding: "pool-events",
				eventType: corev1.EventTypeNormal,
				reason:    eventReasonRolloutCompleted,
				action:    eventActionRollout,
				note:      "Rollout completed at digest " + testDigestB,
			}},
		},
		{
			name:     "initially up to date is not a completed rollout",
			imageRef: testImageDigestRefB,
			previous: bootcv1alpha1.BootcNodePoolStatus{TargetDigest: testDigestB},
			current: bootcv1alpha1.BootcNodePoolStatus{
				TargetDigest: testDigestB,
				Conditions: []metav1.Condition{
					condition(
						bootcv1alpha1.PoolUpToDate,
						metav1.ConditionTrue,
						bootcv1alpha1.PoolAllUpdated,
						"",
					),
				},
			},
		},
		{
			name:     "pool becomes degraded",
			imageRef: testImageDigestRefB,
			previous: bootcv1alpha1.BootcNodePoolStatus{
				Conditions: []metav1.Condition{
					condition(
						bootcv1alpha1.PoolDegraded,
						metav1.ConditionFalse,
						bootcv1alpha1.PoolHealthy,
						"",
					),
				},
			},
			current: bootcv1alpha1.BootcNodePoolStatus{
				Conditions: []metav1.Condition{
					condition(
						bootcv1alpha1.PoolDegraded,
						metav1.ConditionTrue,
						bootcv1alpha1.PoolInvalidSpec,
						"invalid maxUnavailable",
					),
				},
			},
			wantEvents: []capturedEvent{{
				regarding: "pool-events",
				eventType: corev1.EventTypeWarning,
				reason:    bootcv1alpha1.PoolInvalidSpec,
				action:    eventActionPoolDegraded,
				note:      "invalid maxUnavailable",
			}},
		},
		{
			name:     "degraded reason changes",
			imageRef: testImageDigestRefB,
			previous: bootcv1alpha1.BootcNodePoolStatus{
				Conditions: []metav1.Condition{
					condition(
						bootcv1alpha1.PoolDegraded,
						metav1.ConditionTrue,
						bootcv1alpha1.PoolNodeConflict,
						"overlapping selector",
					),
				},
			},
			current: bootcv1alpha1.BootcNodePoolStatus{
				Conditions: []metav1.Condition{
					condition(
						bootcv1alpha1.PoolDegraded,
						metav1.ConditionTrue,
						bootcv1alpha1.PoolRolloutHalted,
						"two unhealthy nodes",
					),
				},
			},
			wantEvents: []capturedEvent{{
				regarding: "pool-events",
				eventType: corev1.EventTypeWarning,
				reason:    bootcv1alpha1.PoolRolloutHalted,
				action:    eventActionPoolDegraded,
				note:      "two unhealthy nodes",
			}},
		},
		{
			name:     "unchanged degraded state",
			imageRef: testImageDigestRefB,
			previous: bootcv1alpha1.BootcNodePoolStatus{
				Conditions: []metav1.Condition{
					condition(
						bootcv1alpha1.PoolDegraded,
						metav1.ConditionTrue,
						bootcv1alpha1.PoolNodeConflict,
						"overlapping selector",
					),
				},
			},
			current: bootcv1alpha1.BootcNodePoolStatus{
				Conditions: []metav1.Condition{
					condition(
						bootcv1alpha1.PoolDegraded,
						metav1.ConditionTrue,
						bootcv1alpha1.PoolNodeConflict,
						"overlapping selector",
					),
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			recorder := &capturingEventRecorder{}
			reconciler := &BootcNodePoolReconciler{Recorder: recorder}
			pool := testutil.NewPool("pool-events", tt.imageRef, testutil.WithWorkerSelector())
			pool.Status = tt.current

			reconciler.recordPoolEvents(pool, &tt.previous, tt.tagChanged)

			g.Expect(recorder.events).To(Equal(tt.wantEvents))
		})
	}
}

func TestResolveTargetDigestReportsTagChange(t *testing.T) {
	g := NewWithT(t)
	pool := testutil.NewPool(
		"tag-change",
		testutil.ImageTaggedRef,
		testutil.WithWorkerSelector(),
	)
	pool.Status.TargetDigest = testDigestA
	reconciler := &BootcNodePoolReconciler{
		TagResolver:           staticTagResolver{digest: testDigestB},
		TagResolutionInterval: time.Hour,
	}

	result, changed, err := reconciler.resolveTargetDigest(context.Background(), pool)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(changed).To(BeTrue())
	g.Expect(pool.Status.TargetDigest).To(Equal(testDigestB))
	g.Expect(result.RequeueAfter).To(Equal(time.Hour))
}

func TestNodeEvent(t *testing.T) {
	tests := []struct {
		name            string
		status          metav1.ConditionStatus
		reason          string
		wantObservation string
		wantReason      string
		wantNote        string
	}{
		{
			name:            "idle",
			status:          metav1.ConditionTrue,
			reason:          bootcv1alpha1.NodeReasonIdle,
			wantObservation: "True:Idle",
		},
		{
			name:            "staging",
			status:          metav1.ConditionFalse,
			reason:          bootcv1alpha1.NodeReasonStaging,
			wantObservation: "False:Staging:" + testImageDigestRefB,
			wantReason:      bootcv1alpha1.NodeReasonStaging,
			wantNote:        "Staging image " + testImageDigestRefB,
		},
		{
			name:            "staged",
			status:          metav1.ConditionFalse,
			reason:          bootcv1alpha1.NodeReasonStaged,
			wantObservation: "False:Staged:" + testImageDigestRefB,
			wantReason:      bootcv1alpha1.NodeReasonStaged,
			wantNote:        "Image " + testImageDigestRefB + " is staged and awaiting reboot",
		},
		{
			name:            "rebooting",
			status:          metav1.ConditionFalse,
			reason:          bootcv1alpha1.NodeReasonRebooting,
			wantObservation: "False:Rebooting:" + testImageDigestRefB,
			wantReason:      bootcv1alpha1.NodeReasonRebooting,
			wantNote:        "Rebooting into image " + testImageDigestRefB,
		},
		{
			name:            "unknown reason does not emit",
			status:          metav1.ConditionFalse,
			reason:          "Unknown",
			wantObservation: "False:Unknown",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			node := testutil.NewNode(
				"node-events",
				testImageDigestRefB,
				testutil.WithNodeCondition(bootcv1alpha1.NodeIdle, tt.status, tt.reason),
			)

			observation, reason, note := nodeEvent(node)

			g.Expect(observation).To(Equal(tt.wantObservation))
			g.Expect(reason).To(Equal(tt.wantReason))
			g.Expect(note).To(Equal(tt.wantNote))
		})
	}
}

func TestRecordNodeEventBeforeMarkerPatch(t *testing.T) {
	g := NewWithT(t)
	recorder := &capturingEventRecorder{}
	reconciler := &BootcNodePoolReconciler{
		Client:   &patchFailingClient{err: errors.New("temporary API error")},
		Recorder: recorder,
	}
	pool := testutil.NewPool(
		"node-patch-events",
		testImageDigestRefB,
		testutil.WithWorkerSelector(),
	)
	node := testutil.NewNode(
		"node-patch-events-worker",
		testImageDigestRefB,
		testutil.WithNodeCondition(
			bootcv1alpha1.NodeIdle,
			metav1.ConditionFalse,
			bootcv1alpha1.NodeReasonStaging,
		),
	)

	reconciler.recordNodeEvents(
		context.Background(),
		pool,
		map[string]*bootcv1alpha1.BootcNode{node.Name: node},
	)

	g.Expect(recorder.events).To(Equal([]capturedEvent{{
		regarding: node.Name,
		related:   pool.Name,
		eventType: corev1.EventTypeNormal,
		reason:    bootcv1alpha1.NodeReasonStaging,
		action:    eventActionNodeUpdate,
		note:      "Staging image " + testImageDigestRefB,
	}}))
	g.Expect(node.Annotations).NotTo(HaveKey(bootcv1alpha1.AnnotationLastObservedState))
}

func TestRecordDrainFailedEvent(t *testing.T) {
	g := NewWithT(t)
	recorder := &capturingEventRecorder{}
	reconciler := &BootcNodePoolReconciler{Recorder: recorder}
	pool := testutil.NewPool("drain-events", testImageDigestRefB, testutil.WithWorkerSelector())
	node := testutil.NewNode("drain-events-worker", testImageDigestRefB)

	reconciler.recordDrainFailedEvent(pool, node, errors.New("timed out waiting for eviction"))

	g.Expect(recorder.events).To(Equal([]capturedEvent{
		{
			regarding: node.Name,
			related:   pool.Name,
			eventType: corev1.EventTypeWarning,
			reason:    eventReasonDrainFailed,
			action:    eventActionDrain,
			note:      "Failed to drain node: timed out waiting for eviction; the drain will be retried",
		},
	}))
}

func TestTruncateEventNote(t *testing.T) {
	tests := []struct {
		name      string
		note      string
		want      string
		truncated bool
	}{
		{name: "short note", note: "drain failed", want: "drain failed"},
		{
			name: "note at limit",
			note: strings.Repeat("a", eventNoteLimit),
			want: strings.Repeat("a", eventNoteLimit),
		},
		{name: "long ASCII note", note: strings.Repeat("a", eventNoteLimit+1), truncated: true},
		{name: "long UTF-8 note", note: strings.Repeat("界", eventNoteLimit), truncated: true},
		{name: "invalid UTF-8", note: string([]byte{'a', 0xff, 'b'}), want: "a\uFFFDb"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			note := truncateEventNote(tt.note)

			g.Expect(len(note)).To(BeNumerically("<=", eventNoteLimit))
			g.Expect(utf8.ValidString(note)).To(BeTrue())
			if tt.want != "" {
				g.Expect(note).To(Equal(tt.want))
			}
			if tt.truncated {
				g.Expect(note).To(HaveSuffix(eventNoteSuffix))
			}
		})
	}
}

func TestRecordDrainStalls(t *testing.T) {
	g := NewWithT(t)
	recorder := &capturingEventRecorder{}
	pool := testutil.NewPool("drain-stall", testImageDigestRefB, testutil.WithWorkerSelector())
	stalledNode := testutil.NewNode("drain-stall-worker", testImageDigestRefB)
	freshNode := testutil.NewNode("drain-fresh-worker", testImageDigestRefB)
	stalledStatus := &drainStatus{startTime: time.Now().Add(-drainStallThreshold)}
	freshStatus := &drainStatus{startTime: time.Now()}
	reconciler := &BootcNodePoolReconciler{
		Recorder: recorder,
		drains: map[string]*drainStatus{
			stalledNode.Name: stalledStatus,
			freshNode.Name:   freshStatus,
		},
	}

	nextCheck := reconciler.recordDrainStalls(
		pool,
		map[string]*bootcv1alpha1.BootcNode{
			stalledNode.Name: stalledNode,
			freshNode.Name:   freshNode,
		},
	)

	g.Expect(stalledStatus.isStalled).To(BeTrue())
	g.Expect(freshStatus.isStalled).To(BeFalse())
	g.Expect(nextCheck).To(BeNumerically(">", drainStallThreshold-time.Second))
	g.Expect(nextCheck).To(BeNumerically("<=", drainStallThreshold))
	g.Expect(recorder.events).To(Equal([]capturedEvent{{
		regarding: stalledNode.Name,
		related:   pool.Name,
		eventType: corev1.EventTypeWarning,
		reason:    eventReasonDrainTakingTooLong,
		action:    eventActionDrain,
		note: fmt.Sprintf(
			"Drain has been running for more than %s; it may be blocked by a PodDisruptionBudget",
			drainStallThreshold,
		),
	}}))

	// The isStalled marker suppresses duplicate events on later reconciles.
	reconciler.recordDrainStalls(
		pool,
		map[string]*bootcv1alpha1.BootcNode{stalledNode.Name: stalledNode},
	)
	g.Expect(recorder.events).To(HaveLen(1))
}

func TestEarlierRequeue(t *testing.T) {
	tests := []struct {
		name      string
		current   time.Duration
		candidate time.Duration
		want      time.Duration
	}{
		{name: "uses first deadline", candidate: 5 * time.Minute, want: 5 * time.Minute},
		{
			name:      "uses earlier deadline",
			current:   time.Hour,
			candidate: 5 * time.Minute,
			want:      5 * time.Minute,
		},
		{
			name:      "keeps earlier deadline",
			current:   time.Minute,
			candidate: 5 * time.Minute,
			want:      time.Minute,
		},
		{name: "ignores absent candidate", current: time.Hour, want: time.Hour},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			g.Expect(earlierRequeue(tt.current, tt.candidate)).To(Equal(tt.want))
		})
	}
}

func TestRolloutAndNodeEvents(t *testing.T) {
	g := NewWithT(t)
	g.SetDefaultEventuallyTimeout(pollTimeout)
	g.SetDefaultEventuallyPollingInterval(pollInterval)
	ctx := context.Background()

	const (
		poolName = "rollout-events"
		nodeName = "rollout-events-worker"
	)

	node := testutil.NewK8sNode(nodeName, testutil.WorkerLabels())
	g.Expect(k8sClient.Create(ctx, node)).To(Succeed())
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, node)
	})

	pool := testutil.NewPool(poolName, testImageDigestRefB, testutil.WithWorkerSelector())
	g.Expect(k8sClient.Create(ctx, pool)).To(Succeed())
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, pool)
	})

	var bootcNode bootcv1alpha1.BootcNode
	g.Eventually(func() error {
		return k8sClient.Get(ctx, client.ObjectKey{Name: nodeName}, &bootcNode)
	}).Should(Succeed())

	g.Eventually(func() ([]eventsv1.Event, error) {
		return eventsForObject(ctx, "BootcNodePool", poolName, pool.UID)
	}).Should(ContainElement(And(
		HaveField("Type", corev1.EventTypeNormal),
		HaveField("Reason", eventReasonRolloutStarted),
		HaveField("Action", eventActionRollout),
		HaveField("Note", "Rollout started toward digest "+testDigestB),
	)))

	nodeTransitions := []struct {
		reason string
		note   string
	}{
		{
			reason: bootcv1alpha1.NodeReasonStaging,
			note:   "Staging image " + testImageDigestRefB,
		},
		{
			reason: bootcv1alpha1.NodeReasonStaged,
			note:   "Image " + testImageDigestRefB + " is staged and awaiting reboot",
		},
		{
			reason: bootcv1alpha1.NodeReasonRebooting,
			note:   "Rebooting into image " + testImageDigestRefB,
		},
	}
	for _, transition := range nodeTransitions {
		simulateDaemonStatus(g, ctx, nodeName, testDigestA, transition.reason)
		g.Eventually(func() ([]eventsv1.Event, error) {
			return eventsForObject(ctx, "BootcNode", nodeName, bootcNode.UID)
		}).Should(ContainElement(And(
			HaveField("Type", corev1.EventTypeNormal),
			HaveField("Reason", transition.reason),
			HaveField("Action", eventActionNodeUpdate),
			HaveField("Note", transition.note),
			HaveField("Related", And(
				Not(BeNil()),
				HaveField("Name", poolName),
			)),
		)))
	}

	simulateDaemonStatus(g, ctx, nodeName, testDigestB, bootcv1alpha1.NodeReasonIdle)
	g.Eventually(func() ([]eventsv1.Event, error) {
		return eventsForObject(ctx, "BootcNodePool", poolName, pool.UID)
	}).Should(ContainElement(And(
		HaveField("Type", corev1.EventTypeNormal),
		HaveField("Reason", eventReasonRolloutCompleted),
		HaveField("Action", eventActionRollout),
		HaveField("Note", "Rollout completed at digest "+testDigestB),
	)))

	g.Consistently(func() (map[string]int, error) {
		events, err := eventsForObject(ctx, "BootcNode", nodeName, bootcNode.UID)
		if err != nil {
			return nil, err
		}
		counts := map[string]int{}
		for _, event := range events {
			occurrences := 1
			if event.Series != nil {
				occurrences = int(event.Series.Count)
			}
			counts[event.Reason] += occurrences
		}
		return counts, nil
	}, time.Second, pollInterval).Should(Equal(map[string]int{
		bootcv1alpha1.NodeReasonStaging:   1,
		bootcv1alpha1.NodeReasonStaged:    1,
		bootcv1alpha1.NodeReasonRebooting: 1,
	}))
}

func TestInvalidSpecEmitsWarningEvent(t *testing.T) {
	g := NewWithT(t)
	g.SetDefaultEventuallyTimeout(pollTimeout)
	g.SetDefaultEventuallyPollingInterval(pollInterval)
	ctx := context.Background()

	pool := testutil.NewPool("invalid-spec-event", "myos:latest", testutil.WithWorkerSelector())
	g.Expect(k8sClient.Create(ctx, pool)).To(Succeed())
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, pool)
	})

	g.Eventually(func() ([]eventsv1.Event, error) {
		return eventsForObject(ctx, "BootcNodePool", pool.Name, pool.UID)
	}).Should(ContainElement(And(
		HaveField("Type", "Warning"),
		HaveField("Reason", bootcv1alpha1.PoolInvalidSpec),
		HaveField("Action", "PoolDegraded"),
		HaveField("Note", ContainSubstring("invalid image ref")),
	)))
}

func eventsForObject(
	ctx context.Context,
	kind, name string,
	uid types.UID,
) ([]eventsv1.Event, error) {
	var eventList eventsv1.EventList
	if err := k8sClient.List(
		ctx,
		&eventList,
		client.InNamespace(metav1.NamespaceDefault),
	); err != nil {
		return nil, err
	}

	return testutil.FilterEventsByObject(eventList.Items, kind, name, uid), nil
}
