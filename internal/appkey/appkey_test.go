package appkey

import (
	"bytes"
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
		if got, err := Load(writeKey(t, mode)); err != nil || !bytes.Equal(got, key) {
			t.Errorf("mode %04o: got %q, %v; want the key", mode, got, err)
		}
	}
}

// TestLoadRefuses checks each refusal names the path. The wrong-owner case
// passes another uid, because tests can't chown without root.
func TestLoadRefuses(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "fifo.pem")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	type refusal struct {
		path string
		uid  int
		want string
	}
	cases := map[string]refusal{
		"wrong owner": {writeKey(t, 0o600), os.Getuid() + 1, "owned by uid"},
		"missing":     {filepath.Join(dir, "missing.pem"), os.Getuid(), "no such file"},
		"directory":   {dir, os.Getuid(), "not a regular file"},
		"FIFO":        {fifo, os.Getuid(), "not a regular file"},
	}
	for _, mode := range []os.FileMode{0o640, 0o604, 0o620, 0o602, 0o644} {
		cases["mode "+mode.String()] = refusal{writeKey(t, mode), os.Getuid(), "gives access to group or others"}
	}
	for name, tt := range cases {
		done := make(chan error, 1)
		go func() {
			_, err := load(tt.path, tt.uid)
			done <- err
		}()
		select {
		case err := <-done:
			if err == nil || !strings.Contains(err.Error(), tt.path) || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("%s: err = %v, want one naming the path and containing %q", name, err, tt.want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: Load blocked", name)
		}
	}
}
