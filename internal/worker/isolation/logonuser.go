// SPDX-License-Identifier: AGPL-3.0-or-later

package isolation

import (
	"context"
	"fmt"
)

// logonFunc performs the OS-level logon for an account that has already been
// validated and confirmed non-privileged, using the secret already retrieved
// from the worker-local CredentialStore. The real implementation
// (provider_windows.go, built only under GOOS=windows) calls the Win32
// LogonUserW API; logonuser_other.go supplies a fail-closed placeholder for
// every other platform, since production never reaches a logonUserProvider
// there at all — this package's own newProvider (provider_unix.go) always
// returns the POSIX unixProvider on non-Windows instead.
//
// Splitting the seam out this way is what lets Resolve's refusal,
// validation, secret-lookup and error-wrapping logic below be exercised by
// tests on every platform: those tests inject their own logonFunc directly on
// a *logonUserProvider value, so no test needs a real Windows logon call —
// mirroring how nss_unix.go's cmdRunner seam lets the POSIX provider's
// fallback logic be tested without a real NSS-backed account.
type logonFunc func(ctx context.Context, user, secret string) (*Credential, error)

// capableFunc reports whether this worker holds the OS privilege(s) needed to
// assign a primary token to a child process. See logonFunc's doc for why the
// real-vs-placeholder split exists.
type capableFunc func() error

// traverseFunc reports whether cred's token carries SeChangeNotifyPrivilege
// ("Bypass traverse checking"). See logonuser_other.go / profile_windows.go
// for the real-vs-placeholder split, and Resolve below for why it matters.
type traverseFunc func(cred *Credential) (bool, error)

// logonUserProvider implements Provider via LOGON32_LOGON_BATCH-style
// credential switching. Its refusal, validation, secret-lookup and
// error-wrapping logic lives here — in a platform-neutral file — so it is
// exercised by tests on every OS; only the logon and capable seams below are
// platform-specific.
type logonUserProvider struct {
	store       CredentialStore
	logon       logonFunc
	capable     capableFunc
	canTraverse traverseFunc
}

// newLogonUserProvider returns the logon_user Windows Provider, wired to the
// real platform seams (logonUserOS, capableOS — one pair per platform, see
// provider_windows.go and logonuser_other.go).
func newLogonUserProvider(store CredentialStore) Provider {
	return &logonUserProvider{
		store:       store,
		logon:       logonUserOS,
		capable:     capableOS,
		canTraverse: canTraverseOS,
	}
}

// Resolve validates spec, refuses a privileged account, retrieves its secret
// from the worker-local CredentialStore, and — only once all of that has
// passed — performs the actual OS-level logon. It MUST return an error rather
// than a nil-credential-plus-nil-error: a caller that proceeded anyway would
// run job code as the daemon while looking isolated. That is also why a
// misbehaving logon seam returning (nil, nil) is treated as an error here
// rather than passed through — nothing downstream should ever have to
// distinguish "no credential, no error" from success.
func (p *logonUserProvider) Resolve(ctx context.Context, spec Spec) (*Credential, error) {
	if err := validateAccountArg(spec.User, "user"); err != nil {
		return nil, err
	}
	// Windows has no uid; passing the sentinel 1 means CheckNotPrivileged's
	// NAME check is the only backstop on this platform, so it runs here
	// rather than a second, Windows-specific privileged-name check.
	if err := CheckNotPrivileged(spec.User, 1); err != nil {
		return nil, err
	}

	secret, err := p.store.Secret(spec.User)
	if err != nil {
		return nil, fmt.Errorf("isolation: secret for %q: %w", spec.User, err)
	}
	if secret == "" {
		return nil, fmt.Errorf("isolation: empty secret for %q", spec.User)
	}

	cred, err := p.logon(ctx, spec.User, secret)
	if err != nil {
		return nil, fmt.Errorf("isolation: logon %q: %w", spec.User, err)
	}
	if cred == nil {
		return nil, fmt.Errorf("isolation: logon %q: logon seam returned no credential and no error", spec.User)
	}

	ok, err := p.canTraverse(cred)
	if err != nil {
		closeCred(cred)
		return nil, fmt.Errorf("isolation: check traverse privilege for %q: %w", spec.User, err)
	}
	if !ok {
		closeCred(cred)
		return nil, fmt.Errorf(
			"isolation: account %q does not hold SeChangeNotifyPrivilege (\"Bypass traverse checking\"); "+
				"an isolated task cannot reach its own session working directory without it, and sqi will "+
				"not widen the directories above it for you — grant the privilege to Everyone, or to this "+
				"account, in the local security policy",
			spec.User,
		)
	}
	return cred, nil
}

// Capable reports whether this worker can assume another identity via
// logon_user. Checked once at boot; fails closed.
func (p *logonUserProvider) Capable() error {
	return p.capable()
}

// closeCred releases a credential Resolve obtained but is not going to
// return. Resolve owns every credential it creates until it hands one back:
// a path that errors after a successful logon must not leak the token (and,
// on Windows, the loaded profile).
func closeCred(cred *Credential) {
	if cred == nil {
		return
	}
	_ = cred.Close() // best-effort release on an error path that is already returning a more useful error
}
