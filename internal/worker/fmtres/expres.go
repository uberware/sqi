// SPDX-License-Identifier: AGPL-3.0-or-later

package fmtres

// This file is the EXPR-aware counterpart of fmtres.go's ResolveAction,
// ResolveEmbeddedFiles and ResolveVars: it evaluates an OpenJD expression
// against the phase-3 symbol table (exprsyms.go's TaskSymbols/EnvSymbols)
// instead of doing plain "{{Name}}" substitution. This is where an
// expression's value reaches a real command line; the earlier evaluations
// (phases 1 and 2, and server-side let bindings) type-check or bind a value
// and discard it.
//
// It sits beside fmtstring.Resolve: a template that does not declare the EXPR
// extension takes fmtstring.Resolve's code path unchanged. The caller
// (executor and session) picks between the two families using the
// assignment's EXPR flag (protocol.AssignMsg.EXPR), the same flag the
// symbol-table builders assume is true.
//
// # Lone versus embedded (section 1.3.2)
//
// This file follows the shape of internal/openjd's checkFormatString
// (exprcheck.go): phases 2 and 3 must differ only in which symbols are
// concrete. Both use fmtstring.LoneRef to ask whether the format string is
// exactly one reference with no surrounding text, and answer it the same way:
//
//   - A lone reference inherits the field's target type (TargetString for a
//     command, an embedded file's data, or a variable value; TargetArgItem,
//     below, for one args entry) and is evaluated once against it, so the
//     implicit coercions section 1.2.3 defines (path -> string, an int read
//     as the field's single scalar target, and so on) apply as they do at
//     submission time.
//   - Anything else (literal text, a reference embedded inside other text,
//     or more than one reference) evaluates each reference unconstrained
//     (expr.TAny) and converts its natural result to text via
//     Value.String(), which matches the language's string() function byte
//     for byte, as openjd-specifications#176 requires ("this same
//     conversion"); expr routes both renderings through one quoter.
//     checkFormatString takes the same branch and ignores its target
//     parameter there: the result is converted to a string whatever the
//     reference produces, so a type that would fail against the field's
//     target is fine embedded in text ("Count: {{ len(myList) }}" ->
//     "Count: 5", RFC 0005's example).
//
// checkFormatString stops at "does this evaluate without error" and
// discards the result; this file keeps the result and turns it into wire
// text (or, for an args entry, into zero, one, or several entries; see
// TargetArgItem below). It cannot call into internal/openjd, which imports
// internal/store, a package cmd/sqi-worker must never depend on.
//
// Rendering decides one case type-checking does not: a null value embedded in
// surrounding text renders as the empty string, not the word "null"
// Value.String() produces for a bare null. RFC 0005 states this for the
// format-string position ("A None/null result is treated as the empty
// string").
//
// # TargetArgItem and list-item flattening
//
// exprcheck.go's TargetArgItem ("a string is one argument, None drops the
// argument, and a list[string] flattens inline", section 1.3.2's list-item
// rule) cannot be imported from here for the same internal/store layering
// reason, so this file carries its own copy, built from the identical
// expr.UnionOf(expr.OptionalOf(expr.TString), expr.ListOf(expr.TString))
// expression. The two copies must stay identical. Unlike a command, an
// embedded file's data or a variable value (all TargetString, always one
// resulting string), a lone args-entry reference can change how many
// command-line arguments the entry produces, and this is where sqi renders
// that; checkFormatString only type-checks against TargetArgItem.
//
// # Metering
//
// Every evaluation in this file is metered by [ExprLimits.OperationLimit] and
// [ExprLimits.MemoryLimit], never expr.Eval's unconfigured defaults. Phase 3
// costs more than phase 1, because concrete strings and paths cost real bytes
// where a placeholder costs none, and the worker is a shared, long-lived
// process evaluating untrusted templates.
//
// The two numbers are operator configuration: they arrive on the
// assignment's [AssignmentBudget] (exprlimits.go), whose defaults are the
// constants below. Every function below that evaluates anything takes that
// budget, so the evaluator cannot be reached with another pair of numbers or
// with none.

import (
	"fmt"
	"strings"

	"github.com/uberware/sqi/internal/openjd/expr"
	"github.com/uberware/sqi/internal/openjd/fmtstring"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// defaultWorkerOperationLimit and defaultWorkerMemoryLimit are the defaults
// for the section 1.3.10/1.3.9 budgets every phase-3 evaluation in this file
// is metered against ([ExprLimits.OperationLimit] and
// [ExprLimits.MemoryLimit], reached via ExprEvalOptions). An operator can
// size them per host, within the ranges exprlimits.go documents.
//
// They are looser than internal/openjd's
// defaultSubmissionOperations/defaultSubmissionMemoryBytes (10,000 ops /
// 1,000,000 bytes), which keep template validation cheap on a synchronous HTTP
// request path evaluating mostly unresolved placeholders. Phase 3 evaluates
// the same expressions against concrete, real-sized values (an embedded
// file's body, a real session working directory), so legitimate templates
// need more room here.
//
// Neither number is derived from internal/openjd/expr's fixed safety guards
// (limits.go's maxElements and maxStringBytes, both 10,000,000), and the
// memory limit is above maxStringBytes. They bound different quantities:
// maxStringBytes caps one produced string, while section 1.3.9's meter sums
// live values and recurses into containers, so a list of two 9 MB strings
// holds ~18 MB with no fixed guard firing; and the operation limit counts
// operations, not elements, so it is not comparable to maxElements.
//
// Neither is an aggregate bound. For the one construct that retains values
// across evaluations, a let: block, a per-Eval budget is not enough: see
// exprsyms.go's defaultLetRetainedBytes, which bounds what one symbol table
// may hold live. Every other evaluation in this file renders its result to
// text and drops the Value, so this pair suffices for them.
const (
	defaultWorkerOperationLimit int64 = 1_000_000
	defaultWorkerMemoryLimit    int64 = 20_000_000
)

// TargetArgItem is this package's copy of exprcheck.go's identically named
// value. See the comment at the top of this file for why a second copy exists;
// it must stay in step with the original.
var TargetArgItem = expr.UnionOf(expr.OptionalOf(expr.TString), expr.ListOf(expr.TString))

// ExprEvalOptions builds the []expr.Option every phase-3 evaluation in this
// package is called with: the assignment's metering (lim's
// OperationLimit/MemoryLimit; a zero ExprLimits normalizes to the defaults
// above, so there is no unmetered spelling), the evaluation's path flavor
// (pathFlavor, exprsyms.go's expr.PathNative: the worker runs on the host a
// task executes on, so its concrete Session.WorkingDirectory and Task.File.*
// values, and any path literal an expression writes, must agree on one
// flavor; see exprsyms.go's doc comment for why that is PathNative here and
// POSIX upstream), and the session's apply_path_mapping rules translated from
// the assignment's wire format via ConvertPathMapRules.
//
// This is the one place these settings are assembled, so a caller cannot use
// the resolvers in this file without expr.WithPathFormat(expr.PathNative).
//
// It wraps exprEvalOptionsFor with pathFlavor. exprEvalOptionsFor takes the
// flavor as a parameter so a white-box test (expres_internal_test.go) can
// force a flavor other than the one this host's pathFlavor.resolve()
// produces, proving expr.WithPathFormat is wired; on a POSIX-only CI host the
// option being set and the option being absent are otherwise
// indistinguishable.
func ExprEvalOptions(lim ExprLimits, pathMap []protocol.PathMapRule) []expr.Option {
	return exprEvalOptionsFor(lim, pathMap, pathFlavor)
}

// exprEvalOptionsFor is ExprEvalOptions' flavor-parameterized implementation;
// see that function's doc comment for why the split exists.
func exprEvalOptionsFor(lim ExprLimits, pathMap []protocol.PathMapRule, flavor expr.PathFormat) []expr.Option {
	opts := lim.orDefaults().evalOptions()
	return append(
		opts,
		expr.WithPathFormat(flavor),
		expr.WithPathMapping(ConvertPathMapRules(pathMap)),
	)
}

// ConvertPathMapRules converts the assignment's wire-format path-mapping
// rules (protocol.PathMapRule, JSON-shaped for the OpenJD pathmapping-1.0
// file) into expr.PathMapRule, the type expr.WithPathMapping accepts.
//
// The conversion is field for field: SourcePath and DestinationPath copy
// verbatim, and SourcePathFormat's three wire spellings ("POSIX", "WINDOWS",
// "URI"; see protocol.PathMapRule's doc comment) correspond one to one with
// expr.PathMapSourceFormat's three constants. convertPathMapSourceFormat's
// default case (an unrecognized spelling) is defensive, like
// EmbeddedFileName's filename checks in fmtres.go: the server builds the
// assignment and writes only those three, and PathMapPOSIX, which assumes no
// particular structure in the text, is the fallback.
func ConvertPathMapRules(rules []protocol.PathMapRule) []expr.PathMapRule {
	if rules == nil {
		return nil
	}
	out := make([]expr.PathMapRule, len(rules))
	for i, r := range rules {
		out[i] = expr.PathMapRule{
			SourceFormat:    convertPathMapSourceFormat(r.SourcePathFormat),
			SourcePath:      r.SourcePath,
			DestinationPath: r.DestinationPath,
		}
	}
	return out
}

func convertPathMapSourceFormat(s string) expr.PathMapSourceFormat {
	switch s {
	case "WINDOWS":
		return expr.PathMapWindows
	case "URI":
		return expr.PathMapURI
	default:
		return expr.PathMapPOSIX
	}
}

// mapPathParamValue is exprsyms.go's [paramValueForBinding] helper for a
// PATH-declared Param.<name>/Task.Param.<name>: it runs raw (the submitted
// parameter text, never a value already rendered in some path flavor) through
// the assignment's path-mapping rules and returns the mapped, concrete path
// Value section 1.2.2 requires Param.<name> to carry.
//
// It calls through the registered apply_path_mapping function (expr.Eval),
// not expr's unexported mapPath, as pathmapping.go's doc comment requires:
// "What makes 'the result is a path in dst' true is the WRAPPER,
// pathMappingFuncs' apply_path_mapping ... Callers wanting a path value must
// do the same rather than assume it." Reusing the wrapper also avoids a
// second implementation of the path-mapping engine.
//
// raw is bound as a string symbol rather than interpolated into the
// expression source, so path text with quotes, backslashes or non-ASCII is
// never parsed as expression syntax. opts comes from [ExprEvalOptions]
// (metering, expr.PathNative, the converted rules), the same options every
// other phase-3 evaluation in this package gets. It is taken pre-built so a
// caller binding many parameters, or many elements of one LIST[PATH],
// assembles the options once rather than once per value; callers still reach
// them only through ExprEvalOptions, so no spelling skips metering.
func mapPathParamValue(raw string, opts []expr.Option) (expr.Value, error) {
	syms := expr.MapSymbols{"src": expr.String(raw)}
	v, err := applyPathMappingExpr.Eval(syms, expr.TPath, opts...)
	if err != nil {
		return expr.Value{}, fmt.Errorf("mapping %q: %w", raw, err)
	}
	return v, nil
}

// applyPathMappingExprSrc is the constant expression [mapPathParamValue]
// evaluates, and applyPathMappingExpr is its parsed form.
//
// It is parsed once, at package init, as expr.Eval's doc comment advises
// ("Prefer Parse plus Expression.Eval when the same expression is evaluated
// more than once"): a LIST[PATH] parameter binds one value per element.
//
// Reusing the parsed expression across calls and goroutines (two task slots
// may build symbol tables concurrently) is safe because
// [expr.Expression.Eval] builds a fresh evaluation context per call and
// treats the parsed tree as read-only. Metering is likewise per call.
//
// The panic is unreachable in practice: the source is a constant this
// package's tests evaluate, so a parse failure would show up at package
// initialization on any run.
const applyPathMappingExprSrc = "apply_path_mapping(src)"

var applyPathMappingExpr = mustParseExpr(applyPathMappingExprSrc)

// mustParseExpr parses a constant expression at package initialization,
// panicking if it does not parse. See [applyPathMappingExpr] for why a panic
// suits a literal the package itself authors.
func mustParseExpr(src string) *expr.Expression {
	e, err := expr.Parse(src)
	if err != nil {
		panic(fmt.Sprintf("fmtres: constant expression %q does not parse: %v", src, err))
	}
	return e
}

// errUnresolvedValue reports that a phase-3 evaluation produced a
// still-unresolved placeholder instead of a concrete value.
//
// exprsyms.go binds only concrete symbols, with one exception that file's doc
// comment names: a declared value whose submitted text failed to parse as its
// declared type (expr.ValueFromText's fallback; an invalid CHUNK[INT] range
// is the case exprsyms_test.go pins) stays Unresolved instead of panicking.
// An expression that reads it can carry the unresolved value through
// coerceTop without error: coerceUnresolved narrows an unresolved value's
// constraint and returns another unresolved value, so Eval(target=TString)
// or Eval(target=TargetArgItem) can return an unresolved Value with no error.
//
// Every call site in this file that reaches a Value payload accessor
// (AsStr, AsList) checks IsUnresolved first and returns this error instead:
// the accessors panic on the wrong code (value.go's mustBe doc comment: "a
// silent zero would flow into a rendered command line unnoticed"), and
// Value.String()'s rendering of an unresolved value ("<unresolved[T]>", a
// diagnostic form) must not reach a command line either.
func errUnresolvedValue(v expr.Value) error {
	return fmt.Errorf(
		"value is unresolved (%s): the submitted text could not be parsed as its declared type", v.Type,
	)
}

// resolveFormatStringExpr resolves one format-string field (a command, an
// embedded file's data, or a variable value) against syms, implementing
// section 1.3.2 as described at the top of this file: a lone reference
// evaluates once against expr.TString and its coerced payload is the result;
// anything else walks fmtstring.Segments, evaluating each reference against
// expr.TAny and rendering it with Value.String() (a null result renders as
// the empty string, per RFC 0005), concatenated with the literal text around
// it.
//
// The returned error carries no field context (no "command" or "variable X"
// prefix); the caller adds it, as in fmtres.go's
// ResolveAction/ResolveEmbeddedFiles/ResolveVars, so a field is named once.
func resolveFormatStringExpr(s string, syms expr.MapSymbols, opts []expr.Option) (string, error) {
	if body, ok := fmtstring.LoneRef(s); ok {
		e, err := expr.Parse(body)
		if err != nil {
			return "", err
		}
		v, err := e.Eval(syms, expr.TString, opts...)
		if err != nil {
			return "", err
		}
		if v.IsUnresolved() {
			return "", errUnresolvedValue(v)
		}
		return v.AsStr(), nil
	}

	segs, err := fmtstring.Segments(s)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, seg := range segs {
		if !seg.IsRef {
			b.WriteString(seg.Literal)
			continue
		}
		v, err := evalEmbeddedRef(seg.Ref, syms, opts)
		if err != nil {
			return "", err
		}
		b.WriteString(v)
	}
	return b.String(), nil
}

// evalEmbeddedRef evaluates one embedded reference (expr.TAny, converted to
// text): the half of section 1.3.2's rule shared by resolveFormatStringExpr's
// segment loop and resolveArgItemExpr's non-lone branch.
func evalEmbeddedRef(ref string, syms expr.MapSymbols, opts []expr.Option) (string, error) {
	e, err := expr.Parse(ref)
	if err != nil {
		return "", err
	}
	v, err := e.Eval(syms, expr.TAny, opts...)
	if err != nil {
		return "", err
	}
	if v.IsUnresolved() {
		return "", errUnresolvedValue(v)
	}
	if v.IsNull() {
		// RFC 0005: "Within a format string... the target type is string?. A
		// None/null result is treated as the empty string". Value.String()'s
		// "null" rendering is a diagnostic form (see its doc comment) not
		// meant for this position.
		return "", nil
	}
	return v.String(), nil
}

// resolveArgItemExpr resolves one Args entry, implementing section 1.3.2's
// list-item rule via TargetArgItem: a lone reference evaluates once against
// it, and its result decides how many arguments this one template entry
// becomes. null drops it (zero arguments), a list[string] flattens inline (one
// argument per element), and anything else is one argument. Anything other
// than a lone reference (literal text, an embedded reference, several
// references) resolves through the same path as every other format-string
// field (resolveFormatStringExpr) and is always one argument, section
// 1.3.2's "a string is one argument" case.
func resolveArgItemExpr(s string, syms expr.MapSymbols, opts []expr.Option) ([]string, error) {
	if body, ok := fmtstring.LoneRef(s); ok {
		return resolveArgLoneRef(body, syms, opts)
	}

	r, err := resolveFormatStringExpr(s, syms, opts)
	if err != nil {
		return nil, err
	}
	return []string{r}, nil
}

// resolveArgLoneRef is resolveArgItemExpr's lone-reference branch, split out
// to keep the caller within golangci-lint's nestif limit.
func resolveArgLoneRef(body string, syms expr.MapSymbols, opts []expr.Option) ([]string, error) {
	e, err := expr.Parse(body)
	if err != nil {
		return nil, err
	}
	v, err := e.Eval(syms, TargetArgItem, opts...)
	if err != nil {
		return nil, err
	}
	switch {
	case v.IsUnresolved():
		return nil, errUnresolvedValue(v)
	case v.IsNull():
		return nil, nil
	case v.Type.Code == expr.CodeList:
		elems := v.AsList()
		out := make([]string, len(elems))
		for i, elem := range elems {
			if elem.IsUnresolved() {
				return nil, errUnresolvedValue(elem)
			}
			out[i] = elem.AsStr()
		}
		return out, nil
	default:
		return []string{v.AsStr()}, nil
	}
}

// ResolveActionExpr is ResolveAction's EXPR-aware counterpart: it returns a
// copy of action with Command resolved via resolveFormatStringExpr and every
// Args entry resolved via resolveArgItemExpr (see that function's doc
// comment for why one entry may become zero, one, or several arguments). All
// other fields (TimeoutSeconds, Cancelation) are copied unchanged. A nil
// action returns (nil, nil).
//
// syms is the phase-3 symbol table (TaskSymbols or EnvSymbols); pathMap is
// the assignment's PathMap field, forwarded to ExprEvalOptions to build this
// call's options, so every call applies metering, the worker's native path
// flavor and the session's path-mapping rules. It takes pathMap rather than a
// variadic opts ...expr.Option parameter: a variadic tail compiles when the
// caller passes nothing, handing every evaluation expr.Eval's unconfigured
// defaults (10x the operations and 5x the memory of this package's limits)
// with nothing to catch it. Taking pathMap rather than a pre-built
// []expr.Option means no options value a caller could construct skips
// metering.
//
// ResolveActionExpr returns an error naming the offending field if any
// reference is malformed, evaluates to an error, or names a symbol syms does
// not provide. The input action is never mutated.
//
// budget is charged one position for the command and one for each Args
// entry, before resolving it, so a budget already exhausted earlier in the
// assignment stops the walk without evaluating that entry. See
// [AssignmentBudget]. It is required; nil (passed by this package's unit
// tests) charges a fresh, default-limited throwaway budget. See
// [budgetOrDefault], which also documents why a production caller must not
// pass nil.
func ResolveActionExpr(
	action *protocol.Action, syms expr.MapSymbols, pathMap []protocol.PathMapRule, budget *AssignmentBudget,
) (*protocol.Action, error) {
	if action == nil {
		return nil, nil
	}
	b := budgetOrDefault(budget)
	opts := ExprEvalOptions(b.Limits(), pathMap)

	if err := b.ChargePositions(1, "command"); err != nil {
		return nil, err
	}
	cmd, err := resolveFormatStringExpr(action.Command, syms, opts)
	if err != nil {
		return nil, fmt.Errorf("command %q: %w", action.Command, err)
	}

	var args []string
	if action.Args != nil {
		args = make([]string, 0, len(action.Args))
		for i, a := range action.Args {
			if err := b.ChargePositions(1, fmt.Sprintf("arg %d", i)); err != nil {
				return nil, err
			}
			resolved, rerr := resolveArgItemExpr(a, syms, opts)
			if rerr != nil {
				return nil, fmt.Errorf("arg %d %q: %w", i, a, rerr)
			}
			args = append(args, resolved...)
		}
	}

	out := *action // shallow copy preserves TimeoutSeconds, Cancelation pointer
	out.Command = cmd
	out.Args = args
	return &out, nil
}

// ResolveEmbeddedFilesExpr is ResolveEmbeddedFiles' EXPR-aware counterpart:
// it returns copies of files with each file's Data resolved via
// resolveFormatStringExpr (TargetString: an embedded file's body is always
// one string, never subject to TargetArgItem's list-item rule). All other
// fields (Name, Filename, Runnable, EndOfLine) are copied unchanged. A nil
// slice returns (nil, nil).
//
// See [ResolveActionExpr] for syms/pathMap/budget. ResolveEmbeddedFilesExpr
// returns an error naming the offending file when a file's
// data is malformed, evaluates to an error, or names a symbol syms does not
// provide. The input slice and its elements are never mutated.
func ResolveEmbeddedFilesExpr(
	files []protocol.EmbeddedFile, syms expr.MapSymbols, pathMap []protocol.PathMapRule, budget *AssignmentBudget,
) ([]protocol.EmbeddedFile, error) {
	if files == nil {
		return nil, nil
	}
	b := budgetOrDefault(budget)
	opts := ExprEvalOptions(b.Limits(), pathMap)
	out := make([]protocol.EmbeddedFile, len(files))
	for i, f := range files {
		if err := b.ChargePositions(1, fmt.Sprintf("embedded file %q", f.Name)); err != nil {
			return nil, err
		}
		data, err := resolveFormatStringExpr(f.Data, syms, opts)
		if err != nil {
			return nil, fmt.Errorf("embedded file %q: %w", f.Name, err)
		}
		f.Data = data // f is a copy; the original element is untouched
		out[i] = f
	}
	return out, nil
}

// ResolveVarsExpr is ResolveVars' EXPR-aware counterpart: it returns a copy
// of vars with every value resolved via resolveFormatStringExpr. Keys are
// copied unchanged. A nil map returns (nil, nil).
//
// See [ResolveActionExpr] for syms/pathMap/budget. ResolveVarsExpr returns an
// error naming the offending variable key if any value is malformed,
// evaluates to an error, or names a symbol syms does not provide. The input
// map is never mutated.
//
// Keys are iterated in map order, which varies between calls. exprcheck.go's
// checker walk sorts, to give a template author reproducible error pointers;
// this function has no such position to keep stable. Which variable trips a
// nearly exhausted budget can vary run to run, but the assignment fails with
// a budget-exceeded error either way; only the key named in it changes.
func ResolveVarsExpr(
	vars map[string]string, syms expr.MapSymbols, pathMap []protocol.PathMapRule, budget *AssignmentBudget,
) (map[string]string, error) {
	if vars == nil {
		return nil, nil
	}
	b := budgetOrDefault(budget)
	opts := ExprEvalOptions(b.Limits(), pathMap)
	out := make(map[string]string, len(vars))
	for k, v := range vars {
		if err := b.ChargePositions(1, fmt.Sprintf("variable %q", k)); err != nil {
			return nil, err
		}
		r, err := resolveFormatStringExpr(v, syms, opts)
		if err != nil {
			return nil, fmt.Errorf("variable %q: %w", k, err)
		}
		out[k] = r
	}
	return out, nil
}
