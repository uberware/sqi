// SPDX-License-Identifier: AGPL-3.0-or-later

package isolation

import (
	"context"
	"fmt"
)

// FakeAccount is one entry in a fake provider's account table.
type FakeAccount struct {
	UID uint32
	GID uint32
	// Groups maps the names of groups this account belongs to onto their
	// gid, so a Spec.Group request can be validated for membership the same
	// way the real provider validates it against os/user.GroupIds(). A group
	// name absent from this map is treated as one the account does not
	// belong to, regardless of whether it "exists" in some other account's
	// entry.
	Groups map[string]uint32
	// SupplementaryGIDs is the raw supplementary group gid list the
	// credential is built from, mirroring what GroupIds()/`id -G` would
	// report for a real account. Unlike Groups, this is not name-keyed and
	// may contain gids with no corresponding entry in Groups (e.g. gid 0, or
	// a privileged named group's gid) — exactly the shape needed to exercise
	// stripGID0FromSupplementary's gid-0 filter and its deliberate choice to
	// leave privileged named groups alone.
	SupplementaryGIDs []uint32
	Home              string
}

type fakeProvider struct {
	accounts map[string]FakeAccount
	// incapable makes Capable() always fail, for exercising the not-capable
	// startup path without a real privileged worker.
	incapable bool
}

// NewFake returns an in-memory Provider for unit tests.
//
// A fake CANNOT verify the things that actually break here — real uid/gid
// switching, supplementary groups, workdir ownership, Setpgid survival. Those
// need make test-isolation against a real OS. Use this only for policy logic.
func NewFake(accounts map[string]FakeAccount) Provider {
	return &fakeProvider{accounts: accounts}
}

// NewFakeIncapable returns an in-memory Provider whose Capable() always
// reports ErrNotCapable, for tests of the not-capable startup path.
func NewFakeIncapable(accounts map[string]FakeAccount) Provider {
	return &fakeProvider{accounts: accounts, incapable: true}
}

func (f *fakeProvider) Resolve(_ context.Context, spec Spec) (*Credential, error) {
	// validateAccountArg is the same gate the real POSIX provider applies
	// before spec.User/spec.Group is used for anything. Skipping it here
	// would mean a "-u"-shaped username passes the fake in tests and then
	// fails only against the real provider — the fake/real divergence class
	// a prior review round already had to close once.
	if err := validateAccountArg(spec.User, "user"); err != nil {
		return nil, err
	}

	acct, ok := f.accounts[spec.User]
	if !ok {
		return nil, fmt.Errorf("isolation: fake: unknown account %q", spec.User)
	}
	if err := CheckNotPrivileged(spec.User, acct.UID); err != nil {
		return nil, err
	}

	gid := acct.GID
	if spec.Group == "" {
		// Mirrors resolveGID's real-provider fix: an account whose PRIMARY
		// group is gid 0 must be refused even with no explicit group
		// requested, since queue config only chooses the username and the
		// username determines the primary group. Deliberately does not run
		// the full name-based CheckGroupNotPrivileged here, for the same
		// reason the real provider doesn't: an account's primary group name
		// (e.g. macOS's "staff") may itself be on privilegedGroupNames
		// without that account being privileged.
		if gid == 0 {
			return nil, fmt.Errorf("%w: user %q has primary gid 0", ErrRefusedPrivileged, spec.User)
		}
	} else {
		if err := validateAccountArg(spec.Group, "group"); err != nil {
			return nil, err
		}
		g, member := acct.Groups[spec.Group]
		if !member {
			return nil, fmt.Errorf("isolation: fake: user %q is not a member of group %q", spec.User, spec.Group)
		}
		if err := CheckGroupNotPrivileged(spec.Group, g); err != nil {
			return nil, err
		}
		gid = g
	}

	// spec.User is threaded through separately from FakeAccount: on Windows,
	// SecureWorkDir/ChownRecursive resolve a Credential's identity by NAME
	// (Credential.User), not by uid/gid, so newFakeCredential needs the
	// account name a real Resolve would have carried even though FakeAccount
	// itself (an entry in a name-keyed map) has no name field of its own.
	return newFakeCredential(spec.User, FakeAccount{UID: acct.UID, GID: gid, Home: acct.Home, SupplementaryGIDs: acct.SupplementaryGIDs}), nil
}

func (f *fakeProvider) Capable() error {
	if f.incapable {
		return fmt.Errorf("%w: fake provider configured as not capable", ErrNotCapable)
	}
	return nil
}
