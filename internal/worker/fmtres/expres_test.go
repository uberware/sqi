// SPDX-License-Identifier: AGPL-3.0-or-later

package fmtres_test

// Tests for expres.go: rendering an expression's value into a real command
// line.
//
// Each test builds its symbol table with fmtres.TaskSymbols, so these
// exercise the phase-3 table an assignment produces rather than a hand-built
// expr.MapSymbols, except where a test covers a construct that needs no
// symbol (a literal list, section 1.3.2's TargetArgItem flattening).

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/openjd"
	"github.com/uberware/sqi/internal/openjd/expr"
	"github.com/uberware/sqi/internal/worker/fmtres"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// wantPathText builds an expected path rendering the way production code
// does (expr.Eval/expr.Path/expr.WithPathFormat/expr.WithPathMapping under an
// explicit expr.PathNative) rather than with Go's os-specific path/filepath
// package (filepath.Join, ToSlash).
//
// A hardcoded POSIX-separator expectation (a literal string, or
// filepath.ToSlash(filepath.Join(...))) would pass only because
// expr.PathNative resolves to expr.PathPOSIX on a POSIX host. Building the
// expectation through the same machinery keeps the two in agreement on POSIX
// and Windows hosts, since both resolve expr.PathNative via the same
// runtime.GOOS-keyed rule (PathFormat.resolve).
//
// syms supplies whatever symbols src references; opts is forwarded after
// the forced expr.WithPathFormat(expr.PathNative), so a caller wanting
// apply_path_mapping's real rules passes expr.WithPathMapping(...) here.
func wantPathText(t *testing.T, src string, syms expr.MapSymbols, opts ...expr.Option) string {
	t.Helper()
	allOpts := append([]expr.Option{expr.WithPathFormat(expr.PathNative)}, opts...)
	v, err := expr.Eval(src, syms, expr.TString, allOpts...)
	if err != nil {
		t.Fatalf("wantPathText(%q): %v", src, err)
	}
	return v.AsStr()
}

// ── Lone reference inherits the field's target type ─────────────────────────

// TestResolveActionExpr_LoneReferenceInheritsTargetType pins section 1.3.2's
// core rule: a format string that is exactly one reference evaluates against
// the field's target type (TargetString for Command), so a path-typed value
// coerces to text via the field's target. Task.Param.Out is bound as a PATH
// value (expr.TPath) by TaskSymbols; Command's target is expr.TString, and
// path -> string is one of section 1.2.3's compatible pairs.
func TestResolveActionExpr_LoneReferenceInheritsTargetType(t *testing.T) {
	msg := &protocol.AssignMsg{
		Parameters:     map[string]string{"Out": "/renders/out.exr"},
		ParameterTypes: map[string]string{"Out": "PATH"},
	}
	syms, err := fmtres.TaskSymbols(msg, "/work", "", false, nil)
	if err != nil {
		t.Fatalf("TaskSymbols: %v", err)
	}

	action := &protocol.Action{Command: "{{ Task.Param.Out }}"}
	out, err := fmtres.ResolveActionExpr(action, syms, nil, nil)
	if err != nil {
		t.Fatalf("ResolveActionExpr: %v", err)
	}
	want := wantPathText(t, "OUT", expr.MapSymbols{"OUT": expr.Path("/renders/out.exr", expr.PathNative)})
	if out.Command != want {
		t.Errorf("Command = %q, want %q", out.Command, want)
	}
}

// TestResolveActionExpr_LoneArgListFlattensAndNullDrops covers the half of
// section 1.3.2's target-inheritance rule that Command's TString target
// cannot exercise: an args entry's target is fmtres.TargetArgItem
// ("string? | list[string]"), so a lone reference's result decides how many
// arguments one template entry becomes. The list and the null are literals,
// so no symbol table is needed.
func TestResolveActionExpr_LoneArgListFlattensAndNullDrops(t *testing.T) {
	action := &protocol.Action{
		Command: "render",
		Args:    []string{"{{ ['a', 'b', 'c'] }}", "{{ None }}", "-x"},
	}
	out, err := fmtres.ResolveActionExpr(action, expr.MapSymbols{}, nil, nil)
	if err != nil {
		t.Fatalf("ResolveActionExpr: %v", err)
	}
	want := []string{"a", "b", "c", "-x"}
	if len(out.Args) != len(want) {
		t.Fatalf("Args = %v, want %v", out.Args, want)
	}
	for i, w := range want {
		if out.Args[i] != w {
			t.Errorf("Args[%d] = %q, want %q", i, out.Args[i], w)
		}
	}
}

// ── Embedded reference stringified ──────────────────────────────────────────

// TestResolveActionExpr_EmbeddedReferenceStringified pins section 1.3.2's
// other branch: a reference alongside other text evaluates unconstrained
// (expr.TAny) and converts its natural result to text. It uses RFC 0005's
// worked example ("Items: {{ [1, 2, 3] }}" -> "Items: [1, 2, 3]"), which a
// TString target would reject (a list cannot coerce to a bare string), so the
// embedded path must be a different evaluation from the lone-reference one.
func TestResolveActionExpr_EmbeddedReferenceStringified(t *testing.T) {
	action := &protocol.Action{Command: "Items: {{ [1, 2, 3] }}"}
	out, err := fmtres.ResolveActionExpr(action, expr.MapSymbols{}, nil, nil)
	if err != nil {
		t.Fatalf("ResolveActionExpr: %v", err)
	}
	if out.Command != "Items: [1, 2, 3]" {
		t.Errorf("Command = %q, want %q", out.Command, "Items: [1, 2, 3]")
	}
}

// TestResolveActionExpr_EmbeddedNullRendersEmpty pins the rendering rule
// expres.go's comment describes beyond checkFormatString's type checking:
// RFC 0005 states a null result embedded in a format string is the empty
// string, not Value.String()'s "null" diagnostic rendering.
func TestResolveActionExpr_EmbeddedNullRendersEmpty(t *testing.T) {
	action := &protocol.Action{Command: "before[{{ None }}]after"}
	out, err := fmtres.ResolveActionExpr(action, expr.MapSymbols{}, nil, nil)
	if err != nil {
		t.Fatalf("ResolveActionExpr: %v", err)
	}
	if out.Command != "before[]after" {
		t.Errorf("Command = %q, want %q", out.Command, "before[]after")
	}
}

// ── Task.Param.* ─────────────────────────────────────────────────────────────

// TestResolveActionExpr_TaskParam checks that Task.Param.Frame is bound as an
// int, not a string, as exprsyms_test.go's TestTaskSymbols_JobParamTypes does
// for job parameters: "71" would mean concatenation instead of addition.
func TestResolveActionExpr_TaskParam(t *testing.T) {
	msg := &protocol.AssignMsg{
		Parameters:     map[string]string{"Frame": "7"},
		ParameterTypes: map[string]string{"Frame": "INT"},
	}
	syms, err := fmtres.TaskSymbols(msg, "/work", "", false, nil)
	if err != nil {
		t.Fatalf("TaskSymbols: %v", err)
	}
	action := &protocol.Action{Command: "{{ Task.Param.Frame + 1 }}"}
	out, err := fmtres.ResolveActionExpr(action, syms, nil, nil)
	if err != nil {
		t.Fatalf("ResolveActionExpr: %v", err)
	}
	if out.Command != "8" {
		t.Errorf("Command = %q, want %q (addition, not concatenation)", out.Command, "8")
	}
}

// ── Session.WorkingDirectory ────────────────────────────────────────────────

// TestResolveActionExpr_SessionWorkingDirectory exercises the "/" path-join
// operator over a concrete Session.WorkingDirectory value, the construct a
// StepScript.let binding like "Session.WorkingDirectory / 'frames'" produces
// on the host.
func TestResolveActionExpr_SessionWorkingDirectory(t *testing.T) {
	const workDir = "/work/session1"
	syms, err := fmtres.TaskSymbols(&protocol.AssignMsg{}, workDir, "", false, nil)
	if err != nil {
		t.Fatalf("TaskSymbols: %v", err)
	}
	action := &protocol.Action{Command: "{{ Session.WorkingDirectory / 'frames' }}"}
	out, err := fmtres.ResolveActionExpr(action, syms, nil, nil)
	if err != nil {
		t.Fatalf("ResolveActionExpr: %v", err)
	}
	want := wantPathText(t, "WD / 'frames'", expr.MapSymbols{"WD": expr.Path(workDir, expr.PathNative)})
	if out.Command != want {
		t.Errorf("Command = %q, want %q", out.Command, want)
	}
}

// ── Task.File.* ──────────────────────────────────────────────────────────────

// TestResolveActionExpr_TaskFile renders a materialized embedded-file path
// (AddFileVars' computation, reached through TaskSymbols) into a command.
func TestResolveActionExpr_TaskFile(t *testing.T) {
	const workDir = "/work/session1"
	msg := &protocol.AssignMsg{
		EmbeddedFiles: []protocol.EmbeddedFile{{Name: "Script", Filename: "run.py"}},
	}
	syms, err := fmtres.TaskSymbols(msg, workDir, "", false, nil)
	if err != nil {
		t.Fatalf("TaskSymbols: %v", err)
	}
	action := &protocol.Action{Command: "python", Args: []string{"{{ Task.File.Script }}"}}
	out, err := fmtres.ResolveActionExpr(action, syms, nil, nil)
	if err != nil {
		t.Fatalf("ResolveActionExpr: %v", err)
	}
	// AddFileVars' on-disk-path computation (filepath.Join) is host-OS
	// driven and used as is; what must be built the way the code under test
	// builds it is the re-render of that string as an expr path value under
	// expr.PathNative (bindFileSymbols' expr.Path(v, pathFlavor) call).
	want := wantPathText(t, "F", expr.MapSymbols{"F": expr.Path(filepath.Join(workDir, "run.py"), expr.PathNative)})
	if len(out.Args) != 1 || out.Args[0] != want {
		t.Errorf("Args = %v, want [%q]", out.Args, want)
	}
}

// ── Evaluation error surfaces with context ──────────────────────────────────

// TestResolveActionExpr_ErrorSurfacesWithContext pins that a failing
// expression's error names both the field it failed in (so a template author
// can find the line) and the evaluator's diagnostic (an unknown-symbol
// message with a column position), following fmtres.go's
// ResolveAction/ResolveVars/ResolveEmbeddedFiles convention of one
// field-context layer around the underlying error.
func TestResolveActionExpr_ErrorSurfacesWithContext(t *testing.T) {
	action := &protocol.Action{Command: "{{ Task.Param.DoesNotExist }}"}
	_, err := fmtres.ResolveActionExpr(action, expr.MapSymbols{}, nil, nil)
	if err == nil {
		t.Fatal("ResolveActionExpr: want an error for an unknown symbol, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "command") {
		t.Errorf("error %q does not name the failing field (\"command\")", msg)
	}
	if !strings.Contains(msg, action.Command) {
		t.Errorf("error %q does not quote the offending format string %q", msg, action.Command)
	}
	if !strings.Contains(msg, "unknown symbol") {
		t.Errorf("error %q does not surface expr's own diagnostic (\"unknown symbol\")", msg)
	}
	if !strings.Contains(msg, "Task.Param.DoesNotExist") {
		t.Errorf("error %q does not name the specific unknown symbol", msg)
	}
}

// TestResolveVarsExpr_ErrorNamesTheVariableKey is the same context guarantee
// for ResolveVarsExpr, whose wrapping names a map key rather than a fixed
// field name like "command".
func TestResolveVarsExpr_ErrorNamesTheVariableKey(t *testing.T) {
	vars := map[string]string{"BAD": "{{ 1 / 0 }}"}
	_, err := fmtres.ResolveVarsExpr(vars, expr.MapSymbols{}, nil, nil)
	if err == nil {
		t.Fatal("ResolveVarsExpr: want an error for a division by zero, got nil")
	}
	if !strings.Contains(err.Error(), `variable "BAD"`) {
		t.Errorf("error %q does not name the offending variable key", err.Error())
	}
}

// TestResolveActionExpr_UnresolvedFallbackErrorsInsteadOfPanicking:
// exprsyms_test.go's TestTaskSymbols_ChunkIntTaskParam_
// InvalidRangeFallsBackToUnresolved shows that an unparseable CHUNK[INT]
// value stays expr.Unresolved. Reading that symbol through expr.TString's
// coercion (coerceUnresolved) returns another unresolved Value with no error,
// so calling Value.AsStr() on it would panic ("expr: read string payload
// from a unresolved[string] value"). A worker process must not crash
// rendering one task's command line; this pins that errUnresolvedValue is
// returned instead.
func TestResolveActionExpr_UnresolvedFallbackErrorsInsteadOfPanicking(t *testing.T) {
	msg := &protocol.AssignMsg{
		Parameters:     map[string]string{"Frames": "not-a-range"},
		ParameterTypes: map[string]string{"Frames": "CHUNK[INT]"},
	}
	syms, err := fmtres.TaskSymbols(msg, "/work", "", false, nil)
	if err != nil {
		t.Fatalf("TaskSymbols: %v", err)
	}
	sym, ok := syms.Lookup("Task.Param.Frames")
	if !ok || !sym.IsUnresolved() {
		t.Fatalf("test premise broken: Task.Param.Frames = %+v, want an unresolved fallback", sym)
	}

	action := &protocol.Action{Command: "{{ Task.Param.Frames }}"}
	_, err = fmtres.ResolveActionExpr(action, syms, nil, nil)
	if err == nil {
		t.Fatal("ResolveActionExpr: want an error for an unresolved fallback value, got nil (and no panic)")
	}
	if !strings.Contains(err.Error(), "unresolved") {
		t.Errorf("error %q does not explain that the value is unresolved", err.Error())
	}
}

// ── Metered evaluation ───────────────────────────────────────────────────────

// TestResolveActionExpr_OverBudgetRejectedByOperationLimit asserts the limit
// sqi configures (fmtres's ExprLimits.OperationLimit), not only that an error
// occurred: a bare error assertion would also pass under expr.Eval's
// unconfigured 10,000,000-operation default. The comprehension iterates well
// past the default OperationLimit (1,000,000) while its always-false filter
// keeps the result list empty, so the rejection is by the operation limit,
// not the memory limit, as in the server-side
// TestCheckTemplateExpressions_AppliesSubmissionLimits.
func TestResolveActionExpr_OverBudgetRejectedByOperationLimit(t *testing.T) {
	action := &protocol.Action{
		Command: "echo",
		Args:    []string{"{{ len([x for x in range(1100000) if x > 999999999]) }}"},
	}
	_, err := fmtres.ResolveActionExpr(action, expr.MapSymbols{}, nil, nil)
	if err == nil {
		t.Fatal("ResolveActionExpr: want an operation-limit error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "operation limit exceeded") {
		t.Fatalf("error %q is not an operation-limit rejection", msg)
	}
	// The configured limit must appear, distinguished from expr.Eval's
	// unconfigured default (10,000,000): strings.Contains(msg, "1000000") is
	// satisfied by "10000000" too. meter.go's error format ("...exceeds the
	// limit of %d") ends the message with the number, so HasSuffix on the
	// trailing phrase is exact.
	const wantSuffix = "the limit of 1000000"
	if !strings.HasSuffix(msg, wantSuffix) {
		t.Errorf("error %q does not end with %q (the configured OperationLimit); "+
			"this test must pin fmtres's OWN metering, not expr.Eval's default of 10,000,000", msg, wantSuffix)
	}
}

// ── apply_path_mapping: the session's real rules ───────────────────────────

// TestResolveActionExpr_ApplyPathMapping executes apply_path_mapping against
// a session rule end to end. ExprEvalOptions threads the assignment's PathMap
// field (fmtres.ConvertPathMapRules) into the evaluation via
// expr.WithPathMapping.
func TestResolveActionExpr_ApplyPathMapping(t *testing.T) {
	pathMap := []protocol.PathMapRule{
		{
			SourcePathFormat: "POSIX",
			SourcePath:       "/mnt/shared",
			DestinationPath:  "/local/cache",
		},
	}
	// msg carries the same PathMap that is passed to ResolveActionExpr, as
	// in production (both TaskSymbols and the resolver read msg.PathMap).
	// Task.Param.Scene therefore binds already mapped, and this explicit
	// apply_path_mapping() call must not map it twice or corrupt it.
	msg := &protocol.AssignMsg{
		Parameters:     map[string]string{"Scene": "/mnt/shared/project/shot.ma"},
		ParameterTypes: map[string]string{"Scene": "PATH"},
		PathMap:        pathMap,
	}
	syms, err := fmtres.TaskSymbols(msg, "/work", "", false, nil)
	if err != nil {
		t.Fatalf("TaskSymbols: %v", err)
	}

	action := &protocol.Action{Command: "{{ apply_path_mapping(Task.Param.Scene) }}"}
	out, err := fmtres.ResolveActionExpr(action, syms, pathMap, nil)
	if err != nil {
		t.Fatalf("ResolveActionExpr: %v", err)
	}
	// Built through the wrapper the code calls (apply_path_mapping via
	// expr.Eval, explicit expr.PathNative) rather than a hardcoded
	// POSIX-separator literal; see wantPathText's doc comment.
	want := wantPathText(
		t, "apply_path_mapping(src)",
		expr.MapSymbols{"src": expr.String("/mnt/shared/project/shot.ma")},
		expr.WithPathMapping(fmtres.ConvertPathMapRules(pathMap)),
	)
	if out.Command != want {
		t.Errorf("apply_path_mapping(%q) via rule %+v = %q, want %q",
			"/mnt/shared/project/shot.ma", pathMap[0], out.Command, want)
	}
}

// TestResolveActionExpr_ApplyPathMappingNoRulesPassesThrough is the negative
// companion: with no PathMap entries (the common case), apply_path_mapping
// passes its input through, normalized as a path in the evaluation's flavor,
// per expr.WithPathMapping's "no rule rewrote it" contract, reached through
// this package's call path.
func TestResolveActionExpr_ApplyPathMappingNoRulesPassesThrough(t *testing.T) {
	msg := &protocol.AssignMsg{
		Parameters:     map[string]string{"Scene": "/mnt/shared/project/shot.ma"},
		ParameterTypes: map[string]string{"Scene": "PATH"},
	}
	syms, err := fmtres.TaskSymbols(msg, "/work", "", false, nil)
	if err != nil {
		t.Fatalf("TaskSymbols: %v", err)
	}
	action := &protocol.Action{Command: "{{ apply_path_mapping(Task.Param.Scene) }}"}
	out, err := fmtres.ResolveActionExpr(action, syms, nil, nil)
	if err != nil {
		t.Fatalf("ResolveActionExpr: %v", err)
	}
	want := wantPathText(
		t, "apply_path_mapping(src)",
		expr.MapSymbols{"src": expr.String("/mnt/shared/project/shot.ma")},
	)
	if out.Command != want {
		t.Errorf("apply_path_mapping with no rules = %q, want passthrough %q", out.Command, want)
	}
}

// ── Path flavor: expr.PathNative is threaded, not left to expr.Eval's own
//    default ─────────────────────────────────────────────────────────────

// TestExprEvalOptions_CarriesNativePathFlavor pins that every phase-3
// evaluation passes expr.WithPathFormat(expr.PathNative), because the worker
// runs on the host a task executes on: without it, a Windows worker would
// evaluate a native-flavored Session.WorkingDirectory value alongside
// POSIX-flavored path literals in the same expression.
//
// Limitation: expr.PathNative resolves to expr.PathPOSIX on every POSIX host
// (macOS, Linux CI), which is also expr.Eval's unconfigured default path
// format. Removing expr.WithPathFormat(pathFlavor) from ExprEvalOptions
// therefore gives identical output on such a host, and no test in this file
// fails.
//
// What this test pins portably: the rendering matches a separately built
// evaluation that names expr.PathNative explicitly (wantPathText). If
// ExprEvalOptions passed another explicit flavor (expr.PathPOSIX or
// expr.PathWindows), this test fails on a Windows host, where
// expr.PathNative and expr.PathPOSIX differ. expres_internal_test.go forces a
// flavor directly to cover the POSIX-host gap.
func TestExprEvalOptions_CarriesNativePathFlavor(t *testing.T) {
	const workDir = "/work/session1"
	syms, err := fmtres.TaskSymbols(&protocol.AssignMsg{}, workDir, "", false, nil)
	if err != nil {
		t.Fatalf("TaskSymbols: %v", err)
	}
	action := &protocol.Action{Command: "{{ Session.WorkingDirectory / 'a' / 'b' }}"}
	got, err := fmtres.ResolveActionExpr(action, syms, nil, nil)
	if err != nil {
		t.Fatalf("ResolveActionExpr: %v", err)
	}

	want := wantPathText(t, "WD / 'a' / 'b'", expr.MapSymbols{"WD": expr.Path(workDir, expr.PathNative)})
	if got.Command != want {
		t.Errorf("Command = %q, want %q (built independently under expr.PathNative)", got.Command, want)
	}
}

// ── TargetArgItem: the two parallel copies must agree ──────────────────────

// TestTargetArgItem_MatchesOpenjdsCopy enforces fmtres.TargetArgItem's doc
// comment: it must stay in step with openjd.TargetArgItem (exprcheck.go).
// Importing internal/openjd from a _test.go file is safe, as in
// exprsyms_test.go's TestPhase2Phase3Agreement: test binaries never link into
// cmd/sqi-worker, so this does not add the internal/store dependency the two
// copies exist to keep out of production code.
func TestTargetArgItem_MatchesOpenjdsCopy(t *testing.T) {
	if !fmtres.TargetArgItem.Equal(openjd.TargetArgItem) {
		t.Errorf("fmtres.TargetArgItem = %s, want it to equal openjd.TargetArgItem = %s "+
			"(the two copies must stay in step -- see fmtres.TargetArgItem's own doc comment)",
			fmtres.TargetArgItem, openjd.TargetArgItem)
	}
}

// ── ConvertPathMapRules ──────────────────────────────────────────────────────

func TestConvertPathMapRules(t *testing.T) {
	tests := []struct {
		wire string
		want expr.PathMapSourceFormat
	}{
		{"POSIX", expr.PathMapPOSIX},
		{"WINDOWS", expr.PathMapWindows},
		{"URI", expr.PathMapURI},
		{"", expr.PathMapPOSIX}, // defensive default; see ConvertPathMapRules' doc comment
	}
	for _, tc := range tests {
		t.Run(tc.wire, func(t *testing.T) {
			rules := []protocol.PathMapRule{{
				SourcePathFormat: tc.wire,
				SourcePath:       "/a",
				DestinationPath:  "/b",
			}}
			out := fmtres.ConvertPathMapRules(rules)
			if len(out) != 1 {
				t.Fatalf("ConvertPathMapRules returned %d rules, want 1", len(out))
			}
			if out[0].SourceFormat != tc.want {
				t.Errorf("SourceFormat = %v, want %v", out[0].SourceFormat, tc.want)
			}
			if out[0].SourcePath != "/a" || out[0].DestinationPath != "/b" {
				t.Errorf("rule = %+v, want SourcePath=/a DestinationPath=/b", out[0])
			}
		})
	}
	if got := fmtres.ConvertPathMapRules(nil); got != nil {
		t.Errorf("ConvertPathMapRules(nil) = %v, want nil", got)
	}
}

// ── ResolveEmbeddedFilesExpr / ResolveVarsExpr: basic correctness ──────────

func TestResolveEmbeddedFilesExpr(t *testing.T) {
	msg := &protocol.AssignMsg{
		Parameters:     map[string]string{"Frame": "3"},
		ParameterTypes: map[string]string{"Frame": "INT"},
	}
	syms, err := fmtres.TaskSymbols(msg, "/work", "", false, nil)
	if err != nil {
		t.Fatalf("TaskSymbols: %v", err)
	}
	files := []protocol.EmbeddedFile{{Name: "Script", Data: "frame = {{ Task.Param.Frame }}"}}
	out, err := fmtres.ResolveEmbeddedFilesExpr(files, syms, nil, nil)
	if err != nil {
		t.Fatalf("ResolveEmbeddedFilesExpr: %v", err)
	}
	if len(out) != 1 || out[0].Data != "frame = 3" {
		t.Errorf("resolved files = %+v, want Data %q", out, "frame = 3")
	}
	if len(files) != 1 || files[0].Data != "frame = {{ Task.Param.Frame }}" {
		t.Error("ResolveEmbeddedFilesExpr mutated its input")
	}
	if got, err := fmtres.ResolveEmbeddedFilesExpr(nil, syms, nil, nil); got != nil || err != nil {
		t.Errorf("ResolveEmbeddedFilesExpr(nil) = (%v, %v), want (nil, nil)", got, err)
	}
}

func TestResolveVarsExpr(t *testing.T) {
	msg := &protocol.AssignMsg{
		JobParameters:     map[string]string{"Scene": "hello"},
		JobParameterTypes: map[string]string{"Scene": "STRING"},
	}
	syms, err := fmtres.TaskSymbols(msg, "/work", "", false, nil)
	if err != nil {
		t.Fatalf("TaskSymbols: %v", err)
	}
	vars := map[string]string{"SCENE_NAME": "{{ Param.Scene }}"}
	out, err := fmtres.ResolveVarsExpr(vars, syms, nil, nil)
	if err != nil {
		t.Fatalf("ResolveVarsExpr: %v", err)
	}
	if out["SCENE_NAME"] != "hello" {
		t.Errorf("SCENE_NAME = %q, want %q", out["SCENE_NAME"], "hello")
	}
	if vars["SCENE_NAME"] != "{{ Param.Scene }}" {
		t.Error("ResolveVarsExpr mutated its input")
	}
	if got, err := fmtres.ResolveVarsExpr(nil, syms, nil, nil); got != nil || err != nil {
		t.Errorf("ResolveVarsExpr(nil) = (%v, %v), want (nil, nil)", got, err)
	}
}

// ── Float carry: section 1.3.4 ──────────────────────────────────────────────

// TestResolveActionExpr_FloatCarryPreservedInReprAndSubstitution pins whether
// repr_* and substitution show a float value's carried text or canonicalise
// it, which the specification leaves underspecified (see
// internal/openjd/expr/value.go's fs field comment). It is observable only
// where an expression's value is turned back into command-line text, which is
// this package.
//
// The carried text is kept. Section 1.3.4 exists so a submitted "3.500"
// survives to the command line; canonicalising inside a rendering function
// (repr_py, or plain substitution's string coercion) would discard that text.
// "3.500" is a valid float literal in Python, in JSON and on a PowerShell
// command line, so keeping it breaks no rendering target.
//
// Mechanism: expr.ValueFromText's CodeFloat case binds a FLOAT job parameter
// via expr.FloatText(f, raw) (internal/openjd/expr/paramtypes.go), which sets
// Value's fs field. Value.String() reads fs before formatFloat, and both paths
// this test exercises end in it: plain substitution's lone-reference coercion
// (coerceScalar's CodeString case, "return String(v.String())") and repr_py's
// pyRepr default case ("int and float already spell themselves the way
// Python does... return v.String()"). This test observes it end to end,
// through a rendered command line via fmtres.TaskSymbols/ResolveActionExpr.
func TestResolveActionExpr_FloatCarryPreservedInReprAndSubstitution(t *testing.T) {
	msg := &protocol.AssignMsg{
		JobParameters:     map[string]string{"Scale": "3.500"},
		JobParameterTypes: map[string]string{"Scale": "FLOAT"},
	}
	syms, err := fmtres.TaskSymbols(msg, "/work", "", false, nil)
	if err != nil {
		t.Fatalf("TaskSymbols: %v", err)
	}

	t.Run("plain substitution", func(t *testing.T) {
		action := &protocol.Action{Command: "{{ Param.Scale }}"}
		out, err := fmtres.ResolveActionExpr(action, syms, nil, nil)
		if err != nil {
			t.Fatalf("ResolveActionExpr: %v", err)
		}
		if out.Command != "3.500" {
			t.Errorf("Command = %q, want %q (the carried text, not the canonical \"3.5\")", out.Command, "3.500")
		}
	})

	t.Run("repr_py", func(t *testing.T) {
		action := &protocol.Action{Command: "{{ repr_py(Param.Scale) }}"}
		out, err := fmtres.ResolveActionExpr(action, syms, nil, nil)
		if err != nil {
			t.Fatalf("ResolveActionExpr: %v", err)
		}
		if out.Command != "3.500" {
			t.Errorf("Command = %q, want %q (the carried text, not the canonical \"3.5\")", out.Command, "3.500")
		}
	})
}

// ── nil action ───────────────────────────────────────────────────────────────

func TestResolveActionExpr_Nil(t *testing.T) {
	out, err := fmtres.ResolveActionExpr(nil, expr.MapSymbols{}, nil, nil)
	if out != nil || err != nil {
		t.Errorf("ResolveActionExpr(nil) = (%v, %v), want (nil, nil)", out, err)
	}
}
