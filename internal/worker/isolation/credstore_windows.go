// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package isolation

import (
	"fmt"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

// fileStore keeps one DPAPI-encrypted secret per account under dir.
//
// CRYPTPROTECT_LOCAL_MACHINE is what lets an elevated Administrator provision
// a blob that the LocalSystem worker can later read — user-scope DPAPI would
// bind the blob to whoever ran set-credential and be undecryptable by the
// service.
//
// The tradeoff is stated plainly in docs/worker-configuration.md and must not
// be softened here: machine scope means ANYTHING running on this host can
// decrypt the blob if it can READ THE FILE. The file ACL is the actual
// security boundary, which is why writeSecured below applies an explicit
// protected DACL rather than relying on whatever the data directory happens
// to inherit, and why the integration suite asserts that a run-as account
// cannot open it.
type fileStore struct{ dir string }

// NewFileStore returns a DPAPI-backed CredentialStore rooted at dir.
func NewFileStore(dir string) CredentialStore { return &fileStore{dir: dir} }

// path returns the on-disk location of user's secret. See credFileName for
// why the username is never joined in directly.
func (s *fileStore) path(user string) string {
	return filepath.Join(s.dir, credFileName(user))
}

// Secret decrypts user's stored secret. A missing file is reported as such
// rather than as an empty secret, so an operator who never ran
// set-credential gets an actionable error instead of "empty secret for ...".
func (s *fileStore) Secret(user string) (string, error) {
	blob, err := os.ReadFile(s.path(user))
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("isolation: no stored credential for %q; run: sqi-worker isolation set-credential %s", user, user)
		}
		return "", fmt.Errorf("isolation: read credential for %q: %w", user, err)
	}
	plain, err := dpapiUnprotect(blob)
	if err != nil {
		return "", fmt.Errorf("isolation: decrypt credential for %q: %w", user, err)
	}
	return string(plain), nil
}

// Put encrypts secret and writes it, creating dir if needed. Used only by the
// set-credential subcommand.
func (s *fileStore) Put(user, secret string) error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("create %q: %w", s.dir, err)
	}
	// Secure the directory itself before anything is ever written into it —
	// see secureDir's doc for why this cannot be skipped even though
	// MkdirAll just ran.
	if err := secureDir(s.dir); err != nil {
		return err
	}
	blob, err := dpapiProtect([]byte(secret))
	if err != nil {
		return fmt.Errorf("encrypt credential for %q: %w", user, err)
	}
	return writeSecured(s.path(user), blob)
}

// secureDir replaces dir's DACL with adminOnlyDACL (acl_windows.go: SYSTEM
// and Administrators only — deliberately nothing else, see that function's
// doc for why even a CREATOR OWNER placeholder is not safe here) and marks
// it PROTECTED, stripping every inherited ACE — typically BUILTIN\Users,
// granted by a ProgramData-rooted install's parent tree. Called
// unconditionally by Put, whether dir is newly created or already existed
// from a previous run: MkdirAll's 0o700 mode argument is inert on NTFS (Go
// maps it only to the read-only attribute, never to an ACL), so it grants
// no protection on its own, and a directory that predates this fix — or one
// an administrator created by hand — is not otherwise guaranteed to carry
// the right ACL.
//
// This is also what closes writeSecured's file-level window, not the
// MkdirAll mode: adminOnlyDACL's entries carry
// SUB_CONTAINERS_AND_OBJECTS_INHERIT, so once the directory is secured, any
// file CreateFile/os.WriteFile subsequently creates inside it inherits a
// locked-down DACL from the instant it is created, before writeSecured ever
// gets to apply its own explicit DACL to the file. Because only SYSTEM and
// Administrators are ever granted here, only a genuinely elevated caller —
// never merely whoever happens to own the directory — can create that first
// file at all; see TestIsolationWindows_CredentialDirectoryExcludesUnprivilegedTrustees
// (test/integration/isolation_windows_test.go) for the elevated-tier proof.
func secureDir(dir string) error {
	acl, err := adminOnlyDACL()
	if err != nil {
		return err
	}
	h, err := openForACL(dir)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h) //nolint:errcheck // best-effort close of a handle we are done with
	return applyProtectedDACL(h, acl)
}

// writeSecured writes data and then locks the file down to SYSTEM and
// Administrators ONLY with a protected DACL — see
// adminOnlyDACL/applyProtectedDACL in acl_windows.go. By the time this runs,
// Put has already secured the directory (secureDir) with that same
// SYSTEM+Administrators-only DACL, so the file is born already inheriting
// it — which is exactly what makes os.WriteFile itself require a genuinely
// elevated caller: an unprivileged process cannot even create the file in
// the first place, let alone reach this function. This function's own
// applyProtectedDACL call reasserts adminOnlyDACL explicitly rather than
// relying on inheritance, so the file's final, persisted DACL is exactly
// SYSTEM+Administrators regardless of who created it.
func writeSecured(path string, data []byte) error {
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write %q: %w", path, err)
	}
	acl, err := adminOnlyDACL()
	if err != nil {
		return err
	}
	h, err := openForACL(path)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h) //nolint:errcheck // best-effort close of a handle we are done with
	return applyProtectedDACL(h, acl)
}

// blobDataPointer returns a pointer CryptProtectData/CryptUnprotectData will
// accept for x, including when x is empty. Both APIs reject a NULL pbData
// with ERROR_INVALID_PARAMETER even when cbData is 0 — verified against the
// real API, not assumed — so an empty slice's natural nil base pointer is
// not good enough on its own; a pointer to a throwaway zero byte is. This is
// what len(x) > 0 guards below actually avoid: without it, `&x[0]` on a
// nil/empty slice panics before either Crypt*Data call is ever reached.
func blobDataPointer(x []byte) *byte {
	if len(x) > 0 {
		return &x[0]
	}
	var zero byte
	return &zero
}

// dpapiProtect encrypts plain with the machine key.
func dpapiProtect(plain []byte) ([]byte, error) {
	var in, out windows.DataBlob
	in.Size = uint32(len(plain)) //nolint:gosec // G115: plain is a single stored credential, never remotely near uint32 range
	in.Data = blobDataPointer(plain)
	if err := windows.CryptProtectData(&in, nil, nil, 0, nil, windows.CRYPTPROTECT_LOCAL_MACHINE, &out); err != nil {
		return nil, err
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data))) //nolint:errcheck // freeing the DPAPI output buffer; nothing actionable on failure
	return append([]byte(nil), unsafe.Slice(out.Data, out.Size)...), nil
}

// dpapiUnprotect decrypts a blob produced by dpapiProtect. An empty blob is
// never produced by dpapiProtect (even protecting an empty secret yields a
// real, non-empty DPAPI envelope), so this is reached only for a corrupted
// or truncated on-disk file; CryptUnprotectData correctly rejects it as
// invalid rather than returning an empty secret.
func dpapiUnprotect(blob []byte) ([]byte, error) {
	var in, out windows.DataBlob
	in.Size = uint32(len(blob)) //nolint:gosec // G115: blob is a single stored credential file read from disk, never remotely near uint32 range
	in.Data = blobDataPointer(blob)
	if err := windows.CryptUnprotectData(&in, nil, nil, 0, nil, windows.CRYPTPROTECT_LOCAL_MACHINE, &out); err != nil {
		return nil, err
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data))) //nolint:errcheck // freeing the DPAPI output buffer; nothing actionable on failure
	return append([]byte(nil), unsafe.Slice(out.Data, out.Size)...), nil
}
