// SPDX-License-Identifier: AGPL-3.0-or-later

package fmtres_test

// The guard that turns "a production path must never resolve phase-3
// expressions on the built-in default limits" into a test failure.
//
// The budget parameter is required, so a caller such as
// executor.resolveAssignmentExpr cannot omit it; omitted, it would meter
// every evaluation (including each PATH parameter's apply_path_mapping)
// against the compiled-in defaults on a host configured otherwise. This test
// catches the remaining spelling, a literal `nil`.
//
// It parses the worker's two phase-3 consumer packages rather than grepping,
// so a call split across lines, renamed through an import alias, or nested
// inside another expression is still found.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// parseProductionFiles parses every non-test Go file directly in dir.
//
// It walks the directory itself rather than calling parser.ParseDir (which Go
// 1.25 deprecated) and skips two kinds of file: _test.go, because tests may
// pass nil, and AppleDouble sidecars ("._foo.go"), which macOS creates on
// non-native volumes and which are not Go source: go/parser rejects them with
// "illegal character NUL", which would stop the guard from running.
// test/conformance's isFixtureFile has the same exclusion for the same
// reason.
func parseProductionFiles(t *testing.T, fset *token.FileSet, dir string) []*ast.File {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var files []*ast.File
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") ||
			strings.HasSuffix(name, "_test.go") || strings.HasPrefix(name, "._") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", filepath.Join(dir, name), err)
		}
		files = append(files, f)
	}
	if len(files) == 0 {
		t.Fatalf("no production Go files found under %s -- the guard would pass vacuously", dir)
	}
	return files
}

// phase3EntryPoints are the exported functions whose last parameter is the
// assignment's *AssignmentBudget. An entry point missing from this list is not
// covered by the guard, so TestPhase3EntryPoints_GuardListIsComplete below
// cross-checks it against the package's source.
var phase3EntryPoints = map[string]bool{
	"TaskSymbols":              true,
	"EnvSymbols":               true,
	"ApplyTaskLet":             true,
	"ApplyEnvLet":              true,
	"ResolveActionExpr":        true,
	"ResolveEmbeddedFilesExpr": true,
	"ResolveVarsExpr":          true,
}

// TestPhase3EntryPoints_ProductionCallersAlwaysPassABudget fails when any
// non-test file in internal/worker/session or internal/worker/executor calls a
// phase-3 entry point with a literal nil budget.
//
// nil is allowed in this package's tests (it means "the defaults, which this
// test is not about"). It is never allowed in production: the operator's
// configured limits reach phase 3 only through the budget, so a nil there
// meters against numbers the operator did not choose, and nothing fails at
// runtime.
func TestPhase3EntryPoints_ProductionCallersAlwaysPassABudget(t *testing.T) {
	for _, dir := range []string{
		filepath.Join("..", "session"),
		filepath.Join("..", "executor"),
	} {
		fset := token.NewFileSet()
		for _, file := range parseProductionFiles(t, fset, dir) {
			checkFileForNilBudget(t, fset, file)
		}
	}
}

// checkFileForNilBudget reports every call to a phase-3 entry point whose final
// argument is the identifier nil.
func checkFileForNilBudget(t *testing.T, fset *token.FileSet, file *ast.File) {
	t.Helper()
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || !phase3EntryPoints[sel.Sel.Name] {
			return true
		}
		if len(call.Args) == 0 {
			return true
		}
		last, ok := call.Args[len(call.Args)-1].(*ast.Ident)
		if !ok || last.Name != "nil" {
			return true
		}
		t.Errorf("%s: %s is called with a nil budget.\n"+
			"A production caller must pass the assignment's own *fmtres.AssignmentBudget "+
			"(session.Session.ExprBudget(), or a fresh NewAssignmentBudget(<that budget>.Limits()) "+
			"where a separate ledger is intended). A nil budget silently meters this evaluation "+
			"against the BUILT-IN DEFAULTS, ignoring the operator's expr: configuration, and "+
			"nothing fails at runtime to say so.",
			fset.Position(call.Lparen), sel.Sel.Name)
		return true
	})
}

// TestPhase3EntryPoints_GuardListIsComplete cross-checks phase3EntryPoints
// against this package's exported functions: any exported function whose last
// parameter is a *AssignmentBudget must be in the list, or the guard above
// does not cover it.
func TestPhase3EntryPoints_GuardListIsComplete(t *testing.T) {
	fset := token.NewFileSet()
	found := map[string]bool{}
	for _, file := range parseProductionFiles(t, fset, ".") {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || !fn.Name.IsExported() || fn.Recv != nil {
				continue
			}
			params := fn.Type.Params.List
			if len(params) == 0 {
				continue
			}
			star, ok := params[len(params)-1].Type.(*ast.StarExpr)
			if !ok {
				continue
			}
			if id, ok := star.X.(*ast.Ident); ok && id.Name == "AssignmentBudget" {
				found[fn.Name.Name] = true
			}
		}
	}

	for name := range found {
		if !phase3EntryPoints[name] {
			t.Errorf("%s takes a trailing *AssignmentBudget but is missing from phase3EntryPoints; "+
				"add it, or production callers may pass it nil unnoticed", name)
		}
	}
	for name := range phase3EntryPoints {
		if !found[name] {
			t.Errorf("phase3EntryPoints lists %s, which no longer takes a trailing *AssignmentBudget; "+
				"the guard is checking something that does not exist", name)
		}
	}
}
