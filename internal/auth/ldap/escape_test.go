// SPDX-License-Identifier: AGPL-3.0-or-later

package ldap

import "testing"

// TestEscapeUsernameFilter pins LDAP filter injection shut. An unescaped
// username of `*)(uid=*` turns "(uid=%s)" into a filter matching every entry,
// which would let an attacker authenticate as whoever the directory returns
// first.
func TestEscapeUsernameFilter(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "alice", "alice"},
		{"wildcard", "*", `\2a`},
		{"filter injection", "*)(uid=*", `\2a\29\28uid=\2a`},
		{"open paren", "(", `\28`},
		{"close paren", ")", `\29`},
		{"backslash", `a\b`, `a\5cb`},
		{"nul byte", "a\x00b", `a\00b`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := EscapeUsernameFilter(tt.in); got != tt.want {
				t.Errorf("EscapeUsernameFilter(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestEscapeUsernameDN pins DN injection shut: a username containing a comma
// or equals sign must not be able to graft extra RDNs onto the template.
func TestEscapeUsernameDN(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "alice", "alice"},
		{"comma", "a,b", `a\,b`},
		{"equals", "a=b", `a\=b`},
		{"plus", "a+b", `a\+b`},
		{"rdn injection", "x,ou=admins", `x\,ou\=admins`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := EscapeUsernameDN(tt.in); got != tt.want {
				t.Errorf("EscapeUsernameDN(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
