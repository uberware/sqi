// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"context"
	"database/sql"
	"time"

	"github.com/uberware/sqi/internal/store"
)

const auditCols = `id, entity_type, entity_id, action, actor, details, created_at`

const (
	sqlInsertAudit = `
INSERT INTO audit_log (id, entity_type, entity_id, action, actor, details, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?)`

	// sqlListAuditByEntity filters by both entity_type and entity_id.
	sqlListAuditByEntity = `
SELECT ` + auditCols + `
FROM audit_log
WHERE entity_type = ? AND entity_id = ?
ORDER BY created_at ASC`

	// sqlListAuditAll returns every entry, used when both filters are empty.
	sqlListAuditAll = `
SELECT ` + auditCols + ` FROM audit_log ORDER BY created_at ASC`
)

func scanAuditEntry(row scanner) (store.AuditEntry, error) {
	var e store.AuditEntry
	var detailsJSON, createdAt string

	if err := row.Scan(
		&e.ID, &e.EntityType, &e.EntityID, &e.Action, &e.Actor, &detailsJSON, &createdAt,
	); err != nil {
		return store.AuditEntry{}, err
	}

	e.CreatedAt = mustTime(createdAt)

	details, err := unmarshalJSON(detailsJSON, map[string]any{})
	if err != nil {
		return store.AuditEntry{}, err
	}
	e.Details = details

	return e, nil
}

// AppendAuditEntry implements [store.AuditStore].
func (s *Store) AppendAuditEntry(ctx context.Context, entry store.AuditEntry) error {
	detailsJSON, err := marshalJSON(entry.Details)
	if err != nil {
		return err
	}
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = time.Now().UTC()
	}
	_, err = s.stmtInsertAudit.ExecContext(ctx,
		entry.ID, entry.EntityType, entry.EntityID, entry.Action, entry.Actor,
		detailsJSON, timeToText(entry.CreatedAt))
	return mapErr(err)
}

// ListAuditEntries implements [store.AuditStore].
// Passing empty strings for both entityType and entityID returns all entries.
func (s *Store) ListAuditEntries(ctx context.Context, entityType, entityID string) ([]store.AuditEntry, error) {
	var (
		rows *sql.Rows
		err  error
	)

	if entityType == "" && entityID == "" {
		rows, err = s.db.QueryContext(ctx, sqlListAuditAll)
	} else {
		rows, err = s.db.QueryContext(ctx, sqlListAuditByEntity, entityType, entityID)
	}
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()

	var entries []store.AuditEntry
	for rows.Next() {
		e, err := scanAuditEntry(rows)
		if err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}
