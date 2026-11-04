// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"sync"

	"github.com/uberware/sqi/internal/ha"
	"github.com/uberware/sqi/internal/health"
)

// roleState is what an HA server reports about its role and its group's
// leader, read by the HTTP gate, /readyz and /leaderz on every request. Safe
// for concurrent use. The role is [ha.RoleLeader] for the one server of the
// group that schedules and serves the API, and [ha.RoleStandby] for one
// waiting, warm, to take over, which serves only its probes and metrics.
type roleState struct {
	mu     sync.Mutex
	role   string
	leader string // the leader's name, "" when none is known
	epoch  int64
}

func newRoleState() *roleState { return &roleState{role: ha.RoleStandby} }

// isLeader reports whether this server leads.
func (r *roleState) isLeader() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.role == ha.RoleLeader
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
	return health.RoleInfo{Role: r.role, IsLeader: r.role == ha.RoleLeader, Leader: r.leader, Epoch: r.epoch}
}

// promote records that this server, named self, leads at epoch.
func (r *roleState) promote(self string, epoch int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.role, r.leader, r.epoch = ha.RoleLeader, self, epoch
}

// demote records that this server no longer leads. The leader is unknown
// until the group is next observed.
func (r *roleState) demote() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.role == ha.RoleLeader {
		r.role, r.leader = ha.RoleStandby, ""
	}
}

// observe records the leader a standby sees, and the epoch. It changes
// nothing while this server leads: its own promotion is the authority then.
func (r *roleState) observe(leader string, epoch int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.role == ha.RoleLeader {
		return
	}
	r.leader, r.epoch = leader, epoch
}
