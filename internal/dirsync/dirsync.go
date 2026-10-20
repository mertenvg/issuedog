// Package dirsync flushes a directory's entries to disk, so a rename or a
// removal in it survives a crash. The daemon's lease, the App key's import
// and a run's result all write through a temp file and a rename, then sync
// the directory with Sync.
package dirsync

import (
	"fmt"
	"os"
)

// Sync flushes dir's entries to disk.
func Sync(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("sync %s: %w", dir, err)
	}
	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("sync %s: %w", dir, err)
	}
	return nil
}
