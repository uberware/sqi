// SPDX-License-Identifier: AGPL-3.0-or-later

package fake

import (
	"context"
	"time"
)

// GetBrokerIdentity implements [store.BrokerStateStore].
func (s *Store) GetBrokerIdentity(_ context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.brokerIdentity, nil
}

// SetBrokerIdentity implements [store.BrokerStateStore].
func (s *Store) SetBrokerIdentity(ctx context.Context, identity string, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.fenceLocked(ctx); err != nil {
		return err
	}
	s.brokerIdentity = identity
	return nil
}
