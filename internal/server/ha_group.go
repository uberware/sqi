// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"context"
	"log/slog"
	"time"

	"github.com/uberware/sqi/internal/ha"
	"github.com/uberware/sqi/internal/store"
)

// watchGroup observes the group at once and then at the membership cadence
// ([ha.RenewInterval]), until ctx ends.
func (s *Server) watchGroup(ctx context.Context) {
	t := time.NewTicker(ha.RenewInterval(s.cfg.HA.LeaseTTL))
	defer t.Stop()
	for {
		s.observeGroup(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// observeGroup reads the lease and the members once and acts on what a
// standby learns from them: the leader's name and epoch for /readyz,
// /leaderz and the API gate's 503 body; a WARN when the leader runs another
// version; and, when the leader grants workers differently, leaving the
// sqi-auth queue group so a worker is never authorized differently depending
// on which server answers. The leader always answers callouts.
func (s *Server) observeGroup(ctx context.Context) {
	if s.ha.role.isLeader() {
		s.ha.broker.SetCalloutActive(true)
		return
	}
	octx, cancel := context.WithTimeout(ctx, ha.RenewInterval(s.cfg.HA.LeaseTTL))
	defer cancel()
	lease, err := s.store.GetLeadership(octx)
	if err != nil {
		s.warnObserve(ctx, err)
		return
	}
	members, _, err := s.store.ListServerMembers(octx)
	if err != nil {
		s.warnObserve(ctx, err)
		return
	}

	leader := leaseHolder(lease, members)
	name := ""
	if leader != nil {
		name = leader.Name
	} else if lease.Held() {
		name = lease.Holder // a holder with no member row: name it by its id
	}
	s.ha.role.observe(name, lease.Epoch)
	s.metrics.HAEpoch.Set(float64(lease.Epoch))

	if leader == nil || leader.InstanceID == s.ha.self.InstanceID {
		s.ha.broker.SetCalloutActive(true)
		return
	}
	s.compareVersion(ctx, leader)
	s.ha.broker.SetCalloutActive(s.compareGrants(ctx, leader))
}

// warnObserve logs a failed observation, unless the server is stopping.
func (s *Server) warnObserve(ctx context.Context, err error) {
	if ctx.Err() == nil {
		s.logger.WarnContext(ctx, "ha: could not read the group's lease and members", slog.Any("error", err))
	}
}

// leaseHolder is the member row of the server holding an unexpired lease, or
// nil when no live lease is held or its holder has no row.
func leaseHolder(lease store.Lease, members []store.ServerMember) *store.ServerMember {
	if !lease.Held() {
		return nil
	}
	for i := range members {
		if members[i].InstanceID == lease.Holder {
			return &members[i]
		}
	}
	return nil
}

// compareVersion logs one WARN per distinct pair when the leader runs another
// version than this standby. A mixed-version group is allowed during a
// rolling upgrade; it is worth knowing about.
func (s *Server) compareVersion(ctx context.Context, leader *store.ServerMember) {
	self := s.ha.self.Version
	if leader.Version == self || !s.firstWarning("version", leader.Version, self) {
		return
	}
	s.logger.WarnContext(ctx, "ha: the leader runs another version of sqi-server than this standby",
		slog.String("leader", leader.Name),
		slog.String("leader_version", leader.Version),
		slog.String("version", self),
		slog.String("impact", "allowed while a rolling upgrade is under way; upgrade every server of the group to the same version"))
}

// compareGrants reports whether this standby grants workers what the leader
// does, logging one WARN per distinct pair when it does not.
func (s *Server) compareGrants(ctx context.Context, leader *store.ServerMember) bool {
	self := s.ha.self.GrantsFingerprint
	if leader.GrantsFingerprint == self {
		return true
	}
	if s.firstWarning("grants", leader.GrantsFingerprint, self) {
		s.logger.WarnContext(ctx, "ha: this standby grants workers different broker permissions than the leader; it answers no worker connects",
			slog.String("leader", leader.Name),
			slog.String("leader_version", leader.Version),
			slog.String("impact", "the other servers of the group answer every worker connect until this one leads or the grants match again"))
	}
	return false
}

// firstWarning reports whether the (leader, self) pair of kind has not been
// warned about yet, and records it.
func (s *Server) firstWarning(kind, leader, self string) bool {
	key := kind + "\x00" + leader + "\x00" + self
	if s.ha.warned[key] {
		return false
	}
	s.ha.warned[key] = true
	return true
}
