// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store/sqlstore"
)

var _ sqlstore.Dialect = sqliteDialect{}

func TestSQLiteDialect_Rebind(t *testing.T) {
	for _, q := range []string{
		"",
		"SELECT 1",
		"INSERT INTO farms (id, name, created_at, updated_at) VALUES (?, ?, ?, ?)",
		"UPDATE jobs SET status = ? WHERE id = ? AND status IN (?, ?)",
	} {
		if got := (sqliteDialect{}).Rebind(q); got != q {
			t.Errorf("Rebind(%q) = %q, want the query unchanged", q, got)
		}
	}
}

func TestSQLiteDialect_CaseInsensitiveFragments(t *testing.T) {
	d := sqliteDialect{}
	for _, c := range []struct{ col, equal, like string }{
		{"owner", "owner = ? COLLATE NOCASE", "owner LIKE ?"},
		{"username", "username = ? COLLATE NOCASE", "username LIKE ?"},
		{"name", "name = ? COLLATE NOCASE", "name LIKE ?"},
	} {
		if got := d.CIEqual(c.col); got != c.equal {
			t.Errorf("CIEqual(%q) = %q, want %q", c.col, got, c.equal)
		}
		if got := d.CILike(c.col); got != c.like {
			t.Errorf("CILike(%q) = %q, want %q", c.col, got, c.like)
		}
	}
}

func TestSQLiteDialect_BindBool(t *testing.T) {
	d := sqliteDialect{}
	if got := d.BindBool(true); got != int64(1) {
		t.Errorf("BindBool(true) = %#v, want int64(1)", got)
	}
	if got := d.BindBool(false); got != int64(0) {
		t.Errorf("BindBool(false) = %#v, want int64(0)", got)
	}
}

func TestSQLiteDialect_BindTime(t *testing.T) {
	// A non-UTC instant with all nine fractional digits: the bound text is the
	// UTC form, fixed at 30 characters.
	at := time.Date(2026, 10, 10, 13, 0, 5, 123456789, time.FixedZone("CET", 3600))
	got := (sqliteDialect{}).BindTime(at)
	const want = "2026-10-10T12:00:05.123456789Z"
	if got != want || len(want) != 30 {
		t.Errorf("BindTime = %#v, want the 30-character string %q", got, want)
	}
	if got != sqlstore.TimeText(at) {
		t.Errorf("BindTime = %#v, want sqlstore.TimeText = %q", got, sqlstore.TimeText(at))
	}
}

func TestSQLiteDialect_Classify(t *testing.T) {
	d := sqliteDialect{}
	for _, c := range []struct {
		name string
		err  error
		want sqlstore.ErrClass
	}{
		{"no rows", sql.ErrNoRows, sqlstore.ErrClassNotFound},
		{"wrapped no rows", fmt.Errorf("scan: %w", sql.ErrNoRows), sqlstore.ErrClassNotFound},
		{"other", errors.New("boom"), sqlstore.ErrClassOther},
	} {
		if got := d.Classify(c.err); got != c.want {
			t.Errorf("%s: Classify = %d, want %d", c.name, got, c.want)
		}
	}
}

// TestSQLiteDialect_LockAnchorsDoesNothing passes no transaction: a dialect
// that locked rows would dereference it.
func TestSQLiteDialect_LockAnchorsDoesNothing(t *testing.T) {
	err := (sqliteDialect{}).LockAnchors(t.Context(), nil,
		sqlstore.Anchor{Table: "jobs", ID: "j1", Mode: sqlstore.LockUpdate},
		sqlstore.Anchor{Table: "jobs", ID: "j2", Mode: sqlstore.LockShare})
	if err != nil {
		t.Fatalf("LockAnchors = %v, want nil", err)
	}
}
