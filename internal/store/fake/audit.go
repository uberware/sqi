// SPDX-License-Identifier: AGPL-3.0-or-later

package fake

import (
	"context"
	"slices"

	"github.com/uberware/sqi/internal/store"
)

// AppendAuditEntry inserts a new audit log entry.
func (s *Store) AppendAuditEntry(_ context.Context, entry store.AuditEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	entry.Details = copyMap(entry.Details)
	s.auditEntries = append(s.auditEntries, entry)
	return nil
}

// ListAuditEntries returns all audit entries for the given entity,
// ordered by CreatedAt ascending. Pass empty strings to list all entries.
func (s *Store) ListAuditEntries(_ context.Context, entityType, entityID string) ([]store.AuditEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entries := make([]store.AuditEntry, 0)
	for _, entry := range s.auditEntries {
		if entityType != "" && entry.EntityType != entityType {
			continue
		}
		if entityID != "" && entry.EntityID != entityID {
			continue
		}
		entry.Details = copyMap(entry.Details)
		entries = append(entries, entry)
	}

	slices.SortStableFunc(entries, func(a, b store.AuditEntry) int {
		if a.CreatedAt.Before(b.CreatedAt) {
			return -1
		}
		if a.CreatedAt.After(b.CreatedAt) {
			return 1
		}
		return 0
	})

	return entries, nil
}
