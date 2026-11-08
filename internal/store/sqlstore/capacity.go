// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlstore

import (
	"context"
	"fmt"
	"time"

	"github.com/uberware/sqi/internal/store"
)

// ── Capacity groups ───────────────────────────────────────────────────────────

const sqlCapacityGroupColumns = `id, farm_id, name, kind, provider, enabled, queue_ids,
       min_machines, max_machines, priority, max_step_up,
       scale_up_cooldown_ms, idle_timeout_ms, boot_timeout_ms,
       retire_action, profile, created_at, updated_at`

const (
	sqlInsertCapacityGroup = `
INSERT INTO capacity_groups (` + sqlCapacityGroupColumns + `)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING ` + sqlCapacityGroupColumns

	sqlGetCapacityGroup = `SELECT ` + sqlCapacityGroupColumns + ` FROM capacity_groups WHERE id = ?`

	sqlGetCapacityGroupByName = `SELECT ` + sqlCapacityGroupColumns + ` FROM capacity_groups WHERE name = ?`

	sqlListCapacityGroups = `SELECT ` + sqlCapacityGroupColumns + ` FROM capacity_groups ORDER BY priority DESC, name`

	// The ID, farm and creation time do not change.
	sqlUpdateCapacityGroup = `
UPDATE capacity_groups
SET name = ?, kind = ?, provider = ?, enabled = ?, queue_ids = ?,
    min_machines = ?, max_machines = ?, priority = ?, max_step_up = ?,
    scale_up_cooldown_ms = ?, idle_timeout_ms = ?, boot_timeout_ms = ?,
    retire_action = ?, profile = ?, updated_at = ?
WHERE id = ?
RETURNING ` + sqlCapacityGroupColumns

	sqlDeleteCapacityGroup = `DELETE FROM capacity_groups WHERE id = ?`
)

func scanCapacityGroup(row scanner) (store.CapacityGroup, error) {
	var g store.CapacityGroup
	var enabled dbBool
	var kind, provider, queueIDs, profile, createdAt, updatedAt string
	var cooldownMs, idleMs, bootMs int64
	if err := row.Scan(
		&g.ID, &g.FarmID, &g.Name, &kind, &provider, &enabled, &queueIDs,
		&g.MinMachines, &g.MaxMachines, &g.Priority, &g.MaxStepUp,
		&cooldownMs, &idleMs, &bootMs,
		&g.RetireAction, &profile, &createdAt, &updatedAt,
	); err != nil {
		return store.CapacityGroup{}, err
	}
	g.Kind = store.CapacityGroupKind(kind)
	g.Provider = store.CapacityProvider(provider)
	g.Enabled = bool(enabled)
	g.ScaleUpCooldown = time.Duration(cooldownMs) * time.Millisecond
	g.IdleTimeout = time.Duration(idleMs) * time.Millisecond
	g.BootTimeout = time.Duration(bootMs) * time.Millisecond
	var err error
	if g.QueueIDs, err = unmarshalJSON[[]string](queueIDs, nil); err != nil {
		return store.CapacityGroup{}, fmt.Errorf("capacity group %s queue_ids: %w", g.ID, err)
	}
	if len(g.QueueIDs) == 0 {
		g.QueueIDs = nil
	}
	if g.Profile, err = unmarshalJSON(profile, store.MachineProfile{}); err != nil {
		return store.CapacityGroup{}, fmt.Errorf("capacity group %s profile: %w", g.ID, err)
	}
	g.CreatedAt = mustTime(createdAt)
	g.UpdatedAt = mustTime(updatedAt)
	return g, nil
}

// capacityGroupJSON encodes the two JSON columns of a group.
func capacityGroupJSON(g store.CapacityGroup) (queueIDs, profile string, err error) {
	ids := g.QueueIDs
	if ids == nil {
		ids = []string{}
	}
	if queueIDs, err = marshalJSON(ids); err != nil {
		return "", "", fmt.Errorf("encode queue_ids: %w", err)
	}
	if profile, err = marshalJSON(g.Profile); err != nil {
		return "", "", fmt.Errorf("encode profile: %w", err)
	}
	return queueIDs, profile, nil
}

// CreateCapacityGroup implements [store.CapacityGroupStore].
func (s *Store) CreateCapacityGroup(ctx context.Context, g store.CapacityGroup) (store.CapacityGroup, error) {
	queueIDs, profile, err := capacityGroupJSON(g)
	if err != nil {
		return store.CapacityGroup{}, err
	}
	now := TimeText(time.Now().UTC())
	out, err := writeRow(ctx, s, "create capacity group", scanCapacityGroup, s.stmtInsertCapacityGroup,
		g.ID, g.FarmID, g.Name, string(g.Kind), string(g.Provider), g.Enabled, queueIDs,
		g.MinMachines, g.MaxMachines, g.Priority, g.MaxStepUp,
		g.ScaleUpCooldown.Milliseconds(), g.IdleTimeout.Milliseconds(), g.BootTimeout.Milliseconds(),
		g.RetireAction, profile, now, now)
	return out, mapErr(err)
}

// GetCapacityGroup implements [store.CapacityGroupStore].
func (s *Store) GetCapacityGroup(ctx context.Context, id string) (store.CapacityGroup, error) {
	out, err := scanCapacityGroup(s.stmtGetCapacityGroup.QueryRowContext(ctx, id))
	return out, mapErr(err)
}

// GetCapacityGroupByName implements [store.CapacityGroupStore].
func (s *Store) GetCapacityGroupByName(ctx context.Context, name string) (store.CapacityGroup, error) {
	out, err := scanCapacityGroup(s.stmtGetCapacityGroupByName.QueryRowContext(ctx, name))
	return out, mapErr(err)
}

// ListCapacityGroups implements [store.CapacityGroupStore].
func (s *Store) ListCapacityGroups(ctx context.Context) ([]store.CapacityGroup, error) {
	rows, err := s.stmtListCapacityGroups.QueryContext(ctx)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()

	var groups []store.CapacityGroup
	for rows.Next() {
		g, err := scanCapacityGroup(rows)
		if err != nil {
			return nil, err
		}
		groups = append(groups, g)
	}
	return groups, rows.Err()
}

// UpdateCapacityGroup implements [store.CapacityGroupStore].
func (s *Store) UpdateCapacityGroup(ctx context.Context, g store.CapacityGroup) (store.CapacityGroup, error) {
	queueIDs, profile, err := capacityGroupJSON(g)
	if err != nil {
		return store.CapacityGroup{}, err
	}
	now := TimeText(time.Now().UTC())
	out, err := writeRow(ctx, s, "update capacity group", scanCapacityGroup, s.stmtUpdateCapacityGroup,
		g.Name, string(g.Kind), string(g.Provider), g.Enabled, queueIDs,
		g.MinMachines, g.MaxMachines, g.Priority, g.MaxStepUp,
		g.ScaleUpCooldown.Milliseconds(), g.IdleTimeout.Milliseconds(), g.BootTimeout.Milliseconds(),
		g.RetireAction, profile, now, g.ID)
	return out, mapErr(err)
}

// DeleteCapacityGroup implements [store.CapacityGroupStore]. A worker that
// belongs to the group makes the foreign key refuse, which is [store.ErrConflict].
func (s *Store) DeleteCapacityGroup(ctx context.Context, id string) error {
	res, err := s.execStmt(ctx, "delete capacity group", s.stmtDeleteCapacityGroup, id)
	if err != nil {
		return mapErr(err)
	}
	return checkRowsAffected(res)
}
