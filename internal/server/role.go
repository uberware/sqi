// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"sync"

	"github.com/uberware/sqi/internal/health"
)

// Role is a server's place in its high-availability group.
type Role string

const (
	// RoleStandby is a server waiting, warm, to take over: it serves only its
	// probes and metrics.
	RoleStandby Role = "standby"
	// RoleLeader is the one server of the group that schedules and serves the
	// API.
	RoleLeader Role = "leader"
)

// roleState is what an HA server reports about its role and its group's
// leader, read by the HTTP gate, /readyz and /leaderz on every request. Safe
// for concurrent use.
type roleState struct {
	mu     sync.Mutex
	role   Role
	leader string // the leader's name, "" when none is known
	epoch  int64
}

func newRoleState() *roleState { return &roleState{role: RoleStandby} }

// isLeader reports whether this server leads.
func (r *roleState) isLeader() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.role == RoleLeader
}

// leaderName is the leader's name, this server's own while it leads.
func (r *roleState) leaderName() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.leader
}

// info is the role as /readyz and /leaderz report it.
func (r *roleState) info() health.RoleInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	return health.RoleInfo{Role: string(r.role), Leader: r.leader, Epoch: r.epoch}
}

// promote records that this server, named self, leads at epoch.
func (r *roleState) promote(self string, epoch int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.role, r.leader, r.epoch = RoleLeader, self, epoch
}

// demote records that this server no longer leads. The leader is unknown
// until the group is next observed.
func (r *roleState) demote() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.role == RoleLeader {
		r.role, r.leader = RoleStandby, ""
	}
}

// observe records the leader a standby sees, and the epoch. It changes
// nothing while this server leads: its own promotion is the authority then.
func (r *roleState) observe(leader string, epoch int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.role == RoleLeader {
		return
	}
	r.leader, r.epoch = leader, epoch
}
