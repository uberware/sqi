// SPDX-License-Identifier: AGPL-3.0-or-later

package rolemap

import "testing"

func TestMap(t *testing.T) {
	mappings := []Mapping{
		{Group: "cn=admins,dc=example,dc=com", Role: "admin"},
		{Group: "cn=artists,dc=example,dc=com", Role: "user"},
	}
	tests := []struct {
		name        string
		mappings    []Mapping
		defaultRole string
		groups      []string
		wantRole    string
		wantOK      bool
	}{
		{
			name: "first match wins, not best match", mappings: mappings, groups: []string{
				"cn=artists,dc=example,dc=com", "cn=admins,dc=example,dc=com",
			},
			wantRole: "admin", wantOK: true,
		},
		{
			name: "case-insensitive and space-trimmed", mappings: mappings,
			groups: []string{" CN=Artists,DC=Example,DC=Com "}, wantRole: "user", wantOK: true,
		},
		{
			name: "no match falls back to default", mappings: mappings, defaultRole: "read-only",
			groups: []string{"cn=nobody,dc=example,dc=com"}, wantRole: "read-only", wantOK: true,
		},
		{
			name: "no match and no default rejects the login", mappings: mappings,
			groups: []string{"cn=nobody,dc=example,dc=com"}, wantOK: false,
		},
		{name: "no groups at all, no default", mappings: mappings, wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			role, ok := Map(tt.mappings, tt.defaultRole, tt.groups)
			if ok != tt.wantOK || role != tt.wantRole {
				t.Fatalf("Map = (%q, %v), want (%q, %v)", role, ok, tt.wantRole, tt.wantOK)
			}
		})
	}
}
