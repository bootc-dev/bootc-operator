// SPDX-License-Identifier: Apache-2.0

package e2eutil

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
)

// BinkProvider provisions nodes using bink (KVM-based local clusters).
type BinkProvider struct {
	clusterName  string
	targetImgRef string
	diskImage    string
}

// NewBinkProvider creates a BinkProvider for the given bink cluster.
func NewBinkProvider(
	clusterName, targetImgRef, diskImage string,
) *BinkProvider {
	return &BinkProvider{
		clusterName:  clusterName,
		targetImgRef: targetImgRef,
		diskImage:    diskImage,
	}
}

func (p *BinkProvider) AddNode(
	ctx context.Context,
	labels map[string]string,
) (string, error) {
	prefix := labels[LabelE2ETest]
	if prefix == "" {
		prefix = "node"
	}

	b := make([]byte, 3)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating random suffix: %w", err)
	}
	nodeName := prefix + "-" + hex.EncodeToString(b)

	args := []string{
		"node", "add", nodeName,
		"--cluster-name", p.clusterName,
	}
	for k, v := range labels {
		args = append(args, "--label", k+"="+v)
	}
	if p.diskImage != "" {
		args = append(args, "--node-image", p.diskImage)
	}
	args = append(args, "--target-imgref", p.targetImgRef)

	cmd := exec.CommandContext(ctx, "bink", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("bink node add %q: %w", nodeName, err)
	}
	return nodeName, nil
}

func (p *BinkProvider) RemoveNode(
	ctx context.Context,
	nodeName string,
) error {
	cmd := exec.CommandContext(
		ctx,
		"bink", "node", "remove", nodeName,
		"--force",
		"--cluster-name", p.clusterName,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("bink node remove %q: %w", nodeName, err)
	}
	return nil
}
