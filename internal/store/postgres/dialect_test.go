// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/uberware/sqi/internal/store/sqlstore"
)

func TestRebind(t *testing.T) {
	tests := []struct{ in, want string }{
		{"SELECT 1", "SELECT 1"},
		{"x = ?", "x = $1"},
		{"a = ? AND b IN (?, ?) LIMIT ?", "a = $1 AND b IN ($2, $3) LIMIT $4"},
	}
	for _, tc := range tests {
		if got := (pgDialect{}).Rebind(tc.in); got != tc.want {
			t.Errorf("Rebind(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestClassify(t *testing.T) {
	pg := func(code string) error { return fmt.Errorf("wrapped: %w", &pgconn.PgError{Code: code}) }
	tests := []struct {
		name string
		err  error
		want sqlstore.ErrClass
	}{
		{"no rows", sql.ErrNoRows, sqlstore.ErrClassNotFound},
		{"unique", pg("23505"), sqlstore.ErrClassConflict},
		{"foreign key", pg("23503"), sqlstore.ErrClassConflict},
		{"serialization", pg("40001"), sqlstore.ErrClassRetry},
		{"deadlock", pg("40P01"), sqlstore.ErrClassRetry},
		{"check", pg("23514"), sqlstore.ErrClassOther},
		{"plain", errors.New("boom"), sqlstore.ErrClassOther},
	}
	for _, tc := range tests {
		if got := (pgDialect{}).Classify(tc.err); got != tc.want {
			t.Errorf("%s: Classify = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestLockSQL(t *testing.T) {
	q, err := lockSQL(sqlstore.Anchor{Table: "jobs", ID: "j", Mode: sqlstore.LockUpdate}, false)
	if err != nil || q != "SELECT 1 FROM jobs WHERE id = $1 FOR UPDATE" {
		t.Fatalf("update: %q, %v", q, err)
	}
	q, err = lockSQL(sqlstore.Anchor{Table: "workers", ID: "w", Mode: sqlstore.LockShare}, false)
	if err != nil || q != "SELECT 1 FROM workers WHERE id = $1 FOR SHARE" {
		t.Fatalf("share: %q, %v", q, err)
	}
	q, err = lockSQL(sqlstore.Anchor{Table: "queues", ID: "q", Mode: sqlstore.LockNoKeyUpdate}, false)
	if err != nil || q != "SELECT 1 FROM queues WHERE id = $1 FOR NO KEY UPDATE" {
		t.Fatalf("no key update: %q, %v", q, err)
	}
	q, err = lockSQL(sqlstore.Anchor{Table: "jobs", ID: "j", Mode: sqlstore.LockUpdate}, true)
	if err != nil || q != "SELECT 1 FROM jobs WHERE id = ANY($1) ORDER BY id FOR UPDATE" {
		t.Fatalf("many: %q, %v", q, err)
	}
	for _, many := range []bool{false, true} {
		if _, err := lockSQL(sqlstore.Anchor{Table: "tasks; DROP TABLE jobs", ID: "x"}, many); err == nil {
			t.Fatal("lockSQL accepted a table outside the allow-list")
		}
	}
}

func TestBindAndCase(t *testing.T) {
	d := pgDialect{}
	if v := d.BindBool(true); v != true {
		t.Errorf("BindBool(true) = %#v", v)
	}
	if got := d.CIEqual("username"); got != "lower(username) = lower(?)" {
		t.Errorf("CIEqual = %q", got)
	}
	if got := d.CILike("name"); got != "name ILIKE ? ESCAPE ''" {
		t.Errorf("CILike = %q", got)
	}
}
