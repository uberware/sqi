// SPDX-License-Identifier: AGPL-3.0-or-later

package certgen

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/uberware/sqi/internal/fsutil"
)

// ErrCAExists is returned by WriteCA when dir already holds a CA key.
// Regenerating a farm CA in place invalidates every certificate issued from
// it, so the caller must move the old one aside deliberately.
var ErrCAExists = errors.New("certgen: CA already exists")

// certMode is the mode a CERTIFICATE carries. A certificate is public by
// design — it is handed to every peer during a handshake — so it is written
// with an ordinary mode and no ACL work.
//
// There is deliberately no keyMode constant any more. A private key goes
// through fsutil.WriteSecret, because a POSIX mode alone protected these keys
// on POSIX only: on Windows os.Chmod maps to the read-only ATTRIBUTE and
// cannot deny read access to anybody, so ca.key and every leaf key landed with
// whatever DACL they inherited. See that function's doc for the full account.
const certMode os.FileMode = 0o644

// writePair writes a certificate and its private key with the right modes.
func writePair(dir, base string, certPEM, keyPEM []byte) error {
	// The directory holds private keys, so it must not be world-readable —
	// and on Windows os.MkdirAll's mode argument is discarded outright, so
	// this cannot be a plain MkdirAll(dir, 0o750).
	if err := fsutil.MkdirSecret(dir); err != nil {
		return fmt.Errorf("certgen: create %s: %w", dir, err)
	}
	certPath := filepath.Join(dir, base+".crt")
	keyPath := filepath.Join(dir, base+".key")
	if err := os.WriteFile(certPath, certPEM, certMode); err != nil {
		return fmt.Errorf("certgen: write %s: %w", certPath, err)
	}
	if err := fsutil.WriteSecret(keyPath, keyPEM); err != nil {
		return fmt.Errorf("certgen: write %s: %w", keyPath, err)
	}
	return nil
}

// WriteCA writes ca.crt (world-readable) and ca.key (owner-only, enforced by
// fsutil.WriteSecret on both platforms) into dir. It refuses to
// overwrite an existing ca.key.
func WriteCA(dir string, ca *CA) error {
	keyPath := filepath.Join(dir, "ca.key")
	if _, err := os.Stat(keyPath); err == nil {
		return fmt.Errorf("%w at %s; to add a worker or rotate the server certificate use `sqi-server tls issue`, "+
			"which signs from this CA — replacing the CA itself means moving it aside first, and every "+
			"certificate ever issued from it stops verifying", ErrCAExists, keyPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("certgen: stat %s: %w", keyPath, err)
	}
	return writePair(dir, "ca", ca.CertPEM, ca.KeyPEM)
}

// WriteLeaf writes <name>.crt (world-readable) and <name>.key (owner-only,
// enforced by fsutil.WriteSecret on both platforms) into dir.
func WriteLeaf(dir, name string, leaf *Leaf) error {
	return writePair(dir, name, leaf.CertPEM, leaf.KeyPEM)
}
