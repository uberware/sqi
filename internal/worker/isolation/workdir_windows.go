// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package isolation

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// windowsIsolationUnsupportedMsg is the single operator-facing explanation
// for why Windows task isolation cannot be used yet, shared by every entry
// point that can observe the gap: Capable() (provider_windows.go, via
// capableOS) and the two functions below. Keeping one literal string means
// the boot-time refusal (verifyIsolationCapability, cmd/sqi-worker/isolation.go)
// and a per-assignment task failure (session.Manager.resolveCredential,
// internal/worker/session/session.go) always read the same way to an
// operator, rather than one clear message and one confusing one for what is,
// underneath, the same missing feature.
//
// What IS implemented on Windows (9bf054a): the logon_user credential
// provider (logonuser.go/provider_windows.go) really calls LogonUserW and
// returns a real, usable primary token — Resolve is not a stub. What is
// MISSING, and is the entire reason this message exists, is everything below
// that needs the token: securing a session working directory is POSIX
// chown/chmod on unix (workdir_unix.go) but requires NTFS ACL work on
// Windows — granting the target user's SID an ACE and stripping inheritance
// — which has not been written. So a Windows worker can obtain a credential
// but can never safely hand a task a working directory scoped to it, and
// Capable() must say so before an operator gets any further, rather than
// discovering it as an obscure failure the moment a real assignment arrives.
//
// Remaining work to make Windows isolation real: (1) NTFS ACL-based
// SecureWorkDir/ChownRecursive to replace the unconditional failures below;
// (2) the s4u provider (currently refused explicitly in provider_windows.go's
// newProvider); (3) a Windows CI runner (make test-isolation today only
// exercises POSIX against a real container) to verify any of it against a
// real account, the way the POSIX path already is.
const windowsIsolationUnsupportedMsg = "task isolation is not yet supported on Windows: session directory ACLs are not implemented"

// SecureWorkDir refuses every isolated request: cred is non-nil exactly when
// session.Manager.resolveCredential already obtained a real token via the
// logon_user provider's Resolve (see windowsIsolationUnsupportedMsg above for
// why Resolve succeeding is not the same as isolation being usable), so this
// is the per-assignment path that fails the one task, plainly, rather than
// producing an obscure error about a work directory.
func SecureWorkDir(_ string, cred *Credential) error {
	if cred == nil {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrNotCapable, windowsIsolationUnsupportedMsg)
}

// ChownRecursive refuses every isolated request for the same reason as
// SecureWorkDir above.
func ChownRecursive(_ string, cred *Credential) error {
	if cred == nil {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrNotCapable, windowsIsolationUnsupportedMsg)
}

// ValidateTraversable is a no-op on Windows: NTFS access control is
// ACL-based, not POSIX permission-bit-based, so there is no ancestor
// "traversable bit" to check the way there is on POSIX. Boot-time ancestor
// validation is gated on isolation.required (cmd/sqi-worker's
// validateIsolationAncestors), and a Windows worker that sets that flag is
// already refused earlier, by Capable() reporting
// windowsIsolationUnsupportedMsg (see provider_windows.go's capableOS) —
// this function existing as a no-op just lets the package compile on every
// platform ahead of a real NTFS ACL implementation; it is never reached on a
// path that matters.
func ValidateTraversable(_ ...string) error {
	return nil
}

// WriteFileFchown (re)writes path with data via remove-then-O_EXCL-create
// (refusing to write THROUGH a pre-existing entry — see the POSIX
// implementation's doc for the full threat model and why a plain O_EXCL-only
// create, this function's own previous shape, incorrectly also refused a
// legitimate duplicate embedded-file name instead of only an attacker-planted
// entry). cred is accepted for cross-platform call-site parity with the
// POSIX implementation but is unused: it is always nil in practice, though
// NOT because Resolve can't produce one — the logon_user provider's Resolve
// is real (see provider_windows.go) and can return a genuine token. Rather,
// every caller reaches this function only after
// session.Manager.resolveCredential has already secured the session working
// directory via SecureWorkDir, which (see that function's doc, above)
// unconditionally fails for a non-nil cred — so a session that carries an
// isolated credential never survives to reach this call at all; only the
// nil-cred, pre-isolation path ever does. There is never anything to chown
// here today, but for the reason above, not the one an earlier revision of
// this comment claimed.
//
// O_NOFOLLOW has no Windows equivalent flag: NTFS symlinks/junctions require
// an explicit reparse-point create call rather than being creatable via a
// plain "create file" open the way POSIX symlinks are, so O_EXCL alone
// already refuses to write through a pre-existing entry of any kind here.
func WriteFileFchown(path string, data []byte, perm os.FileMode, _ *Credential) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove existing %q: %w", path, err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return fmt.Errorf("create %q: %w", path, err)
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("write %q: %w", path, err)
	}
	return nil
}
