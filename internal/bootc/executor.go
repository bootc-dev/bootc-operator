// SPDX-License-Identifier: Apache-2.0

package bootc

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/go-logr/logr"
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
		"--mount", "--pid", "--cgroup",
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

// stageUnitName is the systemd transient unit name used for bootc switch.
const stageUnitName = "bootc-operator-switch.service"

func (e *HostExecutor) Stage(ctx context.Context, image string) error {
	log := logf.FromContext(ctx)

	// Stop any stale unit. This does also mean that if e.g. the daemon is
	// restarted while a unit is outstanding, we'll still stop it even if
	// it was working towards the same target image. This is fine though,
	// we still eventually converge. And at least e.g. any layers already
	// pulled will be skipped. Note also that `bootc switch` returns rc=0
	// if the staged image already matches, so even if the previous unit
	// just completed, this new unit will essentially be a no-op.
	e.stopStageUnit()

	// Ideally we'd use systemd-run's `--pipe` here, which would avoid
	// having to fetch the unit journal down below, but SELinux blocks it
	// (dbus-broker can't access container-labeled fds).
	cmd := e.nsenterCmd(ctx,
		"systemd-run", "--wait", "--collect",
		"--unit", stageUnitName,
		// TODO: use --download-only once available
		// (https://github.com/bootc-dev/bootc/issues/2137)
		"bootc", "switch", image,
	)
	cmd.Cancel = func() error {
		e.stopStageUnit()
		return nil
	}
	log.Info("Executing", "cmd", strings.Join(cmd.Args, " "))
	cursor := e.journalCursor()
	if err := cmd.Run(); err != nil {
		// Were we cancelled?
		if ctx.Err() != nil {
			return ctx.Err()
		}
		e.copyJournalUnitLogs(log, stageUnitName, cursor)
		return fmt.Errorf("running bootc switch: %w", err)
	}
	return nil
}

// stopStageUnit stops the bootc-operator-switch transient unit if it
// is running. Errors are ignored: the unit may not exist.
func (e *HostExecutor) stopStageUnit() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = e.nsenterCmd(ctx, "systemctl", "stop", stageUnitName).Run()
}

// journalCursor returns the current journal cursor position. Used to scope a
// future journal query to only entries after the cursor position.
func (e *HostExecutor) journalCursor() string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := e.nsenterCmd(ctx,
		"journalctl", "--show-cursor", "-n", "0", "-o", "cat",
	).Output()
	if err != nil {
		return ""
	}
	// Output format: "-- cursor: s=...;i=...;b=...;..."
	if after, ok := strings.CutPrefix(strings.TrimSpace(string(out)), "-- cursor: "); ok {
		return after
	}
	return ""
}

// copyJournalUnitLogs logs recent journal output from the given systemd unit.
// If cursor is non-empty, only entries after that cursor are shown; otherwise
// shows all entries.
func (e *HostExecutor) copyJournalUnitLogs(log logr.Logger, unit string, cursor string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	args := []string{"journalctl", "-o", "cat", "--no-pager",
		"-u", unit,
	}
	if cursor != "" {
		args = append(args, "--after-cursor="+cursor)
	}
	out, err := e.nsenterCmd(ctx, args...).Output()
	if err != nil {
		log.Error(err, "Failed to read unit journal", "unit", unit)
		return
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			log.Info(line, "unit", unit)
		}
	}
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
