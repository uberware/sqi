// SPDX-License-Identifier: AGPL-3.0-or-later

package store_test

import (
	"maps"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/storetest"
)

func TestParity_AppendAuditEntry(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			reader, ok := st.(storetest.AuditReader)
			if !ok {
				t.Fatalf("store %T has no audit reader", st)
			}
			base := time.Now().UTC().Truncate(time.Second)
			later := store.AuditEntry{
				ID: uuid.NewString(), EntityType: "job", EntityID: "j1", Action: "canceled", Actor: "alice",
				Details: map[string]any{"reason": "user"}, CreatedAt: base.Add(500 * time.Millisecond),
			}
			earlier := store.AuditEntry{
				ID: uuid.NewString(), EntityType: "job", EntityID: "j1", Action: "submitted",
				Details: map[string]any{"queue": "q1"}, CreatedAt: base,
			}
			other := store.AuditEntry{
				ID: uuid.NewString(), EntityType: "queue", EntityID: "q1", Action: "paused",
				Details: map[string]any{}, CreatedAt: base,
			}
			// Appended out of time order: the log reads back oldest first.
			for _, e := range []store.AuditEntry{later, earlier, other} {
				if err := st.AppendAuditEntry(ctx, e); err != nil {
					t.Fatalf("AppendAuditEntry %s: %v", e.Action, err)
				}
			}
			got, err := reader.ListAuditEntries(ctx, "job", "j1")
			if err != nil || len(got) != 2 {
				t.Fatalf("ListAuditEntries(job, j1) = (%+v, %v), want 2 entries", got, err)
			}
			for i, want := range []store.AuditEntry{earlier, later} {
				e := got[i]
				if e.ID != want.ID || e.EntityType != want.EntityType || e.EntityID != want.EntityID ||
					e.Action != want.Action || e.Actor != want.Actor || !e.CreatedAt.Equal(want.CreatedAt) ||
					!maps.Equal(e.Details, want.Details) {
					t.Fatalf("entry %d = %+v, want %+v", i, e, want)
				}
			}
			if all, err := reader.ListAuditEntries(ctx, "", ""); err != nil || len(all) != 3 {
				t.Fatalf("ListAuditEntries(all) = (%d entries, %v), want 3", len(all), err)
			}
		})
	}
}
