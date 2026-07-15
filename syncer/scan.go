package syncer

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"syscall"

	"github.com/qbisi/mediastub/origin"
)

type remoteSnapshot struct {
	Entries    map[string]origin.Entry
	Duplicates map[string][]origin.Entry
}

func scanRemote(ctx context.Context, upstream origin.Origin) (*remoteSnapshot, error) {
	snapshot := &remoteSnapshot{Entries: map[string]origin.Entry{}, Duplicates: map[string][]origin.Entry{}}
	var walk func(string) error
	walk = func(dir string) error {
		entries, err := upstream.ReadDir(ctx, dir)
		if err != nil {
			return fmt.Errorf("read remote directory %q: %w", dir, err)
		}
		grouped := make(map[string][]origin.Entry)
		for _, entry := range entries {
			grouped[entry.Path] = append(grouped[entry.Path], entry)
		}
		for rel, objects := range grouped {
			if len(objects) != 1 {
				snapshot.Duplicates[rel] = append(snapshot.Duplicates[rel], objects...)
				delete(snapshot.Entries, rel)
				continue
			}
			if _, exists := snapshot.Entries[rel]; exists {
				snapshot.Duplicates[rel] = append(snapshot.Duplicates[rel], snapshot.Entries[rel], objects[0])
				delete(snapshot.Entries, rel)
				continue
			}
			snapshot.Entries[rel] = objects[0]
		}
		for rel, objects := range grouped {
			if len(objects) == 1 && objects[0].IsDir {
				if _, duplicate := snapshot.Duplicates[rel]; !duplicate {
					if err := walk(rel); err != nil {
						return err
					}
				}
			}
		}
		return nil
	}
	if err := walk("."); err != nil {
		return nil, err
	}
	return snapshot, nil
}

type localFile struct {
	Path    string
	Size    int64
	ModTime int64
	Inode   uint64
	Mode    fs.FileMode
}

func localFileInfo(rel string, info fs.FileInfo) localFile {
	var inode uint64
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		inode = stat.Ino
	}
	return localFile{Path: rel, Size: info.Size(), ModTime: info.ModTime().UnixNano(), Inode: inode, Mode: info.Mode()}
}

func statLocal(root, rel string) (localFile, error) {
	info, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		return localFile{}, err
	}
	return localFileInfo(rel, info), nil
}

func scanLocal(root string) (map[string]localFile, error) {
	files := make(map[string]localFile)
	err := filepath.WalkDir(root, func(localPath string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if localPath == root {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, localPath)
		if err != nil {
			return err
		}
		rel = path.Clean(filepath.ToSlash(rel))
		files[rel] = localFileInfo(rel, info)
		return nil
	})
	return files, err
}
