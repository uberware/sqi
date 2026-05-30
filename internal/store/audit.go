// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"time"
)

// AuditEntry is a single append-only record in the audit log. The log captures
// state-changing actions performed by users or internal components so that
// operators can reconstruct what happened and who did it.
//
// Details is a free-form map for action-specific context — before/after values,
// failure reasons, parameter diffs, etc. The interpretation of Details is
// defined by the code that writes the entry.
type AuditEntry struct {
	ID         string
	EntityType string         // e.g. "job", "worker", "queue", "farm"
	EntityID   string         // ID of the affected entity
	Action     string         // e.g. "submitted", "canceled", "priority_changed"
	Actor      string         // authenticated identity; empty in no-auth deployments
	Details    map[string]any // action-specific context
	CreatedAt  time.Time
}

// AuditStore is the persistence interface for [AuditEntry] records.
// The audit log is append-only; there are no update or delete methods.
type AuditStore interface {
	// AppendAuditEntry inserts a new audit log entry. The caller must populate
	// all fields including a unique ID and CreatedAt.
	AppendAuditEntry(ctx context.Context, entry AuditEntry) error

	// ListAuditEntries returns all audit entries for the given entity,
	// ordered by CreatedAt ascending. Pass empty strings to list all entries
	// (for administrative views).
	ListAuditEntries(ctx context.Context, entityType, entityID string) ([]AuditEntry, error)
}
