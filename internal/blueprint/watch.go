package blueprint

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	log "github.com/k8shell-io/common/logger"
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
	if w.stopChan == nil {
		w.stopChan = make(chan struct{})
	}
	fsW, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	w.watcher = fsW
	w.watchedDirs = make(map[string]struct{})

	if err := w.addInitialWatches(); err != nil {
		_ = w.watcher.Close()
		return err
	}

	go w.watchLoop()
	return nil
}

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

func (w *Watcher) watchLoop() {
	for {
		select {
		case event, ok := <-w.watcher.Events:
			if !ok {
				return
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

		case err, ok := <-w.watcher.Errors:
			if !ok {
				return
			}
			w.log.Error().Err(err).Msg("Watcher error")

		case <-w.stopChan:
			return
		}
	}
}

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

func (w *Watcher) scheduleReload() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.reloadTimer != nil {
		w.reloadTimer.Stop()
	}
	w.reloadTimer = time.AfterFunc(w.reloadDelay, func() {
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

func (w *Watcher) Close() error {
	if w.stopChan != nil {
		close(w.stopChan)
	}
	w.mu.Lock()
	if w.reloadTimer != nil {
		w.reloadTimer.Stop()
	}
	fw := w.watcher
	w.watcher = nil
	w.watchedDirs = nil
	w.mu.Unlock()

	if fw != nil {
		return fw.Close()
	}
	w.log.Info().Msg("Watcher closed")
	return nil
}

func isKubeShadow(path string) bool {
	base := filepath.Base(path)
	return strings.HasPrefix(base, "..")
}
