package blueprint

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

// setupWatcher initializes the file system watcher
func (bm *BlueprintManager) setupWatcher() error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}

	bm.watcher = watcher

	if err := bm.addWatchPaths(); err != nil {
		bm.watcher.Close()
		return err
	}

	go bm.watchLoop()

	return nil
}

// addWatchPaths adds all directories to the watcher
func (bm *BlueprintManager) addWatchPaths() error {
	parentDir := filepath.Dir(bm.watchDir)
	if err := bm.watcher.Add(parentDir); err != nil {
		bm.log.Warn().Err(err).Msgf("Could not watch parent directory: %s", parentDir)
	}

	if err := bm.watcher.Add(bm.watchDir); err != nil {
		return fmt.Errorf("failed to watch blueprint directory %s: %w", bm.watchDir, err)
	}

	return filepath.WalkDir(bm.watchDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			bm.log.Warn().Err(err).Msgf("Error accessing path %s", path)
			return nil // Continue walking despite errors
		}
		if d.IsDir() && path != bm.watchDir {
			if err := bm.watcher.Add(path); err != nil {
				bm.log.Warn().Err(err).Msgf("Could not watch directory: %s", path)
			}
		}
		return nil
	})
}

// watchLoop handles file system events
func (bm *BlueprintManager) watchLoop() {
	for {
		select {
		case event, ok := <-bm.watcher.Events:
			if !ok {
				return
			}

			bm.log.Debug().Msgf("File event: %s %s", event.Op, event.Name)

			if event.Op&fsnotify.Remove == fsnotify.Remove || event.Op&fsnotify.Rename == fsnotify.Rename {
				if event.Name == bm.watchDir {
					bm.log.Debug().Msg("Blueprint directory was removed/renamed, reinitializing watcher")
					bm.scheduleWatcherReinit()
					continue
				}
			}

			// Handle new directories being created (ConfigMap remount creates new dirs)
			if event.Op&fsnotify.Create == fsnotify.Create {
				if info, err := os.Stat(event.Name); err == nil && info.IsDir() {
					bm.log.Debug().Msgf("New directory created: %s, adding to watcher", event.Name)
					if err := bm.watcher.Add(event.Name); err != nil {
						bm.log.Warn().Err(err).Msgf("Failed to add new directory to watcher: %s", event.Name)
					}
				} else if err != nil {
					bm.log.Debug().Err(err).Msgf("Could not stat created path: %s", event.Name)
				}
			}

			if !isYAMLFile(event.Name) {
				continue
			}

			switch {
			case event.Op&fsnotify.Write == fsnotify.Write,
				event.Op&fsnotify.Create == fsnotify.Create,
				event.Op&fsnotify.Remove == fsnotify.Remove,
				event.Op&fsnotify.Rename == fsnotify.Rename:
				bm.scheduleReload()
			}

		case err, ok := <-bm.watcher.Errors:
			if !ok {
				return
			}
			bm.log.Error().Err(err).Msg("Watcher error")

		case <-bm.stopChan:
			return
		}
	}
}

// scheduleWatcherReinit reinitializes the watcher after a delay
func (bm *BlueprintManager) scheduleWatcherReinit() {
	go func() {
		time.Sleep(2 * time.Second)

		bm.mu.Lock()
		defer bm.mu.Unlock()

		if bm.watcher != nil {
			bm.watcher.Close()
		}

		if err := bm.setupWatcher(); err != nil {
			bm.log.Error().Err(err).Msg("Failed to reinitialize watcher")
		} else {
			bm.log.Info().Msg("Watcher reinitialized successfully")
		}

		bm.scheduleReload()
	}()
}

// scheduleReload debounces multiple file changes and schedules a reload
func (bm *BlueprintManager) scheduleReload() {
	bm.mu.Lock()
	defer bm.mu.Unlock()

	if bm.reloadTimer != nil {
		bm.reloadTimer.Stop()
	}

	bm.reloadTimer = time.AfterFunc(bm.reloadDelay, func() {
		bm.log.Info().Msg("Reloading blueprints due to file changes")
		if err := bm.loadAndValidateBlueprints(); err != nil {
			bm.log.Error().Err(err).Msg("Failed to reload blueprints")
		} else {
			bm.log.Info().Msg("Blueprints reloaded successfully")
		}
	})
}

// CloseWatcher stops the file watcher and cleans up resources
func (bm *BlueprintManager) CloseWatcher() error {
	if !bm.watchEnabled {
		return nil
	}

	close(bm.stopChan)

	bm.mu.Lock()
	if bm.reloadTimer != nil {
		bm.reloadTimer.Stop()
	}
	bm.mu.Unlock()

	if bm.watcher != nil {
		return bm.watcher.Close()
	}

	bm.log.Info().Msg("Blueprint watcher closed")
	return nil
}
