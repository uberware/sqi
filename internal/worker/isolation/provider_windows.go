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
//
// hasRequiredPrivileges (and therefore this check) is real, working code —
// not a stub — but it is no longer capableOS's own gate below: holding every
// privilege here is necessary for CreateProcessAsUser but not sufficient for
// isolation as a whole. Session working directories now get real NTFS ACLs
// (workdir_windows.go), but the rest of what isolation.go's package doc
// still lists as outstanding — loading the target user's profile,
// job-object-based process reaping, and end-to-end verification against a
// real Windows host — has not landed yet. It stays wired into capableOS so
// it is exercised on every real Windows run (surfacing a missing privilege
// as extra detail) rather than sitting dead until a future revision
// reintroduces it, once the remaining gaps close and privilege really is the
// last gate.
var requiredPrivileges = []string{"SeAssignPrimaryTokenPrivilege", "SeIncreaseQuotaPrivilege"}

// windowsIsolationUnsupportedMsg is the single operator-facing explanation
// for why capableOS still refuses every request, shared by every entry point
// that can observe the gap through Capable(): the boot-time refusal
// (verifyIsolationCapability, cmd/sqi-worker/isolation.go) and a
// per-assignment task failure (session.Manager.resolveCredential,
// internal/worker/session/session.go) always read the same way to an
// operator, rather than one clear message and one confusing one for what is,
// underneath, the same set of missing pieces.
//
// This used to also be workdir_windows.go's own refusal message for
// SecureWorkDir/ChownRecursive when called with a non-nil credential — that
// changed the moment this package started applying real NTFS ACLs there (see
// secureDACL, acl_windows.go). What is still missing, and is why capableOS
// below has not been flipped to report success, is everything isolation.go's
// package doc still lists as outstanding: loading the target user's profile,
// job-object-based process reaping, and verifying the whole path end to end
// on a real Windows host. Reporting "capable" before that lands would let a
// Windows worker with isolation.required: true start successfully and only
// fail, confusingly, partway through a real assignment — exactly the
// half-working state this message exists to prevent.
const windowsIsolationUnsupportedMsg = "task isolation is not yet fully supported on Windows: session directory ACLs are implemented, but profile loading, process reaping, and end-to-end verification are not"

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
// isolation usable while the remaining gaps requiredPrivileges' own doc names
// — profile loading, job-object process reaping, end-to-end verification —
// have not landed (session working directories themselves are now real NTFS
// ACLs; see secureDACL, acl_windows.go). Reporting anything else here would
// let a Windows worker with isolation.required: true start successfully
// (verifyIsolationCapability, cmd/sqi-worker/isolation.go) and only fail,
// confusingly, partway through a real isolated assignment — the exact
// half-working state this function exists to prevent. hasRequiredPrivileges
// still runs (see its own doc) so a missing privilege is named alongside the
// remaining gaps when relevant, but its result never changes the bottom
// line: not capable, in the ErrNotCapable family, with the shared
// operator-facing message every entry point uses.
func capableOS() error {
	if err := hasRequiredPrivileges(); err != nil {
		// Both ErrNotCapable and err are wrapped (Go allows multiple %w verbs
		// in one fmt.Errorf) so errors.Is(result, ErrNotCapable) keeps working
		// while the underlying privilege-check failure stays inspectable too.
		return fmt.Errorf("%w: %s (additionally, %w)", ErrNotCapable, windowsIsolationUnsupportedMsg, err)
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
