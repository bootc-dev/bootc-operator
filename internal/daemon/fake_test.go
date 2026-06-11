// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"sync"

	testutil "github.com/jlebon/bootc-operator/test/util"

	"github.com/jlebon/bootc-operator/internal/bootc"
)

type fakeExecutor struct {
	mu        sync.Mutex
	status    bootc.Status
	statusErr error

	stageErr  error
	stageImg  string
	stageHook func()

	rebooted bool
}

func (f *fakeExecutor) Status(_ context.Context) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.statusErr != nil {
		return nil, f.statusErr
	}
	data, err := json.Marshal(f.status)
	if err != nil {
		return nil, err
	}
	return data, nil
}

func (f *fakeExecutor) Stage(_ context.Context, image string) error {
	f.mu.Lock()
	f.stageImg = image
	f.mu.Unlock()

	if f.stageHook != nil {
		f.stageHook()
	}
	if f.stageErr != nil {
		return f.stageErr
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	_, digest, _ := strings.Cut(image, "@")
	f.status.Status.Staged = newBootEntry(image, digest)
	return nil
}

func (f *fakeExecutor) Reboot(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rebooted = true
	return nil
}

func (f *fakeExecutor) setStatusErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statusErr = err
}

func (f *fakeExecutor) setStageErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stageErr = err
}

func (f *fakeExecutor) setStageHook(hook func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stageHook = hook
}

func (f *fakeExecutor) getStageImg() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stageImg
}

func (f *fakeExecutor) getRebooted() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rebooted
}

func (f *fakeExecutor) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = bootc.Status{}
	f.statusErr = nil
	f.stageErr = nil
	f.stageImg = ""
	f.stageHook = nil
	f.rebooted = false
}

func newBootEntry(image, digest string) *bootc.BootEntry {
	return &bootc.BootEntry{
		Image: &bootc.ImageStatus{
			Image:        bootc.ImageReference{Image: image, Transport: "registry"},
			ImageDigest:  digest,
			Architecture: "amd64",
		},
	}
}

func newBootcStatus(bootedDigest string) bootc.Status {
	return bootc.Status{
		APIVersion: "org.containers.bootc/v1alpha1",
		Kind:       "BootcHost",
		Spec: bootc.StatusSpec{
			Image:     &bootc.ImageReference{Image: testutil.ImageTaggedRef, Transport: "registry"},
			BootOrder: "default",
		},
		Status: bootc.StatusBody{
			Booted: newBootEntry(testutil.ImageTaggedRef, bootedDigest),
		},
	}
}
