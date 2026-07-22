// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package isolation

import (
	"context"
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Credential holds a Windows primary token obtained via LogonUserW.
// os/exec's windows implementation calls CreateProcessAsUser internally
// whenever SysProcAttr.Token is non-zero (see apply_windows.go) — that field
// is the entire integration point; no other Win32 call is needed to make the
// child process actually run as cred's identity.
type Credential struct {
	token windows.Token
	// Home is the target user's profile directory (from
	// Token.GetUserProfileDirectory), or "" when it could not be determined.
	// Empty is tolerated: the session layer (internal/worker/session) falls
	// back to the session working directory for HOME/USERPROFILE in that case.
	Home string
}

// Close releases the token handle. Idempotent and safe to call more than
// once: the token field is zeroed immediately after the underlying handle is
// closed, so a second call takes the zero-token no-op branch instead of
// closing an already-released (and potentially since-reused) handle value a
// second time. The session lifecycle (internal/worker/session) calls Close on
// every path that obtains a credential — normal cleanup and every error
// return in Manager.Create — so this method must tolerate being invoked
// exactly once per credential, from exactly one of those paths, with no
// double-free regardless of which path that turns out to be.
func (c *Credential) Close() error {
	if c.token == 0 {
		return nil
	}
	err := c.token.Close()
	c.token = 0
	return err
}

// LOGON32_LOGON_BATCH and LOGON32_PROVIDER_DEFAULT (Win32 winbase.h). Batch
// logon is the right type for a service performing work on behalf of a user:
// unlike interactive logon it does not require the "log on locally" right,
// and unlike S4U (Task 10) it does grant network credentials, which render
// jobs commonly need for UNC/share access.
const (
	logon32LogonBatch      = 4
	logon32ProviderDefault = 0
)

// advapi32.dll!LogonUserW has no wrapper in golang.org/x/sys/windows — checked
// against the version this module depends on (v0.47.0): neither the function
// nor the LOGON32_* constants are exposed there, unlike most other Win32
// security APIs that package does wrap. It is declared directly here instead,
// the same way golang.org/x/sys/windows's own generated zsyscall_windows.go
// declares every Win32 call it does wrap.
var (
	modAdvapi32    = windows.NewLazySystemDLL("advapi32.dll")
	procLogonUserW = modAdvapi32.NewProc("LogonUserW")
)

// logonUserW wraps the raw LogonUserW syscall. domain may be nil, which asks
// Windows to resolve username against the local account database — the right
// behavior for the local accounts this provider targets (normalizeAccountName
// already strips any ".\"/"DOMAIN\" qualifier before the privileged-name
// check runs, so username here is always the bare account name).
func logonUserW(username, domain, password *uint16, logonType, logonProvider uint32) (windows.Token, error) {
	var token windows.Token
	r1, _, e1 := procLogonUserW.Call(
		uintptr(unsafe.Pointer(username)),
		uintptr(unsafe.Pointer(domain)),
		uintptr(unsafe.Pointer(password)),
		uintptr(logonType),
		uintptr(logonProvider),
		uintptr(unsafe.Pointer(&token)),
	)
	if r1 == 0 {
		if e1 != nil {
			return 0, e1
		}
		return 0, errors.New("LogonUserW: failed with no error code")
	}
	return token, nil
}

// logonUserOS is the real logon seam wired into newLogonUserProvider on
// Windows. Called only after logonUserProvider.Resolve has already validated
// spec.User and refused a privileged account, so this function's only job is
// the OS call itself and translating its result into a Credential.
func logonUserOS(_ context.Context, user, secret string) (*Credential, error) {
	userPtr, err := windows.UTF16PtrFromString(user)
	if err != nil {
		return nil, fmt.Errorf("encode username: %w", err)
	}
	passPtr, err := windows.UTF16PtrFromString(secret)
	if err != nil {
		return nil, fmt.Errorf("encode secret: %w", err)
	}

	tok, err := logonUserW(userPtr, nil, passPtr, logon32LogonBatch, logon32ProviderDefault)
	if err != nil {
		return nil, fmt.Errorf("LogonUserW: %w", err)
	}

	home, err := tok.GetUserProfileDirectory()
	if err != nil {
		// Best-effort: an account with no discoverable profile directory
		// yet (e.g. never logged on interactively) is tolerated — the
		// session layer falls back to its own working directory.
		home = ""
	}
	return &Credential{token: tok, Home: home}, nil
}

// requiredPrivileges are the privileges CreateProcessAsUser needs present on
// the calling process's token: SeAssignPrimaryTokenPrivilege to attach a
// primary token to a new process, and SeIncreaseQuotaPrivilege, which
// CreateProcessAsUser also requires in order to set the new process's quota
// limits under the target account's job/session.
//
// hasRequiredPrivileges (and therefore this check) is real, working code —
// not a stub — but it is no longer capableOS's own gate below: holding every
// privilege here is necessary for CreateProcessAsUser but not sufficient for
// isolation as a whole, since securing a session working directory for the
// target identity needs NTFS ACL work (see windowsIsolationUnsupportedMsg,
// workdir_windows.go) that does not exist yet. It stays wired into capableOS
// so it is exercised on every real Windows run (surfacing a missing
// privilege as extra detail) rather than sitting dead until a future
// revision reintroduces it, once ACL support lands and privilege really is
// the remaining gate.
var requiredPrivileges = []string{"SeAssignPrimaryTokenPrivilege", "SeIncreaseQuotaPrivilege"}

// hasRequiredPrivileges reports whether this worker's process token holds
// every privilege in requiredPrivileges. Fails closed: any lookup error, or
// any missing privilege, is reported as ErrNotCapable rather than silently
// proceeding as if capable.
func hasRequiredPrivileges() error {
	tok := windows.GetCurrentProcessToken()
	for _, name := range requiredPrivileges {
		held, err := tokenHasPrivilege(tok, name)
		if err != nil {
			return fmt.Errorf("%w: checking %s: %w", ErrNotCapable, name, err)
		}
		if !held {
			return fmt.Errorf("%w: %s not held", ErrNotCapable, name)
		}
	}
	return nil
}

// capableOS is the seam wired into newLogonUserProvider's Capable() on
// Windows (see logonuser.go). It ALWAYS reports not-capable: even a process
// token holding every privilege hasRequiredPrivileges checks for cannot make
// isolation usable while SecureWorkDir/ChownRecursive (workdir_windows.go)
// have no NTFS ACL implementation. Reporting anything else here would let a
// Windows worker with isolation.required: true start successfully
// (verifyIsolationCapability, cmd/sqi-worker/isolation.go) and only fail,
// confusingly, the moment a real isolated assignment tried to secure its
// working directory — the exact half-working state this function exists to
// prevent. hasRequiredPrivileges still runs (see its own doc) so a missing
// privilege is named alongside the ACL gap when relevant, but its result
// never changes the bottom line: not capable, in the ErrNotCapable family,
// with the shared operator-facing message every entry point uses.
func capableOS() error {
	if err := hasRequiredPrivileges(); err != nil {
		return fmt.Errorf("%w: %s (additionally, %v)", ErrNotCapable, windowsIsolationUnsupportedMsg, err)
	}
	return fmt.Errorf("%w: %s", ErrNotCapable, windowsIsolationUnsupportedMsg)
}

// tokenHasPrivilege reports whether t's privilege set includes name,
// regardless of whether that privilege is currently enabled —
// CreateProcessAsUser enables the privilege it needs internally, so what
// matters here is only that the token carries it at all. This mirrors how
// the POSIX provider's Capable() checks identity (euid==0) rather than some
// finer-grained enabled/disabled state.
func tokenHasPrivilege(t windows.Token, name string) (bool, error) {
	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return false, fmt.Errorf("encode privilege name: %w", err)
	}
	var luid windows.LUID
	if err := windows.LookupPrivilegeValue(nil, namePtr, &luid); err != nil {
		return false, fmt.Errorf("LookupPrivilegeValue(%s): %w", name, err)
	}

	privs, err := tokenPrivileges(t)
	if err != nil {
		return false, fmt.Errorf("query token privileges: %w", err)
	}
	for _, p := range privs.AllPrivileges() {
		if p.Luid == luid {
			return true, nil
		}
	}
	return false, nil
}

// tokenPrivileges retrieves t's full privilege set via GetTokenInformation,
// growing the buffer if the initial guess is too small — mirroring the same
// growth loop golang.org/x/sys/windows uses internally for this call
// (Token.getInfo), which is unexported and so cannot be called directly from
// this package.
func tokenPrivileges(t windows.Token) (*windows.Tokenprivileges, error) {
	n := uint32(1024)
	for {
		buf := make([]byte, n)
		err := windows.GetTokenInformation(t, windows.TokenPrivileges, &buf[0], uint32(len(buf)), &n)
		if err == nil {
			return (*windows.Tokenprivileges)(unsafe.Pointer(&buf[0])), nil
		}
		if !errors.Is(err, windows.ERROR_INSUFFICIENT_BUFFER) {
			return nil, err
		}
		if n <= uint32(len(buf)) {
			return nil, err
		}
	}
}

// newProvider returns the Windows Provider selected by cfg.Provider:
// "logon_user" (the default, matching workerconfig's own default) or "s4u".
// S4U is Task 10's work and not yet implemented, so it is refused explicitly
// here rather than silently aliased to logon_user — that would run tasks
// under a different credential-acquisition mechanism than the operator
// configured without telling anyone.
func newProvider(cfg Config) (Provider, error) {
	switch cfg.Provider {
	case "", "logon_user":
		return newLogonUserProvider(cfg.CredentialStore), nil
	case "s4u":
		return nil, fmt.Errorf("%w: s4u provider not yet implemented", ErrNotCapable)
	default:
		return nil, fmt.Errorf("isolation: unknown windows isolation provider %q", cfg.Provider)
	}
}

// newFakeCredential builds a Credential from a fake account for tests. It
// keeps Credential opaque outside this package while letting fake.go (which
// is platform-independent) hand back a real *Credential. The fake carries no
// real token — NewFake's own doc already states a fake cannot verify real OS
// identity switching; that needs make test-isolation (POSIX today) or a real
// Windows run.
func newFakeCredential(a FakeAccount) *Credential {
	return &Credential{Home: a.Home}
}
