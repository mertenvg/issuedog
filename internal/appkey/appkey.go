// Package appkey imports and loads the GitHub App private key (SPEC §3.1).
package appkey

import (
	"fmt"
	"io"
	"os"
	"syscall"
)

// Load reads the key file at path. It refuses anything but a regular file
// owned by the current uid with no group or other bits, such as mode 0600.
func Load(path string) ([]byte, error) {
	return load(path, os.Getuid())
}

func load(path string, uid int) ([]byte, error) {
	// O_NONBLOCK keeps a FIFO from blocking the open; regular files ignore it.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("app key: %w", err)
	}
	defer f.Close()

	// Check the opened file, not the path, so it can't be swapped before the read.
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("app key: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("app key %s: not a regular file", path)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, fmt.Errorf("app key %s: can't read the file owner", path)
	}
	if int(st.Uid) != uid {
		return nil, fmt.Errorf("app key %s: owned by uid %d, want uid %d", path, st.Uid, uid)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("app key %s: mode %04o gives access to group or others, want 0600", path, perm)
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("app key %s: %w", path, err)
	}
	return data, nil
}
