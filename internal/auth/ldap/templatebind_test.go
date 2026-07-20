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

func templateCfg() Config {
	return Config{
		URL:             "ldap://dc.example.com:389",
		Timeout:         5 * time.Second,
		UserDNTemplate:  "uid=%s,ou=people,dc=example,dc=com",
		UsernameAttr:    "uid",
		DisplayNameAttr: "displayName",
		RoleSource:      RoleSourceDirectory,
		DefaultRole:     "read-only",
		RoleMap:         []RoleMapping{{Group: "cn=admins,dc=example,dc=com", Role: "admin"}},
	}
}

func TestTemplateBind_Success(t *testing.T) {
	fc := &fakeConn{searchResult: &ldapv3.SearchResult{Entries: []*ldapv3.Entry{
		entry("uid=bob,ou=people,dc=example,dc=com", map[string][]string{
			"uid":         {"bob"},
			"displayName": {"Bob Brown"},
			"memberOf":    {"cn=admins,dc=example,dc=com"},
		}),
	}}}
	v := newTemplateBind(templateCfg(), dialTo(fc))

	id, err := v.Verify(context.Background(), "bob", "pw")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if id.DN != "uid=bob,ou=people,dc=example,dc=com" {
		t.Errorf("DN: got %q", id.DN)
	}
	if id.Username != "bob" {
		t.Errorf("Username: got %q", id.Username)
	}
	if len(id.Groups) != 1 || id.Groups[0] != "cn=admins,dc=example,dc=com" {
		t.Errorf("Groups: got %v", id.Groups)
	}
	// Exactly one bind, as the user: template mode's entire point is having
	// no service account. A second bind would mean one crept back in.
	if len(fc.binds) != 1 || fc.binds[0] != "uid=bob,ou=people,dc=example,dc=com" {
		t.Errorf("binds: got %v, want one user bind", fc.binds)
	}
	if id.DisplayName != "Bob Brown" {
		t.Errorf("DisplayName: got %q", id.DisplayName)
	}
	if !fc.closed {
		t.Error("connection was not closed")
	}
}

// See the search-bind counterpart: an empty password must not reach the wire.
func TestTemplateBind_EmptyPasswordNeverBinds(t *testing.T) {
	fc := &fakeConn{}
	v := newTemplateBind(templateCfg(), dialTo(fc))
	if _, err := v.Verify(context.Background(), "bob", ""); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("expected ErrInvalidCredentials, got %v", err)
	}
	if len(fc.binds) != 0 {
		t.Errorf("an empty password reached the directory: binds %v", fc.binds)
	}
}

// A self-read that errors leaves the login intact: the bind already proved
// the credentials.
func TestTemplateBind_SelfReadErrorStillAuthenticates(t *testing.T) {
	fc := &fakeConn{searchErr: errors.New("insufficient access rights")}
	v := newTemplateBind(templateCfg(), dialTo(fc))
	id, err := v.Verify(context.Background(), "bob", "pw")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(id.Groups) != 0 {
		t.Errorf("Groups: got %v, want none", id.Groups)
	}
	if id.DN != "uid=bob,ou=people,dc=example,dc=com" {
		t.Errorf("DN: got %q", id.DN)
	}
}

func TestTemplateBind_WrongPassword(t *testing.T) {
	fc := &fakeConn{bindErr: map[string]error{
		"uid=bob,ou=people,dc=example,dc=com": errors.New("invalid credentials"),
	}}
	v := newTemplateBind(templateCfg(), dialTo(fc))
	if _, err := v.Verify(context.Background(), "bob", "wrong"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("expected ErrInvalidCredentials, got %v", err)
	}
}

func TestTemplateBind_DialFailure(t *testing.T) {
	v := newTemplateBind(templateCfg(), dialFail())
	if _, err := v.Verify(context.Background(), "bob", "pw"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("expected ErrUnavailable, got %v", err)
	}
}

func TestTemplateBind_EscapesUsernameInDN(t *testing.T) {
	fc := &fakeConn{}
	v := newTemplateBind(templateCfg(), dialTo(fc))
	if _, err := v.Verify(context.Background(), "x,ou=admins", "pw"); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(fc.binds) != 1 {
		t.Fatalf("expected 1 bind, got %v", fc.binds)
	}
	// An unescaped comma would have produced the DN
	// "uid=x,ou=admins,ou=people,..." — binding as a different entry.
	if !strings.HasPrefix(fc.binds[0], `uid=x\,ou\=admins,ou=people`) {
		t.Fatalf("DN was not escaped: %q", fc.binds[0])
	}
}

// The user's own entry is missing or unreadable: authentication succeeded but
// we cannot resolve groups, so there is no basis for a role.
func TestTemplateBind_NoEntryAfterBind(t *testing.T) {
	fc := &fakeConn{searchResult: &ldapv3.SearchResult{}}
	v := newTemplateBind(templateCfg(), dialTo(fc))
	id, err := v.Verify(context.Background(), "bob", "pw")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	// Bind succeeded, so the credentials are valid; groups are simply empty
	// and MapRole will fall back to DefaultRole.
	if len(id.Groups) != 0 {
		t.Errorf("Groups: got %v, want none", id.Groups)
	}
	if id.Username != "bob" {
		t.Errorf("Username: got %q, want the supplied name as fallback", id.Username)
	}
}
