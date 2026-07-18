// SPDX-License-Identifier: AGPL-3.0-or-later

// Package policy is the authorization matrix for sqi's four built-in roles.
// It maps (role, permission) to allow/deny and is consulted by the REST auth
// middleware and the WebSocket subscribe path. It has no dependencies beyond
// the Principal it evaluates, so it stays pure and exhaustively testable.
//
// The matrix here is the server-side source of truth; web/src/auth/policy.ts
// mirrors it and docs/auth.md documents it — keep all three in lockstep.
package policy

import "github.com/uberware/sqi/internal/auth"

// Permission is a per-resource-verb capability gating one or more routes.
type Permission string

const (
	// JobsRead grants read access to jobs.
	JobsRead Permission = "jobs.read"
	// JobsWrite grants create/update/delete access to jobs.
	JobsWrite Permission = "jobs.write"
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
)

// grants maps each built-in role to the set of permissions it holds. A role
// absent from a permission's set is denied that permission.
var grants = map[string]map[Permission]bool{
	"read-only": {
		JobsRead: true, WorkersRead: true, InfraRead: true,
		ProductsRead: true, APIKeysSelf: true,
	},
	"user": {
		JobsRead: true, JobsWrite: true, WorkersRead: true, InfraRead: true,
		ProductsRead: true, APIKeysSelf: true,
	},
	"operator": {
		JobsRead: true, JobsWrite: true, WorkersRead: true, WorkersManage: true,
		InfraRead: true, InfraManage: true, ProductsRead: true, ProductsManage: true,
		DiagnosticsRead: true, APIKeysSelf: true,
	},
	"admin": {
		JobsRead: true, JobsWrite: true, WorkersRead: true, WorkersManage: true,
		InfraRead: true, InfraManage: true, ProductsRead: true, ProductsManage: true,
		DiagnosticsRead: true, UsersRead: true, UsersManage: true,
		APIKeysSelf: true, APIKeysAdmin: true,
	},
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
