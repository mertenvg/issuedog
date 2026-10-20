package dirsync

import (
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// TestDirSyncFlushesADirectory syncs a real directory with no error.
func TestDirSyncFlushesADirectory(t *testing.T) {
	t.Parallel()
	if err := Sync(t.TempDir()); err != nil {
		t.Errorf("Sync = %v, want nil", err)
	}
}

// TestDirSyncReportsAMissingDirectory wraps the open's error and names the
// directory.
func TestDirSyncReportsAMissingDirectory(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "gone")
	err := Sync(dir)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Sync = %v, want an error wrapping fs.ErrNotExist", err)
	}
	if !strings.Contains(err.Error(), dir) {
		t.Errorf("Sync = %v, want it to name %s", err, dir)
	}
}
