// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// TestSessionEnded_OnlyOnAPositiveSignal pins what counts as proof that a
// server-lock session is gone, so its lock went with it. Anything else, a
// timeout above all (a dead network path, where the session may still be
// alive and holding the lock), must not count.
func TestSessionEnded_OnlyOnAPositiveSignal(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"connection already closed", sql.ErrConnDone, true},
		{"bad connection", driver.ErrBadConn, true},
		{"server closed the connection", io.EOF, true},
		{"connection cut mid-reply", fmt.Errorf("read: %w", io.ErrUnexpectedEOF), true},
		{"admin shutdown (terminated backend)", &pgconn.PgError{Code: "57P01"}, true},
		{"crash shutdown", &pgconn.PgError{Code: "57P02"}, true},
		{"cannot connect now", &pgconn.PgError{Code: "57P03"}, true},
		{"wrapped admin shutdown", fmt.Errorf("unlock: %w", &pgconn.PgError{Code: "57P01"}), true},
		{"another server error", &pgconn.PgError{Code: "42883"}, false},
		{"timeout", context.DeadlineExceeded, false},
		{"wrapped timeout", fmt.Errorf("timeout: %w", context.DeadlineExceeded), false},
		{"timeout beside a closed connection", errors.Join(context.DeadlineExceeded, sql.ErrConnDone), false},
		{"canceled", context.Canceled, false},
		{"anything else", errors.New("boom"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sessionEnded(tt.err); got != tt.want {
				t.Fatalf("sessionEnded(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}
