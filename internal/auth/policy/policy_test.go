// SPDX-License-Identifier: AGPL-3.0-or-later

package policy_test

import (
	"reflect"
	"sort"
	"testing"

	"github.com/uberware/sqi/internal/auth"
	"github.com/uberware/sqi/internal/auth/policy"
)

// want is the full matrix, asserted cell-by-cell. Keep in lockstep with the
// grants map in policy.go and the table in docs/auth.md.
var want = map[string]map[policy.Permission]bool{
	"read-only": {
		policy.JobsRead: true, policy.JobsReadAll: true, policy.WorkersRead: true,
		policy.InfraRead: true, policy.ProductsRead: true, policy.APIKeysSelf: true,
	},
	"user": {
		policy.JobsRead: true, policy.JobsWrite: true, policy.WorkersRead: true,
		policy.InfraRead: true, policy.ProductsRead: true, policy.APIKeysSelf: true,
	},
	"operator": {
		policy.JobsRead: true, policy.JobsReadAll: true, policy.JobsWrite: true,
		policy.JobsSubmitAs: true, policy.WorkersRead: true,
		policy.WorkersManage: true, policy.InfraRead: true, policy.InfraManage: true,
		policy.ProductsRead: true, policy.ProductsManage: true,
		policy.DiagnosticsRead: true, policy.APIKeysSelf: true,
	},
	"admin": {
		policy.JobsRead: true, policy.JobsReadAll: true, policy.JobsWrite: true,
		policy.JobsSubmitAs: true, policy.WorkersRead: true,
		policy.WorkersManage: true, policy.InfraRead: true, policy.InfraManage: true,
		policy.ProductsRead: true, policy.ProductsManage: true,
		policy.DiagnosticsRead: true, policy.UsersRead: true, policy.UsersManage: true,
		policy.APIKeysSelf: true, policy.APIKeysAdmin: true,
	},
}

var allPerms = []policy.Permission{
	policy.JobsRead, policy.JobsReadAll, policy.JobsWrite, policy.JobsSubmitAs,
	policy.WorkersRead, policy.WorkersManage,
	policy.InfraRead, policy.InfraManage, policy.ProductsRead, policy.ProductsManage,
	policy.DiagnosticsRead, policy.UsersRead, policy.UsersManage,
	policy.APIKeysSelf, policy.APIKeysAdmin,
}

func TestCan_MatrixExact(t *testing.T) {
	for role, grants := range want {
		for _, perm := range allPerms {
			p := auth.Principal{Roles: []string{role}}
			if got := policy.Can(p, perm); got != grants[perm] {
				t.Errorf("Can(%q, %q) = %v, want %v", role, perm, got, grants[perm])
			}
		}
	}
}

func TestCan_SuperuserAllowsAll(t *testing.T) {
	p := auth.Principal{Superuser: true}
	for _, perm := range allPerms {
		if !policy.Can(p, perm) {
			t.Errorf("Superuser denied %q", perm)
		}
	}
}

func TestCan_UnknownRoleAndEmptyDenyAll(t *testing.T) {
	for _, p := range []auth.Principal{{Roles: []string{"wizard"}}, {Roles: nil}} {
		for _, perm := range allPerms {
			if policy.Can(p, perm) {
				t.Errorf("principal %+v unexpectedly allowed %q", p, perm)
			}
		}
	}
}

func TestCan_UnionAcrossRoles(t *testing.T) {
	// A principal with both read-only and operator gets the union.
	p := auth.Principal{Roles: []string{"read-only", "operator"}}
	if !policy.Can(p, policy.InfraManage) {
		t.Error("union of roles should grant infra.manage")
	}
}

func TestPermissionsFor(t *testing.T) {
	tests := []struct {
		name string
		p    auth.Principal
		want []string
	}{
		{
			name: "read-only",
			p:    auth.Principal{Roles: []string{"read-only"}},
			want: []string{"apikeys.self", "infra.read", "jobs.read", "jobs.read.all", "products.read", "workers.read"},
		},
		{
			name: "superuser gets every permission",
			p:    auth.Principal{Superuser: true},
			want: allPermissionStrings(),
		},
		{
			name: "no roles",
			p:    auth.Principal{},
			want: []string{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := policy.PermissionsFor(tt.p)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("PermissionsFor() = %v, want %v", got, tt.want)
			}
		})
	}
}

// allPermissionStrings returns every declared permission as a sorted []string.
func allPermissionStrings() []string {
	out := make([]string, 0, len(policy.All))
	for _, p := range policy.All {
		out = append(out, string(p))
	}
	sort.Strings(out)
	return out
}

// TestAllCoversEveryGrantedPermission fails when a new Permission constant is
// granted to a role but never added to All — which would silently omit it from
// every client's PermissionsFor response.
//
// This iterates policy.Roles(), a read-only view over the package's real
// (unexported) grants map, rather than the hand-copied `want` mirror above.
// `want` is itself maintained by hand, so a permission added to grants but
// never added to `want` — the same slip this test guards against for All —
// would silently escape a version of this test that iterated `want` instead:
// that omission was the actual gap (proven by sabotage: a new Permission
// granted to a role and left out of All still passed every test in this
// package, because both `want` and `All` were missing it in lockstep).
// Iterating the real grants map via Roles() means the only way to pass is
// for All to actually be complete.
func TestAllCoversEveryGrantedPermission(t *testing.T) {
	inAll := make(map[policy.Permission]bool, len(policy.All))
	for _, p := range policy.All {
		inAll[p] = true
	}
	for role, perms := range policy.Roles() {
		for perm, ok := range perms {
			if ok && !inAll[perm] {
				t.Errorf("permission %q granted to role %q but missing from All", perm, role)
			}
		}
	}
}

func TestB2Permissions(t *testing.T) {
	tests := []struct {
		role string
		perm policy.Permission
		want bool
	}{
		{"read-only", policy.JobsReadAll, true},
		{"user", policy.JobsReadAll, false},
		{"operator", policy.JobsReadAll, true},
		{"admin", policy.JobsReadAll, true},

		{"read-only", policy.JobsSubmitAs, false},
		{"user", policy.JobsSubmitAs, false},
		{"operator", policy.JobsSubmitAs, true},
		{"admin", policy.JobsSubmitAs, true},
	}
	for _, tt := range tests {
		t.Run(tt.role+"/"+string(tt.perm), func(t *testing.T) {
			got := policy.Can(auth.Principal{Roles: []string{tt.role}}, tt.perm)
			if got != tt.want {
				t.Errorf("Can(%q, %q) = %v, want %v", tt.role, tt.perm, got, tt.want)
			}
		})
	}
}

func TestB2PermissionsSuperuserBypass(t *testing.T) {
	su := auth.Principal{Superuser: true}
	for _, perm := range []policy.Permission{policy.JobsReadAll, policy.JobsSubmitAs} {
		if !policy.Can(su, perm) {
			t.Errorf("superuser denied %q — auth-off regression", perm)
		}
	}
}
