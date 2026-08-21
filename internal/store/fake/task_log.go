// SPDX-License-Identifier: AGPL-3.0-or-later

package fake

import (
	"context"
	"slices"

	"github.com/uberware/sqi/internal/store"
)

// CreateTaskLog implements [store.TaskLogStore]. It does not enforce the
// attempt_id foreign key that the SQLite backend does — a log row is
// appended even when AttemptID names no existing task_attempts row. Tests
// that exercise a log-ingest error path tied to a missing/deleted attempt
// need the real SQLite store, or a wrapper around this one, to observe it.
func (s *Store) CreateTaskLog(_ context.Context, log store.TaskLog) (store.TaskLog, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.taskLogs = append(s.taskLogs, log)
	return log, nil
}

// ListTaskLogs implements [store.TaskLogStore].
// Returns chunks for attemptID with NATSSeq > afterNATSSeq, sorted by NATSSeq ascending.
func (s *Store) ListTaskLogs(_ context.Context, attemptID string, afterNATSSeq int64, limit int) ([]store.TaskLog, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if limit <= 0 {
		limit = store.DefaultLimit
	}

	var matching []store.TaskLog
	for _, l := range s.taskLogs {
		if l.AttemptID == attemptID && l.NATSSeq > afterNATSSeq {
			matching = append(matching, l)
		}
	}

	slices.SortStableFunc(matching, func(a, b store.TaskLog) int {
		if a.NATSSeq < b.NATSSeq {
			return -1
		}
		if a.NATSSeq > b.NATSSeq {
			return 1
		}
		return 0
	})

	if len(matching) > limit {
		matching = matching[:limit]
	}
	return matching, nil
}
