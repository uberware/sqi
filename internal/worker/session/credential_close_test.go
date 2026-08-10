// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	workerconfig "github.com/uberware/sqi/internal/worker/config"
	"github.com/uberware/sqi/internal/worker/fmtres"
	"github.com/uberware/sqi/internal/worker/isolation"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// countingCloser wraps closeCredentialFn to count how many times it is
// invoked, without needing a distinct fake Credential TYPE — Credential is a
// concrete struct from another package (its Close is a plain method, not an
// interface), so this seam is the only place a test can observe every call
// to it. On POSIX, Credential.Close is a no-op, which is exactly why a
// dropped call would otherwise be invisible there; on Windows it unloads a
// mounted profile hive and closes a real token handle, so this seam is what
// proves the close actually happens exactly once on the one platform where
// Close does real work.
type countingCloser struct {
	mu    sync.Mutex
	calls int
}

func (c *countingCloser) install(t *testing.T) {
	t.Helper()
	orig := closeCredentialFn
	closeCredentialFn = func(cred *isolation.Credential) error {
		c.mu.Lock()
		c.calls++
		c.mu.Unlock()
		return orig(cred)
	}
	t.Cleanup(func() { closeCredentialFn = orig })
}

func (c *countingCloser) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// TestCredentialClose_ClosedExactlyOnceOnNormalCleanup is the normal-path
// guard: a session that resolves a credential and completes successfully
// must have that credential closed exactly once — by Manager.Cleanup — never
// zero times (a leak) and never more than once (a double-close).
func TestCredentialClose_ClosedExactlyOnceOnNormalCleanup(t *testing.T) {
	closer := &countingCloser{}
	closer.install(t)

	dataDir := newTraversableSessionDataDir(t)
	name := testAccountName()
	account := isolation.FakeAccount{UID: testUID(), GID: testGID()}
	provider := isolation.NewFake(map[string]isolation.FakeAccount{name: account})
	mgr := NewManager(filepath.Join(dataDir, "sessions"), false, provider, workerconfig.IsolationConfig{}, fmtres.ExprLimits{}, nopLogger())

	msg := &protocol.AssignMsg{JobID: "job-close-ok", Isolation: &protocol.IsolationSpec{User: name}}
	s, err := mgr.Create(context.Background(), msg)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if s.Credential() == nil {
		t.Fatal("test setup invariant violated: session must carry a credential")
	}

	mgr.Cleanup(context.Background(), s, false)

	if got := closer.count(); got != 1 {
		t.Errorf("Close called %d times across create+cleanup, want exactly 1", got)
	}
}

// TestCredentialClose_NeverCalledWhenCredentialNeverObtained checks the
// mirror-image invariant: when provider.Resolve itself fails, no credential
// was ever obtained, so there is nothing to close — closeCredentialFn must
// not be invoked at all.
func TestCredentialClose_NeverCalledWhenCredentialNeverObtained(t *testing.T) {
	closer := &countingCloser{}
	closer.install(t)

	dataDir := t.TempDir()
	provider := isolation.NewFake(nil) // no accounts registered: Resolve always fails
	mgr := NewManager(filepath.Join(dataDir, "sessions"), false, provider, workerconfig.IsolationConfig{}, fmtres.ExprLimits{}, nopLogger())

	msg := &protocol.AssignMsg{JobID: "job-close-never", Isolation: &protocol.IsolationSpec{User: "render"}}
	_, err := mgr.Create(context.Background(), msg)
	if err == nil {
		t.Fatal("expected Create to fail: no such account registered with the fake provider")
	}

	if got := closer.count(); got != 0 {
		t.Errorf("Close called %d times when no credential was ever resolved, want 0", got)
	}
}

// TestCredentialClose_ClosedExactlyOnceOnEnterEnvironmentsFailure used to
// live here, exercising one of Manager.Create's own error paths (not just
// the normal Cleanup path): a credential IS obtained (the account resolves),
// but a later OnEnter action fails, so Create tears everything down itself —
// including closing the credential — before returning the error.
//
// It is gone from this file, not merely un-skipped: unlike every other test
// in this package, it drove the REAL applyCredential (isolation.Apply), not
// a fake — Manager.Create's onEnter launch never had it replaced. Making the
// engineered OnEnter failure ("sh -c exit 1") actually happen AS THE
// RESOLVED ACCOUNT, rather than failing earlier with EPERM out of
// isolation.Apply itself, requires a real setuid+setgroups transition to a
// genuinely different uid — which requires real root. That is true on any
// POSIX OS, not just a sandboxed one: exec.Cmd.SysProcAttr.Credential always
// calls setgroups(2), which requires CAP_SETGID even to set a process's own
// CURRENT supplementary group list unless the caller is already privileged —
// so this test could never pass unprivileged, no matter how its temp
// directory was rooted. That is exactly the condition this file's sibling
// tests (TestCredentialClose_ClosedExactlyOnceOnNormalCleanup and
// TestCredentialClose_NeverCalledWhenCredentialNeverObtained) avoid by
// installing a same-uid fake account, resolved but never actually exec'd
// under a switched identity.
//
// The real-root counterpart now lives at
// TestIsolation_CredentialClosedOnEnterEnvironmentsFailure in
// test/integration/isolation_test.go (build tag integration, run by
// `make test-isolation` as real root against a real, genuinely different
// unprivileged account). It cannot reach this package's private
// closeCredentialFn counting seam from there, so it instead asserts the
// externally observable consequence of the SAME teardown branch running:
// Manager.Create's OnEnter-failure path calls closeCredential(cred) then
// unconditionally os.RemoveAll(workDir) in that one branch (see
// internal/worker/session/session.go), so a removed WorkDir after a
// genuinely failing OnEnter proves that branch — and the credential close it
// performs exactly once, never in a loop — ran to completion.
