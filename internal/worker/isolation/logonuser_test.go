// SPDX-License-Identifier: AGPL-3.0-or-later

package isolation

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
		capable: func() error { return nil },
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
