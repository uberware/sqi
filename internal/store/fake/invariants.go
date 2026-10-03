// SPDX-License-Identifier: AGPL-3.0-or-later

package fake

import (
	"slices"

	"github.com/uberware/sqi/internal/store"
)

// ClaimInvariantViolations returns the IDs of active usage claims that break
// invariant I3: the attempt is missing or not running, or its task is
// terminal. Mirrors the SQLite diagnostic of the same name. Not part of
// store.Store.
func (s *Store) ClaimInvariantViolations() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []string
	for id, c := range s.usageClaims {
		if c.ReleasedAt != nil {
			continue
		}
		a, ok := s.taskAttempts[c.TaskAttemptID]
		if !ok || a.Status != store.AttemptStatusRunning {
			ids = append(ids, id)
			continue
		}
		if t, ok := s.tasks[a.TaskID]; ok && (t.Status == store.TaskStatusSucceeded ||
			t.Status == store.TaskStatusFailed || t.Status == store.TaskStatusCanceled) {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids
}
