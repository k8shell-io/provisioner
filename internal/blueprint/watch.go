// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package blueprint

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	log "github.com/k8shell-io/common/pkg/logger"
	"github.com/rs/zerolog"
)

// Watcher watches a directory tree for YAML changes and calls onReload() debounced.
type Watcher struct {
	watcher      *fsnotify.Watcher
	watchDir     string
	log          *zerolog.Logger
	stopChan     chan struct{}
	reloadTimer  *time.Timer
	reloadDelay  time.Duration
	watchEnabled bool

	mu          sync.Mutex
	watchedDirs map[string]struct{}
	reinitOnce  bool
	closed      bool

	reloadWg sync.WaitGroup // tracks in-flight onReload calls

	onReload func() error
}

// NewWatcher constructs a watcher. Call w.Setup() to start.
func NewWatcher(watchDir string, reloadDelay time.Duration, onReload func() error) *Watcher {
	return &Watcher{
		log:          log.NewLogger("watcher"),
		watchDir:     watchDir,
		reloadDelay:  reloadDelay,
		watchEnabled: true,
		onReload:     onReload,
	}
}

// Setup initializes fsnotify and starts the event loop.
func (w *Watcher) Setup() error {
	w.mu.Lock()
	if w.stopChan != nil {
		// drain any previous stop signal before reuse
		select {
		case <-w.stopChan:
		default:
		}
	} else {
		w.stopChan = make(chan struct{})
	}
	w.mu.Unlock()

	fsW, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}

	w.mu.Lock()
	w.watcher = fsW
	w.watchedDirs = make(map[string]struct{})
	w.mu.Unlock()

	if err := w.addInitialWatches(); err != nil {
		_ = fsW.Close()
		return err
	}

	go w.watchLoop(fsW, w.stopChan)
	return nil
}

// addWatch registers path with the underlying fsnotify watcher, skipping
// Kubernetes shadow directories and paths already watched.
func (w *Watcher) addWatch(path string) {
	if isKubeShadow(path) {
		return
	}
	if _, ok := w.watchedDirs[path]; ok {
		return
	}
	if err := w.watcher.Add(path); err != nil {
		w.log.Warn().Err(err).Msgf("Could not watch: %s", path)
		return
	}
	w.watchedDirs[path] = struct{}{}
	w.log.Debug().Msgf("Watching: %s", path)
}

// removeWatch deregisters path from the underlying fsnotify watcher.
func (w *Watcher) removeWatch(path string) {
	if _, ok := w.watchedDirs[path]; !ok {
		return
	}
	if err := w.watcher.Remove(path); err != nil {
		w.log.Debug().Err(err).Msgf("Remove watch benign race: %s", path)
	}
	delete(w.watchedDirs, path)
	w.log.Debug().Msgf("Unwatched: %s", path)
}

// addInitialWatches registers the parent directory, the watch root, and every
// subdirectory beneath it with fsnotify so that new files and directories are
// picked up without restarting the watcher.
func (w *Watcher) addInitialWatches() error {
	parentDir := filepath.Dir(w.watchDir)
	w.addWatch(parentDir)
	w.addWatch(w.watchDir)

	return filepath.WalkDir(w.watchDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			w.log.Warn().Err(err).Msgf("Error accessing %s", path)
			return nil
		}
		if isKubeShadow(path) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.IsDir() || path == w.watchDir {
			return nil
		}
		w.addWatch(path)
		return nil
	})
}

// watchLoop is the main event-processing goroutine. It handles fsnotify events,
// filters irrelevant filesystem noise (chmod, Kubernetes shadow paths), maintains
// the watched-directory set as directories are created or removed, and schedules
// debounced reload callbacks on YAML changes.
func (w *Watcher) watchLoop(fsW *fsnotify.Watcher, stop chan struct{}) {
	for {
		select {
		case event, ok := <-fsW.Events:
			if !ok {
				return
			}

			base := filepath.Base(event.Name)

			// Special-case the ConfigMap flip: ..data symlink changes mean new content.
			if base == "..data" && (event.Op&(fsnotify.Rename|fsnotify.Create)) != 0 {
				w.log.Debug().Msg("Detected ..data symlink flip; scheduling reload")
				// rescan watches if we add/remove subdirs dynamically.
				// w.scheduleReinit() // only if we need to rebuild watches
				w.scheduleReload()
				continue
			}

			if event.Op&fsnotify.Chmod == fsnotify.Chmod || isKubeShadow(event.Name) {
				continue
			}

			w.log.Debug().Msgf("File event: %s %s", event.Op, event.Name)

			if (event.Op&(fsnotify.Remove|fsnotify.Rename) != 0) && event.Name == w.watchDir {
				w.log.Debug().Msg("Root dir removed/renamed; scheduling watcher reinit")
				w.scheduleReinit()
				continue
			}

			if event.Op&fsnotify.Create == fsnotify.Create {
				if info, err := os.Stat(event.Name); err == nil && info.IsDir() && !isKubeShadow(event.Name) {
					w.mu.Lock()
					w.addWatch(event.Name)
					w.mu.Unlock()
				}
			}

			if event.Op&(fsnotify.Remove|fsnotify.Rename) != 0 && !isKubeShadow(event.Name) {
				w.mu.Lock()
				w.removeWatch(event.Name)
				w.mu.Unlock()
			}

			if !isYAMLFile(event.Name) {
				continue
			}
			if event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Remove|fsnotify.Rename) != 0 {
				w.scheduleReload()
			}

		case err, ok := <-fsW.Errors:
			if !ok {
				return
			}
			w.log.Error().Err(err).Msg("Watcher error")

		case <-stop:
			return
		}
	}
}

// scheduleReinit tears down the current fsnotify watcher and recreates it after
// a short delay. This is necessary when the watched root directory is removed or
// renamed (e.g. during a Kubernetes ConfigMap update that replaces the symlink).
func (w *Watcher) scheduleReinit() {
	w.mu.Lock()
	if w.reinitOnce {
		w.mu.Unlock()
		return
	}
	w.reinitOnce = true
	w.mu.Unlock()

	go func() {
		time.Sleep(2 * time.Second)
		w.mu.Lock()
		old := w.watcher
		if old != nil {
			_ = old.Close()
			w.watcher = nil
			w.watchedDirs = nil
		}
		// Allocate a fresh stop channel so the new watchLoop goroutine
		// is independent of any previous one.
		w.stopChan = make(chan struct{})
		w.reinitOnce = false
		w.mu.Unlock()

		if err := w.Setup(); err != nil {
			w.log.Error().Err(err).Msg("Failed to reinitialize watcher")
		} else {
			w.log.Info().Msg("Watcher reinitialized successfully")
		}
		w.scheduleReload()
	}()
}

// scheduleReload arms (or resets) a debounce timer so that onReload is called
// once after reloadDelay has elapsed without further filesystem events.
func (w *Watcher) scheduleReload() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.reloadTimer != nil {
		w.reloadTimer.Stop()
	}
	w.reloadTimer = time.AfterFunc(w.reloadDelay, func() {
		w.mu.Lock()
		if w.closed {
			w.mu.Unlock()
			return
		}
		w.reloadWg.Add(1)
		w.mu.Unlock()

		defer w.reloadWg.Done()
		w.log.Info().Msg("Reloading due to file changes")
		if w.onReload != nil {
			if err := w.onReload(); err != nil {
				w.log.Error().Err(err).Msg("Reload callback failed")
			} else {
				w.log.Info().Msg("Reload callback succeeded")
			}
		}
	})
}

// Close stops the watcher, cancels any pending reload timer, and waits for
// in-flight reload callbacks to finish before returning.
func (w *Watcher) Close() error {
	w.mu.Lock()
	w.closed = true
	stop := w.stopChan
	if w.reloadTimer != nil {
		w.reloadTimer.Stop()
	}
	fw := w.watcher
	w.watcher = nil
	w.watchedDirs = nil
	w.mu.Unlock()

	if stop != nil {
		close(stop)
	}

	// Wait for any in-flight reload callback to finish before releasing resources.
	w.reloadWg.Wait()

	if fw != nil {
		return fw.Close()
	}
	w.log.Info().Msg("Watcher closed")
	return nil
}

// isKubeShadow reports whether path is a Kubernetes ConfigMap internal shadow
// entry (e.g. "..data" or "..2024_01_01_…"). These must be skipped to avoid
// processing the same file twice when both the symlink and the target are seen.
func isKubeShadow(path string) bool {
	base := filepath.Base(path)
	return strings.HasPrefix(base, "..")
}
