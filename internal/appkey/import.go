package appkey

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// DefaultPath is where the daemon keeps the key, on the state volume (§3.1).
const DefaultPath = "/state/app-key.pem"

// maxKeySize bounds the input. A 4096-bit RSA key in PEM is about 3.3 KB.
const maxKeySize = 64 << 10

// Import reads an RSA private key in PEM form from r and writes it to path
// with mode 0600, replacing any key already there. It writes a temporary file
// in the same directory and renames it over path, so input that isn't an RSA
// key, or a failed write, leaves the old key in place.
func Import(path string, r io.Reader) error {
	data, err := io.ReadAll(io.LimitReader(r, maxKeySize+1))
	if err != nil {
		return fmt.Errorf("app key %s: reading the input: %w", path, err)
	}
	if len(data) > maxKeySize {
		return fmt.Errorf("app key %s: input is over %d bytes", path, maxKeySize)
	}
	if err := checkRSA(data); err != nil {
		return fmt.Errorf("app key %s: %w", path, err)
	}

	// CreateTemp creates the file with mode 0600.
	f, err := os.CreateTemp(filepath.Dir(path), ".app-key-*.tmp")
	if err != nil {
		return fmt.Errorf("app key %s: %w", path, err)
	}
	tmp := f.Name()
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil && cerr == nil {
		err = os.Rename(tmp, path)
	} else if err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("app key %s: %w", path, err)
	}
	return nil
}

// checkRSA reports whether data holds an RSA private key in PEM form, either
// PKCS #1 (what GitHub issues) or PKCS #8.
func checkRSA(data []byte) error {
	block, _ := pem.Decode(data)
	if block == nil {
		return errors.New("input has no PEM block")
	}
	switch block.Type {
	case "RSA PRIVATE KEY":
		if _, err := x509.ParsePKCS1PrivateKey(block.Bytes); err != nil {
			return fmt.Errorf("parsing the RSA key: %w", err)
		}
	case "PRIVATE KEY":
		key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return fmt.Errorf("parsing the private key: %w", err)
		}
		if _, ok := key.(*rsa.PrivateKey); !ok {
			return fmt.Errorf("private key is %T, want an RSA key", key)
		}
	default:
		return fmt.Errorf("PEM block is %q, want an RSA private key", block.Type)
	}
	return nil
}
