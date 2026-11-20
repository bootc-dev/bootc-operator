// SPDX-License-Identifier: Apache-2.0

package e2eutil

import (
	"context"
	"os"
	"testing"
)

// NodeProvider abstracts node provisioning so the same tests can run
// against different infrastructure (bink, EKS, etc.).
type NodeProvider interface {
	AddNode(ctx context.Context, labels map[string]string, role string) (nodeName string, err error)
	RemoveNode(ctx context.Context, nodeName string) error
}

// ProviderName returns the active E2E_PROVIDER, defaulting to bink.
func ProviderName() string {
	if p := os.Getenv("E2E_PROVIDER"); p != "" {
		return p
	}
	return "bink"
}

// Providers skips the test if the current E2E_PROVIDER is not in the
// supported list. Call at the top of each test function.
func Providers(t *testing.T, supported ...string) {
	t.Helper()

	provider := ProviderName()
	for _, s := range supported {
		if s == provider {
			return
		}
	}
	t.Skipf("test requires provider %v, got %q", supported, provider)
}
