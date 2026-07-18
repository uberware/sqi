// SPDX-License-Identifier: AGPL-3.0-or-later

package policy_test

import (
	"testing"

	"github.com/uberware/sqi/internal/auth"
	"github.com/uberware/sqi/internal/auth/policy"
)

// want is the full matrix, asserted cell-by-cell. Keep in lockstep with the
// grants map in policy.go and the table in docs/auth.md.
var want = map[string]map[policy.Permission]bool{
	"read-only": {
		policy.JobsRead: true, policy.WorkersRead: true, policy.InfraRead: true,
		policy.ProductsRead: true, policy.APIKeysSelf: true,
	},
	"user": {
		policy.JobsRead: true, policy.JobsWrite: true, policy.WorkersRead: true,
		policy.InfraRead: true, policy.ProductsRead: true, policy.APIKeysSelf: true,
	},
	"operator": {
		policy.JobsRead: true, policy.JobsWrite: true, policy.WorkersRead: true,
		policy.WorkersManage: true, policy.InfraRead: true, policy.InfraManage: true,
		policy.ProductsRead: true, policy.ProductsManage: true,
		policy.DiagnosticsRead: true, policy.APIKeysSelf: true,
	},
	"admin": {
		policy.JobsRead: true, policy.JobsWrite: true, policy.WorkersRead: true,
		policy.WorkersManage: true, policy.InfraRead: true, policy.InfraManage: true,
		policy.ProductsRead: true, policy.ProductsManage: true,
		policy.DiagnosticsRead: true, policy.UsersRead: true, policy.UsersManage: true,
		policy.APIKeysSelf: true, policy.APIKeysAdmin: true,
	},
}

var allPerms = []policy.Permission{
	policy.JobsRead, policy.JobsWrite, policy.WorkersRead, policy.WorkersManage,
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
