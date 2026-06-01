// SPDX-License-Identifier: AGPL-3.0-only

package sqlite

import (
	"context"
	"time"

	"github.com/uberware/sqi/internal/store"
)

const logCols = `id, task_id, attempt_id, worker_seq, nats_seq, at, received_at, stream, data`

const (
	sqlInsertTaskLog = `
INSERT INTO task_logs (id, task_id, attempt_id, worker_seq, nats_seq, at, received_at, stream, data)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING ` + logCols

	// Forward-paging query: return chunks for an attempt with nats_seq > afterNATSSeq,
	// ordered by nats_seq ascending.
	sqlListTaskLogs = `
SELECT ` + logCols + `
FROM   task_logs
WHERE  attempt_id = ?
  AND  nats_seq   > ?
ORDER  BY nats_seq ASC
LIMIT  ?`
)

// CreateTaskLog implements [store.TaskLogStore].
func (s *Store) CreateTaskLog(ctx context.Context, log store.TaskLog) (store.TaskLog, error) {
	now := timeToText(time.Now().UTC())
	row := s.stmtInsertTaskLog.QueryRowContext(
		ctx,
		log.ID, log.TaskID, log.AttemptID,
		log.SeqNum, log.NATSSeq,
		timeToText(log.At), now,
		string(log.Stream), log.Data,
	)
	out, err := scanTaskLog(row)
	return out, mapErr(err)
}

// ListTaskLogs implements [store.TaskLogStore].
func (s *Store) ListTaskLogs(ctx context.Context, attemptID string, afterNATSSeq int64, limit int) ([]store.TaskLog, error) {
	if limit <= 0 {
		limit = store.DefaultLimit
	}
	rows, err := s.stmtListTaskLogs.QueryContext(ctx, attemptID, afterNATSSeq, limit)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()

	var logs []store.TaskLog
	for rows.Next() {
		l, err := scanTaskLog(rows)
		if err != nil {
			return nil, err
		}
		logs = append(logs, l)
	}
	return logs, rows.Err()
}

func scanTaskLog(row scanner) (store.TaskLog, error) {
	var l store.TaskLog
	var atStr, receivedAtStr, streamStr string
	if err := row.Scan(
		&l.ID, &l.TaskID, &l.AttemptID,
		&l.SeqNum, &l.NATSSeq,
		&atStr, &receivedAtStr,
		&streamStr, &l.Data,
	); err != nil {
		return store.TaskLog{}, err
	}
	l.At = mustTime(atStr)
	l.ReceivedAt = mustTime(receivedAtStr)
	l.Stream = store.LogStream(streamStr)
	return l, nil
}
