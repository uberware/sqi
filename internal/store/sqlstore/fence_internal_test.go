// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlstore

import (
	"go/ast"
	"go/constant"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// unfencedWriteCall matches a statement run on the write pool directly, or a
// prepared statement executed directly, outside [Store.write]: a write that a
// leadership context would not fence.
var unfencedWriteCall = regexp.MustCompile(`s\.w\.(Exec|Query|QueryRow)Context\(|s\.stmt\w+\.ExecContext\(`)

// stmtCall matches a prepared statement run directly, capturing its field.
var stmtCall = regexp.MustCompile(`s\.(stmt\w+)\.(Exec|Query|QueryRow)Context\(`)

// unfencedWriteAllowed lists the files that may run the write pool directly:
// the fence itself, the wrappers and the statement preparation.
var unfencedWriteAllowed = map[string]bool{"fence.go": true, "dialect.go": true, "store.go": true}

// writePoolReads lists, by file and exact line, the reads that use the write
// pool to see the store's own committed writes. A read needs no fence: what
// a deposed leader does with it is a write, and that write is fenced.
var writePoolReads = map[string]string{
	"task.go": "rows, err := s.w.QueryContext(ctx, sqlStaleAssignedCandidates, cutoffText)",
}

// TestNoUnfencedWriteCalls keeps every write outside a transaction going
// through [Store.write], so a leadership context fences it: no statement on
// the write pool and no write statement prepared on it is run directly.
func TestNoUnfencedWriteCalls(t *testing.T) {
	writeStmts := preparedWriteStatements(t)
	if len(writeStmts) == 0 {
		t.Fatal("found no prepared write statements in prepareAll; the scan below would prove nothing")
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		// The go command ignores files whose names start with "." or "_",
		// such as the ._ sidecars macOS writes on some volumes.
		if strings.HasSuffix(f, "_test.go") || strings.HasPrefix(f, ".") || strings.HasPrefix(f, "_") || unfencedWriteAllowed[f] {
			continue
		}
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(body), "\n") {
			trimmed := strings.TrimSpace(line)
			if unfencedWriteCall.MatchString(line) && writePoolReads[f] != trimmed {
				t.Errorf("%s:%d writes outside Store.write, so a leadership context does not fence it: %s", f, i+1, trimmed)
				continue
			}
			for _, m := range stmtCall.FindAllStringSubmatch(line, -1) {
				if writeStmts[m[1]] {
					t.Errorf("%s:%d runs write statement %s outside Store.write, so a leadership context does not fence it: %s",
						f, i+1, m[1], trimmed)
				}
			}
		}
	}
}

// preparedWriteStatements returns the Store fields prepareAll fills with a
// statement that is not a plain SELECT (an INSERT, UPDATE or DELETE, with or
// without RETURNING), read from the source: each `s.stmtX, err =
// s.prepare(ctx, sqlY ...)` whose leading SQL constant isReadStatement
// rejects.
func preparedWriteStatements(t *testing.T) map[string]bool {
	t.Helper()
	_, files, info := loadPackage(t)
	out := map[string]bool{}
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			as, ok := n.(*ast.AssignStmt)
			if !ok || len(as.Lhs) != 2 || len(as.Rhs) != 1 {
				return true
			}
			sel, ok := as.Lhs[0].(*ast.SelectorExpr)
			if !ok || !strings.HasPrefix(sel.Sel.Name, "stmt") {
				return true
			}
			call, ok := as.Rhs[0].(*ast.CallExpr)
			if !ok || len(call.Args) != 2 {
				return true
			}
			if fn, ok := call.Fun.(*ast.SelectorExpr); !ok || fn.Sel.Name != "prepare" {
				return true
			}
			query := concatOperands(call.Args[1])[0]
			tv := info.Types[query]
			if tv.Value == nil || tv.Value.Kind() != constant.String {
				t.Errorf("%s is prepared from %T, not a leading SQL constant; the guard cannot classify it", sel.Sel.Name, query)
				return false
			}
			if !isReadStatement(constant.StringVal(tv.Value)) {
				out[sel.Sel.Name] = true
			}
			return false
		})
	}
	return out
}

// TestPreparedWriteStatements_FindsTheKnownWrites pins the guard's reading of
// prepareAll against a few statements whose kind is known, both ways.
func TestPreparedWriteStatements_FindsTheKnownWrites(t *testing.T) {
	got := preparedWriteStatements(t)
	for _, w := range []string{
		"stmtInsertFarm", "stmtDeleteFarm", "stmtSetTaskUnschedulableReason", "stmtSetBrokerIdentity",
		"stmtDeleteExpiredSessions", "stmtUpdateWorkerHeartbeat", "stmtDeleteOfflineWorkers", "stmtUpdateProduct",
	} {
		if !got[w] {
			t.Errorf("%s is not classified as a write", w)
		}
	}
	for _, r := range []string{"stmtGetFarm", "stmtListFarms", "stmtGetBrokerIdentity", "stmtGetProductByName"} {
		if got[r] {
			t.Errorf("%s is classified as a write", r)
		}
	}
}
