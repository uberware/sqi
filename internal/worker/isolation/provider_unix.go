// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build unix

package isolation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/user"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

// Credential holds a resolved POSIX identity.
type Credential struct {
	cred *syscall.Credential
	// Home is the target user's home directory. Callers rewrite HOME to this;
	// leaving the daemon's HOME makes DCCs write configs where the task cannot.
	Home string
}

// Close releases the credential. POSIX credentials hold no OS handle.
func (*Credential) Close() error { return nil }

// userIdentity is the resolved account data Resolve needs, regardless of
// whether it came from pure-Go os/user or the NSS-aware fallback below.
type userIdentity struct {
	uid        uint32
	primaryGID uint32
	// username is what the OS/NSS resolved spec.User to. In the full
	// fallback path (no os/user.User available) it is spec.User echoed back,
	// since id(1) reports no more canonical a name than the input.
	username string
	// groups are supplementary group memberships. May or may not include
	// primaryGID depending on source; callers must not assume either way.
	groups []uint32
	// home is the target account's home directory, or "" when it could not
	// be determined (e.g. no os/user match and getent(1) unavailable/failed).
	home string
}

type unixProvider struct {
	// run executes id(1)/getent(1) for the NSS-aware fallback. Overridden in
	// tests to supply canned output without a real directory-backed account.
	run    cmdRunner
	logger *slog.Logger
}

// newProvider returns the POSIX provider. cfg.CredentialStore is unused here;
// only the Windows logon_user provider needs it.
func newProvider(cfg Config) (Provider, error) {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return unixProvider{run: nssCommandRunner(logger), logger: logger}, nil
}

// callRun invokes p.run, guarding against a zero-value unixProvider (nil
// run) so that returns an error instead of a nil-pointer panic. newProvider
// always sets run, but nothing stops `unixProvider{}` from being built inside
// the package (by a test, for example).
func (p unixProvider) callRun(ctx context.Context, name string, args ...string) ([]byte, error) {
	if p.run == nil {
		return nil, errors.New("isolation: unixProvider has no command runner configured")
	}
	return p.run(ctx, name, args...)
}

func (p unixProvider) Resolve(ctx context.Context, spec Spec) (*Credential, error) {
	if err := validateAccountArg(spec.User, "user"); err != nil {
		return nil, err
	}

	ident, err := p.resolveIdentity(ctx, spec.User)
	if err != nil {
		return nil, fmt.Errorf("isolation: resolve user %q: %w", spec.User, err)
	}
	if err := CheckNotPrivileged(spec.User, ident.uid); err != nil {
		return nil, err
	}
	// spec.User is the caller-supplied string; ident.username is what the
	// resolution settled on. Checking both catches a lookup alias that
	// resolves to a privileged account under a name the raw input would not
	// match. POSIX also has the uid-0 check above; this name check is shared
	// with platforms that do not.
	if err := CheckNotPrivileged(ident.username, ident.uid); err != nil {
		return nil, err
	}

	gid, err := p.resolveGID(ctx, ident, spec.Group)
	if err != nil {
		return nil, err
	}
	groups := finalizeGroups(ident, gid)

	return &Credential{
		cred: &syscall.Credential{Uid: ident.uid, Gid: gid, Groups: groups},
		Home: ident.home,
	}, nil
}

// resolveIdentity tries pure-Go os/user first, falling back to shelling out
// to NSS-aware tools (id(1)/getent(1)) when os/user can't see the account at
// all. Without cgo, os/user reads /etc/passwd and /etc/group directly and
// never consults NSS, so it fails outright for any directory-backed (LDAP/AD/
// sssd) account, the kind of account render-farm queues usually target.
func (p unixProvider) resolveIdentity(ctx context.Context, name string) (userIdentity, error) {
	u, err := user.Lookup(name)
	if err != nil {
		p.logger.DebugContext(ctx, "isolation: pure-go user lookup failed, falling back to NSS-aware tools",
			"user", name, "error", err)
		return p.resolveIdentityViaNSS(ctx, name)
	}
	return p.resolveIdentityFromOSUser(ctx, u)
}

// resolveIdentityFromOSUser fills in a userIdentity from a successful
// os/user.Lookup. It still falls back to id(1) for the group list alone when
// os/user's answer is implausibly empty: a directory-backed account can
// resolve locally (e.g. present in /etc/passwd via sssd's cache, or NSS
// "files" listing the account itself) while its supplementary groups live
// entirely in LDAP/AD and so never appear in the pure-Go /etc/group parse.
// Dropping them would remove access to group-readable project storage and
// show up as a permission error inside a render.
func (p unixProvider) resolveIdentityFromOSUser(ctx context.Context, u *user.User) (userIdentity, error) {
	uid, err := parseID(u.Uid, "uid")
	if err != nil {
		return userIdentity{}, err
	}
	gid, err := parseID(u.Gid, "gid")
	if err != nil {
		return userIdentity{}, err
	}

	groups, groupsErr := supplementaryGroups(u)
	if groupsNeedFallback(groups, groupsErr) {
		p.logger.DebugContext(ctx, "isolation: pure-go group lookup returned implausibly empty result, falling back to NSS-aware tools",
			"user", u.Username, "error", groupsErr)
		fallback, fallbackErr := p.groupsViaNSS(ctx, u.Username)
		switch {
		case fallbackErr == nil:
			groups = fallback
			p.logger.InfoContext(ctx, "isolation: resolved supplementary groups via NSS-aware fallback",
				"user", u.Username, "groups", groups)
		case groupsErr != nil:
			// Pure-Go itself errored and the fallback couldn't recover: no
			// usable group data at all.
			return userIdentity{}, groupsErr
		default:
			// Pure-Go succeeded with a (possibly correct) empty list and the
			// fallback tool is unavailable or failed: carry on rather than
			// failing identity resolution over group data alone, as a
			// missing getent(1) is non-fatal.
			p.logger.DebugContext(ctx, "isolation: NSS-aware group fallback unavailable, keeping pure-go result",
				"user", u.Username, "error", fallbackErr)
		}
	}

	return userIdentity{
		uid: uid, primaryGID: gid, username: u.Username, groups: groups, home: u.HomeDir,
	}, nil
}

// groupsNeedFallback reports whether a pure-Go os/user group lookup should be
// treated as unusable and replaced by the NSS-aware fallback: an outright
// error, or an implausibly empty result. An account that resolved via
// os/user.Lookup but reports zero supplementary groups is the shape a
// directory-only group backend produces: the account itself is visible
// locally (e.g. via sssd's passwd cache), but its group memberships live
// entirely in LDAP/AD and never appear in the pure-Go /etc/group parse.
func groupsNeedFallback(groups []uint32, err error) bool {
	return err != nil || len(groups) == 0
}

// resolveIdentityViaNSS resolves an account pure-Go os/user could not see at
// all, entirely through id(1)/getent(1). The home directory is best-effort:
// id(1) does not report it, and getent(1) is Linux-primary, so its absence
// or failure leaves Credential.Home empty rather than failing the lookup.
func (p unixProvider) resolveIdentityViaNSS(ctx context.Context, name string) (userIdentity, error) {
	uid, err := p.idNumberViaNSS(ctx, "-u", name)
	if err != nil {
		return userIdentity{}, fmt.Errorf("resolve uid via id(1): %w", err)
	}
	gid, err := p.idNumberViaNSS(ctx, "-g", name)
	if err != nil {
		return userIdentity{}, fmt.Errorf("resolve primary gid via id(1): %w", err)
	}
	groups, err := p.groupsViaNSS(ctx, name)
	if err != nil {
		return userIdentity{}, fmt.Errorf("resolve supplementary groups via id(1): %w", err)
	}
	home := p.homeViaNSS(ctx, name)

	p.logger.InfoContext(ctx, "isolation: resolved user identity via NSS-aware fallback (id/getent)",
		"user", name, "uid", uid, "gid", gid)
	return userIdentity{uid: uid, primaryGID: gid, username: name, groups: groups, home: home}, nil
}

// idNumberViaNSS runs `id <flag> <name>` and parses its single decimal output.
func (p unixProvider) idNumberViaNSS(ctx context.Context, flag, name string) (uint32, error) {
	out, err := p.callRun(ctx, "id", flag, name)
	if err != nil {
		return 0, err
	}
	return parseID(strings.TrimSpace(string(out)), "id "+flag+" output")
}

// groupsViaNSS runs `id -G <name>` for the account's full supplementary
// group list, the one piece of data pure-Go os/user cannot get right for a
// directory-backed account. A real id(1) always prints at least the primary
// gid, even for an account with no supplementary groups, so empty output
// means the tool misbehaved (a wrong flag accepted as a no-op, a stuck NSS
// backend answering blank), not that the account has no groups. Treating it
// as success would return a credential with only the primary gid and no
// supplementary groups, the permission failure this fallback exists to
// prevent, so it is an error here, matching the implausible-empty rule
// resolveIdentityFromOSUser applies to the pure-Go path via
// groupsNeedFallback.
func (p unixProvider) groupsViaNSS(ctx context.Context, name string) ([]uint32, error) {
	out, err := p.callRun(ctx, "id", "-G", name)
	if err != nil {
		return nil, err
	}
	groups, err := parseGroupList(string(out))
	if err != nil {
		return nil, err
	}
	if len(groups) == 0 {
		return nil, fmt.Errorf("isolation: id -G %s returned no groups; a working id(1) always reports at least the primary gid", name)
	}
	return groups, nil
}

// homeViaNSS runs `getent passwd <name>` for the home directory. A failure,
// including getent(1) not existing (expected on macOS), is logged and
// ignored: home is left empty and the caller (the HOME rewrite) decides what
// to do, rather than identity resolution failing over a directory it can do
// without.
func (p unixProvider) homeViaNSS(ctx context.Context, name string) string {
	out, err := p.callRun(ctx, "getent", "passwd", name)
	if err != nil {
		p.logger.DebugContext(ctx, "isolation: getent passwd unavailable or failed, leaving home directory empty",
			"user", name, "error", err)
		return ""
	}
	home, ok := parseGetentPasswdHome(string(out), name)
	if !ok {
		p.logger.DebugContext(ctx, "isolation: could not parse getent passwd output, leaving home directory empty",
			"user", name)
		return ""
	}
	return home
}

// resolveGID resolves the gid a process must run under: the user's primary
// group by default, or an explicitly requested group. An explicit group must
// pass the same privileged-group refusal a privileged uid gets, and the
// target user must belong to it; accepting any group the spec
// names, whether or not the user is a member, would be as much an escalation
// as an unchecked privileged gid. This applies identically whether ident came
// from pure-Go os/user or the NSS fallback: a directory-resolved account must
// not get a weaker path than a local one.
//
// The empty-groupName (primary gid) branch does not run the full name-based
// CheckGroupNotPrivileged that the explicit-group branch below gets: on macOS
// "staff" is the ordinary primary group of every regular user account and is
// on privilegedGroupNames (as an explicit target it is the macOS
// admin-equivalent group), so that check would refuse every macOS account.
// gid 0 is refused whatever the platform or group name: queue config chooses
// only the username, and the username determines the primary group, so an
// account whose primary group is gid 0 is reachable from a queue with no
// explicit group involved.
func (p unixProvider) resolveGID(ctx context.Context, ident userIdentity, groupName string) (uint32, error) {
	if groupName == "" {
		if ident.primaryGID == 0 {
			return 0, fmt.Errorf("%w: user %q has primary gid 0", ErrRefusedPrivileged, ident.username)
		}
		return ident.primaryGID, nil
	}
	if err := validateAccountArg(groupName, "group"); err != nil {
		return 0, err
	}

	gid, err := p.groupGID(ctx, groupName)
	if err != nil {
		return 0, err
	}
	if err := CheckGroupNotPrivileged(groupName, gid); err != nil {
		return 0, err
	}
	if !isMember(ident, gid) {
		return 0, fmt.Errorf("isolation: user %q is not a member of group %q", ident.username, groupName)
	}
	return gid, nil
}

// groupGID resolves a group name to a gid, trying pure-Go os/user first and
// falling back to `getent group <name>` when that fails.
func (p unixProvider) groupGID(ctx context.Context, groupName string) (uint32, error) {
	g, err := user.LookupGroup(groupName)
	if err == nil {
		return parseID(g.Gid, "gid")
	}
	p.logger.DebugContext(ctx, "isolation: pure-go group lookup failed, falling back to NSS-aware tools",
		"group", groupName, "error", err)

	out, runErr := p.callRun(ctx, "getent", "group", groupName)
	if runErr != nil {
		return 0, fmt.Errorf("isolation: lookup group %q: pure-go lookup failed: %w; getent fallback failed: %w", groupName, err, runErr)
	}
	gid, ok := parseGetentGroupGID(string(out), groupName)
	if !ok {
		return 0, fmt.Errorf("isolation: lookup group %q: could not parse getent group output", groupName)
	}
	p.logger.InfoContext(ctx, "isolation: resolved group via NSS-aware fallback (getent)",
		"group", groupName, "gid", gid)
	return gid, nil
}

// isMember reports whether ident belongs to gid, either as its primary group
// or one of its supplementary ones.
func isMember(ident userIdentity, gid uint32) bool {
	if ident.primaryGID == gid {
		return true
	}
	return slices.Contains(ident.groups, gid)
}

// ensureMember appends gid to groups if it isn't already present, so an
// explicitly requested group always shows up in the credential's group list
// even on platforms or accounts where GroupIds() did not include it.
func ensureMember(groups []uint32, gid uint32) []uint32 {
	if slices.Contains(groups, gid) {
		return groups
	}
	return append(groups, gid)
}

// finalizeGroups builds the credential's final supplementary group list from
// a resolved identity and the gid the process will run as. Both resolution
// paths go through it before Resolve returns: resolveIdentityFromOSUser
// (pure-Go os/user, possibly topped up from the NSS group fallback) and
// resolveIdentityViaNSS (fully NSS, for accounts pure-Go cannot see) return a
// userIdentity of the same shape, and this is the only place its groups
// become a Credential's, so the gid-0 strip below applies whichever path
// produced ident.groups.
func finalizeGroups(ident userIdentity, gid uint32) []uint32 {
	return ensureMember(stripGID0FromSupplementary(ident.groups), gid)
}

// stripGID0FromSupplementary drops gid 0 unconditionally from a supplementary
// group list, whatever group name sits at gid 0 on this platform ("root" on
// Linux, "wheel" on macOS/BSD). GroupIds()/`id -G` pass a target account's
// supplementary memberships through unfiltered; the primary gid has its own
// check (resolveGID's "primaryGID == 0" refusal), and this applies the same
// rule to the supplementary set, so an account whose supplementary groups
// include gid 0 does not give the child process gid-0 membership.
//
// Unlike CheckGroupNotPrivileged, this strips gid 0 and nothing else. A
// supplementary membership in a named privileged group (docker, disk,
// shadow, wheel, ...) is left as GroupIds()/`id -G` reported it:
//
//   - Those memberships are the target account's own OS grants, not
//     something Spec or queue config hands out. Stripping them would narrow
//     what the account can do compared to running it directly: most
//     supplementary groups grant access to project storage, and a filter
//     broad enough to catch "docker" also catches a group like
//     "render-storage-ro".
//   - CheckGroupNotPrivileged gates only an explicit Spec.Group request,
//     which is what queue config can choose. An account that is already a
//     member of "docker" reaches the docker socket whether or not sqi is
//     involved.
//
// A target account's existing group memberships therefore reach job code.
// The operator must not point a queue's run-as-user at an account that is
// itself in a privileged group, as with running that account's crontab or an
// interactive session as that user.
func stripGID0FromSupplementary(groups []uint32) []uint32 {
	out := make([]uint32, 0, len(groups))
	for _, g := range groups {
		if g == 0 {
			continue
		}
		out = append(out, g)
	}
	return out
}

// supplementaryGroups resolves the target user's secondary groups. Dropping
// them would remove access to group-readable project storage, which shows up
// as permission errors inside a render.
func supplementaryGroups(u *user.User) ([]uint32, error) {
	ids, err := u.GroupIds()
	if err != nil {
		return nil, fmt.Errorf("isolation: group ids for %q: %w", u.Username, err)
	}
	out := make([]uint32, 0, len(ids))
	for _, s := range ids {
		g, err := parseID(s, "supplementary gid")
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, nil
}

// invalidIDSentinel is (uid_t)-1 / (gid_t)-1 (0xFFFFFFFF), the raw bit
// pattern produced when a caller-supplied -1 lands in an unsigned id field.
// setuid(2)/setgid(2) reject it at exec time, so it cannot itself escalate,
// but letting it through here would turn that into an opaque "exec failed"
// in the executor instead of a clear resolution-time error.
const invalidIDSentinel = 0xFFFFFFFF

// parseID parses a uid/gid string from the os/user package, which returns
// them as decimal strings regardless of platform.
func parseID(s, what string) (uint32, error) {
	v, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("isolation: parse %s %q: %w", what, s, err)
	}
	if v == invalidIDSentinel {
		return 0, fmt.Errorf("isolation: %s %q is the invalid id sentinel (uid_t)-1 / %d", what, s, invalidIDSentinel)
	}
	return uint32(v), nil
}

func (unixProvider) Capable() error {
	if syscall.Geteuid() != 0 {
		return fmt.Errorf("%w: worker must run as root to change task identity (euid=%d)",
			ErrNotCapable, syscall.Geteuid())
	}
	return nil
}

// newFakeCredential builds a Credential from a fake account for tests. It
// keeps Credential opaque outside this package while letting fake.go (which is
// platform-independent) hand back a real *Credential. It runs
// a.SupplementaryGIDs through the same finalizeGroups/stripGID0FromSupplementary
// logic the real provider's Resolve applies, so tests written against the
// fake cover supplementary-group handling (gid 0 stripped, a privileged named
// group's gid left alone).
//
// The account-name parameter is unused here (named _, per the
// unused-parameter lint rule): the POSIX Credential switches only on
// uid/gid. It keeps the signature the same as provider_windows.go's, which
// needs the name, because fake.go, which calls this function, has no build
// tag.
func newFakeCredential(_ string, a FakeAccount) *Credential {
	groups := ensureMember(stripGID0FromSupplementary(a.SupplementaryGIDs), a.GID)
	return &Credential{
		cred: &syscall.Credential{Uid: a.UID, Gid: a.GID, Groups: groups},
		Home: a.Home,
	}
}
