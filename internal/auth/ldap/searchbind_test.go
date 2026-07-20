// SPDX-License-Identifier: AGPL-3.0-or-later

package ldap

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

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

	id, err := v.Verify(context.Background(), "alice", "correct-horse")
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
	// Service account first, then a re-bind as the user with their password.
	// If the second bind is missing, any password would authenticate.
	want := []string{"CN=svc,DC=example,DC=com", "CN=Alice,OU=People,DC=example,DC=com"}
	if len(fc.binds) != 2 || fc.binds[0] != want[0] || fc.binds[1] != want[1] {
		t.Errorf("binds: got %v, want %v", fc.binds, want)
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
		bindErr: map[string]error{
			"CN=Alice,OU=People,DC=example,DC=com": errors.New("invalid credentials"),
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
	fc := &fakeConn{bindErr: map[string]error{"CN=svc,DC=example,DC=com": errors.New("bad svc password")}}
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
