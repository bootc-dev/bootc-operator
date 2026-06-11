// SPDX-License-Identifier: Apache-2.0

package bootc

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// Executor abstracts the execution of bootc commands on the host.
// The real implementation uses nsenter to enter the host's mount and
// PID namespaces. Tests can provide a fake implementation.
type Executor interface {
	Status(ctx context.Context) ([]byte, error)
	Stage(ctx context.Context, image string) error
	Reboot(ctx context.Context) error
}

// HostExecutor runs bootc commands on the host via nsenter.
// It requires hostPID: true and privileged: true in the pod spec.
type HostExecutor struct{}

func NewHostExecutor() *HostExecutor {
	return &HostExecutor{}
}

func (e *HostExecutor) nsenterCmd(ctx context.Context, args ...string) *exec.Cmd {
	base := []string{
		"--target", "1",
		"--mount", "--pid",
		"--setuid", "0", "--setgid", "0",
		"--env", "--",
	}
	return exec.CommandContext(ctx, "nsenter", append(base, args...)...)
}

func (e *HostExecutor) Status(ctx context.Context) ([]byte, error) {
	cmd := e.nsenterCmd(ctx, "bootc", "status", "--json", "--format-version", "1")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("running bootc status: %w", err)
	}
	return out, nil
}

func (e *HostExecutor) Stage(ctx context.Context, image string) error {
	log := logf.FromContext(ctx)

	// TODO: use --download-only once available (https://github.com/bootc-dev/bootc/issues/2137)
	cmd := e.nsenterCmd(ctx, "bootc", "switch", image)
	log.Info("Executing", "cmd", strings.Join(cmd.Args, " "))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("running bootc switch: %s: %w", out, err)
	}
	return nil
}

func (e *HostExecutor) Reboot(ctx context.Context) error {
	log := logf.FromContext(ctx)

	cmd := e.nsenterCmd(ctx, "systemctl", "reboot")
	log.Info("Executing", "cmd", strings.Join(cmd.Args, " "))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("running systemctl reboot: %s: %w", out, err)
	}
	return nil
}
