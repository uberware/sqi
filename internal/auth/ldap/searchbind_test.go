// SPDX-License-Identifier: AGPL-3.0-or-later

package ldap

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	ldapv3 "github.com/go-ldap/ldap/v3"
)

func searchCfg() Config {
	return Config{
		URL:             "ldap://dc.example.com:389",
		Timeout:         5 * time.Second,
		BindDN:          "CN=svc,DC=example,DC=com",
		BindPassword:    "svcpass",
		BaseDN:          "DC=example,DC=com",
		UserFilter:      "(sAMAccountName=%s)",
		UsernameAttr:    "sAMAccountName",
		DisplayNameAttr: "displayName",
		RoleSource:      RoleSourceDirectory,
		DefaultRole:     "read-only",
		RoleMap:         []RoleMapping{{Group: "CN=Admins,DC=example,DC=com", Role: "admin"}},
	}
}

func aliceEntry() *ldapv3.Entry {
	return entry("CN=Alice,OU=People,DC=example,DC=com", map[string][]string{
		"sAMAccountName": {"alice"},
		"displayName":    {"Alice Anderson"},
		"memberOf":       {"CN=Admins,DC=example,DC=com", "CN=Artists,DC=example,DC=com"},
	})
}

func TestSearchBind_Success(t *testing.T) {
	fc := &fakeConn{searchResult: &ldapv3.SearchResult{Entries: []*ldapv3.Entry{aliceEntry()}}}
	v := newSearchBind(searchCfg(), dialTo(fc))

	id, err := v.Verify(context.Background(), "alice", "alice-secret")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if id.DN != "CN=Alice,OU=People,DC=example,DC=com" {
		t.Errorf("DN: got %q", id.DN)
	}
	if id.Username != "alice" {
		t.Errorf("Username: got %q, want alice", id.Username)
	}
	if id.DisplayName != "Alice Anderson" {
		t.Errorf("DisplayName: got %q", id.DisplayName)
	}
	if len(id.Groups) != 2 {
		t.Errorf("Groups: got %v", id.Groups)
	}
	// Service account first, then a re-bind as the user. If the second bind is
	// missing, any password would authenticate.
	//
	// Both the DN *and* the password are pinned. The DN alone proves only that
	// a second bind happened; it is the password that authenticates, and a
	// re-bind carrying the service account's password (or "", or a constant)
	// would authenticate every user in the directory while leaving a DN-only
	// assertion perfectly green.
	want := []bindKey{
		{DN: "CN=svc,DC=example,DC=com", PW: "svcpass"},
		{DN: "CN=Alice,OU=People,DC=example,DC=com", PW: "alice-secret"},
	}
	if len(fc.binds) != 2 || fc.binds[0] != want[0] || fc.binds[1] != want[1] {
		t.Errorf("binds: got %v, want %v", fc.binds, want)
	}
	// Stated separately from the slice compare so a regression names itself
	// instead of printing two structs and leaving the reader to diff them.
	if len(fc.binds) == 2 {
		switch pw := fc.binds[1].PW; pw {
		case "alice-secret": // correct: the password the caller supplied
		case searchCfg().BindPassword:
			t.Errorf("the user re-bind sent the SERVICE ACCOUNT password %q: every user would authenticate with valid service credentials", pw)
		case "":
			t.Error("the user re-bind sent an empty password: LDAP treats that as an unauthenticated bind and returns success")
		default:
			t.Errorf("the user re-bind sent %q, want the caller's password %q", pw, "alice-secret")
		}
	}
	if !fc.closed {
		t.Error("connection was not closed")
	}
}

// The nested lookup failing must degrade to the flat memberOf values rather
// than fail the login — but only genuinely, so this asserts the fallback
// values arrive rather than merely that no error surfaced.
func TestSearchBind_NestedGroupsFallsBackOnSearchError(t *testing.T) {
	cfg := searchCfg()
	cfg.NestedGroups = true
	fc := &fakeConn{
		searchResult:  &ldapv3.SearchResult{Entries: []*ldapv3.Entry{aliceEntry()}},
		searchErr:     errors.New("nested search: size limit exceeded"),
		searchErrFrom: 2, // the user search succeeds; only the expansion fails
	}
	v := newSearchBind(cfg, dialTo(fc))

	id, err := v.Verify(context.Background(), "alice", "pw")
	if err != nil {
		t.Fatalf("Verify: a failed nested lookup must not fail the login: %v", err)
	}
	if len(fc.searchFilters) != 2 {
		t.Fatalf("expected the nested search to have been attempted, got %v", fc.searchFilters)
	}
	// The flat memberOf values from the user's entry, not an empty slice: an
	// empty result would silently drop every role mapping.
	want := []string{"CN=Admins,DC=example,DC=com", "CN=Artists,DC=example,DC=com"}
	if len(id.Groups) != len(want) || id.Groups[0] != want[0] || id.Groups[1] != want[1] {
		t.Errorf("Groups: got %v, want the flat memberOf values %v", id.Groups, want)
	}
}

// The failure mode with no error to key on: the nested search succeeds and
// returns zero entries. That is what a base_dn scoped to the user subtree
// produces when groups live elsewhere — a completely normal, successful
// search. Before this, the empty slice replaced the flat memberOf values and
// every user silently landed on default_role.
func TestSearchBind_NestedGroupsFallsBackOnEmptyResult(t *testing.T) {
	cfg := searchCfg()
	cfg.NestedGroups = true
	fc := &fakeConn{
		searchResult: &ldapv3.SearchResult{Entries: []*ldapv3.Entry{aliceEntry()}},
		// Search #2 is the nested expansion: successful, and empty.
		searchResultAt: map[int]*ldapv3.SearchResult{2: {}},
	}
	v := newSearchBind(cfg, dialTo(fc))

	id, err := v.Verify(context.Background(), "alice", "alice-secret")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(fc.searchFilters) != 2 {
		t.Fatalf("expected the nested search to have been attempted, got %v", fc.searchFilters)
	}
	want := []string{"CN=Admins,DC=example,DC=com", "CN=Artists,DC=example,DC=com"}
	if len(id.Groups) != len(want) || id.Groups[0] != want[0] || id.Groups[1] != want[1] {
		t.Errorf("Groups: got %v, want the flat memberOf values %v — an empty nested result must not discard them", id.Groups, want)
	}
}

// The empty-result fallback must be as visible as the error one, or the only
// documented signal never fires for the likelier misconfiguration.
func TestSearchBind_NestedEmptyResultLogsWarning(t *testing.T) {
	var buf bytes.Buffer
	cfg := searchCfg()
	cfg.NestedGroups = true
	cfg.Logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	fc := &fakeConn{
		searchResult:   &ldapv3.SearchResult{Entries: []*ldapv3.Entry{aliceEntry()}},
		searchResultAt: map[int]*ldapv3.SearchResult{2: {}},
	}
	v := newSearchBind(cfg, dialTo(fc))
	if _, err := v.Verify(context.Background(), "alice", "alice-secret"); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "level=WARN") {
		t.Errorf("expected a WARN record, got %q", got)
	}
	if !strings.Contains(got, "nested group expansion failed") {
		t.Errorf("expected the shared fallback message, got %q", got)
	}
	// The operator needs to be pointed at the actual cause, which is a config
	// mistake rather than a directory fault.
	if !strings.Contains(got, "base_dn") {
		t.Errorf("expected the warning to name base_dn as the thing to check, got %q", got)
	}
}

// A user genuinely in no groups must still end up with no groups: the
// fallback is guarded on the flat list being non-empty precisely so it cannot
// resurrect values that were never there.
func TestSearchBind_NestedEmptyResultWithEmptyFlatStaysEmpty(t *testing.T) {
	cfg := searchCfg()
	cfg.NestedGroups = true
	groupless := entry("uid=alice,dc=example,dc=com", map[string][]string{
		"sAMAccountName": {"alice"},
		"displayName":    {"Alice Anderson"},
	})
	fc := &fakeConn{
		searchResult:   &ldapv3.SearchResult{Entries: []*ldapv3.Entry{groupless}},
		searchResultAt: map[int]*ldapv3.SearchResult{2: {}},
	}
	v := newSearchBind(cfg, dialTo(fc))

	id, err := v.Verify(context.Background(), "alice", "alice-secret")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(id.Groups) != 0 {
		t.Errorf("Groups: got %v, want empty", id.Groups)
	}
}

// The user search failing is a directory fault, not a credential problem.
func TestSearchBind_UserSearchErrorIsUnavailable(t *testing.T) {
	fc := &fakeConn{searchErr: errors.New("server is busy")}
	v := newSearchBind(searchCfg(), dialTo(fc))
	if _, err := v.Verify(context.Background(), "alice", "pw"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("expected ErrUnavailable, got %v", err)
	}
}

// An empty password is an LDAP *unauthenticated* bind, which a directory
// answers with success — so it must never reach the wire.
func TestSearchBind_EmptyPasswordNeverBinds(t *testing.T) {
	fc := &fakeConn{searchResult: &ldapv3.SearchResult{Entries: []*ldapv3.Entry{aliceEntry()}}}
	v := newSearchBind(searchCfg(), dialTo(fc))
	if _, err := v.Verify(context.Background(), "alice", ""); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("expected ErrInvalidCredentials, got %v", err)
	}
	if len(fc.binds) != 0 {
		t.Errorf("an empty password reached the directory: binds %v", fc.binds)
	}
}

func TestSearchBind_WrongPassword(t *testing.T) {
	fc := &fakeConn{
		searchResult: &ldapv3.SearchResult{Entries: []*ldapv3.Entry{aliceEntry()}},
		// Keyed on the password too: the directory rejects THIS password for
		// this DN. A verifier that re-bound with the service password would
		// miss this entry entirely and be reported as a successful login.
		bindErr: map[bindKey]error{
			{DN: "CN=Alice,OU=People,DC=example,DC=com", PW: "wrong"}: errors.New("invalid credentials"),
		},
	}
	v := newSearchBind(searchCfg(), dialTo(fc))
	if _, err := v.Verify(context.Background(), "alice", "wrong"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("expected ErrInvalidCredentials, got %v", err)
	}
}

func TestSearchBind_UserNotFound(t *testing.T) {
	fc := &fakeConn{searchResult: &ldapv3.SearchResult{}}
	v := newSearchBind(searchCfg(), dialTo(fc))
	if _, err := v.Verify(context.Background(), "ghost", "pw"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("expected ErrInvalidCredentials, got %v", err)
	}
}

// A second matching entry means the filter is ambiguous. Binding as "the
// first one" would be a coin flip over which identity the caller gets, so
// this must fail rather than guess.
func TestSearchBind_MultipleEntriesRejected(t *testing.T) {
	fc := &fakeConn{searchResult: &ldapv3.SearchResult{Entries: []*ldapv3.Entry{aliceEntry(), aliceEntry()}}}
	v := newSearchBind(searchCfg(), dialTo(fc))
	if _, err := v.Verify(context.Background(), "alice", "pw"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("expected ErrInvalidCredentials, got %v", err)
	}
}

func TestSearchBind_ServiceAccountBindFails(t *testing.T) {
	fc := &fakeConn{bindErr: map[bindKey]error{
		{DN: "CN=svc,DC=example,DC=com", PW: "svcpass"}: errors.New("bad svc password"),
	}}
	v := newSearchBind(searchCfg(), dialTo(fc))
	// A broken service account is an infrastructure fault, not a bad user
	// password: the distinction drives whether an operator gets paged.
	if _, err := v.Verify(context.Background(), "alice", "pw"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("expected ErrUnavailable, got %v", err)
	}
}

func TestSearchBind_DialFailure(t *testing.T) {
	v := newSearchBind(searchCfg(), dialFail())
	if _, err := v.Verify(context.Background(), "alice", "pw"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("expected ErrUnavailable, got %v", err)
	}
}

// The escaping unit test proves the helper works; this proves it is actually
// wired into the filter that reaches the directory.
func TestSearchBind_EscapesUsernameInFilter(t *testing.T) {
	fc := &fakeConn{searchResult: &ldapv3.SearchResult{}}
	v := newSearchBind(searchCfg(), dialTo(fc))
	// The escaped filter matches nothing, which is the point: unescaped it
	// would have matched every entry in the base DN.
	if _, err := v.Verify(context.Background(), "*)(uid=*", "pw"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("expected ErrInvalidCredentials, got %v", err)
	}
	if len(fc.searchFilters) != 1 {
		t.Fatalf("expected 1 search, got %d", len(fc.searchFilters))
	}
	if strings.Contains(fc.searchFilters[0], "*)(") {
		t.Fatalf("filter was not escaped: %q", fc.searchFilters[0])
	}
	if !strings.Contains(fc.searchFilters[0], `\2a`) {
		t.Fatalf("expected escaped wildcard in filter, got %q", fc.searchFilters[0])
	}
}

func TestSearchBind_NestedGroupsFilter(t *testing.T) {
	cfg := searchCfg()
	cfg.NestedGroups = true
	fc := &fakeConn{searchResult: &ldapv3.SearchResult{Entries: []*ldapv3.Entry{aliceEntry()}}}
	v := newSearchBind(cfg, dialTo(fc))
	if _, err := v.Verify(context.Background(), "alice", "pw"); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	// The second search is the nested-group expansion.
	if len(fc.searchFilters) != 2 {
		t.Fatalf("expected 2 searches with nested_groups, got %d: %v", len(fc.searchFilters), fc.searchFilters)
	}
	if !strings.Contains(fc.searchFilters[1], "1.2.840.113556.1.4.1941") {
		t.Errorf("expected LDAP_MATCHING_RULE_IN_CHAIN filter, got %q", fc.searchFilters[1])
	}
}

// An empty bind_dn selects anonymous search — a world-readable OpenLDAP with
// no service account, which is a real deployment pattern. Binding "" with ""
// is not that: go-ldap rejects it client-side before any network I/O, so every
// login would fail as ErrUnavailable and blame a service account that does not
// exist. No service bind must be attempted, and the user re-bind must still do
// the authenticating.
func TestSearchBind_AnonymousSearchSkipsServiceBind(t *testing.T) {
	cfg := searchCfg()
	cfg.BindDN = ""
	cfg.BindPassword = ""
	fc := &fakeConn{searchResult: &ldapv3.SearchResult{Entries: []*ldapv3.Entry{aliceEntry()}}}
	v := newSearchBind(cfg, dialTo(fc))

	id, err := v.Verify(context.Background(), "alice", "alice-secret")
	if err != nil {
		t.Fatalf("Verify: an anonymous-search config must authenticate, got %v", err)
	}
	// Exactly one bind, and it is the user's — not an anonymous "" bind.
	want := bindKey{DN: "CN=Alice,OU=People,DC=example,DC=com", PW: "alice-secret"}
	if len(fc.binds) != 1 || fc.binds[0] != want {
		t.Fatalf("binds: got %v, want exactly the user re-bind %v", fc.binds, want)
	}
	if id.DN != "CN=Alice,OU=People,DC=example,DC=com" {
		t.Errorf("DN: got %q", id.DN)
	}
	// The search still ran, on the anonymous connection.
	if len(fc.searchFilters) != 1 {
		t.Errorf("expected the user search to have run, got %v", fc.searchFilters)
	}
}

// Anonymous search must not become a way to skip authentication: with no
// service bind, the user re-bind is the ONLY bind, so its failure has to be
// what fails the login.
func TestSearchBind_AnonymousSearchStillRejectsBadPassword(t *testing.T) {
	cfg := searchCfg()
	cfg.BindDN = ""
	cfg.BindPassword = ""
	fc := &fakeConn{
		searchResult: &ldapv3.SearchResult{Entries: []*ldapv3.Entry{aliceEntry()}},
		bindErr: map[bindKey]error{
			{DN: "CN=Alice,OU=People,DC=example,DC=com", PW: "wrong"}: errors.New("invalid credentials"),
		},
	}
	v := newSearchBind(cfg, dialTo(fc))
	if _, err := v.Verify(context.Background(), "alice", "wrong"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("expected ErrInvalidCredentials, got %v", err)
	}
}

// The nested expansion searches group objects, which ordinary users often may
// not read. Run on the user's connection it would fail on every login and
// degrade to flat memberOf permanently. It must therefore execute while the
// connection still carries the service account — i.e. before the user re-bind.
func TestSearchBind_NestedExpansionRunsOnServiceConnection(t *testing.T) {
	cfg := searchCfg()
	cfg.NestedGroups = true
	fc := &fakeConn{searchResult: &ldapv3.SearchResult{Entries: []*ldapv3.Entry{aliceEntry()}}}
	v := newSearchBind(cfg, dialTo(fc))
	if _, err := v.Verify(context.Background(), "alice", "alice-secret"); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	// The full interleaving: service bind, user search, nested search, and
	// only then the user re-bind. The nested search sitting before the last
	// bind is the whole point — after it, the connection is the user's.
	want := []string{
		"bind:CN=svc,DC=example,DC=com",
		"search:(sAMAccountName=alice)",
		// EscapeFilter escapes only `()*\` and non-ASCII, so a plain DN passes
		// through as written.
		"search:(member:" + matchingRuleInChain + ":=CN=Alice,OU=People,DC=example,DC=com)",
		"bind:CN=Alice,OU=People,DC=example,DC=com",
	}
	if len(fc.ops) != len(want) {
		t.Fatalf("ops: got %v, want %v", fc.ops, want)
	}
	for i := range want {
		if fc.ops[i] != want[i] {
			t.Errorf("ops[%d]: got %q, want %q", i, fc.ops[i], want[i])
		}
	}
	// The reordering must not have cost us the authenticating bind.
	if last := fc.binds[len(fc.binds)-1]; last.DN != "CN=Alice,OU=People,DC=example,DC=com" || last.PW != "alice-secret" {
		t.Errorf("last bind: got %v, want the user's DN and password", last)
	}
}

// Reordering the nested lookup ahead of the re-bind must not let a bad
// password through: the bind still gates the result.
func TestSearchBind_NestedGroupsWrongPasswordStillRejected(t *testing.T) {
	cfg := searchCfg()
	cfg.NestedGroups = true
	fc := &fakeConn{
		searchResult: &ldapv3.SearchResult{Entries: []*ldapv3.Entry{aliceEntry()}},
		bindErr: map[bindKey]error{
			{DN: "CN=Alice,OU=People,DC=example,DC=com", PW: "wrong"}: errors.New("invalid credentials"),
		},
	}
	v := newSearchBind(cfg, dialTo(fc))
	if _, err := v.Verify(context.Background(), "alice", "wrong"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("expected ErrInvalidCredentials, got %v", err)
	}
}

// LDAP attribute descriptions are case-insensitive (RFC 4512 §2.5) and a
// directory may echo back any casing. Matched byte-for-byte, a "memberof"
// reply yields zero groups — which is not an error anywhere, it just silently
// drops every role mapping and demotes the user to DefaultRole.
func TestSearchBind_MemberOfIsCaseInsensitive(t *testing.T) {
	e := entry("CN=Alice,OU=People,DC=example,DC=com", map[string][]string{
		"sAMAccountName": {"alice"},
		"memberof":       {"CN=Admins,DC=example,DC=com"},
	})
	fc := &fakeConn{searchResult: &ldapv3.SearchResult{Entries: []*ldapv3.Entry{e}}}
	v := newSearchBind(searchCfg(), dialTo(fc))

	id, err := v.Verify(context.Background(), "alice", "alice-secret")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(id.Groups) != 1 || id.Groups[0] != "CN=Admins,DC=example,DC=com" {
		t.Fatalf("Groups: got %v, want the lowercase-attribute value to be found", id.Groups)
	}
	// The consequence the casing bug actually has: the admin mapping is lost
	// and the user is silently demoted to DefaultRole.
	if role, ok := searchCfg().MapRole(id.Groups); !ok || role != "admin" {
		t.Errorf("MapRole: got %q (ok=%v), want admin", role, ok)
	}
}

// firstAttr shares the same case-insensitive lookup as the memberOf
// resolution above it, rather than the byte-for-byte match it used to be — so
// a directory that echoes UsernameAttr or DisplayNameAttr back in a different
// case still populates Identity.Username and Identity.DisplayName.
func TestSearchBind_UsernameAndDisplayNameAreCaseInsensitive(t *testing.T) {
	e := entry("CN=Alice,OU=People,DC=example,DC=com", map[string][]string{
		"samaccountname": {"alice"},
		"DISPLAYNAME":    {"Alice Anderson"},
	})
	fc := &fakeConn{searchResult: &ldapv3.SearchResult{Entries: []*ldapv3.Entry{e}}}
	v := newSearchBind(searchCfg(), dialTo(fc))

	id, err := v.Verify(context.Background(), "alice", "alice-secret")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if id.Username != "alice" {
		t.Errorf("Username: got %q, want the differently-cased samaccountname value", id.Username)
	}
	if id.DisplayName != "Alice Anderson" {
		t.Errorf("DisplayName: got %q, want the differently-cased DISPLAYNAME value", id.DisplayName)
	}
}

// The nested fallback is silent by construction — it returns a valid login. A
// persistently failing expansion would demote every admin who holds admin only
// through a nested group, with no signal anywhere, so it must be logged.
func TestSearchBind_NestedFallbackLogsWarning(t *testing.T) {
	var buf bytes.Buffer
	cfg := searchCfg()
	cfg.NestedGroups = true
	cfg.Logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	fc := &fakeConn{
		searchResult:  &ldapv3.SearchResult{Entries: []*ldapv3.Entry{aliceEntry()}},
		searchErr:     errors.New("insufficient access rights"),
		searchErrFrom: 2,
	}
	v := newSearchBind(cfg, dialTo(fc))
	if _, err := v.Verify(context.Background(), "alice", "alice-secret"); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "level=WARN") {
		t.Errorf("expected a WARN record, got %q", got)
	}
	if !strings.Contains(got, "nested group expansion failed") {
		t.Errorf("expected the fallback to be named in the log, got %q", got)
	}
	// The underlying cause has to reach the operator, or the warning is
	// unactionable.
	if !strings.Contains(got, "insufficient access rights") {
		t.Errorf("expected the search error in the log, got %q", got)
	}
}

// A nil Logger is the default and must stay safe: the package is constructed
// without one today, and the fallback path is exactly where a nil deref would
// turn a degraded login into a panic.
func TestSearchBind_NilLoggerOnNestedFallbackIsSafe(t *testing.T) {
	cfg := searchCfg()
	cfg.NestedGroups = true
	cfg.Logger = nil
	fc := &fakeConn{
		searchResult:  &ldapv3.SearchResult{Entries: []*ldapv3.Entry{aliceEntry()}},
		searchErr:     errors.New("insufficient access rights"),
		searchErrFrom: 2,
	}
	v := newSearchBind(cfg, dialTo(fc))
	id, err := v.Verify(context.Background(), "alice", "alice-secret")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(id.Groups) != 2 {
		t.Errorf("Groups: got %v, want the flat memberOf fallback", id.Groups)
	}
}

func TestSearchBind_NoNestedSearchWhenDisabled(t *testing.T) {
	fc := &fakeConn{searchResult: &ldapv3.SearchResult{Entries: []*ldapv3.Entry{aliceEntry()}}}
	v := newSearchBind(searchCfg(), dialTo(fc))
	if _, err := v.Verify(context.Background(), "alice", "pw"); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(fc.searchFilters) != 1 {
		t.Fatalf("expected exactly 1 search without nested_groups, got %d", len(fc.searchFilters))
	}
}

// TestSearchBind_RequestsAndReadsUniqueIDAttr pins both halves of the stable
// identifier: that the search asks for it, and that the reply is carried into
// Identity.
//
// Asserting the REQUEST is the part worth having. entryUUID is an operational
// attribute, so most directories omit it from a response unless it is named
// explicitly. A fake returns whatever the test scripts, so dropping the name
// from the request list passes every parse-only assertion and then, against a
// real server, makes every login look like a brand-new person — provisioning
// duplicate accounts with no error anywhere.
func TestSearchBind_RequestsAndReadsUniqueIDAttr(t *testing.T) {
	e := aliceEntry()
	e.Attributes = append(e.Attributes, ldapv3.NewEntryAttribute("entryUUID", []string{"uuid-1234"}))
	fc := &fakeConn{searchResult: &ldapv3.SearchResult{Entries: []*ldapv3.Entry{e}}}
	cfg := searchCfg()
	cfg.UniqueIDAttr = "entryUUID"
	v := newSearchBind(cfg, dialTo(fc))

	id, err := v.Verify(context.Background(), "alice", "pw")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if id.ExternalID != "uuid-1234" {
		t.Fatalf("ExternalID = %q, want %q", id.ExternalID, "uuid-1234")
	}
	if len(fc.searchAttrs) == 0 {
		t.Fatal("no search was issued")
	}
	if !slices.Contains(fc.searchAttrs[0], "entryUUID") {
		t.Fatalf("user search requested %v, want it to include entryUUID; an operational "+
			"attribute a real directory will not return unless it is named", fc.searchAttrs[0])
	}
}

// Active Directory returns objectGUID as a raw 16-byte octet string that is
// not valid UTF-8. Stored as-is it would corrupt, so it is hex-encoded. This
// encoding is permanent: changing it later orphans every account already
// stamped with the old form.
func TestSearchBind_HexEncodesBinaryUniqueID(t *testing.T) {
	raw := []byte{
		0x00, 0x01, 0x02, 0x03, 0xff, 0xfe, 0x80, 0x81,
		0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17,
	}
	e := withRawAttr(aliceEntry(), "objectGUID", raw)
	fc := &fakeConn{searchResult: &ldapv3.SearchResult{Entries: []*ldapv3.Entry{e}}}
	cfg := searchCfg()
	cfg.UniqueIDAttr = "objectGUID"
	v := newSearchBind(cfg, dialTo(fc))

	id, err := v.Verify(context.Background(), "alice", "pw")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if id.ExternalID != "00010203fffe80811011121314151617" {
		t.Fatalf("ExternalID = %q, want the hex encoding of the raw GUID", id.ExternalID)
	}
	if !utf8.ValidString(id.ExternalID) {
		t.Fatal("ExternalID is not valid UTF-8 and cannot be stored as text")
	}
}

// A directory that returns no value for the configured attribute must yield an
// empty ExternalID rather than a guess. The login path refuses on empty; a
// fallback to the username here would reinstate name matching under another
// name.
func TestSearchBind_MissingUniqueIDIsEmpty(t *testing.T) {
	fc := &fakeConn{searchResult: &ldapv3.SearchResult{Entries: []*ldapv3.Entry{aliceEntry()}}}
	cfg := searchCfg()
	cfg.UniqueIDAttr = "entryUUID"
	v := newSearchBind(cfg, dialTo(fc))

	id, err := v.Verify(context.Background(), "alice", "pw")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if id.ExternalID != "" {
		t.Fatalf("ExternalID = %q, want empty when the directory returned nothing", id.ExternalID)
	}
}
