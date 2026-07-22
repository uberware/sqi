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
type Credential struct {
	// Home mirrors the POSIX Credential's field so cross-platform callers
	// (internal/worker/session) can rewrite HOME/USERPROFILE without a
	// build-tagged branch. Always empty in practice: Resolve below never
	// returns a non-nil Credential.
	Home string
}

// Close releases the credential. The placeholder holds no OS handle.
func (*Credential) Close() error { return nil }

// windowsProvider carries the configured credential mechanism (Config.Provider,
// "logon_user" or "s4u") purely so error messages name it accurately. Neither
// mechanism is implemented yet — every request is refused regardless of which
// was selected — but wiring the value through (rather than discarding
// Config.Provider entirely) means the eventual LogonUser/S4U implementation
// has a place to dispatch on, and an operator's misconfigured value is
// visible in the error rather than silently ignored.
type windowsProvider struct {
	mechanism string
}

// newProvider returns the Windows provider. Until a later task implements
// LogonUser/S4U-based switching, it refuses every request rather than
// silently running unisolated.
func newProvider(cfg Config) (Provider, error) {
	return windowsProvider{mechanism: cfg.Provider}, nil
}

// label returns the configured mechanism name, defaulting to "logon_user"
// (workerconfig's own default) when unset so an unconfigured worker's error
// message still names a concrete mechanism.
func (p windowsProvider) label() string {
	if p.mechanism == "" {
		return "logon_user"
	}
	return p.mechanism
}

func (p windowsProvider) Resolve(_ context.Context, spec Spec) (*Credential, error) {
	return nil, fmt.Errorf("isolation: windows run-as-user (%s) is not yet implemented (user %q): %w", p.label(), spec.User, ErrNotCapable)
}

func (p windowsProvider) Capable() error {
	return fmt.Errorf("%w: windows run-as-user (%s) is not yet implemented", ErrNotCapable, p.label())
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
