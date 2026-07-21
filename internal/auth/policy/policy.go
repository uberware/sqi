// SPDX-License-Identifier: AGPL-3.0-or-later

// Package policy is the authorization matrix for sqi's four built-in roles.
// It maps (role, permission) to allow/deny and is consulted by the REST auth
// middleware and the WebSocket subscribe path. It has no dependencies beyond
// the Principal it evaluates, so it stays pure and exhaustively testable.
//
// The matrix here is the server-side source of truth; web/src/auth/policy.ts
// mirrors it and docs/auth.md documents it — keep all three in lockstep.
package policy

import (
	"maps"
	"slices"
	"sort"

	"github.com/uberware/sqi/internal/auth"
)

// Permission is a per-resource-verb capability gating one or more routes.
type Permission string

const (
	// JobsRead grants read access to jobs.
	JobsRead Permission = "jobs.read"
	// JobsWrite grants create/update/delete access to jobs.
	JobsWrite Permission = "jobs.write"
	// JobsReadAll grants visibility of jobs owned by anyone. Without it a
	// principal is owner-scoped: it sees, and may mutate, only jobs it owns.
	// This single predicate governs both reads and writes — a principal that
	// may not see another user's job must not be able to mutate it either.
	JobsReadAll Permission = "jobs.read.all"
	// JobsSubmitAs grants setting Job.Owner to a user other than yourself.
	// Modeled as a permission rather than an operator/admin role check so a
	// proxy service (a pipeline gateway submitting for artists) can later hold
	// it via a narrow role without also inheriting workers.manage.
	JobsSubmitAs Permission = "jobs.submit_as"
	// WorkersRead grants read access to workers.
	WorkersRead Permission = "workers.read"
	// WorkersManage grants management access to workers (drain, restart, etc).
	WorkersManage Permission = "workers.manage"
	// InfraRead grants read access to infrastructure.
	InfraRead Permission = "infra.read"
	// InfraManage grants management access to infrastructure.
	InfraManage Permission = "infra.manage"
	// ProductsRead grants read access to products.
	ProductsRead Permission = "products.read"
	// ProductsManage grants management access to products.
	ProductsManage Permission = "products.manage"
	// DiagnosticsRead grants read access to server diagnostics.
	DiagnosticsRead Permission = "diagnostics.read"
	// UsersRead grants read access to users.
	UsersRead Permission = "users.read"
	// UsersManage grants management access to users.
	UsersManage Permission = "users.manage"
	// APIKeysSelf grants access to manage own API keys.
	APIKeysSelf Permission = "apikeys.self"
	// APIKeysAdmin grants access to manage all API keys.
	APIKeysAdmin Permission = "apikeys.admin"
	// IsolationManage grants setting a queue's run-as-user, i.e. choosing the OS
	// identity that arbitrary job code executes under. Deliberately separate
	// from InfraManage — which operator holds — because it is an escalation
	// surface, not ordinary queue configuration.
	IsolationManage Permission = "isolation.manage"
)

// grants maps each built-in role to the set of permissions it holds. A role
// absent from a permission's set is denied that permission.
var grants = map[string]map[Permission]bool{
	"read-only": {
		JobsRead: true, JobsReadAll: true, WorkersRead: true, InfraRead: true,
		ProductsRead: true, APIKeysSelf: true,
	},
	"user": {
		JobsRead: true, JobsWrite: true, WorkersRead: true, InfraRead: true,
		ProductsRead: true, APIKeysSelf: true,
	},
	"operator": {
		JobsRead: true, JobsReadAll: true, JobsWrite: true, JobsSubmitAs: true,
		WorkersRead: true, WorkersManage: true,
		InfraRead: true, InfraManage: true, ProductsRead: true, ProductsManage: true,
		DiagnosticsRead: true, APIKeysSelf: true,
	},
	"admin": {
		JobsRead: true, JobsReadAll: true, JobsWrite: true, JobsSubmitAs: true,
		WorkersRead: true, WorkersManage: true,
		InfraRead: true, InfraManage: true, ProductsRead: true, ProductsManage: true,
		DiagnosticsRead: true, UsersRead: true, UsersManage: true,
		APIKeysSelf: true, APIKeysAdmin: true, IsolationManage: true,
	},
}

// All is every declared permission. It is the source for PermissionsFor's
// superuser answer and for the exhaustiveness test that keeps this list in
// step with the constants above.
var All = []Permission{
	JobsRead, JobsReadAll, JobsWrite, JobsSubmitAs, WorkersRead, WorkersManage,
	InfraRead, InfraManage, ProductsRead, ProductsManage,
	DiagnosticsRead, UsersRead, UsersManage,
	APIKeysSelf, APIKeysAdmin, IsolationManage,
}

// Roles returns a read-only snapshot of the role → permission grants matrix,
// keyed by role name. It exists so tests (policy_test is an external test
// package and cannot see the unexported grants var directly) can assert
// against the real matrix instead of maintaining their own hand-copied
// mirror of it — a mirror that, by construction, cannot notice when the real
// matrix and the mirror drift apart. Each call returns a fresh deep copy, so
// callers cannot mutate package state through it.
func Roles() map[string]map[Permission]bool {
	out := make(map[string]map[Permission]bool, len(grants))
	for role, perms := range grants {
		permsCopy := make(map[Permission]bool, len(perms))
		maps.Copy(permsCopy, perms)
		out[role] = permsCopy
	}
	return out
}

// IsRole reports whether name is a role the policy matrix defines. It is the
// authority for role validity — API-level validation calls it rather than
// keeping its own list, so a role can never be assignable-but-ungranted (a
// user with zero permissions and no error) or granted-but-unassignable.
func IsRole(name string) bool {
	_, ok := grants[name]
	return ok
}

// superuserPermissions is the full permission set as sorted strings, built
// once. PermissionsFor's superuser branch is a pure function of All, so there
// is no reason to rebuild and re-sort it per call.
var superuserPermissions = func() []string {
	out := make([]string, 0, len(All))
	for _, perm := range All {
		out = append(out, string(perm))
	}
	sort.Strings(out)
	return out
}()

// PermissionsFor returns p's effective permissions as sorted strings, for
// transmission to clients over GET /auth/me. Clients gate on these strings
// directly rather than re-deriving them from roles, so the policy matrix lives
// in exactly one place. A Superuser principal (auth disabled) reports the full
// set, which is what keeps auth-off clients showing every control enabled.
//
// Always returns a non-nil slice so it marshals as [] rather than null.
func PermissionsFor(p auth.Principal) []string {
	if p.Superuser {
		// Copied so a caller mutating the returned slice cannot corrupt the
		// shared value.
		return slices.Clone(superuserPermissions)
	}
	seen := make(map[Permission]bool)
	for _, role := range p.Roles {
		for perm, ok := range grants[role] {
			if ok {
				seen[perm] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for perm := range seen {
		out = append(out, string(perm))
	}
	sort.Strings(out)
	return out
}

// Can reports whether p may perform perm. A Superuser principal (the anonymous
// identity injected when auth is disabled, and reserved bootstrap) bypasses the
// matrix. Otherwise the permission is granted if any of p.Roles grants it.
func Can(p auth.Principal, perm Permission) bool {
	if p.Superuser {
		return true
	}
	for _, role := range p.Roles {
		if grants[role][perm] {
			return true
		}
	}
	return false
}
