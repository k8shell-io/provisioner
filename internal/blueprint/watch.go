package blueprint

import (
	"fmt"
	"io/fs"
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

	err = filepath.WalkDir(bm.watchDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return bm.watcher.Add(path)
		}
		return nil
	})

	if err != nil {
		bm.watcher.Close()
		return err
	}

	go bm.watchLoop()

	return nil
}

// watchLoop handles file system events
func (bm *BlueprintManager) watchLoop() {
	for {
		select {
		case event, ok := <-bm.watcher.Events:
			if !ok {
				return
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
			fmt.Printf("Watcher error: %v\n", err)

		case <-bm.stopChan:
			return
		}
	}
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
