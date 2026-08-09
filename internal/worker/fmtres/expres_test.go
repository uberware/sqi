// SPDX-License-Identifier: AGPL-3.0-or-later

package fmtres_test

// Tests for expres.go -- EXPR sub-project E4a, Task 4: rendering an
// expression's value into a real command line for the first time in the
// whole EXPR program.
//
// Each test builds its symbol table with fmtres.TaskSymbols (Task 3), so
// these exercise the real phase-3 table an actual assignment would produce,
// not a hand-rolled expr.MapSymbols standing in for it -- except where a
// test's whole point is a construct TaskSymbols cannot produce (a literal
// list, section 1.3.2's TargetArgItem flattening), which needs no symbol at
// all.

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/openjd/expr"
	"github.com/uberware/sqi/internal/worker/fmtres"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// ── Lone reference inherits the field's target type ─────────────────────────

// TestResolveActionExpr_LoneReferenceInheritsTargetType pins section 1.3.2's
// core rule: a format string that is EXACTLY one reference evaluates against
// the FIELD's own target type (TargetString for Command), so a path-typed
// value coerces to text via the field's target rather than by any type the
// reference happens to produce on its own. Task.Param.Out is bound as a
// PATH value (expr.TPath) by TaskSymbols; Command's target is
// expr.TString, and path -> string is section 1.2.3's own named compatible
// pair.
func TestResolveActionExpr_LoneReferenceInheritsTargetType(t *testing.T) {
	msg := &protocol.AssignMsg{
		Parameters:     map[string]string{"Out": "/renders/out.exr"},
		ParameterTypes: map[string]string{"Out": "PATH"},
	}
	syms, err := fmtres.TaskSymbols(msg, "/work", "", false)
	if err != nil {
		t.Fatalf("TaskSymbols: %v", err)
	}

	action := &protocol.Action{Command: "{{ Task.Param.Out }}"}
	out, err := fmtres.ResolveActionExpr(action, syms, fmtres.ExprEvalOptions(nil)...)
	if err != nil {
		t.Fatalf("ResolveActionExpr: %v", err)
	}
	if out.Command != "/renders/out.exr" {
		t.Errorf("Command = %q, want %q", out.Command, "/renders/out.exr")
	}
}

// TestResolveActionExpr_LoneArgListFlattensAndNullDrops covers the OTHER half
// of section 1.3.2's target-inheritance rule that Command's plain TString
// target cannot exercise on its own: an args entry's target is
// fmtres.TargetArgItem ("string? | list[string]"), so a lone reference's
// result decides how many actual arguments ONE template entry becomes.
// Needs no symbol table at all -- the list and the null are both literals.
func TestResolveActionExpr_LoneArgListFlattensAndNullDrops(t *testing.T) {
	action := &protocol.Action{
		Command: "render",
		Args:    []string{"{{ ['a', 'b', 'c'] }}", "{{ None }}", "-x"},
	}
	out, err := fmtres.ResolveActionExpr(action, expr.MapSymbols{}, fmtres.ExprEvalOptions(nil)...)
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
// (expr.TAny) and converts its NATURAL result to text -- RFC 0005's own
// worked example, verbatim ("Items: {{ [1, 2, 3] }}" -> "Items: [1, 2,
// 3]"), which a plain TString target could never accept directly (a
// list cannot coerce to a bare string), proving the embedded path really is
// a different evaluation from the lone-reference one, not the same call
// with the result concatenated.
func TestResolveActionExpr_EmbeddedReferenceStringified(t *testing.T) {
	action := &protocol.Action{Command: "Items: {{ [1, 2, 3] }}"}
	out, err := fmtres.ResolveActionExpr(action, expr.MapSymbols{}, fmtres.ExprEvalOptions(nil)...)
	if err != nil {
		t.Fatalf("ResolveActionExpr: %v", err)
	}
	if out.Command != "Items: [1, 2, 3]" {
		t.Errorf("Command = %q, want %q", out.Command, "Items: [1, 2, 3]")
	}
}

// TestResolveActionExpr_EmbeddedNullRendersEmpty pins the one reading this
// file's own doc comment flags as going beyond checkFormatString's
// type-checking-only concern: RFC 0005 states a null result embedded in a
// format string is the empty string, not Value.String()'s own "null"
// diagnostic rendering.
func TestResolveActionExpr_EmbeddedNullRendersEmpty(t *testing.T) {
	action := &protocol.Action{Command: "before[{{ None }}]after"}
	out, err := fmtres.ResolveActionExpr(action, expr.MapSymbols{}, fmtres.ExprEvalOptions(nil)...)
	if err != nil {
		t.Fatalf("ResolveActionExpr: %v", err)
	}
	if out.Command != "before[]after" {
		t.Errorf("Command = %q, want %q", out.Command, "before[]after")
	}
}

// ── Task.Param.* ─────────────────────────────────────────────────────────────

// TestResolveActionExpr_TaskParam proves Task.Param.Frame is bound as a real
// int (not a string), the same defect exprsyms_test.go's own
// TestTaskSymbols_JobParamTypes guards against: "71" would mean
// concatenation happened instead of addition.
func TestResolveActionExpr_TaskParam(t *testing.T) {
	msg := &protocol.AssignMsg{
		Parameters:     map[string]string{"Frame": "7"},
		ParameterTypes: map[string]string{"Frame": "INT"},
	}
	syms, err := fmtres.TaskSymbols(msg, "/work", "", false)
	if err != nil {
		t.Fatalf("TaskSymbols: %v", err)
	}
	action := &protocol.Action{Command: "{{ Task.Param.Frame + 1 }}"}
	out, err := fmtres.ResolveActionExpr(action, syms, fmtres.ExprEvalOptions(nil)...)
	if err != nil {
		t.Fatalf("ResolveActionExpr: %v", err)
	}
	if out.Command != "8" {
		t.Errorf("Command = %q, want %q (addition, not concatenation)", out.Command, "8")
	}
}

// ── Session.WorkingDirectory ────────────────────────────────────────────────

// TestResolveActionExpr_SessionWorkingDirectory exercises the "/" path-join
// operator over a real concrete Session.WorkingDirectory value -- the
// EXR sub-project C4/D construct a StepScript.let binding like
// "Session.WorkingDirectory / 'frames'" is meant to produce for real, on the
// host, for the first time.
func TestResolveActionExpr_SessionWorkingDirectory(t *testing.T) {
	const workDir = "/work/session1"
	syms, err := fmtres.TaskSymbols(&protocol.AssignMsg{}, workDir, "", false)
	if err != nil {
		t.Fatalf("TaskSymbols: %v", err)
	}
	action := &protocol.Action{Command: "{{ Session.WorkingDirectory / 'frames' }}"}
	out, err := fmtres.ResolveActionExpr(action, syms, fmtres.ExprEvalOptions(nil)...)
	if err != nil {
		t.Fatalf("ResolveActionExpr: %v", err)
	}
	want := filepath.ToSlash(filepath.Join(workDir, "frames"))
	if out.Command != want {
		t.Errorf("Command = %q, want %q", out.Command, want)
	}
}

// ── Task.File.* ──────────────────────────────────────────────────────────────

// TestResolveActionExpr_TaskFile exercises a materialized embedded-file path
// -- AddFileVars' own computation, reached through TaskSymbols, rendered
// into a real command for the first time.
func TestResolveActionExpr_TaskFile(t *testing.T) {
	const workDir = "/work/session1"
	msg := &protocol.AssignMsg{
		EmbeddedFiles: []protocol.EmbeddedFile{{Name: "Script", Filename: "run.py"}},
	}
	syms, err := fmtres.TaskSymbols(msg, workDir, "", false)
	if err != nil {
		t.Fatalf("TaskSymbols: %v", err)
	}
	action := &protocol.Action{Command: "python", Args: []string{"{{ Task.File.Script }}"}}
	out, err := fmtres.ResolveActionExpr(action, syms, fmtres.ExprEvalOptions(nil)...)
	if err != nil {
		t.Fatalf("ResolveActionExpr: %v", err)
	}
	want := filepath.Join(workDir, "run.py")
	if len(out.Args) != 1 || out.Args[0] != want {
		t.Errorf("Args = %v, want [%q]", out.Args, want)
	}
}

// ── Evaluation error surfaces with context ──────────────────────────────────

// TestResolveActionExpr_ErrorSurfacesWithContext pins that a failing
// expression's error names BOTH the field it failed in (so a template
// author can find which line broke) and the expression evaluator's own
// diagnostic (an unknown-symbol message with a column position) -- matching
// fmtres.go's existing ResolveAction/ResolveVars/ResolveEmbeddedFiles
// wrapping convention (one field-context layer around the underlying
// error), now extended to expr's own positioned errors.
func TestResolveActionExpr_ErrorSurfacesWithContext(t *testing.T) {
	action := &protocol.Action{Command: "{{ Task.Param.DoesNotExist }}"}
	_, err := fmtres.ResolveActionExpr(action, expr.MapSymbols{}, fmtres.ExprEvalOptions(nil)...)
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
// for ResolveVarsExpr, whose wrapping names a MAP KEY rather than a fixed
// field name like "command".
func TestResolveVarsExpr_ErrorNamesTheVariableKey(t *testing.T) {
	vars := map[string]string{"BAD": "{{ 1 / 0 }}"}
	_, err := fmtres.ResolveVarsExpr(vars, expr.MapSymbols{}, fmtres.ExprEvalOptions(nil)...)
	if err == nil {
		t.Fatal("ResolveVarsExpr: want an error for a division by zero, got nil")
	}
	if !strings.Contains(err.Error(), `variable "BAD"`) {
		t.Errorf("error %q does not name the offending variable key", err.Error())
	}
}

// TestResolveActionExpr_UnresolvedFallbackErrorsInsteadOfPanicking is a
// regression test for a real defect self-review caught while implementing
// this file: exprsyms_test.go's own TestTaskSymbols_ChunkIntTaskParam_
// InvalidRangeFallsBackToUnresolved documents that an unparseable CHUNK[INT]
// value stays expr.Unresolved rather than concrete. Reading that symbol
// through expr.TString's own coercion (coerceUnresolved) returns ANOTHER
// unresolved Value with NO error -- Eval succeeds -- so the first cut of
// resolveFormatStringExpr called Value.AsStr() on it directly and PANICKED
// ("expr: read string payload from a unresolved[string] value"). A worker
// process must not crash rendering one task's command line; this pins the
// fix (errUnresolvedValue) instead.
func TestResolveActionExpr_UnresolvedFallbackErrorsInsteadOfPanicking(t *testing.T) {
	msg := &protocol.AssignMsg{
		Parameters:     map[string]string{"Frames": "not-a-range"},
		ParameterTypes: map[string]string{"Frames": "CHUNK[INT]"},
	}
	syms, err := fmtres.TaskSymbols(msg, "/work", "", false)
	if err != nil {
		t.Fatalf("TaskSymbols: %v", err)
	}
	sym, ok := syms.Lookup("Task.Param.Frames")
	if !ok || !sym.IsUnresolved() {
		t.Fatalf("test premise broken: Task.Param.Frames = %+v, want an unresolved fallback", sym)
	}

	action := &protocol.Action{Command: "{{ Task.Param.Frames }}"}
	_, err = fmtres.ResolveActionExpr(action, syms, fmtres.ExprEvalOptions(nil)...)
	if err == nil {
		t.Fatal("ResolveActionExpr: want an error for an unresolved fallback value, got nil (and no panic)")
	}
	if !strings.Contains(err.Error(), "unresolved") {
		t.Errorf("error %q does not explain that the value is unresolved", err.Error())
	}
}

// ── Metered evaluation ───────────────────────────────────────────────────────

// TestResolveActionExpr_OverBudgetRejectedByOperationLimit is the test the
// brief calls out by name: it must assert the NAMED limit sqi configured
// (fmtres's own workerOperationLimit), not merely that some error occurred
// -- a bare error assertion would still pass under expr.Eval's own
// unconfigured 10,000,000-operation default and would pin nothing about
// THIS package's metering. The comprehension iterates well past
// workerOperationLimit (1,000,000) while its filter (always false) keeps
// the result list empty, so this is an OPERATION-limit rejection
// specifically, not a memory one -- isolating exactly the bound under test,
// the same discriminating-example discipline
// TestCheckTemplateExpressions_AppliesSubmissionLimits documents for the
// equivalent server-side pair.
func TestResolveActionExpr_OverBudgetRejectedByOperationLimit(t *testing.T) {
	action := &protocol.Action{
		Command: "echo",
		Args:    []string{"{{ len([x for x in range(1100000) if x > 999999999]) }}"},
	}
	_, err := fmtres.ResolveActionExpr(action, expr.MapSymbols{}, fmtres.ExprEvalOptions(nil)...)
	if err == nil {
		t.Fatal("ResolveActionExpr: want an operation-limit error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "operation limit exceeded") {
		t.Fatalf("error %q is not an operation-limit rejection", msg)
	}
	// The named limit itself must appear -- proof this is
	// workerOperationLimit (1,000,000) doing the rejecting, not
	// expr.Eval's own unconfigured default (10,000,000), which this
	// expression's ~1,100,001 operations would sail under.
	if !strings.Contains(msg, "1000000") {
		t.Errorf("error %q does not name the configured limit (1000000); "+
			"this test must pin fmtres's OWN metering, not expr.Eval's default", msg)
	}
}

// ── apply_path_mapping: the session's real rules, for the first time ───────

// TestResolveActionExpr_ApplyPathMapping is the test EXPR sub-project E4a's
// brief calls out explicitly: sub-project D built the path-mapping engine
// and E2 gated apply_path_mapping to host-context scopes, but nothing has
// ever executed it against a real session rule end to end until this test.
// ExprEvalOptions is what threads the assignment's PathMap field
// (fmtres.ConvertPathMapRules) into the evaluation via expr.WithPathMapping.
func TestResolveActionExpr_ApplyPathMapping(t *testing.T) {
	msg := &protocol.AssignMsg{
		Parameters:     map[string]string{"Scene": "/mnt/shared/project/shot.ma"},
		ParameterTypes: map[string]string{"Scene": "PATH"},
	}
	syms, err := fmtres.TaskSymbols(msg, "/work", "", false)
	if err != nil {
		t.Fatalf("TaskSymbols: %v", err)
	}

	pathMap := []protocol.PathMapRule{
		{
			SourcePathFormat: "POSIX",
			SourcePath:       "/mnt/shared",
			DestinationPath:  "/local/cache",
		},
	}

	action := &protocol.Action{Command: "{{ apply_path_mapping(Task.Param.Scene) }}"}
	out, err := fmtres.ResolveActionExpr(action, syms, fmtres.ExprEvalOptions(pathMap)...)
	if err != nil {
		t.Fatalf("ResolveActionExpr: %v", err)
	}
	const want = "/local/cache/project/shot.ma"
	if out.Command != want {
		t.Errorf("apply_path_mapping(%q) via rule %+v = %q, want %q",
			"/mnt/shared/project/shot.ma", pathMap[0], out.Command, want)
	}
}

// TestResolveActionExpr_ApplyPathMappingNoRulesPassesThrough is the negative
// companion: with no PathMap entries (the common case -- most tasks
// reference no named storage location), apply_path_mapping must pass its
// input through unchanged, normalized as a path in the evaluation's own
// flavor -- expr.WithPathMapping's own documented "no rule rewrote it"
// contract, now confirmed reachable from this package's real call path.
func TestResolveActionExpr_ApplyPathMappingNoRulesPassesThrough(t *testing.T) {
	msg := &protocol.AssignMsg{
		Parameters:     map[string]string{"Scene": "/mnt/shared/project/shot.ma"},
		ParameterTypes: map[string]string{"Scene": "PATH"},
	}
	syms, err := fmtres.TaskSymbols(msg, "/work", "", false)
	if err != nil {
		t.Fatalf("TaskSymbols: %v", err)
	}
	action := &protocol.Action{Command: "{{ apply_path_mapping(Task.Param.Scene) }}"}
	out, err := fmtres.ResolveActionExpr(action, syms, fmtres.ExprEvalOptions(nil)...)
	if err != nil {
		t.Fatalf("ResolveActionExpr: %v", err)
	}
	const want = "/mnt/shared/project/shot.ma"
	if out.Command != want {
		t.Errorf("apply_path_mapping with no rules = %q, want passthrough %q", out.Command, want)
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
	syms, err := fmtres.TaskSymbols(msg, "/work", "", false)
	if err != nil {
		t.Fatalf("TaskSymbols: %v", err)
	}
	files := []protocol.EmbeddedFile{{Name: "Script", Data: "frame = {{ Task.Param.Frame }}"}}
	out, err := fmtres.ResolveEmbeddedFilesExpr(files, syms, fmtres.ExprEvalOptions(nil)...)
	if err != nil {
		t.Fatalf("ResolveEmbeddedFilesExpr: %v", err)
	}
	if len(out) != 1 || out[0].Data != "frame = 3" {
		t.Errorf("resolved files = %+v, want Data %q", out, "frame = 3")
	}
	if len(files) != 1 || files[0].Data != "frame = {{ Task.Param.Frame }}" {
		t.Error("ResolveEmbeddedFilesExpr mutated its input")
	}
	if got, err := fmtres.ResolveEmbeddedFilesExpr(nil, syms, fmtres.ExprEvalOptions(nil)...); got != nil || err != nil {
		t.Errorf("ResolveEmbeddedFilesExpr(nil) = (%v, %v), want (nil, nil)", got, err)
	}
}

func TestResolveVarsExpr(t *testing.T) {
	msg := &protocol.AssignMsg{
		JobParameters:     map[string]string{"Scene": "hello"},
		JobParameterTypes: map[string]string{"Scene": "STRING"},
	}
	syms, err := fmtres.TaskSymbols(msg, "/work", "", false)
	if err != nil {
		t.Fatalf("TaskSymbols: %v", err)
	}
	vars := map[string]string{"SCENE_NAME": "{{ Param.Scene }}"}
	out, err := fmtres.ResolveVarsExpr(vars, syms, fmtres.ExprEvalOptions(nil)...)
	if err != nil {
		t.Fatalf("ResolveVarsExpr: %v", err)
	}
	if out["SCENE_NAME"] != "hello" {
		t.Errorf("SCENE_NAME = %q, want %q", out["SCENE_NAME"], "hello")
	}
	if vars["SCENE_NAME"] != "{{ Param.Scene }}" {
		t.Error("ResolveVarsExpr mutated its input")
	}
	if got, err := fmtres.ResolveVarsExpr(nil, syms, fmtres.ExprEvalOptions(nil)...); got != nil || err != nil {
		t.Errorf("ResolveVarsExpr(nil) = (%v, %v), want (nil, nil)", got, err)
	}
}

// ── nil action ───────────────────────────────────────────────────────────────

func TestResolveActionExpr_Nil(t *testing.T) {
	out, err := fmtres.ResolveActionExpr(nil, expr.MapSymbols{}, fmtres.ExprEvalOptions(nil)...)
	if out != nil || err != nil {
		t.Errorf("ResolveActionExpr(nil) = (%v, %v), want (nil, nil)", out, err)
	}
}
