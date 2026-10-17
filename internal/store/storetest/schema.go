// SPDX-License-Identifier: AGPL-3.0-or-later

package storetest

import (
	"context"
	"database/sql"
	"testing"

	"github.com/uberware/sqi/internal/store/migrations"
)

// RecordNewerSchema records, in db's goose table, one more applied schema
// version than this binary knows, as a newer sqi-server's migration would have
// left it, and returns that version. driver is "sqlite" or "postgres".
func RecordNewerSchema(t testing.TB, db *sql.DB, driver string) int64 {
	t.Helper()
	maxV, err := migrations.MaxVersion(driver)
	if err != nil {
		t.Fatal(err)
	}
	q := `INSERT INTO goose_db_version (version_id, is_applied) VALUES (?, 1)`
	if driver == "postgres" {
		q = `INSERT INTO goose_db_version (version_id, is_applied) VALUES ($1, true)`
	}
	if _, err := db.ExecContext(context.Background(), q, maxV+1); err != nil {
		t.Fatalf("record schema version %d: %v", maxV+1, err)
	}
	return maxV + 1
}
