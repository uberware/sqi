// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"context"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	workerconfig "github.com/uberware/sqi/internal/worker/config"
	"github.com/uberware/sqi/internal/worker/isolation"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// countingCloser wraps closeCredentialFn to count how many times it is
// invoked, without needing a distinct fake Credential TYPE — Credential is a
// concrete struct from another package (its Close is a plain method, not an
// interface), so this seam is the only place a test can observe every call
// to it. Both current implementations of Credential.Close (POSIX and the
// Windows placeholder) are no-ops, which is exactly why a dropped call would
// otherwise be invisible: nothing today notices whether it happened at all.
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
	if runtime.GOOS == "windows" {
		t.Skip("test resolves a real POSIX fake credential")
	}
	closer := &countingCloser{}
	closer.install(t)

	dataDir := t.TempDir()
	makeDataDirAncestorsTraversableForTest(t, dataDir)
	account := isolation.FakeAccount{UID: testUID(), GID: testGID()}
	provider := isolation.NewFake(map[string]isolation.FakeAccount{"render": account})
	mgr := NewManager(filepath.Join(dataDir, "sessions"), false, provider, workerconfig.IsolationConfig{}, nopLogger())

	msg := &protocol.AssignMsg{JobID: "job-close-ok", Isolation: &protocol.IsolationSpec{User: "render"}}
	s, err := mgr.Create(context.Background(), msg)
	skipIfEnvironmentBlocksSessionTraversal(t, err, dataDir)
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
	mgr := NewManager(filepath.Join(dataDir, "sessions"), false, provider, workerconfig.IsolationConfig{}, nopLogger())

	msg := &protocol.AssignMsg{JobID: "job-close-never", Isolation: &protocol.IsolationSpec{User: "render"}}
	_, err := mgr.Create(context.Background(), msg)
	if err == nil {
		t.Fatal("expected Create to fail: no such account registered with the fake provider")
	}

	if got := closer.count(); got != 0 {
		t.Errorf("Close called %d times when no credential was ever resolved, want 0", got)
	}
}

// TestCredentialClose_ClosedExactlyOnceOnEnterEnvironmentsFailure exercises
// one of Manager.Create's own error paths (not just the normal Cleanup
// path): a credential IS obtained (the account resolves), but a later
// OnEnter action fails, so Create tears everything down itself — including
// closing the credential — before returning the error. Must still be
// exactly one call, not zero (a leak on this path specifically) and not more
// than one.
func TestCredentialClose_ClosedExactlyOnceOnEnterEnvironmentsFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses POSIX shell commands")
	}
	closer := &countingCloser{}
	closer.install(t)

	dataDir := t.TempDir()
	makeDataDirAncestorsTraversableForTest(t, dataDir)
	account := isolation.FakeAccount{UID: testUID(), GID: testGID()}
	provider := isolation.NewFake(map[string]isolation.FakeAccount{"render": account})
	mgr := NewManager(filepath.Join(dataDir, "sessions"), false, provider, workerconfig.IsolationConfig{}, nopLogger())

	msg := &protocol.AssignMsg{
		JobID:     "job-close-enter-fail",
		Isolation: &protocol.IsolationSpec{User: "render"},
		Environments: []protocol.AssignEnvironment{
			{
				Name: "bad-env",
				OnEnter: &protocol.Action{
					Command: "sh",
					Args:    []string{"-c", "exit 1"},
				},
			},
		},
	}

	_, err := mgr.Create(context.Background(), msg)
	skipIfEnvironmentBlocksSessionTraversal(t, err, dataDir)
	if err == nil {
		t.Fatal("expected Create to fail when OnEnter fails")
	}

	if got := closer.count(); got != 1 {
		t.Errorf("Close called %d times on the enterEnvironments failure path, want exactly 1", got)
	}
}
