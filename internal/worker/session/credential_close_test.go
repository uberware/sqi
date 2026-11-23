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
// invoked. Credential is a concrete struct from another package (its Close is
// a plain method, not an interface), so this seam is the only place a test
// can observe every call to it. On POSIX, Credential.Close is a no-op, so a
// dropped call would otherwise be invisible there; on Windows it unloads a
// mounted profile hive and closes a token handle.
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
// must have that credential closed once, by Manager.Cleanup: never zero times
// (a leak) and never more than once (a double-close).
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

// One of Manager.Create's error paths is not tested in this file: a
// credential is obtained (the account resolves), but a later OnEnter action
// fails, so Create tears everything down itself, closing the credential,
// before returning the error.
//
// That path runs the real applyCredential (isolation.Apply): Manager.Create's
// onEnter launch does not replace it. For the OnEnter failure ("sh -c exit 1")
// to happen as the resolved account, rather than earlier with EPERM from
// isolation.Apply, needs a setuid+setgroups transition to a different uid,
// and so root, on any POSIX OS: exec.Cmd.SysProcAttr.Credential always calls
// setgroups(2), which requires CAP_SETGID even to set a process's own current
// supplementary group list. This file's tests
// (TestCredentialClose_ClosedExactlyOnceOnNormalCleanup and
// TestCredentialClose_NeverCalledWhenCredentialNeverObtained) avoid that by
// installing a same-uid fake account that is resolved but never exec'd under
// a switched identity.
//
// The root test is TestIsolation_CredentialClosedOnEnterEnvironmentsFailure
// in test/integration/isolation_test.go (build tag integration, run by
// `make test-isolation` as root against a different unprivileged account). It
// cannot reach this package's closeCredentialFn seam, so it asserts what the
// same teardown branch does visibly: Manager.Create's OnEnter-failure path
// calls closeCredential(cred) and then os.RemoveAll(workDir) (see
// session.go), so a removed WorkDir after a failing OnEnter shows that branch
// ran to completion.
