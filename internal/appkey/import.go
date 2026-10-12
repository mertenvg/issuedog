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

// Import writes the RSA private key (PEM) read from r to path with mode 0600,
// through a temp file renamed over path, so bad input leaves any old key.
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

	f, err := os.CreateTemp(filepath.Dir(path), ".app-key-*.tmp")
	if err != nil {
		return fmt.Errorf("app key %s: %w", path, err)
	}
	tmp := f.Name()
	if err = f.Chmod(0o600); err == nil { // CreateTemp's 0600 is subject to the umask
		if _, err = f.Write(data); err == nil {
			err = f.Sync()
		}
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

// checkRSA accepts an RSA private key in PEM, PKCS #1 (GitHub's) or PKCS #8.
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
