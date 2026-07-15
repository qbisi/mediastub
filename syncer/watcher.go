package syncer

import (
	"context"
	"io/fs"
	"log"
	"os"
	"path/filepath"

	"github.com/fsnotify/fsnotify"
)

func watchTree(ctx context.Context, root string, logger *log.Logger, logLevel string) (<-chan struct{}, func() error, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, nil, err
	}
	addTree := func(start string) error {
		return filepath.WalkDir(start, func(name string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return w.Add(name)
			}
			return nil
		})
	}
	if err := addTree(root); err != nil {
		_ = w.Close()
		return nil, nil, err
	}
	events := make(chan struct{}, 1)
	go func() {
		defer close(events)
		for {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-w.Events:
				if !ok {
					return
				}
				if event.Has(fsnotify.Create) {
					if info, err := os.Stat(event.Name); err == nil && info.IsDir() {
						_ = addTree(event.Name)
					}
				}
				if logLevel == "debug" {
					logger.Printf("fsnotify path=%q operations=%s", event.Name, event.Op)
				}
				select {
				case events <- struct{}{}:
				default:
				}
			case err, ok := <-w.Errors:
				if !ok {
					return
				}
				logger.Printf("fsnotify error: %v", err)
			}
		}
	}()
	return events, w.Close, nil
}
