// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build unix

package isolation

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// fallbackUser is a username guaranteed not to exist as a real local or
// NSS-visible account on the machine running these tests, so
// user.Lookup(fallbackUser) always fails and Resolve always takes the
// NSS-aware fallback path — deterministically, on any box, with or without
// cgo. That determinism is exactly what makes "the fallback path enforces
// every refusal" provable without a real directory server.
const fallbackUser = "sqi-nss-fallback-test-user"

// nssResponse is one canned answer a fakeRunner hands back for a specific
// argv, so tests never depend on id(1)/getent(1) actually being installed or
// on any real account existing.
type nssResponse struct {
	out string
	err error
}

// fakeRunner returns a cmdRunner that serves canned output keyed by the exact
// argv it's called with, failing the test on any call it wasn't told to
// expect. This is the injection point the fallback design relies on: every
// production code path in this file reaches id(1)/getent(1) only through
// unixProvider.run, so swapping that one field is enough to make the whole
// fallback hermetic.
func fakeRunner(t *testing.T, responses map[string]nssResponse) cmdRunner {
	t.Helper()
	return func(_ context.Context, name string, args ...string) ([]byte, error) {
		key := name + " " + strings.Join(args, " ")
		resp, ok := responses[key]
		if !ok {
			t.Fatalf("unexpected command: %s", key)
		}
		if resp.err != nil {
			return nil, resp.err
		}
		return []byte(resp.out), nil
	}
}

func newFallbackTestProvider(run cmdRunner) unixProvider {
	return unixProvider{run: run, logger: slog.New(slog.DiscardHandler)}
}

// warnLevelRecorder is a minimal slog.Handler that records whether any
// record at slog.LevelWarn or above was emitted, for tests asserting that a
// given code path logs at WARN rather than a quieter level.
type warnLevelRecorder struct {
	sawWarn bool
}

func (*warnLevelRecorder) Enabled(context.Context, slog.Level) bool { return true }

func (r *warnLevelRecorder) Handle(_ context.Context, rec slog.Record) error {
	if rec.Level >= slog.LevelWarn {
		r.sawWarn = true
	}
	return nil
}

func (r *warnLevelRecorder) WithAttrs([]slog.Attr) slog.Handler { return r }

func (r *warnLevelRecorder) WithGroup(string) slog.Handler { return r }

// --- username/group validation -------------------------------------------

func TestValidateAccountArgRejections(t *testing.T) {
	cases := []struct {
		name string
		arg  string
	}{
		{"empty", ""},
		{"leading dash", "-rf"},
		{"embedded newline", "a\nb"},
		{"embedded NUL", "a\x00b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateAccountArg(tc.arg, "user"); err == nil {
				t.Errorf("validateAccountArg(%q) = nil, want an error", tc.arg)
			}
		})
	}
}

func TestValidateAccountArgAllowsOrdinaryNames(t *testing.T) {
	for _, name := range []string{"render-svc", "render.svc", "render_svc01"} {
		if err := validateAccountArg(name, "user"); err != nil {
			t.Errorf("validateAccountArg(%q) = %v, want nil", name, err)
		}
	}
}

// TestUnixProviderRejectsInvalidUsernameBeforeAnyLookup proves validation
// happens before spec.User is used for anything at all — including the
// pure-Go path — not merely before a subprocess launch. The injected runner
// panics via t.Fatalf on any call, so this also proves no command is ever
// invoked for a rejected username.
func TestUnixProviderRejectsInvalidUsernameBeforeAnyLookup(t *testing.T) {
	p := newFallbackTestProvider(fakeRunner(t, nil))
	for _, name := range []string{"", "-rf", "a\nb", "a\x00b"} {
		if _, err := p.Resolve(context.Background(), Spec{User: name}); err == nil {
			t.Errorf("Resolve(User=%q) = nil, want a validation error", name)
		}
	}
}

// --- parsers ---------------------------------------------------------------

func TestParseGroupList(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want []uint32
	}{
		{"space separated multi-gid", "1001 2001 2010\n", []uint32{1001, 2001, 2010}},
		{"single gid", "1001\n", []uint32{1001}},
		{"no trailing newline", "1001 2001", []uint32{1001, 2001}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseGroupList(tc.out)
			if err != nil {
				t.Fatalf("parseGroupList(%q): %v", tc.out, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("parseGroupList(%q) = %v, want %v", tc.out, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("parseGroupList(%q) = %v, want %v", tc.out, got, tc.want)
				}
			}
		})
	}
}

func TestParseGroupListRejectsGarbage(t *testing.T) {
	if _, err := parseGroupList("1001 not-a-gid\n"); err == nil {
		t.Error("parseGroupList with a non-numeric field must fail")
	}
}

func TestParseGetentPasswdHome(t *testing.T) {
	home, ok := parseGetentPasswdHome("render-svc:x:1001:2001::/home/render-svc:/bin/bash\n", "render-svc")
	if !ok || home != "/home/render-svc" {
		t.Errorf("parseGetentPasswdHome = (%q, %v), want (/home/render-svc, true)", home, ok)
	}
	if _, ok := parseGetentPasswdHome("too:few:fields\n", "too"); ok {
		t.Error("parseGetentPasswdHome on a short line must report ok=false")
	}
}

// TestParseGetentPasswdHomeRejectsNameMismatch is the reproduction from the
// review: a getent line that parses fine but names a DIFFERENT account than
// the one requested must not be accepted — that's an NSS-aliasing bug (or
// attack surface) handing back the wrong account's home directory under the
// requested name.
func TestParseGetentPasswdHomeRejectsNameMismatch(t *testing.T) {
	if _, ok := parseGetentPasswdHome("other-user:x:1001:2001::/home/other-user:/bin/bash\n", "render-svc"); ok {
		t.Error("parseGetentPasswdHome must reject a line naming a different account than requested")
	}
}

// TestParseGetentPasswdHomeAcceptsCaseDifferingName is the Important-2 fix's
// reproduction: AD and many LDAP backends are case-insensitive and
// canonicalize the name they return, so `getent passwd RenderSvc` can
// legitimately answer with a differently-cased "rendersvc:...". Refusing
// that as a name mismatch would fail a correctly-configured AD-backed farm —
// the exact deployment the NSS fallback exists to serve.
func TestParseGetentPasswdHomeAcceptsCaseDifferingName(t *testing.T) {
	home, ok := parseGetentPasswdHome("rendersvc:x:1001:2001::/home/rendersvc:/bin/bash\n", "RenderSvc")
	if !ok || home != "/home/rendersvc" {
		t.Errorf("parseGetentPasswdHome(case-differing name) = (%q, %v), want (/home/rendersvc, true)", home, ok)
	}
}

func TestParseGetentGroupGID(t *testing.T) {
	gid, ok := parseGetentGroupGID("render:x:2010:render-svc,other-user\n", "render")
	if !ok || gid != 2010 {
		t.Errorf("parseGetentGroupGID = (%d, %v), want (2010, true)", gid, ok)
	}
	if _, ok := parseGetentGroupGID("too:few\n", "too"); ok {
		t.Error("parseGetentGroupGID on a short line must report ok=false")
	}
}

// TestParseGetentGroupGIDRejectsNameMismatch is the review's confirmed
// repro ("C: ACCEPTED gid=2010 from a getent line naming a different
// group"): a syntactically valid getent group line for the WRONG group must
// not be accepted as an answer for the requested one.
func TestParseGetentGroupGIDRejectsNameMismatch(t *testing.T) {
	if _, ok := parseGetentGroupGID("other-group:x:2010:someone\n", "render"); ok {
		t.Error("parseGetentGroupGID must reject a line naming a different group than requested")
	}
}

// TestParseGetentGroupGIDAcceptsCaseDifferingName is the Important-2 fix's
// direct reproduction of the review's confirmed repro ("K: REFUSED ...
// lookup group \"Renderers\": could not parse getent group output"):
// `getent group Renderers` legitimately answering "renderers:x:3000:" on an
// AD/LDAP-backed system must be accepted, not refused as a name mismatch.
func TestParseGetentGroupGIDAcceptsCaseDifferingName(t *testing.T) {
	gid, ok := parseGetentGroupGID("renderers:x:3000:\n", "Renderers")
	if !ok || gid != 3000 {
		t.Errorf("parseGetentGroupGID(case-differing name) = (%d, %v), want (3000, true)", gid, ok)
	}
}

// --- groupsNeedFallback decision --------------------------------------------

func TestGroupsNeedFallback(t *testing.T) {
	cases := []struct {
		name   string
		groups []uint32
		err    error
		want   bool
	}{
		{"error", nil, errors.New("boom"), true},
		{"empty no error", []uint32{}, nil, true},
		{"nil no error", nil, nil, true},
		{"non-empty", []uint32{2001}, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := groupsNeedFallback(tc.groups, tc.err); got != tc.want {
				t.Errorf("groupsNeedFallback(%v, %v) = %v, want %v", tc.groups, tc.err, got, tc.want)
			}
		})
	}
}

// --- fallback identity resolution ------------------------------------------

func TestUnixProviderFallbackResolvesFullIdentity(t *testing.T) {
	run := fakeRunner(t, map[string]nssResponse{
		"id -u " + fallbackUser:         {out: "1001\n"},
		"id -g " + fallbackUser:         {out: "2001\n"},
		"id -G " + fallbackUser:         {out: "2001 2010\n"},
		"getent passwd " + fallbackUser: {out: fallbackUser + ":x:1001:2001::/home/" + fallbackUser + ":/bin/bash\n"},
	})
	p := newFallbackTestProvider(run)

	cred, err := p.Resolve(context.Background(), Spec{User: fallbackUser})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cred.cred.Uid != 1001 {
		t.Errorf("Uid = %d, want 1001", cred.cred.Uid)
	}
	if cred.cred.Gid != 2001 {
		t.Errorf("Gid = %d, want 2001", cred.cred.Gid)
	}
	wantGroups := []uint32{2001, 2010}
	if len(cred.cred.Groups) != len(wantGroups) {
		t.Fatalf("Groups = %v, want %v", cred.cred.Groups, wantGroups)
	}
	for i, g := range wantGroups {
		if cred.cred.Groups[i] != g {
			t.Fatalf("Groups = %v, want %v", cred.cred.Groups, wantGroups)
		}
	}
	if cred.Home != "/home/"+fallbackUser {
		t.Errorf("Home = %q, want /home/%s", cred.Home, fallbackUser)
	}
}

// TestUnixProviderFallbackHomeUnavailableIsNonFatal proves getent(1) being
// entirely absent (the normal case on macOS) degrades gracefully: Resolve
// still succeeds, just with an empty Home, rather than failing the whole
// identity resolution over a directory it doesn't strictly need.
func TestUnixProviderFallbackHomeUnavailableIsNonFatal(t *testing.T) {
	run := fakeRunner(t, map[string]nssResponse{
		"id -u " + fallbackUser:         {out: "1001\n"},
		"id -g " + fallbackUser:         {out: "2001\n"},
		"id -G " + fallbackUser:         {out: "2001\n"},
		"getent passwd " + fallbackUser: {err: errors.New("exec: \"getent\": executable file not found in $PATH")},
	})
	p := newFallbackTestProvider(run)

	cred, err := p.Resolve(context.Background(), Spec{User: fallbackUser})
	if err != nil {
		t.Fatalf("Resolve: %v, want nil (getent absence must not be fatal)", err)
	}
	if cred.Home != "" {
		t.Errorf("Home = %q, want empty when getent is unavailable", cred.Home)
	}
}

// --- fallback path enforces every refusal -----------------------------------
//
// These mirror TestUnixProviderRefusesRoot / RefusesPrivilegedGroup /
// RefusesGroupUserIsNotAMemberOf in provider_unix_test.go, but force
// resolution through the NSS-aware fallback (via fallbackUser, which no real
// os/user.Lookup can ever satisfy) instead of the pure-Go path. A directory-
// resolved account must not get a weaker path than a local one: these prove
// CheckNotPrivileged / CheckGroupNotPrivileged / the membership check all
// still run on whatever id(1)/getent(1) hand back.

func TestUnixProviderFallbackRefusesRootUID(t *testing.T) {
	run := fakeRunner(t, map[string]nssResponse{
		"id -u " + fallbackUser:         {out: "0\n"},
		"id -g " + fallbackUser:         {out: "0\n"},
		"id -G " + fallbackUser:         {out: "0\n"},
		"getent passwd " + fallbackUser: {err: errors.New("exec: \"getent\": executable file not found in $PATH")},
	})
	p := newFallbackTestProvider(run)

	_, err := p.Resolve(context.Background(), Spec{User: fallbackUser})
	if !errors.Is(err, ErrRefusedPrivileged) {
		t.Fatalf("Resolve(uid=0 via fallback) = %v, want ErrRefusedPrivileged", err)
	}
}

func TestUnixProviderFallbackRefusesPrivilegedGroupByGID(t *testing.T) {
	const group = "sqi-nss-fallback-test-group"
	run := fakeRunner(t, map[string]nssResponse{
		"id -u " + fallbackUser:         {out: "1001\n"},
		"id -g " + fallbackUser:         {out: "2001\n"},
		"id -G " + fallbackUser:         {out: "2001 2010\n"},
		"getent passwd " + fallbackUser: {err: errors.New("exec: \"getent\": executable file not found in $PATH")},
		"getent group " + group:         {out: group + ":x:0:" + fallbackUser + "\n"},
	})
	p := newFallbackTestProvider(run)

	_, err := p.Resolve(context.Background(), Spec{User: fallbackUser, Group: group})
	if !errors.Is(err, ErrRefusedPrivileged) {
		t.Fatalf("Resolve(group gid=0 via fallback) = %v, want ErrRefusedPrivileged", err)
	}
}

func TestUnixProviderFallbackRefusesNonMemberGroup(t *testing.T) {
	const group = "sqi-nss-fallback-test-other-group"
	run := fakeRunner(t, map[string]nssResponse{
		"id -u " + fallbackUser:         {out: "1001\n"},
		"id -g " + fallbackUser:         {out: "2001\n"},
		"id -G " + fallbackUser:         {out: "2001 2010\n"}, // does not include 3000
		"getent passwd " + fallbackUser: {err: errors.New("exec: \"getent\": executable file not found in $PATH")},
		"getent group " + group:         {out: group + ":x:3000:someone-else\n"},
	})
	p := newFallbackTestProvider(run)

	_, err := p.Resolve(context.Background(), Spec{User: fallbackUser, Group: group})
	if err == nil {
		t.Fatal("Resolve(group the fallback-resolved user is not a member of) = nil, want an error")
	}
	if errors.Is(err, ErrRefusedPrivileged) {
		t.Fatalf("Resolve = %v, want a membership error, not ErrRefusedPrivileged (group is not privileged)", err)
	}
}

// TestUnixProviderFallbackGroupUnavailableFailsClosed proves that when an
// explicit group is requested but neither pure-Go os/user nor getent(1) can
// resolve it (e.g. macOS with no getent binary and no local group entry),
// Resolve fails rather than silently proceeding without verifying membership
// — the one place "degrade gracefully" must NOT apply, unlike the home
// directory.
func TestUnixProviderFallbackGroupUnavailableFailsClosed(t *testing.T) {
	const group = "sqi-nss-fallback-test-unresolvable-group"
	run := fakeRunner(t, map[string]nssResponse{
		"id -u " + fallbackUser:         {out: "1001\n"},
		"id -g " + fallbackUser:         {out: "2001\n"},
		"id -G " + fallbackUser:         {out: "2001\n"},
		"getent passwd " + fallbackUser: {err: errors.New("exec: \"getent\": executable file not found in $PATH")},
		"getent group " + group:         {err: errors.New("exec: \"getent\": executable file not found in $PATH")},
	})
	p := newFallbackTestProvider(run)

	if _, err := p.Resolve(context.Background(), Spec{User: fallbackUser, Group: group}); err == nil {
		t.Fatal("Resolve with an unresolvable explicit group must fail, never proceed unisolated")
	}
}

// --- primary gid 0 (Critical 1) ---------------------------------------------

// TestResolveGIDRefusesPrimaryGIDZero is the direct unit reproduction of the
// review's confirmed repro ("A: ACCEPTED uid=1001 GID=0 groups=[0 2010]"): an
// account whose PRIMARY group is gid 0, with no explicit Spec.Group involved
// at all, must be refused.
func TestResolveGIDRefusesPrimaryGIDZero(t *testing.T) {
	p := newFallbackTestProvider(fakeRunner(t, nil))
	ident := userIdentity{uid: 1001, primaryGID: 0, username: "someuser"}

	if _, err := p.resolveGID(context.Background(), ident, ""); !errors.Is(err, ErrRefusedPrivileged) {
		t.Fatalf("resolveGID(primaryGID=0, group=\"\") = %v, want ErrRefusedPrivileged", err)
	}
}

// TestResolveGIDAllowsOrdinaryStaffStylePrimaryGID guards against
// over-correcting Critical 1: "staff" is on privilegedGroupNames (it's the
// macOS admin-equivalent group when granted as an explicit target) AND is
// the ordinary primary group of every regular macOS user account. Running
// the full name-based CheckGroupNotPrivileged on the primary-gid branch
// would refuse every macOS account outright; only gid 0 itself may be
// refused there.
func TestResolveGIDAllowsOrdinaryStaffStylePrimaryGID(t *testing.T) {
	p := newFallbackTestProvider(fakeRunner(t, nil))
	const macOSStaffGID = 20 // "staff" on macOS/BSD
	ident := userIdentity{uid: 1001, primaryGID: macOSStaffGID, username: "someuser"}

	gid, err := p.resolveGID(context.Background(), ident, "")
	if err != nil {
		t.Fatalf("resolveGID(primaryGID=%d/staff, group=\"\") = %v, want nil", macOSStaffGID, err)
	}
	if gid != macOSStaffGID {
		t.Fatalf("resolveGID = %d, want %d", gid, macOSStaffGID)
	}
}

// TestUnixProviderFallbackRefusesPrimaryGIDZero proves the same refusal end
// to end through Resolve via the NSS fallback path (id -g), not just at the
// resolveGID unit level — matching the review's exact repro shape: no
// explicit group requested, primary gid resolves to 0.
func TestUnixProviderFallbackRefusesPrimaryGIDZero(t *testing.T) {
	run := fakeRunner(t, map[string]nssResponse{
		"id -u " + fallbackUser:         {out: "1001\n"},
		"id -g " + fallbackUser:         {out: "0\n"},
		"id -G " + fallbackUser:         {out: "0 2010\n"},
		"getent passwd " + fallbackUser: {err: errors.New("exec: \"getent\": executable file not found in $PATH")},
	})
	p := newFallbackTestProvider(run)

	_, err := p.Resolve(context.Background(), Spec{User: fallbackUser})
	if !errors.Is(err, ErrRefusedPrivileged) {
		t.Fatalf("Resolve(primary gid=0 via fallback) = %v, want ErrRefusedPrivileged", err)
	}
}

// TestUnixProviderFallbackAllowsOrdinaryStaffStylePrimaryGID is the Resolve-
// level companion to TestResolveGIDAllowsOrdinaryStaffStylePrimaryGID,
// proving the fix doesn't over-correct through the full fallback path either.
func TestUnixProviderFallbackAllowsOrdinaryStaffStylePrimaryGID(t *testing.T) {
	const macOSStaffGID = "20"
	run := fakeRunner(t, map[string]nssResponse{
		"id -u " + fallbackUser:         {out: "1001\n"},
		"id -g " + fallbackUser:         {out: macOSStaffGID + "\n"},
		"id -G " + fallbackUser:         {out: macOSStaffGID + "\n"},
		"getent passwd " + fallbackUser: {err: errors.New("exec: \"getent\": executable file not found in $PATH")},
	})
	p := newFallbackTestProvider(run)

	cred, err := p.Resolve(context.Background(), Spec{User: fallbackUser})
	if err != nil {
		t.Fatalf("Resolve(primary gid=20/staff via fallback) = %v, want nil", err)
	}
	if cred.cred.Gid != 20 {
		t.Fatalf("Gid = %d, want 20", cred.cred.Gid)
	}
}

// --- NSS group-list implausible-empty judgement (Important 2) --------------

// TestGroupsViaNSSRejectsEmptyOutput is the direct unit reproduction of the
// review's confirmed repro ("B: ACCEPTED with groups=[2001] (empty id -G
// silently accepted)"): id -G producing entirely empty output means the tool
// misbehaved (a real id(1) always reports at least the primary gid), and
// must surface as an error rather than a silently-accepted empty group list.
func TestGroupsViaNSSRejectsEmptyOutput(t *testing.T) {
	run := fakeRunner(t, map[string]nssResponse{
		"id -G someuser": {out: ""},
	})
	p := newFallbackTestProvider(run)

	if _, err := p.groupsViaNSS(context.Background(), "someuser"); err == nil {
		t.Fatal("groupsViaNSS with entirely empty id -G output = nil error, want an error")
	}
}

// TestUnixProviderFallbackRejectsEmptyGroupList proves the same judgement
// end to end through Resolve via the full NSS fallback path (an account
// pure-Go os/user cannot see at all): empty id -G output must fail identity
// resolution rather than silently producing a credential with only the
// primary gid and every supplementary group dropped.
func TestUnixProviderFallbackRejectsEmptyGroupList(t *testing.T) {
	run := fakeRunner(t, map[string]nssResponse{
		"id -u " + fallbackUser: {out: "1001\n"},
		"id -g " + fallbackUser: {out: "2001\n"},
		"id -G " + fallbackUser: {out: ""},
	})
	p := newFallbackTestProvider(run)

	if _, err := p.Resolve(context.Background(), Spec{User: fallbackUser}); err == nil {
		t.Fatal("Resolve with empty id -G output via fallback = nil, want an error")
	}
}

// --- absolute NSS tool paths (Important 3) ----------------------------------

func TestResolveToolPathPrefersExistingCandidate(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "id")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\necho hi\n"), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got := resolveToolPath(context.Background(), slog.New(slog.DiscardHandler), "id", []string{fake})
	if got != fake {
		t.Errorf("resolveToolPath = %q, want %q", got, fake)
	}
}

func TestResolveToolPathFallsBackToBareNameWhenUnresolvable(t *testing.T) {
	const missing = "sqi-test-tool-that-does-not-exist-anywhere"
	got := resolveToolPath(context.Background(), slog.New(slog.DiscardHandler), missing, []string{"/no/such/path/" + missing})
	if got != missing {
		t.Errorf("resolveToolPath fallback = %q, want bare name %q when no candidate exists and LookPath also fails", got, missing)
	}
}

// TestResolveToolPathLogsWarnOnFallback is the Minor-4 fix: falling back to a
// $PATH search for a tool this package is about to run as root must be
// visible at WARN, not buried at Debug, since it silently re-opens the
// trojan-binary exposure nssToolCandidates exists to close.
func TestResolveToolPathLogsWarnOnFallback(t *testing.T) {
	var rec warnLevelRecorder
	logger := slog.New(&rec)

	resolveToolPath(context.Background(), logger, "id", []string{"/no/such/path/id"})

	if !rec.sawWarn {
		t.Error("resolveToolPath falling back to a PATH search must log at WARN")
	}
}

func TestNewNSSCmdSetsWaitDelay(t *testing.T) {
	cmd := newNSSCmd(context.Background(), slog.New(slog.DiscardHandler), "id", "-u", "someuser")
	if cmd.WaitDelay != nssCommandTimeout {
		t.Errorf("WaitDelay = %v, want %v", cmd.WaitDelay, nssCommandTimeout)
	}
}

// TestRunNSSCommandCapsOutput proves a pathological NSS backend that prints
// far more than any real id(1)/getent(1) answer can't make runNSSCommand
// buffer unbounded output: the write is rejected once the cap is exceeded,
// rather than growing without limit.
func TestRunNSSCommandCapsOutput(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "big-output.sh")
	body := "#!/bin/sh\nhead -c " + strconv.Itoa(nssOutputCap*2) + " /dev/zero | tr '\\0' 'a'\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := runNSSCommand(context.Background(), slog.New(slog.DiscardHandler), script)
	if err == nil {
		t.Fatal("runNSSCommand with output exceeding the cap = nil, want an error")
	}
}

// --- getent runErr not discarded (Minor 7) ----------------------------------

// TestGroupGIDIncludesGetentFailureReason proves the getent(1) failure
// reason reaches the returned error rather than being discarded in favor of
// only the pure-Go os/user.LookupGroup error, which — for a group that only
// exists in a directory getent can't see either — previously surfaced no
// information about why the fallback itself failed.
func TestGroupGIDIncludesGetentFailureReason(t *testing.T) {
	const wantSubstring = "getent backend exploded"
	run := fakeRunner(t, map[string]nssResponse{
		"getent group sqi-test-no-such-group-xyz": {err: errors.New(wantSubstring)},
	})
	p := newFallbackTestProvider(run)

	_, err := p.groupGID(context.Background(), "sqi-test-no-such-group-xyz")
	if err == nil {
		t.Fatal("groupGID for a nonexistent group = nil error, want an error")
	}
	if !strings.Contains(err.Error(), wantSubstring) {
		t.Errorf("groupGID error = %q, want it to include the getent failure reason %q", err.Error(), wantSubstring)
	}
}

// TestGroupGIDAcceptsCaseDifferingGetentAnswer is the end-to-end companion to
// TestParseGetentGroupGIDAcceptsCaseDifferingName, proving the fix through
// groupGID's actual getent fallback wiring: a group name that pure-Go
// os/user.LookupGroup can't resolve (forcing the getent fallback), answered
// by getent(1) with a differently-cased, canonicalized name — the AD/LDAP
// case from the review — must resolve successfully rather than fail with
// "could not parse getent group output".
func TestGroupGIDAcceptsCaseDifferingGetentAnswer(t *testing.T) {
	const group = "sqi-test-no-such-group-Renderers"
	run := fakeRunner(t, map[string]nssResponse{
		"getent group " + group: {out: strings.ToLower(group) + ":x:3000:\n"},
	})
	p := newFallbackTestProvider(run)

	gid, err := p.groupGID(context.Background(), group)
	if err != nil {
		t.Fatalf("groupGID(case-differing getent answer) = %v, want nil", err)
	}
	if gid != 3000 {
		t.Errorf("groupGID = %d, want 3000", gid)
	}
}

// --- id(1)/getent(1) sentinel id rejection (Minor 8) ------------------------

func TestParseIDRejectsInvalidSentinel(t *testing.T) {
	if _, err := parseID("4294967295", "uid"); err == nil {
		t.Error("parseID(4294967295) = nil, want an error: that's (uid_t)-1, not a real id")
	}
}

func TestParseIDAcceptsOrdinaryValues(t *testing.T) {
	got, err := parseID("1001", "uid")
	if err != nil {
		t.Fatalf("parseID(1001) = %v, want nil", err)
	}
	if got != 1001 {
		t.Errorf("parseID(1001) = %d, want 1001", got)
	}
}

func TestUnixProviderFallbackRejectsSentinelUID(t *testing.T) {
	run := fakeRunner(t, map[string]nssResponse{
		"id -u " + fallbackUser: {out: "4294967295\n"},
	})
	p := newFallbackTestProvider(run)

	if _, err := p.Resolve(context.Background(), Spec{User: fallbackUser}); err == nil {
		t.Fatal("Resolve with id -u returning the invalid sentinel = nil, want an error")
	}
}

// --- zero-value provider does not panic (Minor 10) --------------------------

// TestZeroValueProviderDoesNotPanic proves a unixProvider{} (nil run, nil
// logger) fails cleanly on the fallback path instead of a nil-pointer panic
// on p.run(...). fallbackUser forces the fallback path deterministically.
func TestZeroValueProviderDoesNotPanic(t *testing.T) {
	var p unixProvider
	p.logger = slog.New(slog.DiscardHandler) // avoid a nil-logger panic unrelated to what this test targets

	_, err := p.Resolve(context.Background(), Spec{User: fallbackUser})
	if err == nil {
		t.Fatal("Resolve on a zero-value unixProvider (nil run) = nil, want an error")
	}
}

// --- stderr captured on a failing NSS command (Minor 3) ---------------------

// TestRunNSSCommandIncludesStderrInError proves a failing id(1)/getent(1)
// invocation's own error text reaches the caller. Before this fix,
// runNSSCommand set cmd.Stdout to a custom writer but left cmd.Stderr nil,
// which — unlike cmd.Output() — means *exec.ExitError.Stderr is never
// populated, so a failing tool logged strictly less than it used to.
func TestRunNSSCommandIncludesStderrInError(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fail.sh")
	body := "#!/bin/sh\necho 'boom from stderr' >&2\nexit 1\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := runNSSCommand(context.Background(), slog.New(slog.DiscardHandler), script)
	if err == nil {
		t.Fatal("runNSSCommand with a failing command = nil error, want an error")
	}
	if !strings.Contains(err.Error(), "boom from stderr") {
		t.Errorf("runNSSCommand error = %q, want it to include the command's stderr text", err.Error())
	}
}

// --- supplementary-group gid-0 stripping (Important 1) ----------------------
//
// ident.groups (from GroupIds() or `id -G`) previously passed through
// finalizeGroups completely unfiltered, so an account whose supplementary
// groups happened to include gid 0 handed the child process gid-0 group
// membership even though every other check in this package refuses gid 0 as
// an explicit target. These tests pin: gid 0 is dropped unconditionally, an
// ordinary non-privileged supplementary gid survives untouched, and — the
// deliberate part of the decision, not an oversight — a privileged NAMED
// group's gid (e.g. 999, docker's gid on many distros) also survives, so a
// later reader does not "fix" that away. See stripGID0FromSupplementary's
// doc comment in provider_unix.go for the full rationale.

// TestStripGID0FromSupplementary is the direct unit reproduction of the
// review's confirmed repro shapes ("E: ACCEPTED gid=2001 Groups=[2001 0
// 2010]" and "F: ACCEPTED Groups=[2001 999]"): it exercises the single
// function both the pure-Go (resolveIdentityFromOSUser) and NSS-aware
// (resolveIdentityViaNSS) identity paths funnel through via finalizeGroups
// before Resolve ever builds a Credential, so this one test covers both.
func TestStripGID0FromSupplementary(t *testing.T) {
	cases := []struct {
		name   string
		groups []uint32
		want   []uint32
	}{
		{"gid 0 dropped", []uint32{2001, 0, 2010}, []uint32{2001, 2010}},
		{"non-privileged gid survives untouched", []uint32{2001, 2010}, []uint32{2001, 2010}},
		{
			"privileged named group's gid survives by design (docker-style 999)",
			[]uint32{2001, 999},
			[]uint32{2001, 999},
		},
		{"all zero", []uint32{0, 0}, []uint32{}},
		{"empty", nil, []uint32{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := stripGID0FromSupplementary(tc.groups)
			if len(got) != len(tc.want) {
				t.Fatalf("stripGID0FromSupplementary(%v) = %v, want %v", tc.groups, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("stripGID0FromSupplementary(%v) = %v, want %v", tc.groups, got, tc.want)
				}
			}
		})
	}
}

// TestUnixProviderFallbackStripsGID0FromSupplementaryGroups proves the strip
// end to end through Resolve via the NSS-aware fallback path (`id -G`
// reporting "2001 0 2010"), matching the review's exact repro shape E.
func TestUnixProviderFallbackStripsGID0FromSupplementaryGroups(t *testing.T) {
	run := fakeRunner(t, map[string]nssResponse{
		"id -u " + fallbackUser:         {out: "1001\n"},
		"id -g " + fallbackUser:         {out: "2001\n"},
		"id -G " + fallbackUser:         {out: "2001 0 2010\n"},
		"getent passwd " + fallbackUser: {err: errors.New("exec: \"getent\": executable file not found in $PATH")},
	})
	p := newFallbackTestProvider(run)

	cred, err := p.Resolve(context.Background(), Spec{User: fallbackUser})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	for _, g := range cred.cred.Groups {
		if g == 0 {
			t.Fatalf("Groups = %v, want gid 0 stripped from the supplementary set", cred.cred.Groups)
		}
	}
	wantGroups := []uint32{2001, 2010}
	if len(cred.cred.Groups) != len(wantGroups) {
		t.Fatalf("Groups = %v, want %v", cred.cred.Groups, wantGroups)
	}
	for i, g := range wantGroups {
		if cred.cred.Groups[i] != g {
			t.Fatalf("Groups = %v, want %v", cred.cred.Groups, wantGroups)
		}
	}
}

// TestUnixProviderFallbackPreservesPrivilegedNamedGroupGID is the Resolve-
// level companion proving the deliberate half of the decision: a
// supplementary gid that happens to belong to a privileged NAMED group
// (999, matching repro F) is NOT stripped — only literal gid 0 is.
func TestUnixProviderFallbackPreservesPrivilegedNamedGroupGID(t *testing.T) {
	run := fakeRunner(t, map[string]nssResponse{
		"id -u " + fallbackUser:         {out: "1001\n"},
		"id -g " + fallbackUser:         {out: "2001\n"},
		"id -G " + fallbackUser:         {out: "2001 999\n"},
		"getent passwd " + fallbackUser: {err: errors.New("exec: \"getent\": executable file not found in $PATH")},
	})
	p := newFallbackTestProvider(run)

	cred, err := p.Resolve(context.Background(), Spec{User: fallbackUser})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	wantGroups := []uint32{2001, 999}
	if len(cred.cred.Groups) != len(wantGroups) {
		t.Fatalf("Groups = %v, want %v (privileged named group's gid must survive)", cred.cred.Groups, wantGroups)
	}
	for i, g := range wantGroups {
		if cred.cred.Groups[i] != g {
			t.Fatalf("Groups = %v, want %v", cred.cred.Groups, wantGroups)
		}
	}
}
