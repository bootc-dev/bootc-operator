// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/event"

	testutil "github.com/bootc-dev/bootc-operator/test/util"
)

func startWatcher(t *testing.T, w *StatusWatcher) (done <-chan error, cancel context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan error, 1)
	go func() { ch <- w.Start(ctx) }()
	<-w.Ready
	return ch, cancel
}

func newTestWatcher(ostreePath, composefsPath string, pollInterval time.Duration) *StatusWatcher {
	f := &fakeExecutor{}
	f.status = newBootcStatus(testutil.DigestA)
	return &StatusWatcher{
		PollInterval:  pollInterval,
		OstreePath:    ostreePath,
		ComposefsPath: composefsPath,
		Events:        make(chan event.GenericEvent, 1),
		NodeName:      "test-node",
		Executor:      f,
		Ready:         make(chan struct{}),
	}
}

type triggerKind int

const (
	triggerNone   triggerKind = iota
	triggerChmod              // ostree: mtime change fires ATTRIB
	triggerCreate             // composefs: new deploy dir fires CREATE
)

func TestWatcherEvents(t *testing.T) {
	tests := []struct {
		name         string
		mkOstree     bool
		mkComposefs  bool
		trigger      triggerKind
		pollInterval time.Duration
	}{
		{
			name:         "OstreeChmod",
			mkOstree:     true,
			trigger:      triggerChmod,
			pollInterval: 10 * time.Minute,
		},
		{
			name:         "ComposefsCreate",
			mkComposefs:  true,
			trigger:      triggerCreate,
			pollInterval: 10 * time.Minute,
		},
		{
			name:         "PollOnly",
			pollInterval: 200 * time.Millisecond,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			ostreePath := filepath.Join(dir, "bootc")
			composefsPath := filepath.Join(dir, "deploy")

			if tt.mkOstree {
				if err := os.Mkdir(ostreePath, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if tt.mkComposefs {
				if err := os.Mkdir(composefsPath, 0o755); err != nil {
					t.Fatal(err)
				}
			}

			w := newTestWatcher(ostreePath, composefsPath, tt.pollInterval)

			done, cancel := startWatcher(t, w)
			defer cancel()

			switch tt.trigger {
			case triggerChmod:
				now := time.Now()
				if err := os.Chtimes(ostreePath, now, now); err != nil {
					t.Fatal(err)
				}
			case triggerCreate:
				if err := os.Mkdir(filepath.Join(composefsPath, "new-deploy"), 0o755); err != nil {
					t.Fatal(err)
				}
			}

			select {
			case ev := <-w.Events:
				if ev.Object.GetName() != "test-node" {
					t.Errorf("expected node name test-node, got %s", ev.Object.GetName())
				}
			case <-time.After(5 * time.Second):
				t.Fatal("timed out waiting for event")
			}

			cancel()
			if err := <-done; err != nil {
				t.Fatalf("watcher returned error: %v", err)
			}
		})
	}
}

func TestWatcherCachesStatus(t *testing.T) {
	dir := t.TempDir()
	w := newTestWatcher(filepath.Join(dir, "nonexistent"), filepath.Join(dir, "nonexistent2"), 200*time.Millisecond)

	done, cancel := startWatcher(t, w)
	defer cancel()

	// Wait for the poll to fire and populate the cache.
	select {
	case <-w.Events:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for event")
	}

	status, err := w.GetStatus(context.Background())
	if err != nil {
		t.Fatalf("GetStatus returned error: %v", err)
	}
	if status.Status.Booted == nil || status.Status.Booted.Image == nil {
		t.Fatal("expected booted entry in cached status")
	}
	if status.Status.Booted.Image.ImageDigest != testutil.DigestA {
		t.Errorf("expected digest %s, got %s", testutil.DigestA, status.Status.Booted.Image.ImageDigest)
	}

	// Change the executor's data to simulate a stale cache where
	// fsnotify hasn't fired yet. GetStatus must still return the
	// cached (old) value, proving it serves from cache.
	f := w.Executor.(*fakeExecutor)
	f.mu.Lock()
	f.status = newBootcStatus(testutil.DigestB)
	f.mu.Unlock()

	status, err = w.GetStatus(context.Background())
	if err != nil {
		t.Fatalf("GetStatus returned error after executor change: %v", err)
	}
	if status.Status.Booted.Image.ImageDigest != testutil.DigestA {
		t.Errorf("expected cached digest %s, got %s", testutil.DigestA, status.Status.Booted.Image.ImageDigest)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("watcher returned error: %v", err)
	}
}

func TestWatcherGetStatusColdCache(t *testing.T) {
	f := &fakeExecutor{}
	f.status = newBootcStatus(testutil.DigestA)

	w := &StatusWatcher{
		Events:   make(chan event.GenericEvent, 1),
		NodeName: "test-node",
		Executor: f,
	}

	status, err := w.GetStatus(context.Background())
	if err != nil {
		t.Fatalf("GetStatus returned error: %v", err)
	}
	if status.Status.Booted == nil || status.Status.Booted.Image == nil || status.Status.Booted.Image.ImageDigest != testutil.DigestA {
		t.Fatalf("expected booted digest %s", testutil.DigestA)
	}
}

func TestWatcherShutdown(t *testing.T) {
	dir := t.TempDir()
	watchDir := filepath.Join(dir, "bootc")
	if err := os.Mkdir(watchDir, 0o755); err != nil {
		t.Fatal(err)
	}

	w := newTestWatcher(watchDir, filepath.Join(dir, "nonexistent"), 10*time.Minute)

	done, cancel := startWatcher(t, w)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("watcher returned error on shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for watcher to shut down")
	}
}

func TestWatcherNoPollWhenFsnotifyHealthy(t *testing.T) {
	dir := t.TempDir()
	ostreePath := filepath.Join(dir, "bootc")
	if err := os.Mkdir(ostreePath, 0o755); err != nil {
		t.Fatal(err)
	}

	w := newTestWatcher(ostreePath, filepath.Join(dir, "nonexistent"), 200*time.Millisecond)

	done, cancel := startWatcher(t, w)
	defer cancel()

	// With fsnotify healthy and no filesystem events, the short poll
	// interval should NOT fire (polling is deferred until fsnotify fails).
	select {
	case <-w.Events:
		t.Fatal("received unexpected poll event while fsnotify is healthy")
	case <-time.After(500 * time.Millisecond):
		// expected: no events
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("watcher returned error: %v", err)
	}
}
