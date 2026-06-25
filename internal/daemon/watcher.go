// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"bytes"
	"context"
	"os"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	bootcv1alpha1 "github.com/bootc-dev/bootc-operator/api/v1alpha1"
	"github.com/bootc-dev/bootc-operator/internal/bootc"
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
	Executor     bootc.Executor
	Ready        chan struct{}

	mu      sync.RWMutex
	cached  *bootc.Status
	lastRaw []byte
	started bool
}

// GetStatus returns the cached bootc status when the watcher loop is
// running (fsnotify or polling keeps the cache fresh). If the watcher
// has not been started, it always reads fresh from the host.
func (w *StatusWatcher) GetStatus(ctx context.Context) (*bootc.Status, error) {
	w.mu.RLock()
	s := w.cached
	started := w.started
	w.mu.RUnlock()
	if started && s != nil {
		return s, nil
	}
	s, _, err := w.refresh(ctx)
	return s, err
}

// refresh reads bootc status from the host and updates the cache.
// Returns the new status or an error.
func (w *StatusWatcher) refresh(ctx context.Context) (*bootc.Status, bool, error) {
	data, err := w.Executor.Status(ctx)
	if err != nil {
		return nil, false, err
	}
	s, err := bootc.ParseStatus(data)
	if err != nil {
		return nil, false, err
	}
	w.mu.Lock()
	changed := !bytes.Equal(data, w.lastRaw)
	w.cached = s
	w.lastRaw = data
	w.mu.Unlock()
	return s, changed, nil
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

	// Only start polling once fsnotify is unavailable.
	var ticker *time.Ticker
	var tickerCh <-chan time.Time
	if fsWatcher == nil {
		ticker = time.NewTicker(w.PollInterval)
		tickerCh = ticker.C
	}
	defer func() {
		if ticker != nil {
			ticker.Stop()
		}
	}()

	var evCh <-chan fsnotify.Event
	var errCh <-chan error
	if fsWatcher != nil {
		evCh = fsWatcher.Events
		errCh = fsWatcher.Errors
	}

	w.mu.Lock()
	w.started = true
	w.mu.Unlock()

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
				w.refreshAndNotify(ctx, log)
			}
		// Tear down fsnotify so the loop continues with polling only.
		// A broken inotify fd never delivers events again, so without this
		// the watcher silently stops reacting to filesystem changes.
		case err := <-errCh:
			log.Error(err, "fsnotify error, degrading to polling only")
			closeFsWatcher()
			evCh = nil
			errCh = nil
			ticker = time.NewTicker(w.PollInterval)
			tickerCh = ticker.C
		case <-tickerCh:
			log.V(1).Info("Polling bootc status")
			w.refreshAndNotify(ctx, log)
		}
	}
}

// refreshAndNotify reads bootc status, updates the cache, and sends
// an event to trigger reconciliation.
func (w *StatusWatcher) refreshAndNotify(ctx context.Context, log logr.Logger) {
	_, changed, err := w.refresh(ctx)
	if err != nil {
		log.Error(err, "Failed to refresh bootc status")
		return
	}
	if !changed {
		return
	}
	w.sendEvent()
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
