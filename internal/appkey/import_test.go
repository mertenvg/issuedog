package appkey

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
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
