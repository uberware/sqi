// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestDispatchHasOneGatedPath is the completeness guard behind exprCapsBlock:
// the EXPR cap gate lives in leaseGatesPass, which
// protects exactly one path to a worker — tryLeaseTask -> buildAssignPayload.
// A second dispatch path added later would hand out EXPR assignments without
// ever consulting a worker's advertised caps, and every test in exprcaps_test.go
// would stay green because they all drive the existing path.
//
// It asserts two things and refuses to pass vacuously (an empty result fails):
//
//  1. buildAssignPayload, the only production function that marshals a
//     protocol.AssignMsg, is called from exactly one production function; and
//  2. that function is tryLeaseTask, which calls leaseGatesPass.
//
// If a new dispatch path is genuinely wanted, the fix is to gate it too and
// then extend this test — not to delete it.
func TestDispatchHasOneGatedPath(t *testing.T) {
	callers := productionCallersOf(t, "buildAssignPayload")
	if len(callers) == 0 {
		t.Fatal("found no production caller of buildAssignPayload: this guard has stopped " +
			"guarding anything (renamed function? changed package layout?)")
	}
	if len(callers) != 1 || callers[0] != "tryLeaseTask" {
		t.Fatalf("buildAssignPayload is called from %v; want exactly [tryLeaseTask].\n"+
			"Every path that hands an assignment to a worker must first pass the EXPR "+
			"capability gate in leaseGatesPass (exprcaps.go), or a worker whose advertised "+
			"EXPR caps are below this server's will be given work it cannot run — the "+
			"accepted-then-failed-per-task incident design spec §2 records.", callers)
	}

	gates := productionCallersOf(t, "leaseGatesPass")
	if len(gates) != 1 || gates[0] != "tryLeaseTask" {
		t.Fatalf("leaseGatesPass is called from %v; want exactly [tryLeaseTask] — the same "+
			"function that builds the payload, so the gate cannot be bypassed", gates)
	}

	// The operator-visible half: the sweep must consult the same gate, or a
	// task no worker may be given sits ready with nothing said about it.
	if blocked := productionCallersOf(t, "exprCapsBlock"); len(blocked) != 2 ||
		!slices.Contains(blocked, "leaseGatesPass") || !slices.Contains(blocked, "evaluateSchedulability") {
		t.Fatalf("exprCapsBlock is called from %v; want exactly [leaseGatesPass "+
			"evaluateSchedulability]: the first is the bound, the second is what the "+
			"operator sees", blocked)
	}

	// exprCapShortfall is the single implementation of the four comparisons.
	// Post-review the lease path hoists it out of the candidate loop, so the
	// two gate call sites reach it by different routes -- selectLeaseBatch
	// computes it once per batch and passes it down, evaluateSchedulability
	// computes it per worker. warnOnExprCapShortfall is the third caller: it
	// computes a shortfall for its diagnostic warning only, not as a bound.
	// All three must still go through THIS accessor.
	if short := productionCallersOf(t, "workerExprShortfall"); len(short) != 3 ||
		!slices.Contains(short, "selectLeaseBatch") || !slices.Contains(short, "evaluateSchedulability") ||
		!slices.Contains(short, "warnOnExprCapShortfall") {
		t.Fatalf("workerExprShortfall is called from %v; want exactly [selectLeaseBatch "+
			"evaluateSchedulability warnOnExprCapShortfall] -- a call site that computes its own "+
			"comparison instead is a second implementation waiting to drift", short)
	}
	if raw := productionCallersOf(t, "exprCapShortfall"); len(raw) != 1 ||
		!slices.Contains(raw, "workerExprShortfall") {
		t.Fatalf("exprCapShortfall is called from %v; want exactly [workerExprShortfall] -- "+
			"every caller reaches the comparison through that accessor", raw)
	}
}

// productionCallersOf returns the names of the non-test functions in this
// package that call name, sorted by first appearance and de-duplicated.
func productionCallersOf(t *testing.T, name string) []string {
	t.Helper()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	var callers []string
	seen := map[string]bool{}
	for _, e := range entries {
		// The "._" prefix skip is for macOS AppleDouble sidecar files, which
		// live beside the sources on some volumes and are not Go at all.
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") ||
			strings.HasSuffix(e.Name(), "_test.go") || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(".", e.Name()), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", e.Name(), err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if calleeName(call.Fun) == name && !seen[fn.Name.Name] {
					seen[fn.Name.Name] = true
					callers = append(callers, fn.Name.Name)
				}
				return true
			})
		}
	}
	return callers
}

// calleeName returns the function name a call expression targets, for both
// plain calls (f()) and method/selector calls (s.f()).
func calleeName(fun ast.Expr) string {
	switch f := fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		return f.Sel.Name
	}
	return ""
}
