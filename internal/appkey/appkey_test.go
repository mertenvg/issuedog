package appkey

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

var key = []byte("-----BEGIN RSA PRIVATE KEY-----\ntest\n-----END RSA PRIVATE KEY-----\n")

// writeKey writes key to a new file with the given mode, set after creation
// so the umask doesn't change it.
func writeKey(t *testing.T, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "app-key.pem")
	if err := os.WriteFile(path, key, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadAccepts(t *testing.T) {
	for _, mode := range []os.FileMode{0o600, 0o400} {
		got, err := Load(writeKey(t, mode))
		if err != nil {
			t.Errorf("mode %04o: %v", mode, err)
			continue
		}
		if !bytes.Equal(got, key) {
			t.Errorf("mode %04o: got %q, want %q", mode, got, key)
		}
	}
}

func TestLoadRefusesGroupOrOtherAccess(t *testing.T) {
	for _, mode := range []os.FileMode{0o640, 0o604, 0o620, 0o602, 0o644} {
		path := writeKey(t, mode)
		_, err := Load(path)
		if err == nil {
			t.Errorf("mode %04o: loaded, want refused", mode)
			continue
		}
		if !strings.Contains(err.Error(), path) {
			t.Errorf("mode %04o: error %q doesn't name the path", mode, err)
		}
	}
}

func TestLoadRefusesWrongOwner(t *testing.T) {
	path := writeKey(t, 0o600)
	_, err := load(path, os.Getuid()+1)
	if err == nil {
		t.Fatal("loaded a file owned by another uid, want refused")
	}
	if !strings.Contains(err.Error(), "owned by uid") {
		t.Errorf("error %q doesn't mention the owner", err)
	}
}

func TestLoadRefusesMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.pem")
	_, err := Load(path)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("got %v, want a not-exist error", err)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q doesn't name the path", err)
	}
}

func TestLoadRefusesDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Fatal("loaded a directory, want refused")
	}
}

func TestLoadRefusesFIFO(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app-key.pem")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := Load(path)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("loaded a FIFO, want refused")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Load blocked on a FIFO")
	}
}
