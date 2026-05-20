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
	"testing"

	"github.com/distribution/reference"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	bootcv1alpha1 "github.com/jlebon/bootc-operator/api/v1alpha1"
)

func TestClassifyNode(t *testing.T) {
	const (
		desiredImage  = testImageDigestRefA
		desiredDigest = testDigestA
		otherDigest   = testDigestB
		nodeName      = "test-node"
	)

	idleCond := func(status metav1.ConditionStatus, reason string) metav1.Condition {
		return metav1.Condition{
			Type:   bootcv1alpha1.NodeIdle,
			Status: status,
			Reason: reason,
		}
	}

	degradedCond := func(status metav1.ConditionStatus, reason string) metav1.Condition {
		return metav1.Condition{
			Type:   bootcv1alpha1.NodeDegraded,
			Status: status,
			Reason: reason,
		}
	}

	tests := []struct {
		name         string
		bootedDigest string
		conditions   []metav1.Condition
		want         nodeState
	}{
		{
			name:         "Idle: image matches, Idle=True",
			bootedDigest: desiredDigest,
			conditions:   []metav1.Condition{idleCond(metav1.ConditionTrue, bootcv1alpha1.NodeReasonIdle)},
			want:         nodeStateIdle,
		},
		// Daemon is idle but there's a diff; for now mark as Idle. See related
		// comment in classifyNode().
		{
			name:         "Idle: image differs, Idle=True",
			bootedDigest: otherDigest,
			conditions:   []metav1.Condition{idleCond(metav1.ConditionTrue, bootcv1alpha1.NodeReasonIdle)},
			want:         nodeStateIdle,
		},
		{
			name:         "Idle: no booted status yet (daemon starting)",
			bootedDigest: "",
			conditions:   nil,
			want:         nodeStateIdle,
		},
		{
			name:         "Staging: image differs, Idle=False reason=Staging",
			bootedDigest: otherDigest,
			conditions:   []metav1.Condition{idleCond(metav1.ConditionFalse, bootcv1alpha1.NodeReasonStaging)},
			want:         nodeStateStaging,
		},
		{
			name:         "Staged: image differs, Idle=False reason=Staged",
			bootedDigest: otherDigest,
			conditions:   []metav1.Condition{idleCond(metav1.ConditionFalse, bootcv1alpha1.NodeReasonStaged)},
			want:         nodeStateStaged,
		},
		{
			name:         "Rebooting: image differs, Idle=False reason=Rebooting",
			bootedDigest: otherDigest,
			conditions:   []metav1.Condition{idleCond(metav1.ConditionFalse, bootcv1alpha1.NodeReasonRebooting)},
			want:         nodeStateRebooting,
		},
		{
			name:         "Staging: Degraded=False does not affect classification",
			bootedDigest: otherDigest,
			conditions: []metav1.Condition{
				idleCond(metav1.ConditionFalse, bootcv1alpha1.NodeReasonStaging),
				degradedCond(metav1.ConditionFalse, bootcv1alpha1.NodeReasonHealthy),
			},
			want: nodeStateStaging,
		},
		{
			name:         "Degraded: Degraded=True takes priority over Idle",
			bootedDigest: desiredDigest,
			conditions: []metav1.Condition{
				idleCond(metav1.ConditionTrue, bootcv1alpha1.NodeReasonIdle),
				degradedCond(metav1.ConditionTrue, bootcv1alpha1.NodeReasonError),
			},
			want: nodeStateDegraded,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)

			bn := &bootcv1alpha1.BootcNode{
				Spec: bootcv1alpha1.BootcNodeSpec{
					DesiredImage: desiredImage,
				},
				Status: bootcv1alpha1.BootcNodeStatus{
					Conditions: tt.conditions,
				},
			}
			bn.Name = nodeName
			if tt.bootedDigest != "" {
				bn.Status.Booted = &bootcv1alpha1.ImageInfo{
					ImageDigest: tt.bootedDigest,
				}
			}
			got, err := classifyNode(bn)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(got).To(Equal(tt.want))
		})
	}
}

func TestParseImageRef(t *testing.T) {
	tests := []struct {
		name       string
		ref        string
		wantErr    bool
		wantDigest string
		wantTag    string
	}{
		{
			name:       "digest ref",
			ref:        testImageDigestRefA,
			wantDigest: testDigestA,
		},
		{
			name:       "digest ref with tag",
			ref:        "quay.io/example/myos:latest@" + testDigestA,
			wantDigest: testDigestA,
			wantTag:    "latest",
		},
		{
			name:    "tag ref",
			ref:     "quay.io/example/myos:latest",
			wantTag: "latest",
		},
		{
			name: "bare image",
			ref:  "quay.io/example/myos",
		},
		{
			name:    "empty string",
			ref:     "",
			wantErr: true,
		},
		{
			name:    "invalid characters",
			ref:     "INVALID:///ref",
			wantErr: true,
		},
		{
			name:    "short name",
			ref:     "myos:latest",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)

			ref, err := parseImageRef(tt.ref)
			if tt.wantErr {
				g.Expect(err).To(HaveOccurred())
				return
			}
			g.Expect(err).NotTo(HaveOccurred())

			if tt.wantDigest != "" {
				digested, ok := ref.(reference.Digested)
				g.Expect(ok).To(BeTrue(), "expected digest in %q", tt.ref)
				g.Expect(digested.Digest().String()).To(Equal(tt.wantDigest))
			}
			if tt.wantTag != "" {
				tagged, ok := ref.(reference.Tagged)
				g.Expect(ok).To(BeTrue(), "expected tag in %q", tt.ref)
				g.Expect(tagged.Tag()).To(Equal(tt.wantTag))
			}
		})
	}
}
