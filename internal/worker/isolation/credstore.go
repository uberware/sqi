// SPDX-License-Identifier: AGPL-3.0-or-later

package isolation

import (
	"encoding/hex"
	"path/filepath"
)

// credFileName maps an account name to the file its secret is stored in.
//
// The username is NEVER joined into a path directly. validateAccountArg
// (validate.go) permits "\", "/" and ".." — they are legal in the DOMAIN\user
// forms normalizeAccountName handles — so a queue configured with
// `..\..\..\Windows\Temp\x` would otherwise read and write outside the worker
// data dir. Hex encoding produces a name that cannot contain a path
// separator, a drive letter, or a dot segment, whatever the input.
//
// Encoding the NORMALIZED name (not the raw one) is what makes provisioning
// and lookup agree: an operator running set-credential for `CORP\render-svc`
// and a queue configured with plain `render-svc` must resolve to one file, or
// provisioning silently succeeds and every assignment fails with an empty
// secret.
func credFileName(user string) string {
	return hex.EncodeToString([]byte(normalizeAccountName(user))) + ".cred"
}

// CredentialDir returns the directory holding stored run-as-user secrets for
// a worker whose data directory is dataDir. One helper, used by both the
// set-credential subcommand and the worker's own provider construction, so
// the two can never disagree about where the file lives.
func CredentialDir(dataDir string) string {
	return filepath.Join(dataDir, "isolation")
}
