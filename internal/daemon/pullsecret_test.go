// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	. "github.com/onsi/gomega"

	testutil "github.com/bootc-dev/bootc-operator/test/util"
)

// authAtStage records the content of /run/ostree/auth.json under the test
// host root each time the fake executor stages, or "<absent>" if missing.
type authAtStage struct {
	mu   sync.Mutex
	seen []string
}

func recordAuthAtStage(fake *fakeExecutor) *authAtStage {
	a := &authAtStage{}
	fake.setStageHook(func() {
		content := "<absent>"
		if data, err := os.ReadFile(testAuthFile()); err == nil {
			content = string(data)
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		a.seen = append(a.seen, content)
	})
	return a
}

func (a *authAtStage) get() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.seen)
}

func testAuthFile() string {
	return filepath.Join(testHostRoot, "run", "ostree", "auth.json")
}

func writeMountedPullSecret(t *testing.T, name, namespace, content string) {
	t.Helper()
	path := filepath.Join(testPullSecretDir, namespace+"_"+name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestStageUsesMountedPullSecret(t *testing.T) {
	g := NewWithT(t)
	g.SetDefaultEventuallyTimeout(pollTimeout)
	g.SetDefaultEventuallyPollingInterval(pollInterval)
	ctx := context.Background()

	fake := newTestEnv(t)
	fake.status = newBootcStatus(testutil.DigestA)
	auth := recordAuthAtStage(fake)

	dockerConfig := `{"auths":{"registry.example.com":{}}}`
	writeMountedPullSecret(t, "my-secret", "my-ns", dockerConfig)

	bn := testutil.NewNode(testNodeName, testutil.ImageDigestRefB,
		testutil.WithNodePullSecret("my-secret", "my-ns"))
	g.Expect(k8sClient.Create(ctx, bn)).To(Succeed())
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, bn) })

	g.Eventually(auth.get).Should(Equal([]string{dockerConfig}))
}

func writeStaleAuthFile(t *testing.T) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(testAuthFile()), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(testAuthFile(), []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestStageRemovesAuthFileWithoutMountedEntry(t *testing.T) {
	g := NewWithT(t)
	g.SetDefaultEventuallyTimeout(pollTimeout)
	g.SetDefaultEventuallyPollingInterval(pollInterval)
	ctx := context.Background()

	fake := newTestEnv(t)
	fake.status = newBootcStatus(testutil.DigestA)
	auth := recordAuthAtStage(fake)
	writeStaleAuthFile(t)

	// The kubelet has not synced the entry yet, or it was never written.
	bn := testutil.NewNode(testNodeName, testutil.ImageDigestRefB,
		testutil.WithNodePullSecret("not-mounted", "my-ns"))
	g.Expect(k8sClient.Create(ctx, bn)).To(Succeed())
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, bn) })

	g.Eventually(auth.get).Should(Equal([]string{"<absent>"}))
}

func TestStageRemovesAuthFileWithoutPullSecretRef(t *testing.T) {
	g := NewWithT(t)
	g.SetDefaultEventuallyTimeout(pollTimeout)
	g.SetDefaultEventuallyPollingInterval(pollInterval)
	ctx := context.Background()

	fake := newTestEnv(t)
	fake.status = newBootcStatus(testutil.DigestA)
	auth := recordAuthAtStage(fake)
	writeStaleAuthFile(t)

	bn := testutil.NewNode(testNodeName, testutil.ImageDigestRefB)
	g.Expect(k8sClient.Create(ctx, bn)).To(Succeed())
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, bn) })

	g.Eventually(auth.get).Should(Equal([]string{"<absent>"}))
}

func TestStageRetryUsesRotatedPullSecret(t *testing.T) {
	g := NewWithT(t)
	g.SetDefaultEventuallyTimeout(pollTimeout)
	g.SetDefaultEventuallyPollingInterval(pollInterval)
	ctx := context.Background()

	fake := newTestEnv(t)
	fake.status = newBootcStatus(testutil.DigestA)
	fake.setStageErr(errors.New("unauthorized"))
	auth := recordAuthAtStage(fake)

	writeMountedPullSecret(t, "my-secret", "my-ns", "old")

	bn := testutil.NewNode(testNodeName, testutil.ImageDigestRefB,
		testutil.WithNodePullSecret("my-secret", "my-ns"))
	g.Expect(k8sClient.Create(ctx, bn)).To(Succeed())
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, bn) })

	g.Eventually(auth.get).Should(Equal([]string{"old"}))

	// The kubelet syncs the rotated secret; the next attempt uses it.
	writeMountedPullSecret(t, "my-secret", "my-ns", "new")
	fake.setStageErr(nil)
	g.Eventually(auth.get).Should(Equal([]string{"old", "new"}))
}
