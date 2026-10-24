package dirsync

import (
	"errors"
	"io/fs"
	"path/filepath"
	"slices"
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

// fakeDir is a dirHandle whose Sync and Close return syncErr and closeErr,
// and which records the calls it gets.
type fakeDir struct {
	syncErr, closeErr error
	calls             []string
}

func (f *fakeDir) Sync() error {
	f.calls = append(f.calls, "sync")
	return f.syncErr
}

func (f *fakeDir) Close() error {
	f.calls = append(f.calls, "close")
	return f.closeErr
}

// openFake returns an opener for syncDir that hands out d, and checks it's
// asked to open want.
func openFake(t *testing.T, d *fakeDir, want string) func(string) (dirHandle, error) {
	t.Helper()
	return func(path string) (dirHandle, error) {
		if path != want {
			t.Errorf("opened %s, want %s", path, want)
		}
		return d, nil
	}
}

// TestDirSyncReportsAFailedFsync wraps the fsync's error, names the
// directory, and still closes it.
func TestDirSyncReportsAFailedFsync(t *testing.T) {
	t.Parallel()
	errSync := errors.New("fsync failed")
	d := &fakeDir{syncErr: errSync}
	err := syncDir("/state/runs", openFake(t, d, "/state/runs"))
	if !errors.Is(err, errSync) {
		t.Fatalf("syncDir = %v, want an error wrapping the fsync's", err)
	}
	if !strings.Contains(err.Error(), "/state/runs") {
		t.Errorf("syncDir = %v, want it to name /state/runs", err)
	}
	if !slices.Equal(d.calls, []string{"sync", "close"}) {
		t.Errorf("calls = %q, want sync then close", d.calls)
	}
}

// TestDirSyncReportsAFailedClose wraps the close's error after a good fsync,
// and names the directory.
func TestDirSyncReportsAFailedClose(t *testing.T) {
	t.Parallel()
	errClose := errors.New("close failed")
	d := &fakeDir{closeErr: errClose}
	err := syncDir("/state", openFake(t, d, "/state"))
	if !errors.Is(err, errClose) {
		t.Fatalf("syncDir = %v, want an error wrapping the close's", err)
	}
	if !strings.Contains(err.Error(), "/state") {
		t.Errorf("syncDir = %v, want it to name /state", err)
	}
	if !slices.Equal(d.calls, []string{"sync", "close"}) {
		t.Errorf("calls = %q, want sync then close", d.calls)
	}
}

// TestDirSyncFsyncErrorBeatsCloseError returns the fsync's error when the
// close fails too, as the fsync's is the one that says the entries may not
// be on disk.
func TestDirSyncFsyncErrorBeatsCloseError(t *testing.T) {
	t.Parallel()
	errSync, errClose := errors.New("fsync failed"), errors.New("close failed")
	d := &fakeDir{syncErr: errSync, closeErr: errClose}
	err := syncDir("/state", openFake(t, d, "/state"))
	if !errors.Is(err, errSync) {
		t.Fatalf("syncDir = %v, want an error wrapping the fsync's", err)
	}
	if errors.Is(err, errClose) {
		t.Errorf("syncDir = %v, want the fsync's error only, not the close's", err)
	}
}

// TestDirSyncReportsAFailedOpen wraps the opener's error and names the
// directory, with nothing to sync or close.
func TestDirSyncReportsAFailedOpen(t *testing.T) {
	t.Parallel()
	errOpen := errors.New("open failed")
	err := syncDir("/state", func(string) (dirHandle, error) { return nil, errOpen })
	if !errors.Is(err, errOpen) {
		t.Fatalf("syncDir = %v, want an error wrapping the open's", err)
	}
	if !strings.Contains(err.Error(), "/state") {
		t.Errorf("syncDir = %v, want it to name /state", err)
	}
}
