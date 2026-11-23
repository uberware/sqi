// SPDX-License-Identifier: AGPL-3.0-or-later

package isolation

import (
	"context"
	"errors"
	"fmt"
	"syscall"
)

// errLogonTypeNotGranted is Win32 ERROR_LOGON_TYPE_NOT_GRANTED (1385), the
// error LogonUserW returns when the account is real and the password correct
// but it lacks the logon right the requested logon type needs: for this
// provider's LOGON32_LOGON_BATCH, "Log on as a batch job"
// (SeBatchLogonRight). It is declared as a plain syscall.Errno rather than
// windows.ERROR_LOGON_TYPE_NOT_GRANTED so this file stays platform-neutral and
// unit-testable on every OS: the real Windows logon seam returns exactly this
// errno (procLogonUserW.Call yields a syscall.Errno), so errors.Is matches it
// through the seam's wrapping, and a test can reproduce it without Windows.
const errLogonTypeNotGranted = syscall.Errno(1385)

// logonFunc performs the OS-level logon for an account that has already been
// validated and confirmed non-privileged, using the secret already retrieved
// from the worker-local CredentialStore. The real implementation
// (provider_windows.go, built only under GOOS=windows) calls the Win32
// LogonUserW API; logonuser_other.go supplies a fail-closed placeholder for
// every other platform, where production never reaches a logonUserProvider:
// newProvider (provider_unix.go) returns the POSIX unixProvider there.
//
// The seam lets tests on every platform exercise Resolve's refusal,
// validation, secret-lookup and error-wrapping logic below by injecting their
// own logonFunc on a *logonUserProvider value, with no Windows logon call, as
// nss_unix.go's cmdRunner seam does for the POSIX provider's NSS fallback.
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
// error-wrapping logic is in this platform-neutral file so tests on every OS
// exercise it; only the logon, capable and traverse seams are
// platform-specific.
type logonUserProvider struct {
	store       CredentialStore
	logon       logonFunc
	capable     capableFunc
	canTraverse traverseFunc
}

// newLogonUserProvider returns the logon_user Windows Provider, wired to the
// real platform seams (logonUserOS, capableOS and canTraverseOS, one set per
// platform; see provider_windows.go, profile_windows.go and
// logonuser_other.go).
func newLogonUserProvider(store CredentialStore) Provider {
	return &logonUserProvider{
		store:       store,
		logon:       logonUserOS,
		capable:     capableOS,
		canTraverse: canTraverseOS,
	}
}

// Resolve validates spec, refuses a privileged account, retrieves its secret
// from the worker-local CredentialStore and, once all of that has passed,
// performs the OS-level logon. It must return an error rather than a nil
// credential with a nil error: a caller that proceeded would run job code as
// the worker while looking isolated. For the same reason a logon seam
// returning (nil, nil) is treated as an error here rather than passed
// through.
func (p *logonUserProvider) Resolve(ctx context.Context, spec Spec) (*Credential, error) {
	if err := validateAccountArg(spec.User, "user"); err != nil {
		return nil, err
	}
	// Windows has no uid; with the sentinel 1, CheckNotPrivileged's name
	// check is the only privileged-account check on this platform.
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

	// logonUserOS calls LogonUserW with a NULL domain, which asks Windows to
	// resolve the username against the LOCAL account database (see that
	// function's doc). A NULL domain does not itself strip a ".\"/"DOMAIN\"
	// qualifier or a trailing "@domain" UPN suffix from the username
	// string: LogonUserW would look up a literal local account named, for
	// example, ".\render-svc" and fail. normalizeAccountName is the
	// transform credFileName applies when it maps spec.User to a credential
	// file (see that function's doc), so applying it here keeps the account
	// the secret was stored for and the account logged on as the same local
	// account, whichever qualified or bare spelling a queue's run_as_user
	// used.
	loginUser := normalizeAccountName(spec.User)
	cred, err := p.logon(ctx, loginUser, secret)
	if err != nil {
		// The batch-logon-right failure is an operator misconfiguration with
		// a specific fix, not a bad password. Report it as the
		// SeChangeNotifyPrivilege refusal below does, naming the right and
		// how to grant it, rather than passing through the opaque
		// "the user has not been granted the requested logon type at this
		// computer." The raw error is still wrapped (%w), so errors.Is and an
		// operator reading the log both keep the underlying Win32 detail.
		if errors.Is(err, errLogonTypeNotGranted) {
			return nil, fmt.Errorf(
				"isolation: account %q has not been granted the \"Log on as a batch job\" right "+
					"(SeBatchLogonRight), which this provider's batch logon requires; grant it to the "+
					"account under Local Security Policy → Local Policies → User Rights "+
					"Assignment → \"Log on as a batch job\" (or, on an edition without secpol.msc, "+
					"via LsaAddAccountRights) and retry — see docs/worker-configuration.md: %w",
				spec.User, err,
			)
		}
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
