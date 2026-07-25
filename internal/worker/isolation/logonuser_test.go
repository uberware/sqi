// SPDX-License-Identifier: AGPL-3.0-or-later

package isolation

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"syscall"
	"testing"
)

// fakeStore is an in-memory CredentialStore for tests: a lookup miss reports
// an error (mirroring a real Windows Credential Manager entry that was never
// provisioned), rather than returning ("", nil), so Resolve's "empty secret"
// and "store error" paths stay distinguishable in tests.
type fakeStore map[string]string

func (f fakeStore) Secret(user string) (string, error) {
	s, ok := f[user]
	if !ok {
		return "", fmt.Errorf("fakeStore: no secret stored for %q", user)
	}
	return s, nil
}

// TestLogonUserRefusesPrivilegedAccounts is the brief's own reproduction: a
// privileged account name must be refused before any secret lookup or logon
// call, using the shared CheckNotPrivileged backstop rather than a
// second, Windows-specific check.
func TestLogonUserRefusesPrivilegedAccounts(t *testing.T) {
	p := newLogonUserProvider(fakeStore{"Administrator": "pw"})

	if _, err := p.Resolve(context.Background(), Spec{User: "Administrator"}); !errors.Is(err, ErrRefusedPrivileged) {
		t.Errorf("err = %v, want ErrRefusedPrivileged", err)
	}
}

// TestLogonUserFailsWithoutStoredSecret proves Resolve never falls back to
// the daemon identity when the CredentialStore has nothing for the
// requested user: it must return an error, not a nil-credential-plus-nil-error.
func TestLogonUserFailsWithoutStoredSecret(t *testing.T) {
	p := newLogonUserProvider(fakeStore{})

	if _, err := p.Resolve(context.Background(), Spec{User: "render-svc"}); err == nil {
		t.Error("missing credential must fail, never fall back to the daemon identity")
	}
}

// TestLogonUserResolve_EmptySecretRefused covers a store that returns success
// with an empty string — distinct from an outright lookup error — which must
// still be refused rather than attempting a logon with a blank password.
func TestLogonUserResolve_EmptySecretRefused(t *testing.T) {
	p := newLogonUserProvider(fakeStore{"render-svc": ""})

	if _, err := p.Resolve(context.Background(), Spec{User: "render-svc"}); err == nil {
		t.Error("an empty stored secret must be refused, not passed through to logon")
	}
}

// TestLogonUserResolve_StoreErrorWrapped checks that a CredentialStore
// failure is wrapped with enough context (the account name) for an operator
// to act on, and that the underlying error is preserved for errors.Is/As.
func TestLogonUserResolve_StoreErrorWrapped(t *testing.T) {
	sentinel := errors.New("credential manager unavailable")
	p := &logonUserProvider{
		store: storeFunc(func(string) (string, error) { return "", sentinel }),
		logon: func(context.Context, string, string) (*Credential, error) {
			t.Fatal("logon must not be called when the store itself fails")
			return nil, nil
		},
		capable: func() error { return nil },
	}

	_, err := p.Resolve(context.Background(), Spec{User: "render-svc"})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want it to wrap the store's sentinel error", err)
	}
	if !strings.Contains(err.Error(), "render-svc") {
		t.Errorf("err = %v, want it to name the account", err)
	}
}

// TestLogonUserResolve_LogonErrorWrapped checks that a failure from the OS
// logon seam is wrapped with the account name rather than surfacing bare.
func TestLogonUserResolve_LogonErrorWrapped(t *testing.T) {
	sentinel := errors.New("ERROR_LOGON_FAILURE")
	p := &logonUserProvider{
		store: fakeStore{"render-svc": "s3cr3t"},
		logon: func(context.Context, string, string) (*Credential, error) {
			return nil, sentinel
		},
		capable: func() error { return nil },
	}

	_, err := p.Resolve(context.Background(), Spec{User: "render-svc"})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want it to wrap the logon seam's sentinel error", err)
	}
	if !strings.Contains(err.Error(), "render-svc") {
		t.Errorf("err = %v, want it to name the account", err)
	}
}

// TestLogonUserResolve_BatchLogonRightMissingIsActionable proves the specific
// ERROR_LOGON_TYPE_NOT_GRANTED failure — the account lacks "Log on as a batch
// job" — is translated into an operator-actionable message naming the right
// and its fix, rather than surfacing the opaque Win32 "has not been granted
// the requested logon type" text. The underlying errno must still be wrapped
// so errors.Is keeps working. This is the exact failure a first real elevated
// run hit against a freshly created standard account.
func TestLogonUserResolve_BatchLogonRightMissingIsActionable(t *testing.T) {
	// The real Windows seam wraps the raw errno exactly this way
	// (logonUserOS: "LogonUserW: %w" over procLogonUserW.Call's syscall.Errno),
	// so reproducing that shape here is what proves the errors.Is match holds
	// through the seam's own wrapping.
	seamErr := fmt.Errorf("LogonUserW: %w", errLogonTypeNotGranted)
	p := &logonUserProvider{
		store: fakeStore{"render-svc": "s3cr3t"},
		logon: func(context.Context, string, string) (*Credential, error) {
			return nil, seamErr
		},
		capable: func() error { return nil },
	}

	_, err := p.Resolve(context.Background(), Spec{User: "render-svc"})
	if err == nil {
		t.Fatal("Resolve = nil error for an account lacking the batch-logon right, want a refusal")
	}
	if !errors.Is(err, errLogonTypeNotGranted) {
		t.Errorf("err = %v, want it to still wrap ERROR_LOGON_TYPE_NOT_GRANTED for errors.Is", err)
	}
	for _, want := range []string{"render-svc", "Log on as a batch job", "SeBatchLogonRight"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want it to mention %q", err.Error(), want)
		}
	}
}

// TestLogonUserResolve_OtherLogonErrorNotRewritten proves the batch-logon-right
// translation is narrow: a different logon failure (a bad password, say) is
// wrapped with the account name but NOT given the SeBatchLogonRight guidance,
// which would misdirect an operator.
func TestLogonUserResolve_OtherLogonErrorNotRewritten(t *testing.T) {
	// ERROR_LOGON_FAILURE (1326): right to log on held, credentials wrong.
	seamErr := fmt.Errorf("LogonUserW: %w", syscall.Errno(1326))
	p := &logonUserProvider{
		store:   fakeStore{"render-svc": "s3cr3t"},
		logon:   func(context.Context, string, string) (*Credential, error) { return nil, seamErr },
		capable: func() error { return nil },
	}

	_, err := p.Resolve(context.Background(), Spec{User: "render-svc"})
	if err == nil {
		t.Fatal("Resolve = nil error, want the logon failure surfaced")
	}
	if strings.Contains(err.Error(), "SeBatchLogonRight") {
		t.Errorf("err = %q, must not attach batch-logon-right guidance to an unrelated logon failure", err.Error())
	}
	if !errors.Is(err, seamErr) {
		t.Errorf("err = %v, want it to wrap the original logon error", err)
	}
}

// TestLogonUserResolve_NilCredentialNilErrorRejected guards against a
// misbehaving logon seam that returns (nil, nil): Resolve must still error
// rather than handing back a usable-looking nil credential, since a caller
// checking only `err != nil` would otherwise proceed as if isolated while
// actually holding no credential at all.
func TestLogonUserResolve_NilCredentialNilErrorRejected(t *testing.T) {
	p := &logonUserProvider{
		store: fakeStore{"render-svc": "s3cr3t"},
		logon: func(context.Context, string, string) (*Credential, error) {
			return nil, nil
		},
		capable: func() error { return nil },
	}

	cred, err := p.Resolve(context.Background(), Spec{User: "render-svc"})
	if err == nil {
		t.Fatal("a logon seam returning (nil, nil) must still produce a Resolve error")
	}
	if cred != nil {
		t.Errorf("cred = %v, want nil alongside the error", cred)
	}
}

// TestLogonUserResolve_Success proves the happy path: a validated,
// non-privileged account with a stored secret reaches the logon seam exactly
// once and Resolve returns the credential it produces, unmodified.
func TestLogonUserResolve_Success(t *testing.T) {
	want := &Credential{Home: `C:\Users\render-svc`}
	calls := 0
	p := &logonUserProvider{
		store: fakeStore{"render-svc": "s3cr3t"},
		logon: func(_ context.Context, user, secret string) (*Credential, error) {
			calls++
			if user != "render-svc" || secret != "s3cr3t" {
				t.Errorf("logon called with (%q, %q), want (render-svc, s3cr3t)", user, secret)
			}
			return want, nil
		},
		capable:     func() error { return nil },
		canTraverse: func(*Credential) (bool, error) { return true, nil },
	}

	got, err := p.Resolve(context.Background(), Spec{User: "render-svc"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != want {
		t.Errorf("Resolve returned %v, want the exact credential the logon seam produced", got)
	}
	if calls != 1 {
		t.Errorf("logon called %d times, want exactly 1", calls)
	}
}

// TestLogonUserResolve_NormalizesQualifiedUsernameBeforeLogon proves the
// username reaching the OS logon seam is always the bare, normalized account
// name, even when Spec.User carries a ".\"/"DOMAIN\" qualifier or a trailing
// "@domain" UPN suffix. logonUserOS calls LogonUserW with a NULL domain (see
// logonUserW's doc), which does not itself strip either form from the
// username string — a qualified username reaching it verbatim fails to
// resolve, surfacing as an opaque logon error a queue operator would
// misdiagnose as a bad password rather than a qualifier the provider should
// have stripped. The CredentialStore lookup still receives the RAW Spec.User
// (see fileStore.Secret / credFileName, which normalize internally) —
// matching production, only the OS call itself needs the pre-normalized
// form.
func TestLogonUserResolve_NormalizesQualifiedUsernameBeforeLogon(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec string
		want string
	}{
		{"dot-qualified", `.\render-svc`, "render-svc"},
		{"domain-qualified", `CORP\Render-Svc`, "render-svc"},
		{"upn", "render-svc@corp.example.com", "render-svc"},
		{"upn-mixed-case", "Render-Svc@CORP.EXAMPLE.COM", "render-svc"},
		{"bare", "render-svc", "render-svc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotUser string
			p := &logonUserProvider{
				store: fakeStore{tc.spec: "s3cr3t"},
				logon: func(_ context.Context, user, _ string) (*Credential, error) {
					gotUser = user
					return &Credential{}, nil
				},
				capable:     func() error { return nil },
				canTraverse: func(*Credential) (bool, error) { return true, nil },
			}
			if _, err := p.Resolve(context.Background(), Spec{User: tc.spec}); err != nil {
				t.Fatalf("Resolve(%q): %v", tc.spec, err)
			}
			if gotUser != tc.want {
				t.Errorf("logon called with user %q, want %q", gotUser, tc.want)
			}
		})
	}
}

// TestLogonUserResolve_ValidateAccountArgRejectsBadInput proves Resolve
// applies validateAccountArg BEFORE anything else — the store is never
// consulted for an argument-injection-shaped username.
func TestLogonUserResolve_ValidateAccountArgRejectsBadInput(t *testing.T) {
	for _, name := range []string{"", "-rf", "a\nb", "a\x00b"} {
		t.Run(name, func(t *testing.T) {
			p := &logonUserProvider{
				store: storeFunc(func(string) (string, error) {
					t.Fatal("store must not be consulted for an invalid username")
					return "", nil
				}),
				logon:   func(context.Context, string, string) (*Credential, error) { return nil, nil },
				capable: func() error { return nil },
			}
			if _, err := p.Resolve(context.Background(), Spec{User: name}); err == nil {
				t.Errorf("Resolve(User=%q) = nil error, want a validation error", name)
			}
		})
	}
}

// TestLogonUserCapable_ForwardsSeamError proves Capable() surfaces the
// capable seam's error (and that it satisfies errors.Is(ErrNotCapable), the
// contract every Provider.Capable() caller relies on) without alteration.
func TestLogonUserCapable_ForwardsSeamError(t *testing.T) {
	p := &logonUserProvider{
		store:   fakeStore{},
		logon:   func(context.Context, string, string) (*Credential, error) { return nil, nil },
		capable: func() error { return fmt.Errorf("%w: SeAssignPrimaryTokenPrivilege not held", ErrNotCapable) },
	}
	if err := p.Capable(); !errors.Is(err, ErrNotCapable) {
		t.Errorf("Capable() = %v, want ErrNotCapable", err)
	}
}

// TestLogonUserCapable_Success proves Capable() returns nil when the seam
// reports the worker is capable.
func TestLogonUserCapable_Success(t *testing.T) {
	p := &logonUserProvider{
		store:   fakeStore{},
		logon:   func(context.Context, string, string) (*Credential, error) { return nil, nil },
		capable: func() error { return nil },
	}
	if err := p.Capable(); err != nil {
		t.Errorf("Capable() = %v, want nil", err)
	}
}

// storeFunc adapts a plain function to CredentialStore, for tests that need
// to assert a store method is (or is not) called rather than canning a fixed
// table of secrets.
type storeFunc func(user string) (string, error)

func (f storeFunc) Secret(user string) (string, error) { return f(user) }

// TestLogonUserResolve_RefusesTargetWithoutBypassTraverse proves Resolve
// refuses an account that cannot traverse to its own session directory.
//
// Windows grants "Bypass traverse checking" (SeChangeNotifyPrivilege) to
// Everyone by default, which is why ValidateTraversable is a no-op on this
// platform. Hardening guides do sometimes strip it, and when they have, an
// isolated task fails with an opaque access-denied on a path whose ACL looks
// correct. Refusing here, naming the policy, is the Windows equivalent of the
// POSIX ancestor check — report, never silently widen.
func TestLogonUserResolve_RefusesTargetWithoutBypassTraverse(t *testing.T) {
	p := &logonUserProvider{
		store: fakeStore{"render-svc": "hunter2"},
		logon: func(context.Context, string, string) (*Credential, error) {
			// Not Credential{User: "render-svc"}: this file carries no build
			// tag, so it compiles on every platform, and only the
			// Windows-only Credential (provider_windows.go) has a User
			// field — the POSIX one (provider_unix.go, //go:build unix)
			// does not. The test does not inspect User, so the zero value
			// costs nothing.
			return &Credential{}, nil
		},
		capable:     func() error { return nil },
		canTraverse: func(*Credential) (bool, error) { return false, nil },
	}

	_, err := p.Resolve(context.Background(), Spec{User: "render-svc"})

	if err == nil {
		t.Fatal("Resolve = nil error for an account without bypass-traverse, want a refusal")
	}
	if !strings.Contains(err.Error(), "SeChangeNotifyPrivilege") {
		t.Errorf("Resolve error = %q, want it to name SeChangeNotifyPrivilege", err.Error())
	}
}
