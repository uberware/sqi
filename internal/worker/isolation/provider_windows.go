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
	// User is the account name this credential was resolved for, kept so the
	// workdir ACL functions can resolve its SID without a second lookup path
	// that could disagree with the one Resolve used.
	User string
	// Home is the target user's profile directory (from
	// Token.GetUserProfileDirectory), or "" when it could not be determined.
	// Empty is tolerated: the session layer (internal/worker/session) falls
	// back to the session working directory for HOME/USERPROFILE in that case.
	Home string
	// profile is the handle returned by LoadUserProfileW, unloaded by Close
	// before the token is released. Zero when no profile was loaded.
	profile windows.Handle
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
	// Unload the hive BEFORE closing the token: UnloadUserProfileW needs the
	// same token that loaded it, so the reverse order would leak the mounted
	// hive for the lifetime of the worker.
	profErr := unloadProfile(c.token, c.profile)
	c.profile = 0
	err := c.token.Close()
	c.token = 0
	if profErr != nil {
		return profErr
	}
	return err
}

// LOGON32_LOGON_BATCH and LOGON32_PROVIDER_DEFAULT (Win32 winbase.h). Batch
// logon is the right type for a service performing work on behalf of a
// user: unlike interactive logon it does not require the "log on locally"
// right, and unlike an S4U logon it does grant network credentials, which
// render jobs commonly need for UNC/share access. (S4U is not implemented —
// see newProvider.)
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

	prof, err := loadProfile(tok, user)
	if err != nil {
		tok.Close() // already returning a more useful error
		return nil, err
	}

	home, err := tok.GetUserProfileDirectory()
	if err != nil {
		// Tolerated: the session layer falls back to its own working
		// directory. Far less likely now that loadProfile has run, which
		// creates the profile directory if it did not exist.
		home = ""
	}
	return &Credential{token: tok, profile: prof, User: user, Home: home}, nil
}

// requiredPrivileges are the privileges CreateProcessAsUser needs present on
// the calling process's token: SeAssignPrimaryTokenPrivilege to attach a
// primary token to a new process, and SeIncreaseQuotaPrivilege, which
// CreateProcessAsUser also requires in order to set the new process's quota
// limits under the target account's job/session.
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

// capableOS reports whether this worker can attach a primary token to a child
// process, which is what SysProcAttr.Token ultimately requires.
//
// The message names LocalSystem because the natural first attempt is an
// elevated Administrator, and that does NOT work: SeAssignPrimaryTokenPrivilege
// is held by default only by LocalSystem and the LOCAL/NETWORK SERVICE
// accounts. Without saying so, an operator sees a refusal from a shell they
// already ran "as administrator" and reasonably concludes sqi is broken.
func capableOS() error {
	if err := hasRequiredPrivileges(); err != nil {
		return fmt.Errorf(
			"%w: run the worker as a service under LocalSystem, or grant its account "+
				"SeAssignPrimaryTokenPrivilege — an elevated Administrator does not hold it by default (%v)",
			ErrNotCapable, err,
		)
	}
	return nil
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
		// buf was allocated with make([]byte, n) immediately above, so
		// len(buf) == int(n) exactly; converting it back to uint32 can never
		// overflow — it is the same value n already held as a uint32.
		bufLen := uint32(len(buf)) //nolint:gosec // len(buf) == n (a uint32) by construction; cannot overflow
		err := windows.GetTokenInformation(t, windows.TokenPrivileges, &buf[0], bufLen, &n)
		if err == nil {
			return (*windows.Tokenprivileges)(unsafe.Pointer(&buf[0])), nil
		}
		if !errors.Is(err, windows.ERROR_INSUFFICIENT_BUFFER) {
			return nil, err
		}
		if n <= bufLen {
			return nil, err
		}
	}
}

// newProvider returns the Windows Provider selected by cfg.Provider:
// "logon_user" (the default, matching workerconfig's own default). "s4u" is
// refused explicitly rather than silently aliased to logon_user — that would
// run tasks under a different credential-acquisition mechanism than the
// operator configured without telling anyone. See
// workerconfig.IsolationConfig.Provider for why s4u is not implemented.
func newProvider(cfg Config) (Provider, error) {
	switch cfg.Provider {
	case "", "logon_user":
		return newLogonUserProvider(cfg.CredentialStore), nil
	case "s4u":
		return nil, fmt.Errorf("%w: the s4u provider is not implemented; its logon carries no network credentials, so UNC and share access fail as ANONYMOUS inside the task — use logon_user", ErrNotCapable)
	default:
		return nil, fmt.Errorf("isolation: unknown windows isolation provider %q", cfg.Provider)
	}
}

// newFakeCredential builds a Credential from a fake account for tests. It
// keeps Credential opaque outside this package while letting fake.go (which
// is platform-independent) hand back a real *Credential. The fake carries no
// real token — NewFake's own doc already states a fake cannot verify real OS
// identity switching; that needs make test-isolation-windows or a real
// Windows run.
//
// User is set to name (the account-map key a real Resolve would have
// received as spec.User), NOT left empty: SecureWorkDir/ChownRecursive
// resolve a Credential's identity by NAME via lookupUserSID, so a fake
// credential with no name cannot exercise those real, production ACL calls
// at all — the fake would look isolated while SecureWorkDir failed on every
// use. Callers building fake accounts for a test that reaches SecureWorkDir
// must key the account (and the Isolation.User the assignment carries) with
// a name that actually resolves via LookupSID on the machine the test runs
// on — typically the test process's own account, the same self-service
// trick testUID/testGID play for uid/gid on POSIX.
func newFakeCredential(name string, a FakeAccount) *Credential {
	return &Credential{User: name, Home: a.Home}
}
