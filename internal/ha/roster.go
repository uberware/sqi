// SPDX-License-Identifier: AGPL-3.0-or-later

package ha

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/uberware/sqi/internal/store"
)

// The roles a member of the group can have in a [Roster].
const (
	// RoleLeader is the member holding the unexpired leader lease.
	RoleLeader = "leader"
	// RoleStandby is a member seen within the lease TTL that does not lead.
	RoleStandby = "standby"
	// RoleMissing is a member not seen within the lease TTL: stopped, crashed
	// or cut off from the database.
	RoleMissing = "missing"
)

// RosterMember is one server of the group as an operator sees it.
type RosterMember struct {
	InstanceID string
	Name       string
	Host       string
	PID        int
	Version    string
	Role       string
	StartedAt  time.Time
	// LastSeenAgo is how long before the database clock read the member last
	// refreshed its row. Never negative.
	LastSeenAgo time.Duration
	// BrokerOK reports whether the member reached its broker within the lease
	// TTL.
	BrokerOK bool
}

// Roster is the group as the database holds it: the current epoch and every
// member, leader first.
type Roster struct {
	Epoch   int64
	Members []RosterMember
}

// BuildRoster classifies members against lease on the database clock nowUS,
// so no server's own clock enters it. A member leads when it holds the lease
// and the lease has not lapsed; otherwise it is a standby when seen within
// ttl, and missing when not. Members are ordered leader first, then by name,
// then by instance id. Members is never nil.
func BuildRoster(lease store.Lease, members []store.ServerMember, nowUS int64, ttl time.Duration) Roster {
	out := make([]RosterMember, 0, len(members))
	leased := lease.Held()
	for _, m := range members {
		age := max(time.Duration(nowUS-m.LastSeenUS)*time.Microsecond, 0)
		role := RoleMissing
		switch {
		case leased && m.InstanceID == lease.Holder:
			role = RoleLeader
		case age <= ttl:
			role = RoleStandby
		}
		out = append(out, RosterMember{
			InstanceID:  m.InstanceID,
			Name:        m.Name,
			Host:        m.Host,
			PID:         m.PID,
			Version:     m.Version,
			Role:        role,
			StartedAt:   m.StartedAt,
			LastSeenAgo: age,
			BrokerOK:    m.BrokerSeenWithin(nowUS, ttl),
		})
	}
	slices.SortFunc(out, func(a, b RosterMember) int {
		if al, bl := a.Role == RoleLeader, b.Role == RoleLeader; al != bl {
			if al {
				return -1
			}
			return 1
		}
		if c := strings.Compare(a.Name, b.Name); c != 0 {
			return c
		}
		return strings.Compare(a.InstanceID, b.InstanceID)
	})
	return Roster{Epoch: lease.Epoch, Members: out}
}

// ReadRoster reads the lease and the membership from st and classifies them
// ([BuildRoster]) against ttl, the group's lease_ttl.
func ReadRoster(ctx context.Context, st store.LeadershipStore, ttl time.Duration) (Roster, error) {
	lease, err := st.GetLeadership(ctx)
	if err != nil {
		return Roster{}, err
	}
	members, nowUS, err := st.ListServerMembers(ctx)
	if err != nil {
		return Roster{}, err
	}
	return BuildRoster(lease, members, nowUS, ttl), nil
}
