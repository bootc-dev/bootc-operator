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

func (f *fakeExecutor) set(data []byte, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.data = data
	f.err = err
}
