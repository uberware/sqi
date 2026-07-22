// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package isolation

import (
	"context"
	"fmt"
	"os/exec"
)

// Credential is an opaque placeholder for the Windows identity handle.
// The real LogonUser-based provider lands in a later task; until then this
// package still must build on GOOS=windows since sqi ships a Windows worker
// binary, and windowsProvider below refuses every request rather than
// silently proceeding without isolation.
type Credential struct{}

// Close releases the credential. The placeholder holds no OS handle.
func (*Credential) Close() error { return nil }

type windowsProvider struct{}

// newProvider returns the Windows provider. Until a later task implements
// LogonUser-based switching, it refuses every request rather than silently
// running unisolated.
func newProvider(_ Config) (Provider, error) {
	return windowsProvider{}, nil
}

func (windowsProvider) Resolve(_ context.Context, spec Spec) (*Credential, error) {
	return nil, fmt.Errorf("isolation: windows run-as-user is not yet implemented (user %q): %w", spec.User, ErrNotCapable)
}

func (windowsProvider) Capable() error {
	return fmt.Errorf("%w: windows run-as-user is not yet implemented", ErrNotCapable)
}

// newFakeCredential builds a Credential from a fake account for tests. It
// keeps Credential opaque outside this package while letting fake.go (which is
// platform-independent) hand back a real *Credential.
func newFakeCredential(_ FakeAccount) *Credential {
	return &Credential{}
}

// apply is unreachable in practice: Resolve above never returns a non-nil
// Credential, so Apply's nil-check always takes the no-op path first. It
// exists so the package compiles on Windows ahead of the real provider.
func apply(_ *exec.Cmd, _ *Credential) error {
	return fmt.Errorf("%w: windows run-as-user is not yet implemented", ErrNotCapable)
}
