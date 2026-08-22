// SPDX-License-Identifier: AGPL-3.0-or-later

package certgen

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrCAExists is returned by WriteCA when dir already holds a CA key.
// Regenerating a farm CA in place invalidates every certificate issued from
// it, so the caller must move the old one aside deliberately.
var ErrCAExists = errors.New("certgen: CA already exists")

const (
	certMode os.FileMode = 0o644
	keyMode  os.FileMode = 0o600
)

// writePair writes a certificate and its private key with the right modes.
func writePair(dir, base string, certPEM, keyPEM []byte) error {
	// 0750: the directory holds private keys, so it must not be world-readable.
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("certgen: create %s: %w", dir, err)
	}
	certPath := filepath.Join(dir, base+".crt")
	keyPath := filepath.Join(dir, base+".key")
	if err := os.WriteFile(certPath, certPEM, certMode); err != nil {
		return fmt.Errorf("certgen: write %s: %w", certPath, err)
	}
	if err := os.WriteFile(keyPath, keyPEM, keyMode); err != nil {
		return fmt.Errorf("certgen: write %s: %w", keyPath, err)
	}
	return nil
}

// WriteCA writes ca.crt (0644) and ca.key (0600) into dir. It refuses to
// overwrite an existing ca.key.
func WriteCA(dir string, ca *CA) error {
	keyPath := filepath.Join(dir, "ca.key")
	if _, err := os.Stat(keyPath); err == nil {
		return fmt.Errorf("%w at %s; move it aside to generate a new one (every certificate issued from it stops verifying)", ErrCAExists, keyPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("certgen: stat %s: %w", keyPath, err)
	}
	return writePair(dir, "ca", ca.CertPEM, ca.KeyPEM)
}

// WriteLeaf writes <name>.crt (0644) and <name>.key (0600) into dir.
func WriteLeaf(dir, name string, leaf *Leaf) error {
	return writePair(dir, name, leaf.CertPEM, leaf.KeyPEM)
}
