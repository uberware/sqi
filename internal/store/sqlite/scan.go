// SPDX-License-Identifier: AGPL-3.0-only

package sqlite

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/uberware/sqi/internal/store"
)

// scanner is satisfied by both *sql.Row and *sql.Rows so scan helpers can
// accept either without duplicating code.
type scanner interface {
	Scan(dest ...any) error
}

// ── Time helpers ─────────────────────────────────────────────────────────────

const timeLayout = time.RFC3339Nano

// timeToText formats t as an ISO 8601 string for storage in SQLite TEXT columns.
func timeToText(t time.Time) string {
	return t.UTC().Format(timeLayout)
}

// textToTime parses an ISO 8601 string read from a SQLite TEXT column.
func textToTime(s string) (time.Time, error) {
	t, err := time.Parse(timeLayout, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse time %q: %w", s, err)
	}
	return t, nil
}

// mustTime is like textToTime but panics on error. Used only where the value
// was written by this package and a parse failure indicates a bug.
func mustTime(s string) time.Time {
	t, err := textToTime(s)
	if err != nil {
		panic(err)
	}
	return t
}

// nullTimeToText converts a *time.Time to a sql.NullString for nullable
// timestamp columns.
func nullTimeToText(t *time.Time) sql.NullString {
	if t == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: timeToText(*t), Valid: true}
}

// nullTextToTime converts a sql.NullString read from a nullable timestamp
// column back to *time.Time.
func nullTextToTime(s sql.NullString) *time.Time {
	if !s.Valid || s.String == "" {
		return nil
	}
	t, err := textToTime(s.String)
	if err != nil {
		return nil
	}
	return &t
}

// ── JSON helpers ─────────────────────────────────────────────────────────────

// marshalJSON serializes v to a JSON string for storage in SQLite TEXT columns.
func marshalJSON(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("marshal json: %w", err)
	}
	return string(b), nil
}

// unmarshalJSON deserializes a JSON string from a SQLite TEXT column into a
// value of type T. Returns def if s is empty or "null".
func unmarshalJSON[T any](s string, def T) (T, error) {
	if s == "" || s == "null" {
		return def, nil
	}
	var v T
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return def, fmt.Errorf("unmarshal json: %w", err)
	}
	return v, nil
}

// ── Sort helpers ──────────────────────────────────────────────────────────────

// sortDirKeyword returns the SQL keyword ("ASC" or "DESC") for the given
// [store.SortDir]. Unrecognized values default to "ASC" to prevent injection.
func sortDirKeyword(d store.SortDir) string {
	if d == store.SortDesc {
		return "DESC"
	}
	return "ASC"
}

// ── Bool helpers ──────────────────────────────────────────────────────────────

// boolToInt converts a Go bool to 0/1 for SQLite INTEGER columns.
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ── Null helpers ──────────────────────────────────────────────────────────────

// nullString wraps s as a sql.NullString; empty string → invalid (NULL).
func nullString(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}

// ── Error mapping ─────────────────────────────────────────────────────────────

// mapErr translates driver-level errors to the sentinel errors defined in
// [store]. It covers:
//   - [sql.ErrNoRows] → [store.ErrNotFound]
//   - UNIQUE constraint violations → [store.ErrConflict]
//
// UNIQUE constraint detection uses the error message string because
// modernc.org/sqlite surfaces this reliably and consistently. If the driver
// changes, revisit this check.
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return store.ErrNotFound
	}
	if strings.Contains(err.Error(), "UNIQUE constraint failed") {
		return fmt.Errorf("%w: %s", store.ErrConflict, err.Error())
	}
	return err
}

// checkRowsAffected returns [store.ErrNotFound] when a DELETE or UPDATE that
// should have modified exactly one row affected zero rows instead.
func checkRowsAffected(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}
