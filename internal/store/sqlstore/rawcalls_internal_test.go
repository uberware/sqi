// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlstore

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// rawCall matches a database/sql statement call made outside the wrappers,
// which would skip Rebind, argument binding and error classification. The
// receivers are the raw handles the wrappers hold: a pool's db, dbConn's q, a
// transaction's tx and a prepared statement's st.
var rawCall = regexp.MustCompile(`\b(s\.db|s\.rdb|db|rdb|q|tx\.tx|st\.st)\.(Exec|Query|QueryRow|Prepare)(Context)?\(`)

// rawCallAllowed lists the files that may call database/sql directly: the
// wrappers themselves (dialect.go) and the statement preparation (store.go).
// The SQLite-only operations (PRAGMA, VACUUM INTO) live in internal/store/sqlite,
// which this test does not scan.
var rawCallAllowed = map[string]bool{"dialect.go": true, "store.go": true}

func TestNoRawStatementCalls(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || rawCallAllowed[f] {
			continue
		}
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(body), "\n") {
			if rawCall.MatchString(line) {
				t.Errorf("%s:%d calls database/sql directly; use the dbPool/dbTx/dbStmt wrappers: %s", f, i+1, strings.TrimSpace(line))
			}
		}
	}
}
