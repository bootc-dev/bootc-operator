// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/event"
)

func startWatcher(t *testing.T, w *StatusWatcher) (done <-chan error, cancel context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan error, 1)
	go func() { ch <- w.Start(ctx) }()
	<-w.Ready
	return ch, cancel
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

			events := make(chan event.GenericEvent, 1)
			w := &StatusWatcher{
				PollInterval:  tt.pollInterval,
				OstreePath:    ostreePath,
				ComposefsPath: composefsPath,
				Events:        events,
				NodeName:      "test-node",
				Ready:         make(chan struct{}),
			}

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
			case ev := <-events:
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

func TestWatcherShutdown(t *testing.T) {
	dir := t.TempDir()
	watchDir := filepath.Join(dir, "bootc")
	if err := os.Mkdir(watchDir, 0o755); err != nil {
		t.Fatal(err)
	}

	w := &StatusWatcher{
		PollInterval: 10 * time.Minute,
		OstreePath:  watchDir,
		ComposefsPath: filepath.Join(dir, "nonexistent"),
		Events:       make(chan event.GenericEvent, 1),
		NodeName:     "test-node",
		Ready:        make(chan struct{}),
	}

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
