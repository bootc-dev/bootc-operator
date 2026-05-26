// SPDX-License-Identifier: Apache-2.0

package bootc

import (
	"context"
	"fmt"
	"os/exec"
)

// Executor abstracts the execution of bootc commands on the host.
// The real implementation uses nsenter to enter the host's mount and
// PID namespaces. Tests can provide a fake implementation.
type Executor interface {
	Status(ctx context.Context) ([]byte, error)
}

// HostExecutor runs bootc commands on the host via nsenter.
// It requires hostPID: true and privileged: true in the pod spec.
type HostExecutor struct{}

func NewHostExecutor() *HostExecutor {
	return &HostExecutor{}
}

func (e *HostExecutor) Status(ctx context.Context) ([]byte, error) {
	cmd := exec.CommandContext(ctx,
		"nsenter",
		"--target", "1",
		"--mount",
		"--pid",
		"--setuid", "0",
		"--setgid", "0",
		"--env",
		"--",
		"bootc", "status", "--json", "--format-version", "1",
	)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("running bootc status: %w", err)
	}
	return out, nil
}
