// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// rawCall matches a database/sql statement call made outside the wrappers,
// which would skip Rebind, argument binding and error classification.
var rawCall = regexp.MustCompile(`\b(s\.db|s\.rdb|db|rdb|tx\.tx|p\.db|st\.st)\.(Exec|Query|QueryRow|Prepare)Context\(`)

// rawCallAllowed lists the files that may call database/sql directly: the
// wrappers themselves and the SQLite-only operations, whose SQL (PRAGMA,
// VACUUM INTO) is not shared.
var rawCallAllowed = map[string]bool{
	"dialect.go": true, "store.go": true, "checkpoint.go": true, "backup.go": true,
}

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
