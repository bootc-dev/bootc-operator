// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"sync"
)

type fakeExecutor struct {
	mu   sync.Mutex
	data []byte
	err  error
}

func (f *fakeExecutor) Status(_ context.Context) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.data, f.err
}

func (f *fakeExecutor) Stage(_ context.Context, _ string) error {
	return nil
}

func (f *fakeExecutor) Reboot(_ context.Context) error {
	return nil
}

func (f *fakeExecutor) set(data []byte, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.data = data
	f.err = err
}
