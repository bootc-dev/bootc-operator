// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"os"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	bootcv1alpha1 "github.com/bootc-dev/bootc-operator/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

const (
	DefaultOstreePath    = "/proc/1/root/ostree/bootc"
	DefaultComposefsPath = "/proc/1/root/sysroot/state/deploy"
)

type StatusWatcher struct {
	PollInterval time.Duration
	OstreePath    string
	ComposefsPath string
	Events       chan event.GenericEvent
	NodeName     string
	Ready        chan struct{}
}

// Start implements manager.Runnable.
func (w *StatusWatcher) Start(ctx context.Context) error {
	log := logf.FromContext(ctx).WithName("status-watcher")

	watchPath := w.resolveWatchPath()

	fsWatcher := w.setupFsnotify(log, watchPath)

	closeFsWatcher := func() {
		if fsWatcher != nil {
			_ = fsWatcher.Close()
			fsWatcher = nil
		}
	}
	defer closeFsWatcher()

	if w.PollInterval <= 0 {
		w.PollInterval = 5 * time.Minute
	}

	ticker := time.NewTicker(w.PollInterval)
	defer ticker.Stop()

	var evCh <-chan fsnotify.Event
	var errCh <-chan error
	if fsWatcher != nil {
		evCh = fsWatcher.Events
		errCh = fsWatcher.Errors
	}

	if w.Ready != nil {
		close(w.Ready)
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case ev := <-evCh:
			// ostree backend: mtime change triggers ATTRIB (Chmod).
			// composefs backend: new deploy directory triggers Create.
			if ev.Has(fsnotify.Chmod) || ev.Has(fsnotify.Create) {
				log.V(1).Info("Detected bootc status change via fsnotify")
				w.sendEvent()
			}
		// Tear down fsnotify so the loop continues with polling only.
		// A broken inotify fd never delivers events again, so without this
		// the watcher silently stops reacting to filesystem changes.
		case err := <-errCh:
			log.Error(err, "fsnotify error, degrading to polling only")
			closeFsWatcher()
			evCh = nil
			errCh = nil
		case <-ticker.C:
			log.V(1).Info("Polling bootc status")
			w.sendEvent()
		}
	}
}

func (w *StatusWatcher) setupFsnotify(log logr.Logger, watchPath string) *fsnotify.Watcher {
	if watchPath == "" {
		log.Info("No bootc paths to watch found, using polling only")
		return nil
	}

	fsWatcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Error(err, "Failed to create fsnotify watcher, falling back to polling")
		return nil
	}

	if err := fsWatcher.Add(watchPath); err != nil {
		log.Error(err, "Failed to watch path, falling back to polling", "path", watchPath)
		_ = fsWatcher.Close()
		return nil
	}

	log.Info("Watching path for bootc status changes", "path", watchPath)
	return fsWatcher
}

func (w *StatusWatcher) resolveWatchPath() string {
	if _, err := os.Stat(w.OstreePath); err == nil {
		return w.OstreePath
	}
	if _, err := os.Stat(w.ComposefsPath); err == nil {
		return w.ComposefsPath
	}
	return ""
}

func (w *StatusWatcher) sendEvent() {
	ev := event.GenericEvent{
		Object: &bootcv1alpha1.BootcNode{
			ObjectMeta: metav1.ObjectMeta{Name: w.NodeName},
		},
	}
	select {
	case w.Events <- ev:
	default:
	}
}
