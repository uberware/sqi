// SPDX-License-Identifier: AGPL-3.0-or-later

package ldap

import "testing"

func TestConfig_MapRole(t *testing.T) {
	cfg := Config{
		RoleMap: []RoleMapping{
			{Group: "CN=Farm Admins,OU=Groups,DC=example,DC=com", Role: "admin"},
			{Group: "CN=Leads,OU=Groups,DC=example,DC=com", Role: "operator"},
			{Group: "CN=Artists,OU=Groups,DC=example,DC=com", Role: "user"},
		},
		DefaultRole: "read-only",
	}

	tests := []struct {
		name     string
		groups   []string
		want     string
		wantOK   bool
		useEmpty bool // run against a config with no DefaultRole
	}{
		{
			name:   "single match",
			groups: []string{"CN=Artists,OU=Groups,DC=example,DC=com"},
			want:   "user", wantOK: true,
		},
		{
			// Config order is how an operator expresses precedence: Farm
			// Admins is listed first, so it wins regardless of the order the
			// directory returned the groups in.
			name: "first mapping wins over later ones",
			groups: []string{
				"CN=Artists,OU=Groups,DC=example,DC=com",
				"CN=Farm Admins,OU=Groups,DC=example,DC=com",
			},
			want: "admin", wantOK: true,
		},
		{
			// AD treats DNs case-insensitively; a directory that returns a
			// differently-cased DN must still match.
			name:   "case-insensitive DN match",
			groups: []string{"cn=farm admins,ou=groups,dc=example,dc=com"},
			want:   "admin", wantOK: true,
		},
		{
			name:   "no match falls back to default",
			groups: []string{"CN=Nobody,DC=example,DC=com"},
			want:   "read-only", wantOK: true,
		},
		{
			name:   "no groups at all falls back to default",
			groups: nil,
			want:   "read-only", wantOK: true,
		},
		{
			name:   "no match with empty default rejects",
			groups: []string{"CN=Nobody,DC=example,DC=com"},
			want:   "", wantOK: false, useEmpty: true,
		},
		{
			name:   "match still wins with empty default",
			groups: []string{"CN=Leads,OU=Groups,DC=example,DC=com"},
			want:   "operator", wantOK: true, useEmpty: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := cfg
			if tt.useEmpty {
				c.DefaultRole = ""
			}
			got, ok := c.MapRole(tt.groups)
			if got != tt.want || ok != tt.wantOK {
				t.Errorf("MapRole(%v) = (%q, %v), want (%q, %v)", tt.groups, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}
