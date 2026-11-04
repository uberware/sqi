// SPDX-License-Identifier: AGPL-3.0-or-later

package fake

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/uberware/sqi/internal/store"
)

// leadershipState is the fake's server_lease row and server_members table. Its clock stands in for the database's.
type leadershipState struct {
	holder    string // "" when released
	epoch     int64
	expiresUS int64
	members   map[string]store.ServerMember
	nowUS     func() int64 // time.Now().UnixMicro when nil
}

// now returns the fake database clock in microseconds.
func (l *leadershipState) now() int64 {
	if l.nowUS != nil {
		return l.nowUS()
	}
	return time.Now().UnixMicro()
}

// SetClock replaces the clock the lease and membership timestamps are read
// from, so a test moves time without sleeping. now returns microseconds since
// the Unix epoch; nil restores the real clock.
func (s *Store) SetClock(now func() int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.leadership.nowUS = now
}

// AcquireLeadership implements [store.LeadershipStore].
func (s *Store) AcquireLeadership(_ context.Context, holder string, ttl time.Duration) (epoch int64, ok bool, err error) {
	ttlUS, err := store.LeaseTTLMicros(ttl)
	if err != nil {
		return 0, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	l := &s.leadership
	now := l.now()
	if (store.Lease{Holder: l.holder, ExpiresUS: l.expiresUS, NowUS: now}).Held() {
		return 0, false, nil
	}
	l.holder = holder
	l.epoch++
	l.expiresUS = now + ttlUS
	return l.epoch, true, nil
}

// RenewLeadership implements [store.LeadershipStore].
func (s *Store) RenewLeadership(_ context.Context, holder string, epoch int64, ttl time.Duration) (bool, error) {
	ttlUS, err := store.LeaseTTLMicros(ttl)
	if err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	l := &s.leadership
	if l.holder != holder || l.epoch != epoch {
		return false, nil
	}
	l.expiresUS = l.now() + ttlUS
	return true, nil
}

// ReleaseLeadership implements [store.LeadershipStore].
func (s *Store) ReleaseLeadership(_ context.Context, holder string, epoch int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if l := &s.leadership; l.holder == holder && l.epoch == epoch {
		l.holder = ""
	}
	return nil
}

// GetLeadership implements [store.LeadershipStore].
func (s *Store) GetLeadership(_ context.Context) (store.Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := &s.leadership
	return store.Lease{Holder: l.holder, Epoch: l.epoch, ExpiresUS: l.expiresUS, NowUS: l.now()}, nil
}

// UpsertServerMember implements [store.LeadershipStore].
func (s *Store) UpsertServerMember(_ context.Context, m store.ServerMember) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := &s.leadership
	now := l.now()
	m.LastSeenUS = now
	m.BrokerOKUS = nil
	if prev, ok := l.members[m.InstanceID]; ok {
		m.BrokerOKUS = prev.BrokerOKUS
	}
	if m.BrokerOK {
		m.BrokerOKUS = &now
	}
	m.BrokerOK = false
	if l.members == nil {
		l.members = make(map[string]store.ServerMember)
	}
	l.members[m.InstanceID] = m
	return nil
}

// ListServerMembers implements [store.LeadershipStore].
func (s *Store) ListServerMembers(_ context.Context) ([]store.ServerMember, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := &s.leadership
	out := make([]store.ServerMember, 0, len(l.members))
	for _, m := range l.members {
		out = append(out, m)
	}
	slices.SortFunc(out, func(a, b store.ServerMember) int {
		if c := strings.Compare(a.Name, b.Name); c != 0 {
			return c
		}
		return strings.Compare(a.InstanceID, b.InstanceID)
	})
	return out, l.now(), nil
}

// DeleteServerMembersUnseenFor implements [store.LeadershipStore].
func (s *Store) DeleteServerMembersUnseenFor(_ context.Context, d time.Duration) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := &s.leadership
	cutoff := l.now() - d.Microseconds()
	n := 0
	for id, m := range l.members {
		if m.LastSeenUS < cutoff {
			delete(l.members, id)
			n++
		}
	}
	return n, nil
}
