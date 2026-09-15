// SPDX-License-Identifier: Apache-2.0

package e2eutil

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	autoscalingtypes "github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type ec2RunInstancesAPI interface {
	RunInstances(
		ctx context.Context,
		params *ec2.RunInstancesInput,
		optFns ...func(*ec2.Options),
	) (*ec2.RunInstancesOutput, error)
}

type ec2TerminateInstancesAPI interface {
	TerminateInstances(
		ctx context.Context,
		params *ec2.TerminateInstancesInput,
		optFns ...func(*ec2.Options),
	) (*ec2.TerminateInstancesOutput, error)
}

type ec2DescribeLaunchTemplateVersionsAPI interface {
	DescribeLaunchTemplateVersions(
		ctx context.Context,
		params *ec2.DescribeLaunchTemplateVersionsInput,
		optFns ...func(*ec2.Options),
	) (*ec2.DescribeLaunchTemplateVersionsOutput, error)
}

var _ NodeProvider = (*eksProvider)(nil)

// eksProvider provisions nodes by launching standalone EC2 instances
// that join an existing EKS cluster using the node group's launch
// template.
type eksProvider struct {
	ec2Runner      ec2RunInstancesAPI
	ec2Terminator  ec2TerminateInstancesAPI
	k8s            client.Client
	ltID           string
	ltVersion      string
	subnetID       string
	securityGroups []string
	// nodes maps K8s node names to EC2 instance IDs so that
	// RemoveNode can terminate instances even if the K8s node
	// object is already gone.
	nodes map[string]string
}

// newEKSProvider creates an eksProvider by discovering the Auto Scaling
// group for the given eksctl node group (via tags) and extracting its
// launch template.
func newEKSProvider(
	clusterName, nodeGroup, region string,
	k8sClient client.Client,
) (*eksProvider, error) {
	ctx := context.Background()
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("loading AWS config: %w", err)
	}

	asgClient := autoscaling.NewFromConfig(cfg)
	ec2Client := ec2.NewFromConfig(cfg)

	asg, err := findASGByTags(ctx, asgClient, clusterName, nodeGroup)
	if err != nil {
		return nil, err
	}

	asgName := aws.ToString(asg.AutoScalingGroupName)
	if len(asg.AvailabilityZones) == 0 {
		return nil, fmt.Errorf("ASG %q has no availability zones", asgName)
	}

	subnetID, err := subnetFromASG(asg)
	if err != nil {
		return nil, err
	}

	lt, err := discoverLaunchTemplate(ctx, ec2Client, asg)
	if err != nil {
		return nil, err
	}

	return &eksProvider{
		ec2Runner:      ec2Client,
		ec2Terminator:  ec2Client,
		k8s:            k8sClient,
		ltID:           lt.id,
		ltVersion:      lt.version,
		subnetID:       subnetID,
		securityGroups: lt.securityGroups,
		nodes:          make(map[string]string),
	}, nil
}

func (p *eksProvider) AddNode(
	ctx context.Context,
	labels map[string]string,
) (string, error) {
	out, err := p.ec2Runner.RunInstances(ctx, &ec2.RunInstancesInput{
		LaunchTemplate: &ec2types.LaunchTemplateSpecification{
			LaunchTemplateId: &p.ltID,
			Version:          &p.ltVersion,
		},
		NetworkInterfaces: []ec2types.InstanceNetworkInterfaceSpecification{
			{
				DeviceIndex:              aws.Int32(0),
				SubnetId:                 &p.subnetID,
				AssociatePublicIpAddress: aws.Bool(true),
				Groups:                   p.securityGroups,
			},
		},
		MinCount: aws.Int32(1),
		MaxCount: aws.Int32(1),
	})
	if err != nil {
		return "", fmt.Errorf("launching instance: %w", err)
	}
	if len(out.Instances) == 0 {
		return "", fmt.Errorf("RunInstances returned no instances")
	}
	instanceID := aws.ToString(out.Instances[0].InstanceId)

	nodeName, err := p.waitForNode(ctx, instanceID)
	if err != nil {
		p.terminateInstance(ctx, instanceID)
		return "", err
	}

	if err := p.patchLabels(ctx, nodeName, labels); err != nil {
		p.terminateInstance(ctx, instanceID)
		return "", err
	}

	p.nodes[nodeName] = instanceID
	return nodeName, nil
}

func (p *eksProvider) RemoveNode(
	ctx context.Context,
	nodeName string,
) error {
	instanceID, ok := p.nodes[nodeName]
	if !ok {
		node := &corev1.Node{}
		if err := p.k8s.Get(ctx, client.ObjectKey{Name: nodeName}, node); err != nil {
			return fmt.Errorf("getting node %q: %w", nodeName, err)
		}
		var err error
		instanceID, err = instanceIDFromProviderID(node.Spec.ProviderID)
		if err != nil {
			return err
		}
	}

	_, err := p.ec2Terminator.TerminateInstances(ctx, &ec2.TerminateInstancesInput{
		InstanceIds: []string{instanceID},
	})
	if err != nil {
		return fmt.Errorf("terminating instance %q: %w", instanceID, err)
	}

	delete(p.nodes, nodeName)

	node := &corev1.Node{}
	if err := p.k8s.Get(ctx, client.ObjectKey{Name: nodeName}, node); err != nil {
		return nil
	}
	if err := p.k8s.Delete(ctx, node); err != nil {
		return fmt.Errorf("deleting k8s node %q: %w", nodeName, err)
	}
	return nil
}

func (p *eksProvider) terminateInstance(ctx context.Context, instanceID string) {
	_, err := p.ec2Terminator.TerminateInstances(ctx, &ec2.TerminateInstancesInput{
		InstanceIds: []string{instanceID},
	})
	if err != nil {
		slog.Error("failed to terminate leaked EC2 instance",
			"instanceID", instanceID, "error", err)
	}
}

func (p *eksProvider) waitForNode(
	ctx context.Context,
	instanceID string,
) (string, error) {
	var lastErr error
	for attempt := 0; attempt < 120; attempt++ {
		var nodeList corev1.NodeList
		if err := p.k8s.List(ctx, &nodeList); err != nil {
			lastErr = err
		} else {
			for j := range nodeList.Items {
				if strings.HasSuffix(nodeList.Items[j].Spec.ProviderID, "/"+instanceID) {
					return nodeList.Items[j].Name, nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	if lastErr != nil {
		return "", fmt.Errorf(
			"timed out waiting for k8s node with instance ID %s (last error: %w)",
			instanceID, lastErr,
		)
	}
	return "", fmt.Errorf(
		"timed out waiting for k8s node with instance ID %s", instanceID,
	)
}

func (p *eksProvider) patchLabels(
	ctx context.Context,
	nodeName string,
	labels map[string]string,
) error {
	node := &corev1.Node{}
	if err := p.k8s.Get(ctx, client.ObjectKey{Name: nodeName}, node); err != nil {
		return fmt.Errorf("getting node %q for label patch: %w", nodeName, err)
	}

	modified := node.DeepCopy()
	if modified.Labels == nil {
		modified.Labels = make(map[string]string)
	}
	for k, v := range labels {
		modified.Labels[k] = v
	}

	if err := p.k8s.Patch(ctx, modified, client.MergeFrom(node)); err != nil {
		return fmt.Errorf("patching labels on node %q: %w", nodeName, err)
	}
	return nil
}

// instanceIDFromProviderID extracts the EC2 instance ID from a k8s
// node's providerID (format: "aws:///ZONE/INSTANCE-ID").
func instanceIDFromProviderID(providerID string) (string, error) {
	parts := strings.Split(providerID, "/")
	if len(parts) < 2 {
		return "", fmt.Errorf(
			"unexpected providerID format: %q", providerID,
		)
	}
	id := parts[len(parts)-1]
	if !strings.HasPrefix(id, "i-") {
		return "", fmt.Errorf(
			"providerID %q does not contain an EC2 instance ID", providerID,
		)
	}
	return id, nil
}

func findASGByTags(
	ctx context.Context,
	asgClient *autoscaling.Client,
	clusterName, nodeGroup string,
) (autoscalingtypes.AutoScalingGroup, error) {
	out, err := asgClient.DescribeAutoScalingGroups(
		ctx,
		&autoscaling.DescribeAutoScalingGroupsInput{
			Filters: []autoscalingtypes.Filter{
				{
					Name:   aws.String("tag:eksctl.cluster.k8s.io/v1alpha1/cluster-name"),
					Values: []string{clusterName},
				},
				{
					Name:   aws.String("tag:eksctl.io/v1alpha2/nodegroup-name"),
					Values: []string{nodeGroup},
				},
			},
		},
	)
	if err != nil {
		return autoscalingtypes.AutoScalingGroup{}, fmt.Errorf(
			"finding ASG for cluster %q node group %q: %w",
			clusterName, nodeGroup, err,
		)
	}
	if len(out.AutoScalingGroups) == 0 {
		return autoscalingtypes.AutoScalingGroup{}, fmt.Errorf(
			"no ASG found for cluster %q node group %q",
			clusterName, nodeGroup,
		)
	}
	return out.AutoScalingGroups[0], nil
}

func subnetFromASG(asg autoscalingtypes.AutoScalingGroup) (string, error) {
	vpc := aws.ToString(asg.VPCZoneIdentifier)
	if vpc == "" {
		return "", fmt.Errorf(
			"ASG %q has no VPCZoneIdentifier",
			aws.ToString(asg.AutoScalingGroupName),
		)
	}
	subnet, _, _ := strings.Cut(vpc, ",")
	return subnet, nil
}

type launchTemplateInfo struct {
	id             string
	version        string
	securityGroups []string
}

func discoverLaunchTemplate(
	ctx context.Context,
	ec2Client ec2DescribeLaunchTemplateVersionsAPI,
	asg autoscalingtypes.AutoScalingGroup,
) (launchTemplateInfo, error) {
	asgName := aws.ToString(asg.AutoScalingGroupName)

	var ltID, ltVersion string
	switch {
	case asg.LaunchTemplate != nil:
		ltID = aws.ToString(asg.LaunchTemplate.LaunchTemplateId)
		ltVersion = aws.ToString(asg.LaunchTemplate.Version)
	case asg.MixedInstancesPolicy != nil &&
		asg.MixedInstancesPolicy.LaunchTemplate != nil &&
		asg.MixedInstancesPolicy.LaunchTemplate.LaunchTemplateSpecification != nil:
		spec := asg.MixedInstancesPolicy.LaunchTemplate.LaunchTemplateSpecification
		ltID = aws.ToString(spec.LaunchTemplateId)
		ltVersion = aws.ToString(spec.Version)
	default:
		return launchTemplateInfo{}, fmt.Errorf("ASG %q has no launch template", asgName)
	}

	resolved, err := ec2Client.DescribeLaunchTemplateVersions(
		ctx,
		&ec2.DescribeLaunchTemplateVersionsInput{
			LaunchTemplateId: &ltID,
			Versions:         []string{ltVersion},
		},
	)
	if err != nil {
		return launchTemplateInfo{}, fmt.Errorf(
			"describing launch template %q version %q: %w",
			ltID, ltVersion, err,
		)
	}
	if len(resolved.LaunchTemplateVersions) == 0 {
		return launchTemplateInfo{}, fmt.Errorf(
			"launch template %q version %q not found", ltID, ltVersion,
		)
	}
	v := resolved.LaunchTemplateVersions[0].VersionNumber
	if v == nil {
		return launchTemplateInfo{}, fmt.Errorf("launch template version number is nil")
	}

	lt := launchTemplateInfo{
		id:      ltID,
		version: fmt.Sprintf("%d", *v),
	}

	ltData := resolved.LaunchTemplateVersions[0].LaunchTemplateData
	if ltData != nil {
		for _, ni := range ltData.NetworkInterfaces {
			if ni.DeviceIndex != nil && *ni.DeviceIndex == 0 && len(ni.Groups) > 0 {
				lt.securityGroups = ni.Groups
				break
			}
		}
		if len(lt.securityGroups) == 0 {
			lt.securityGroups = ltData.SecurityGroupIds
		}
	}

	return lt, nil
}
