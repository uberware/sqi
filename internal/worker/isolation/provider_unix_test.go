// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build unix

package isolation_test

import (
	"context"
	"errors"
	"os/user"
	"strconv"
	"testing"

	"github.com/uberware/sqi/internal/worker/isolation"
)

// nonPrivilegedTestUser returns the OS account running the test, skipping if
// that account is itself privileged (uid 0, or a privileged name) — which
// happens routinely in containerized CI that runs as root. The group tests
// below need a legitimate, existing, non-privileged account, and the invoking
// user is the only one guaranteed to exist without shelling out to useradd.
func nonPrivilegedTestUser(t *testing.T) *user.User {
	t.Helper()
	u, err := user.Current()
	if err != nil {
		t.Skipf("user.Current: %v", err)
	}
	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		t.Skipf("parse uid %q: %v", u.Uid, err)
	}
	if isolation.CheckNotPrivileged(u.Username, uint32(uid)) != nil {
		t.Skipf("test is running as privileged account %q; skipping", u.Username)
	}
	return u
}

// TestUnixProviderRefusesRoot is the highest-value assertion in this file: it
// needs no privileges and proves the refusal sits in the PRODUCTION Resolve
// path (unixProvider, reached only via the exported NewProvider), not just in
// the fake that every other test in this package exercises.
func TestUnixProviderRefusesRoot(t *testing.T) {
	p, err := isolation.NewProvider(isolation.Config{})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	if _, err := p.Resolve(context.Background(), isolation.Spec{User: "root"}); !errors.Is(err, isolation.ErrRefusedPrivileged) {
		t.Fatalf("Resolve(root) = %v, want ErrRefusedPrivileged", err)
	}
}

func TestUnixProviderUnknownUser(t *testing.T) {
	p, err := isolation.NewProvider(isolation.Config{})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	if _, err := p.Resolve(context.Background(), isolation.Spec{User: "sqi-test-no-such-user"}); err == nil {
		t.Error("Resolve of an unknown user must fail, never fall back")
	}
}

func TestUnixProviderUnknownGroup(t *testing.T) {
	u := nonPrivilegedTestUser(t)
	p, err := isolation.NewProvider(isolation.Config{})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	spec := isolation.Spec{User: u.Username, Group: "sqi-test-no-such-group"}
	if _, err := p.Resolve(context.Background(), spec); err == nil {
		t.Error("Resolve with an unknown group must fail, never fall back to the primary group")
	}
}

// TestUnixProviderRefusesPrivilegedGroup is the empirical reproduction from
// the review: resolving a legitimate, unprivileged user with an explicit
// privileged group must be refused. gid 0's group name is platform-dependent
// ("wheel" on macOS/BSD, "root" on Linux) but is always in privilegedGroupNames
// either way, so looking it up dynamically keeps this test portable while
// still proving the exact bypass ("Group: wheel" / "Group: docker" style
// requests) is now closed in the real provider, not just in CheckGroupNotPrivileged.
func TestUnixProviderRefusesPrivilegedGroup(t *testing.T) {
	g, err := user.LookupGroupId("0")
	if err != nil {
		t.Skipf("LookupGroupId(0): %v", err)
	}
	u := nonPrivilegedTestUser(t)

	p, err := isolation.NewProvider(isolation.Config{})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	spec := isolation.Spec{User: u.Username, Group: g.Name}
	if _, err := p.Resolve(context.Background(), spec); !errors.Is(err, isolation.ErrRefusedPrivileged) {
		t.Fatalf("Resolve(user=%q, group=%q) = %v, want ErrRefusedPrivileged", u.Username, g.Name, err)
	}
}

// TestUnixProviderRefusesGroupUserIsNotAMemberOf finds a group that
// genuinely exists on the box but that the invoking user does not belong to,
// by scanning low system gids and checking each one against the user's real
// membership set — so the test is self-verifying rather than assuming any
// particular group layout. It skips rather than falsely failing if no such
// group can be found.
func TestUnixProviderRefusesGroupUserIsNotAMemberOf(t *testing.T) {
	u := nonPrivilegedTestUser(t)

	memberships, err := u.GroupIds()
	if err != nil {
		t.Fatalf("GroupIds: %v", err)
	}
	isMember := map[string]bool{u.Gid: true}
	for _, gid := range memberships {
		isMember[gid] = true
	}

	var target *user.Group
	for gid := 1; gid < 200; gid++ {
		g, err := user.LookupGroupId(strconv.Itoa(gid))
		if err != nil {
			continue
		}
		if !isMember[g.Gid] {
			target = g
			break
		}
	}
	if target == nil {
		t.Skip("could not find an existing group the test user is not a member of")
	}

	p, err := isolation.NewProvider(isolation.Config{})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	spec := isolation.Spec{User: u.Username, Group: target.Name}
	if _, err := p.Resolve(context.Background(), spec); err == nil {
		t.Fatalf("Resolve(user=%q, group=%q) = nil, want an error: user is not a member of that group", u.Username, target.Name)
	}
}
