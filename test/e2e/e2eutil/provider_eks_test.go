// SPDX-License-Identifier: Apache-2.0

package e2eutil

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type mockEC2Runner struct {
	instanceID string
	lastInput  *ec2.RunInstancesInput
}

func (m *mockEC2Runner) RunInstances(
	_ context.Context,
	input *ec2.RunInstancesInput,
	_ ...func(*ec2.Options),
) (*ec2.RunInstancesOutput, error) {
	m.lastInput = input
	return &ec2.RunInstancesOutput{
		Instances: []ec2types.Instance{
			{InstanceId: aws.String(m.instanceID)},
		},
	}, nil
}

type mockEC2Terminator struct {
	terminated []string
}

func (m *mockEC2Terminator) TerminateInstances(
	_ context.Context,
	input *ec2.TerminateInstancesInput,
	_ ...func(*ec2.Options),
) (*ec2.TerminateInstancesOutput, error) {
	m.terminated = append(m.terminated, input.InstanceIds...)
	return &ec2.TerminateInstancesOutput{}, nil
}

func fakeK8sClient(objs ...client.Object) client.Client {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		Build()
}

func TestEKSAddNode(t *testing.T) {
	const (
		instanceID = "i-0123456789abcdef0"
		nodeName   = "ip-10-0-1-100.ec2.internal"
		providerID = "aws:///us-east-1a/" + instanceID
	)

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: nodeName},
		Spec:       corev1.NodeSpec{ProviderID: providerID},
	}

	runner := &mockEC2Runner{instanceID: instanceID}
	provider := &eksProvider{
		ec2Runner:      runner,
		ec2Terminator:  &mockEC2Terminator{},
		k8s:            fakeK8sClient(node),
		ltID:           "lt-012345",
		ltVersion:      "1",
		securityGroups: []string{"sg-aaa", "sg-bbb"},
		nodes:          make(map[string]string),
	}

	labels := map[string]string{LabelE2ETest: "testid"}
	got, err := provider.AddNode(context.Background(), labels)
	if err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if got != nodeName {
		t.Fatalf("expected node name %q, got %q", nodeName, got)
	}

	var updated corev1.Node
	if err := provider.k8s.Get(
		context.Background(),
		client.ObjectKey{Name: nodeName},
		&updated,
	); err != nil {
		t.Fatalf("getting node: %v", err)
	}
	if v := updated.Labels[LabelE2ETest]; v != "testid" {
		t.Fatalf("expected label %s=testid, got %q", LabelE2ETest, v)
	}

	if len(runner.lastInput.NetworkInterfaces) == 0 {
		t.Fatal("expected NetworkInterfaces in RunInstances input")
	}
	gotGroups := runner.lastInput.NetworkInterfaces[0].Groups
	if len(gotGroups) != 2 || gotGroups[0] != "sg-aaa" || gotGroups[1] != "sg-bbb" {
		t.Fatalf("expected security groups [sg-aaa sg-bbb], got %v", gotGroups)
	}
}

func TestEKSRemoveNode(t *testing.T) {
	const (
		instanceID = "i-abcdef0123456789a"
		nodeName   = "ip-10-0-2-50.ec2.internal"
		providerID = "aws:///us-west-2b/" + instanceID
	)

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: nodeName},
		Spec:       corev1.NodeSpec{ProviderID: providerID},
	}

	terminator := &mockEC2Terminator{}
	provider := &eksProvider{
		ec2Runner:     &mockEC2Runner{},
		ec2Terminator: terminator,
		k8s:           fakeK8sClient(node),
		ltID:          "lt-012345",
		ltVersion:     "1",
	}

	if err := provider.RemoveNode(context.Background(), nodeName); err != nil {
		t.Fatalf("RemoveNode: %v", err)
	}
	if len(terminator.terminated) != 1 || terminator.terminated[0] != instanceID {
		t.Fatalf(
			"expected TerminateInstances(%q), got %v",
			instanceID, terminator.terminated,
		)
	}

	// Verify the k8s Node object was deleted.
	var deleted corev1.Node
	err := provider.k8s.Get(
		context.Background(),
		client.ObjectKey{Name: nodeName},
		&deleted,
	)
	if err == nil {
		t.Fatal("expected node to be deleted, but it still exists")
	}
}

func TestInstanceIDFromProviderID(t *testing.T) {
	tests := []struct {
		name       string
		providerID string
		wantID     string
		wantErr    bool
	}{
		{
			name:       "standard format",
			providerID: "aws:///us-east-1a/i-0123456789abcdef0",
			wantID:     "i-0123456789abcdef0",
		},
		{
			name:       "different zone",
			providerID: "aws:///eu-west-1c/i-abcdef0123456789a",
			wantID:     "i-abcdef0123456789a",
		},
		{
			name:       "no instance id prefix",
			providerID: "aws:///us-east-1a/not-an-instance",
			wantErr:    true,
		},
		{
			name:       "empty",
			providerID: "",
			wantErr:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := instanceIDFromProviderID(tt.providerID)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.wantID {
				t.Fatalf("expected %q, got %q", tt.wantID, got)
			}
		})
	}
}
