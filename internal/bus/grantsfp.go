// SPDX-License-Identifier: AGPL-3.0-or-later

package bus

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"

	"github.com/nats-io/jwt/v2"
	natsserver "github.com/nats-io/nats-server/v2/server"

	"github.com/uberware/sqi/internal/brokerauth"
)

// fingerprintSample is the worker ID [GrantsFingerprint] samples the grants
// for. Any valid ID would do; it is fixed so that every server hashes the
// same input.
const fingerprintSample = "fingerprint-sample"

// grantsFingerprintVersion is folded into the hash so that a change to the
// encoding below never matches a server still on the old one.
const grantsFingerprintVersion = 1

// GrantsFingerprint is a hex SHA-256 of what a worker is granted: the
// permissions [brokerauth.WorkerPermissions] returns for a fixed sample
// worker ID, and the permission part of the user JWT the auth callout issues
// with them. Two servers with the same fingerprint authorize any worker
// identically, so it does not matter which of them the callout queue group
// picks; a standby whose fingerprint differs from the leader's leaves the
// group.
//
// The encoding is canonical: subject lists are sorted, so the same grants
// listed in another order give the same fingerprint.
func GrantsFingerprint() string {
	return grantsFingerprint(brokerauth.WorkerPermissions)
}

// grantsFingerprint is [GrantsFingerprint] over the grants perms returns.
func grantsFingerprint(perms func(workerID string) *natsserver.Permissions) string {
	server := perms(fingerprintSample)
	uc := jwt.NewUserClaims(fingerprintSample)
	applyWorkerGrants(&uc.User, server)
	user := uc.User
	sortUserPermissions(&user.Permissions)

	body, err := json.Marshal(struct {
		Version int                      `json:"version"`
		Server  natsserver.Permissions   `json:"server"`
		User    jwt.UserPermissionLimits `json:"user"`
	}{grantsFingerprintVersion, sortedServerPermissions(server), user.UserPermissionLimits})
	if err != nil {
		// Plain structs of strings and numbers always encode; hashing the
		// error keeps the result a fingerprint no healthy server has.
		body = []byte("unencodable: " + err.Error())
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// applyWorkerGrants sets on u everything a worker's user JWT grants it: the
// permissions p, in JWT form, and the connection types it may use. The
// callout and [GrantsFingerprint] both build the grants here, so the
// fingerprint covers what the callout actually issues.
func applyWorkerGrants(u *jwt.User, p *natsserver.Permissions) {
	u.Permissions = brokerauth.UserPermissions(p)
	// nats-server enforces this for callout users from version 2.15.2.
	u.AllowedConnectionTypes.Add(jwt.ConnectionTypeStandard)
}

// sortedServerPermissions returns a copy of p with every subject list sorted.
func sortedServerPermissions(p *natsserver.Permissions) natsserver.Permissions {
	sorted := func(sp *natsserver.SubjectPermission) *natsserver.SubjectPermission {
		if sp == nil {
			return nil
		}
		return &natsserver.SubjectPermission{Allow: sortedCopy(sp.Allow), Deny: sortedCopy(sp.Deny)}
	}
	out := natsserver.Permissions{Publish: sorted(p.Publish), Subscribe: sorted(p.Subscribe)}
	if p.Response != nil {
		resp := *p.Response
		out.Response = &resp
	}
	return out
}

// sortUserPermissions sorts p's subject lists in place.
func sortUserPermissions(p *jwt.Permissions) {
	slices.Sort(p.Pub.Allow)
	slices.Sort(p.Pub.Deny)
	slices.Sort(p.Sub.Allow)
	slices.Sort(p.Sub.Deny)
}

func sortedCopy(s []string) []string {
	if s == nil {
		return nil
	}
	return slices.Sorted(slices.Values(s))
}
