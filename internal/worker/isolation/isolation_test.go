// SPDX-License-Identifier: AGPL-3.0-or-later

package isolation_test

import (
	"context"
	"errors"
	"os/exec"
	"testing"

	"github.com/uberware/sqi/internal/worker/isolation"
)

func TestApplyNilCredentialLeavesCmdUnchanged(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "true")
	before := cmd.SysProcAttr

	if err := isolation.Apply(cmd, nil); err != nil {
		t.Fatalf("Apply(nil) = %v, want nil", err)
	}
	if cmd.SysProcAttr != before {
		t.Error("nil credential must leave SysProcAttr untouched (pre-isolation path)")
	}
}

// Apply must not destroy SysProcAttr fields the caller already set. The executor
// sets Setpgid before calling Apply, and losing it means a canceled task leaks
// its process tree — worst of all for a privileged-drop child.
func TestApplyPreservesExistingSysProcAttr(t *testing.T) {
	p := isolation.NewFake(map[string]isolation.FakeAccount{
		"render-svc": {UID: 1001, GID: 2001},
	})
	cred, err := p.Resolve(context.Background(), isolation.Spec{User: "render-svc"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	cmd := exec.CommandContext(context.Background(), "true")
	configureTestProcessGroup(cmd) // sets Setpgid, mirroring the executor

	if err := isolation.Apply(cmd, cred); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	assertProcessGroupStillSet(t, cmd)
}

func TestFakeProviderResolvesAndRefuses(t *testing.T) {
	p := isolation.NewFake(map[string]isolation.FakeAccount{
		"render-svc": {UID: 1001, GID: 2001},
	})

	if _, err := p.Resolve(context.Background(), isolation.Spec{User: "render-svc"}); err != nil {
		t.Fatalf("Resolve(render-svc) = %v, want nil", err)
	}
	if _, err := p.Resolve(context.Background(), isolation.Spec{User: "root"}); err == nil {
		t.Error("Resolve(root) must be refused even by the fake")
	}
	if _, err := p.Resolve(context.Background(), isolation.Spec{User: "nope"}); err == nil {
		t.Error("Resolve of an unknown account must fail, never fall back")
	}
}

// TestFakeProviderValidatesGroup exercises the same policy Critical-1 added
// to the real provider (privileged-group refusal, membership verification)
// against the fake, so that later tests written against the fake actually
// prove something instead of silently passing regardless of Group.
func TestFakeProviderValidatesGroup(t *testing.T) {
	p := isolation.NewFake(map[string]isolation.FakeAccount{
		"render-svc": {
			UID:    1001,
			GID:    2001,
			Groups: map[string]uint32{"render": 2010, "wheel": 0},
			Home:   "/home/render-svc",
		},
	})

	if _, err := p.Resolve(context.Background(), isolation.Spec{User: "render-svc", Group: "render"}); err != nil {
		t.Errorf("Resolve with a group the account belongs to = %v, want nil", err)
	}
	if _, err := p.Resolve(context.Background(), isolation.Spec{User: "render-svc", Group: "wheel"}); !errors.Is(err, isolation.ErrRefusedPrivileged) {
		t.Errorf("Resolve(group=wheel) = %v, want ErrRefusedPrivileged", err)
	}
	if _, err := p.Resolve(context.Background(), isolation.Spec{User: "render-svc", Group: "not-a-member"}); err == nil {
		t.Error("Resolve with a group the account does not belong to must fail")
	}
}

// TestFakeProviderRejectsInvalidUsername is the Minor-9 fake/real parity
// fix: the real provider rejects a "-u"-shaped username before it's used
// for anything (TestUnixProviderRejectsInvalidUsernameBeforeAnyLookup); the
// fake must reject the same shapes, or a test written against the fake
// would pass with a username that fails against the real provider.
func TestFakeProviderRejectsInvalidUsername(t *testing.T) {
	p := isolation.NewFake(map[string]isolation.FakeAccount{
		"-rf": {UID: 1001, GID: 2001},
	})
	for _, name := range []string{"", "-rf", "a\nb", "a\x00b"} {
		if _, err := p.Resolve(context.Background(), isolation.Spec{User: name}); err == nil {
			t.Errorf("fake Resolve(User=%q) = nil, want a validation error", name)
		}
	}
}

// TestFakeProviderRejectsInvalidGroup is the group-side counterpart of
// TestFakeProviderRejectsInvalidUsername.
func TestFakeProviderRejectsInvalidGroup(t *testing.T) {
	p := isolation.NewFake(map[string]isolation.FakeAccount{
		"render-svc": {UID: 1001, GID: 2001, Groups: map[string]uint32{"-rf": 2010}},
	})
	if _, err := p.Resolve(context.Background(), isolation.Spec{User: "render-svc", Group: "-rf"}); err == nil {
		t.Error("fake Resolve(Group=\"-rf\") = nil, want a validation error")
	}
}

// TestFakeProviderRejectsPrimaryGIDZero mirrors Critical 1's real-provider
// fix (TestResolveGIDRefusesPrimaryGIDZero) in the fake, so a test written
// against the fake proves something about an account whose primary group is
// gid 0 instead of silently accepting it.
func TestFakeProviderRejectsPrimaryGIDZero(t *testing.T) {
	p := isolation.NewFake(map[string]isolation.FakeAccount{
		"render-svc": {UID: 1001, GID: 0},
	})
	if _, err := p.Resolve(context.Background(), isolation.Spec{User: "render-svc"}); !errors.Is(err, isolation.ErrRefusedPrivileged) {
		t.Errorf("fake Resolve(primary gid=0) = %v, want ErrRefusedPrivileged", err)
	}
}

// TestFakeProviderAllowsOrdinaryPrimaryGID is the fake-provider companion to
// TestResolveGIDAllowsOrdinaryStaffStylePrimaryGID, guarding against the fake
// over-correcting the same way the real provider must not.
func TestFakeProviderAllowsOrdinaryPrimaryGID(t *testing.T) {
	p := isolation.NewFake(map[string]isolation.FakeAccount{
		"render-svc": {UID: 1001, GID: 2001},
	})
	if _, err := p.Resolve(context.Background(), isolation.Spec{User: "render-svc"}); err != nil {
		t.Errorf("fake Resolve(primary gid=2001) = %v, want nil", err)
	}
}

// TestFakeProviderStripsGID0FromSupplementaryGroups is the Minor-6 fix: the
// fake previously populated no Groups at all on the returned Credential, so
// supplementary-group behavior — including the Important-1 gid-0 strip — was
// structurally invisible to every fake-based test. This proves the fake now
// applies the same stripGID0FromSupplementary logic the real provider does.
func TestFakeProviderStripsGID0FromSupplementaryGroups(t *testing.T) {
	p := isolation.NewFake(map[string]isolation.FakeAccount{
		"render-svc": {
			UID:               1001,
			GID:               2001,
			SupplementaryGIDs: []uint32{2001, 0, 2010},
		},
	})

	cred, err := p.Resolve(context.Background(), isolation.Spec{User: "render-svc"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	cmd := exec.CommandContext(context.Background(), "true")
	if err := isolation.Apply(cmd, cred); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if cmd.SysProcAttr == nil || cmd.SysProcAttr.Credential == nil {
		t.Fatal("Apply must install a non-nil Credential")
	}
	for _, g := range cmd.SysProcAttr.Credential.Groups {
		if g == 0 {
			t.Fatalf("Groups = %v, want gid 0 stripped from the fake's supplementary set too", cmd.SysProcAttr.Credential.Groups)
		}
	}
}

func TestFakeIncapable(t *testing.T) {
	incapable := isolation.NewFakeIncapable(map[string]isolation.FakeAccount{
		"render-svc": {UID: 1001, GID: 2001},
	})
	if err := incapable.Capable(); !errors.Is(err, isolation.ErrNotCapable) {
		t.Errorf("NewFakeIncapable.Capable() = %v, want ErrNotCapable", err)
	}

	capable := isolation.NewFake(map[string]isolation.FakeAccount{
		"render-svc": {UID: 1001, GID: 2001},
	})
	if err := capable.Capable(); err != nil {
		t.Errorf("NewFake.Capable() = %v, want nil", err)
	}
}
