package appkey

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func newKey(t *testing.T, pkcs8 bool) []byte {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	if !pkcs8 {
		return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
	}
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

// TestImportWritesAndReplaces imports a PKCS #1 key, then a PKCS #8 one, under a umask that would deny all access.
func TestImportWritesAndReplaces(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app-key.pem")
	defer syscall.Umask(syscall.Umask(0o777))
	for _, data := range [][]byte{newKey(t, false), newKey(t, true)} {
		if err := Import(path, bytes.NewReader(data)); err != nil {
			t.Fatal(err)
		}
		if got, err := Load(path); err != nil || !bytes.Equal(got, data) {
			t.Fatalf("Load after Import: err %v, same key %v", err, bytes.Equal(got, data))
		}
		if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
			t.Errorf("mode %04o, want 0600", info.Mode().Perm())
		}
	}
}

func TestImportReportsTempFileCleanupError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app-key.pem")
	if err := os.MkdirAll(filepath.Join(path, "x"), 0o700); err != nil { // a full directory makes the rename fail
		t.Fatal(err)
	}
	errRemove := errors.New("remove failed")
	defer func(old func(string) error) { remove = old }(remove)
	remove = func(string) error { return errRemove }
	err := Import(path, bytes.NewReader(newKey(t, false)))
	var lerr *os.LinkError
	if !errors.Is(err, errRemove) || !errors.As(err, &lerr) {
		t.Errorf("err = %v, want the rename and the remove errors", err)
	}
}

// TestImportDirSyncAfterRename syncs the key's directory once, after the
// key is renamed into place. It replaces dirSync, so it isn't parallel.
func TestImportDirSyncAfterRename(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app-key.pem")
	data := newKey(t, false)
	var synced []string
	defer func(old func(string) error) { dirSync = old }(dirSync)
	dirSync = func(d string) error {
		synced = append(synced, d)
		if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, data) {
			t.Errorf("at the sync, the key file has %d bytes, err %v; want the new key in place", len(got), err)
		}
		return nil
	}
	if err := Import(path, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if len(synced) != 1 || synced[0] != dir {
		t.Errorf("synced %q, want [%q]", synced, dir)
	}
}

// TestImportDirSyncErrorReturned returns a failed sync's error, wrapped and
// naming the key, and leaves the renamed key and no temp file. It replaces
// dirSync, so it isn't parallel.
func TestImportDirSyncErrorReturned(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app-key.pem")
	data := newKey(t, false)
	errSync := errors.New("sync failed")
	defer func(old func(string) error) { dirSync = old }(dirSync)
	dirSync = func(string) error { return errSync }
	err := Import(path, bytes.NewReader(data))
	if !errors.Is(err, errSync) {
		t.Fatalf("err = %v, want the sync error", err)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("err = %v, want it to name %s", err, path)
	}
	if got, rerr := os.ReadFile(path); rerr != nil || !bytes.Equal(got, data) {
		t.Errorf("key file has %d bytes, err %v; want the new key", len(got), rerr)
	}
	entries, rerr := os.ReadDir(dir)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if len(entries) != 1 {
		t.Errorf("dir has %d entries, want only the key", len(entries))
	}
}

func TestImportRefusesBadInput(t *testing.T) {
	ec, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ecDER, _ := x509.MarshalPKCS8PrivateKey(ec)
	block := func(typ string, der []byte) []byte { return pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}) }
	old := newKey(t, false)
	for name, data := range map[string][]byte{
		"empty":         nil,
		"not PEM":       []byte("not a key\n"),
		"broken RSA":    block("RSA PRIVATE KEY", []byte("junk")),
		"EC key":        block("PRIVATE KEY", ecDER),
		"certificate":   block("CERTIFICATE", []byte("junk")),
		"over the size": bytes.Repeat([]byte("a"), maxKeySize+1),
	} {
		dir := t.TempDir()
		path := filepath.Join(dir, "app-key.pem")
		if err := Import(path, bytes.NewReader(old)); err != nil {
			t.Fatal(err)
		}
		if err := Import(path, bytes.NewReader(data)); err == nil || !strings.Contains(err.Error(), path) {
			t.Errorf("%s: err = %v, want an error naming the path", name, err)
		}
		if got, _ := os.ReadFile(path); !bytes.Equal(got, old) {
			t.Errorf("%s: the old key was changed", name)
		}
		if entries, _ := os.ReadDir(dir); len(entries) != 1 {
			t.Errorf("%s: %d files in the directory, want only the key", name, len(entries))
		}
	}
}
