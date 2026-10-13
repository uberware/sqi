// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlstore

import (
	"go/ast"
	"go/constant"
	"go/importer"
	"go/parser"
	"go/printer"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// loadPackage parses and type-checks this package's non-test files, so the
// guards below can tell a constant from a variable.
func loadPackage(t *testing.T) (*token.FileSet, []*ast.File, *types.Info) {
	t.Helper()
	fset := token.NewFileSet()
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	for _, n := range names {
		if strings.HasSuffix(n, "_test.go") {
			continue
		}
		src, err := os.ReadFile(n)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, n, src, parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	info := &types.Info{
		Types: map[ast.Expr]types.TypeAndValue{},
		Uses:  map[*ast.Ident]types.Object{},
		Defs:  map[*ast.Ident]types.Object{},
	}
	conf := types.Config{Importer: importer.ForCompiler(fset, "source", nil)}
	if _, err := conf.Check("github.com/uberware/sqi/internal/store/sqlstore", fset, files, info); err != nil {
		t.Fatalf("type-check sqlstore: %v", err)
	}
	return fset, files, info
}

// misplacedQuestion reports whether sql has a "?" inside a single-quoted
// literal or inside a "--" or "/* */" comment. Rebind rewrites every "?", so a
// placeholder-looking character in either place would be corrupted on
// PostgreSQL.
//
// It is a scanner rather than a regular expression because a pattern such as
// `'[^']*\?[^']*'` would also match from the closing quote of one literal to
// the opening quote of the next, flagging a placeholder between two literals.
// A doubled quote inside a literal is an escaped quote and keeps the scanner
// in the literal.
func misplacedQuestion(sql string) bool {
	const (
		code = iota
		literal
		lineComment
		blockComment
	)
	state := code
	for i := 0; i < len(sql); i++ {
		c := sql[i]
		switch state {
		case code:
			switch {
			case c == '\'':
				state = literal
			case c == '-' && i+1 < len(sql) && sql[i+1] == '-':
				state, i = lineComment, i+1
			case c == '/' && i+1 < len(sql) && sql[i+1] == '*':
				state, i = blockComment, i+1
			}
		case literal:
			switch {
			case c == '?':
				return true
			case c == '\'' && i+1 < len(sql) && sql[i+1] == '\'':
				i++ // an escaped quote
			case c == '\'':
				state = code
			}
		case lineComment:
			switch c {
			case '?':
				return true
			case '\n':
				state = code
			}
		case blockComment:
			switch {
			case c == '?':
				return true
			case c == '*' && i+1 < len(sql) && sql[i+1] == '/':
				state, i = code, i+1
			}
		}
	}
	return false
}

// TestMisplacedQuestion pins the scanner itself, so the constant check below
// cannot pass because the scanner finds nothing.
func TestMisplacedQuestion(t *testing.T) {
	for _, tc := range []struct {
		name string
		sql  string
		want bool
	}{
		{"placeholders only", `UPDATE t SET a = ? WHERE id = ?`, false},
		{"placeholder between two literals", `SET s = 'a', x = ?, y = 'b' WHERE id = ?`, false},
		{"literal then placeholder", `WHERE status = 'ready' AND id = ?`, false},
		{"escaped quote then placeholder", `WHERE s = 'it''s' AND id = ?`, false},
		{"question mark in literal", `WHERE s = 'why?'`, true},
		{"question mark in literal after placeholder", `WHERE id = ? AND s = 'a?b'`, true},
		{"question mark after escaped quote", `WHERE s = 'it''s?'`, true},
		{"question mark in line comment", "SELECT 1 -- really?\nFROM t", true},
		{"placeholder after a line comment", "SELECT 1 -- note\nFROM t WHERE id = ?", false},
		{"question mark in block comment", `SELECT /* why? */ 1`, true},
		{"placeholder after a block comment", `SELECT /* note */ 1 WHERE id = ?`, false},
		{"single dash is not a comment", `SELECT a - ? FROM t`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := misplacedQuestion(tc.sql); got != tc.want {
				t.Errorf("misplacedQuestion(%q) = %v, want %v", tc.sql, got, tc.want)
			}
		})
	}
}

// TestSQLConstants_NoQuestionMarkInLiteralsOrComments checks every string
// constant of the package.
func TestSQLConstants_NoQuestionMarkInLiteralsOrComments(t *testing.T) {
	_, files, info := loadPackage(t)
	for _, f := range files {
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for _, name := range vs.Names {
					c, ok := info.Defs[name].(*types.Const)
					if !ok || c.Val().Kind() != constant.String {
						continue
					}
					if misplacedQuestion(constant.StringVal(c.Val())) {
						t.Errorf("%s has a ? inside a quoted literal or a comment; Rebind would rewrite it", name.Name)
					}
				}
			}
		}
	}
}

// concatAllowed names the non-constant operands a SQL string may be built
// from. Each was audited when it was added: it comes from an allow-list, a
// dialect method, or a builder that only emits "?" placeholders, never from
// input. Add to it only with the same audit.
//
// An entry matches by source text anywhere in the package, not at one site, so
// a new variable that reuses a name here is accepted without a new audit.
// Read the comment on an entry as the contract any variable of that name must
// keep.
var concatAllowed = []string{
	"col",                            // scan.go orderByClause: a *SortColumns allow-list value or a literal fallback, from the four List* callers
	"dir",                            // scan.go orderByClause: "ASC" or "DESC" from sortDirKeyword
	"nulls",                          // scan.go orderByClause: "ASC" or "DESC", picked from a literal inside the function
	"orderByClause(col, dir, false)", // job.go, queue.go, task.go: col and dir as above, the flag a literal
	"orderByClause(col, dir, opts.SortBy == store.WorkerSortByLastHeartbeatAt)", // worker.go: the flag is a bool
	"where",                   // List{Jobs,Queues,Tasks,Workers}: literal fragments, CIEqual of a literal, a "?" IN list, and searchClause output
	`s.d.CIEqual("owner")`,    // job.go: Dialect method, literal column
	`s.d.CIEqual("username")`, // store.go prepareAll: Dialect method, literal column
	`s.d.CIEqual("name")`,     // store.go prepareAll (products): Dialect method, literal column
	"qmarks(len(deps))",       // finalize.go: scan.go qmarks, only "?" and ","
	"placeholders",            // job.go expiredJobsQuery, task.go RetryTasks and reclaimStaleBatch, attemptclose.go closeTasksAttemptsTx: scan.go qmarks output, only ? marks and ","
	"string(placeholders)",    // task.go ListTasks: a byte slice that only receives ',' and '?'
	"inSuffix",                // task.go RetryTasks: "" or a literal around a "?" list
	"query",                   // job.go DeleteTerminalJobsBefore: expiredJobsQuery output, two literals around a "?" list
	"frag",                    // job.go ListJobs, worker.go ListWorkers: searchClause (search.go) output, literals and Dialect.CILike over literal column lists (each renders as the column, a "?" and literals only: `col LIKE ?` on SQLite, `col ILIKE ? ESCAPE ''` on PostgreSQL), "?" for each term, which is bound

	// job.go runInsertTasks: taskRowsValues is one constant "(?, …)" row per
	// task joined by ",", only "?" marks, parentheses, commas and spaces; its
	// argument is an int, so the length of a chunk of tasks is all that varies.
	"taskRowsValues(len(chunk))",

	// job.go lockJobLoggedTasksTx: Dialect.ForUpdate, a constant clause per
	// dialect (" FOR UPDATE" on PostgreSQL; SQLite's "" is never appended).
	"forUpdate",
}

// sqlKeyword marks a string literal as SQL.
var sqlKeyword = regexp.MustCompile(`(?i)\b(SELECT|UPDATE|DELETE|INSERT|WHERE|ORDER BY|AND|SET|FROM|LIMIT)\b`)

// TestSQL_ConcatenatesOnlyAuditedPieces fails on a "+" or "+=" that joins SQL
// text to anything other than a constant or an audited operand. gosec cannot
// see SQL built through the dialect wrappers, so this guard stands in for it.
//
// It covers the two concatenation operators only. A string assembled with
// strings.Builder, fmt.Sprintf or strings.Join is not seen; the one Builder
// in the package (searchClause) is audited by hand, and a new one needs the
// same audit.
//
// "+" is checked when an operand is a SQL literal. "+=" is checked whenever a
// string variable is appended to with a non-constant, because the right-hand
// side of "where += frag" holds no SQL text to recognize: the variable it
// extends is what makes it SQL.
func TestSQL_ConcatenatesOnlyAuditedPieces(t *testing.T) {
	fset, files, info := loadPackage(t)
	audit := func(operands []ast.Expr) {
		for _, op := range operands {
			if tv := info.Types[op]; tv.Value != nil {
				continue // a constant
			}
			src := exprString(fset, op)
			if !slices.Contains(concatAllowed, src) {
				t.Errorf("%s: SQL built from unaudited operand %q; add it to concatAllowed only after checking it cannot carry input",
					fset.Position(op.Pos()), src)
			}
		}
	}
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.BinaryExpr:
				if n.Op != token.ADD || !isSQLConcat(n, info) {
					return true
				}
				audit(concatOperands(n))
				return false
			case *ast.AssignStmt:
				if n.Tok != token.ADD_ASSIGN || len(n.Lhs) != 1 || len(n.Rhs) != 1 || !isStringType(info.TypeOf(n.Lhs[0])) {
					return true
				}
				audit(concatOperands(n.Rhs[0]))
				return false
			}
			return true
		})
	}
}

// isStringType reports whether t is string or a named type over it.
func isStringType(t types.Type) bool {
	if t == nil {
		return false
	}
	b, ok := t.Underlying().(*types.Basic)
	return ok && b.Info()&types.IsString != 0
}

// isSQLConcat reports whether be is a string concatenation with a SQL literal
// or SQL constant among its operands.
func isSQLConcat(be *ast.BinaryExpr, info *types.Info) bool {
	for _, op := range concatOperands(be) {
		tv := info.Types[op]
		if tv.Value != nil && tv.Value.Kind() == constant.String && sqlKeyword.MatchString(constant.StringVal(tv.Value)) {
			return true
		}
	}
	return false
}

// concatOperands flattens a chain of "+" into its operands.
func concatOperands(e ast.Expr) []ast.Expr {
	if be, ok := e.(*ast.BinaryExpr); ok && be.Op == token.ADD {
		return append(concatOperands(be.X), concatOperands(be.Y)...)
	}
	if p, ok := e.(*ast.ParenExpr); ok {
		return concatOperands(p.X)
	}
	return []ast.Expr{e}
}

func exprString(fset *token.FileSet, e ast.Expr) string {
	var b strings.Builder
	if err := printer.Fprint(&b, fset, e); err != nil {
		return "?"
	}
	return b.String()
}
