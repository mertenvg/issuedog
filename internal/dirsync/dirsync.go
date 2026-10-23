// Package dirsync flushes a directory's entries to disk, so a rename or a
// removal in it survives a crash. The daemon's lease, the App key's import
// and a run's result all write through a temp file and a rename, then sync
// the directory with Sync.
package dirsync

import (
	"fmt"
	"os"
)

// dirHandle is an open directory, as syncDir uses it: an *os.File, or a
// test's fake.
type dirHandle interface {
	Sync() error
	Close() error
}

// Sync flushes dir's entries to disk.
func Sync(dir string) error {
	return syncDir(dir, openDir)
}

// openDir opens path for Sync.
func openDir(path string) (dirHandle, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening the directory: %w", err)
	}
	return f, nil
}

// syncDir flushes path's entries to disk, opening it with openDirFunc, which
// is openDir but in tests. It closes what it opened even when the sync
// fails, and then returns the sync's error, not the close's.
func syncDir(path string, openDirFunc func(string) (dirHandle, error)) error {
	d, err := openDirFunc(path)
	if err != nil {
		return fmt.Errorf("sync %s: %w", path, err)
	}
	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("sync %s: %w", path, err)
	}
	return nil
}
