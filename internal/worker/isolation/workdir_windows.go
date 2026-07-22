// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package isolation

import "fmt"

// SecureWorkDir is unreachable in practice: the Windows provider's Resolve
// never returns a non-nil Credential (see provider_windows.go), so callers
// only ever invoke this with a nil cred, which is a no-op. It exists so the
// package compiles on Windows ahead of the real LogonUser-based provider.
func SecureWorkDir(_ string, cred *Credential) error {
	if cred == nil {
		return nil
	}
	return fmt.Errorf("%w: windows run-as-user is not yet implemented", ErrNotCapable)
}

// ChownRecursive is unreachable in practice for the same reason as
// SecureWorkDir above.
func ChownRecursive(_ string, cred *Credential) error {
	if cred == nil {
		return nil
	}
	return fmt.Errorf("%w: windows run-as-user is not yet implemented", ErrNotCapable)
}
