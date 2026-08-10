// SPDX-License-Identifier: AGPL-3.0-or-later

package fmtres

// This file is the EXPR-aware counterpart of fmtres.go's ResolveAction,
// ResolveEmbeddedFiles and ResolveVars: it evaluates an OpenJD expression
// against Task 3's phase-3 symbol table (exprsyms.go's TaskSymbols/
// EnvSymbols) instead of doing plain "{{Name}}" substitution. This is the
// first point in the whole EXPR program where an expression's VALUE reaches
// a real command line -- every earlier evaluation (phases 1 and 2, and
// sub-project E3's let bindings) type-checked or bound a value and then
// discarded it.
//
// It sits BESIDE fmtstring.Resolve, not instead of it: a template that does
// not declare the EXPR extension must keep taking fmtstring.Resolve's exact
// code path, byte for byte -- the design spec's section 4 states this
// explicitly, mirroring the repo's own "auth-off path stays byte-for-byte
// unchanged" rule. The caller (EXPR sub-project E4a's Task 6, not yet built)
// picks between the two families using the assignment's own EXPR flag
// (protocol.AssignMsg.EXPR), the same flag Task 3's symbol-table builders
// already assume is true.
//
// # Lone versus embedded (section 1.3.2)
//
// internal/openjd's checkFormatString (exprcheck.go) is the shape this file
// follows, deliberately: phases 2 and 3 differing in STRUCTURE, not just in
// which symbols are concrete, is exactly what this sub-project exists to
// avoid, per the design spec's own "one-code-path claim" (section 1.2). Both
// functions use fmtstring.LoneRef to ask the same question first -- is this
// format string EXACTLY one reference with no surrounding text? -- and
// answer it the same way:
//
//   - A LONE reference inherits the field's own target type (TargetString
//     for a command, an embedded file's data, or a variable value;
//     TargetArgItem, below, for one args entry) and is evaluated ONCE
//     against it, so the implicit coercions section 1.2.3 defines (path ->
//     string, an int naturally read as the field's single scalar target,
//     and so on) apply exactly as they do at submission time.
//   - Anything else -- literal text, a reference embedded inside other
//     text, or more than one reference -- evaluates each reference
//     unconstrained (expr.TAny) and converts its NATURAL result to text via
//     Value.String(), which funcsconv.go's own string() row documents as
//     agreeing byte-for-byte with the language's string() function for
//     every case that has been checked. checkFormatString takes exactly
//     this same branch for the identical position and ignores its own
//     target parameter there, for the identical reason: the result is
//     converted to a string regardless of what the reference itself
//     produces, so a type that would fail against the field's real target
//     is still fine embedded in text ("Count: {{ len(myList) }}" -> "Count:
//     5", RFC 0005's own example).
//
// checkFormatString stops at "does this evaluate without error" and
// discards the result; this file's whole job is to keep the result and turn
// it into wire text (or, for an args entry, into zero, one, or several
// entries -- see TargetArgItem below), which is why it cannot simply call
// into that package (nor could it: internal/openjd pulls internal/store,
// which cmd/sqi-worker must never depend on -- Task 3's own note on
// exprsyms.go).
//
// One deliberate reading beyond what checkFormatString needs to decide,
// because rendering forces the question in a way type-checking never does:
// a NULL value embedded in surrounding text renders as the empty string,
// not the word "null" Value.String() would otherwise produce for a bare
// null. RFC 0005 states this directly for the format-string position ("A
// None/null result is treated as the empty string") and it has no
// type-checking-only analog for checkFormatString to have already decided.
//
// # TargetArgItem and list-item flattening
//
// exprcheck.go's TargetArgItem -- "a string is one argument, None drops the
// argument, and a list[string] flattens inline" (section 1.3.2's
// list-item rule) -- cannot be imported from here for the same
// internal/store layering reason as symbolsFor/checkFormatString
// themselves, so this file carries its OWN copy, built from the identical
// expr.UnionOf(expr.OptionalOf(expr.TString), expr.ListOf(expr.TString))
// expression. The two copies must stay identical; this is the same
// parallel-copy pattern jobParamTypes' own doc comment already establishes
// for internal/openjd/expr.JobParamTypes/TaskParamType (one shared
// definition, reached from two packages that cannot import each other).
// Unlike Command/embedded-file-data/variable-value (all TargetString, always
// exactly one resulting string), a LONE args-entry reference can change how
// many actual command-line arguments the entry produces -- this is the
// first place in sqi that renders that behavior for real, since
// checkFormatString only ever type-checked against TargetArgItem and threw
// the result away.
//
// # Metering (design spec section 4.1)
//
// Every evaluation in this file is metered by [ExprLimits.OperationLimit] and
// [ExprLimits.MemoryLimit] -- never expr.Eval's own unconfigured defaults,
// precisely so a caller cannot forget the budget the way an unmetered path
// would. Phase 3 is the MORE expensive phase (E2's Task 10 measured ~40x one
// template's cost here versus phase 1, because concrete strings and paths
// cost real bytes where a placeholder costs none), and the worker is a
// shared, long-lived process evaluating untrusted templates -- not a request
// handler that returns after one evaluation -- so an unmetered path here is a
// worse liability than the one E2's whole-branch review found and fixed
// server-side (a 183 KB template body reaching 6.9 GB).
//
// EXPR sub-project E4d's Task 2 turned the two numbers into operator
// configuration: they now arrive on the assignment's own [AssignmentBudget]
// (exprlimits.go), whose defaults are the constants this file used to
// hardcode. Every function below that evaluates anything takes that budget,
// so there is no way to reach the evaluator with some OTHER pair of numbers,
// and no way to reach it with none.

import (
	"fmt"
	"strings"

	"github.com/uberware/sqi/internal/openjd/expr"
	"github.com/uberware/sqi/internal/openjd/fmtstring"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// defaultWorkerOperationLimit and defaultWorkerMemoryLimit are the DEFAULTS
// for the section 1.3.10/1.3.9 budgets every phase-3 evaluation in this file
// is metered against ([ExprLimits.OperationLimit] and
// [ExprLimits.MemoryLimit], reached via ExprEvalOptions). Before E4d's Task 2
// they were the only possible values; an operator can now size them per host,
// within the ranges exprlimits.go documents.
//
// The two numbers are deliberately LOOSER than internal/openjd's
// defaultSubmissionOperations/defaultSubmissionMemoryBytes (10,000 ops /
// 1,000,000 bytes): that pair exists to keep template VALIDATION cheap on a
// synchronous HTTP request path evaluating mostly UNRESOLVED placeholders,
// where a template needing more than that budget just to type-check was
// already a submission-time liability. Phase 3 evaluates the SAME
// expressions against concrete, real-sized values -- an embedded file's
// actual body, a real session working directory -- so legitimate templates
// need materially more room here than they did to type-check; the
// requirement is that SOME budget applies, not that it is the tightest one in
// the codebase.
//
// AN EARLIER REVISION OF THIS COMMENT CLAIMED BOTH NUMBERS "sit comfortably
// UNDER internal/openjd/expr's own hard, non-configurable safety floors
// (limits.go's maxElements and maxStringBytes, both 10,000,000)". THAT IS
// FALSE FOR THE MEMORY LIMIT, which is 20,000,000 -- twice maxStringBytes --
// and the argument behind it was wrong twice over: the two bound different
// quantities (maxStringBytes caps one PRODUCED STRING, while section 1.3.9's
// meter sums LIVE VALUES and recurses into containers, so a list of two 9 MB
// strings holds ~18 MB with no fixed guard firing), and the operation limit
// is a count of operations, not of elements, so comparing it to maxElements
// compares nothing. The claim is withdrawn rather than repaired: neither
// number is derived from a fixed guard.
//
// NEITHER OF THESE IS AN AGGREGATE BOUND, and for the one construct that
// retains values across evaluations -- a let: block -- a per-Eval budget is
// not enough on its own: see exprsyms.go's defaultLetRetainedBytes, which
// bounds what ONE symbol table may hold live. Every other evaluation in this
// file renders its result to text and drops the Value, which is why the pair
// below suffices for them.
const (
	defaultWorkerOperationLimit int64 = 1_000_000
	defaultWorkerMemoryLimit    int64 = 20_000_000
)

// TargetArgItem is this package's own copy of exprcheck.go's identically
// named value -- see this file's own doc comment, above, for why a second
// copy exists and must be kept in step with the original.
var TargetArgItem = expr.UnionOf(expr.OptionalOf(expr.TString), expr.ListOf(expr.TString))

// ExprEvalOptions builds the []expr.Option every phase-3 evaluation in this
// package should be called with: the assignment's own metering (lim's
// OperationLimit/MemoryLimit -- a zero ExprLimits normalizes to the defaults
// above, so there is no unmetered spelling), the evaluation's path
// flavor (pathFlavor, exprsyms.go's expr.PathNative -- the worker runs ON
// the host a task executes on, so its own concrete Session.WorkingDirectory
// and Task.File.* values, and any path literal an expression writes, must
// agree on the SAME flavor; see exprsyms.go's own doc comment for why that
// is PathNative here and POSIX everywhere upstream of it), and the
// session's real apply_path_mapping rules translated from the assignment's
// wire format via ConvertPathMapRules.
//
// This is deliberately the ONE place all four settings are assembled: EXPR
// sub-project E4a's Task 3 recorded expr.WithPathFormat(expr.PathNative) as
// "a requirement for the wiring... wherever the options are assembled" --
// this function is that place, so a later caller (Task 6) cannot wire the
// resolvers in this file while forgetting it.
//
// ExprEvalOptions itself is a thin wrapper over exprEvalOptionsFor with
// pathFlavor (this package's fixed choice, exprsyms.go); exprEvalOptionsFor
// takes the flavor as a parameter purely so a white-box test
// (expres_internal_test.go) can force a flavor other than whatever this
// host's runtime.GOOS-driven pathFlavor.resolve() happens to produce, and
// so prove expr.WithPathFormat is genuinely wired rather than merely
// present in source with no observable effect -- see that test's own doc
// comment for why a POSIX-only CI host cannot otherwise distinguish the
// option being set from the option having been deleted.
func ExprEvalOptions(lim ExprLimits, pathMap []protocol.PathMapRule) []expr.Option {
	return exprEvalOptionsFor(lim, pathMap, pathFlavor)
}

// exprEvalOptionsFor is ExprEvalOptions' flavor-parameterized implementation
// -- see that function's doc comment for why the split exists.
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
// The conversion is a clean, field-for-field mapping: SourcePath and
// DestinationPath copy verbatim, and SourcePathFormat's three wire spellings
// ("POSIX", "WINDOWS", "URI" -- protocol.PathMapRule's own doc comment)
// correspond one to one with expr.PathMapSourceFormat's three constants.
// convertPathMapSourceFormat's default case (an unrecognized spelling) is
// defensive only, exactly like EmbeddedFileName's own filename checks in
// fmtres.go: the assignment is server-signed, so a wire value other than
// the three the server ever writes is not expected in production, and
// falling back to PathMapPOSIX (the format that requires nothing of the
// text to already be structured a particular way) is the least surprising
// choice for a value this function cannot recognize.
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
// PATH-declared Param.<name>/Task.Param.<name>: it runs raw -- the
// UNTOUCHED submitted parameter text, never a value already rendered in
// some path flavor -- through the assignment's real path-mapping rules and
// returns the mapped, concrete path.Value section 1.2.2 requires
// Param.<name> to carry.
//
// This calls THROUGH the registered apply_path_mapping function
// (expr.Eval), not this package's own unexported mapPath, per
// pathmapping.go's own doc comment: "What makes 'the result is a path in
// dst' true is the WRAPPER, pathMappingFuncs' apply_path_mapping ...
// Callers wanting a path value must do the same rather than assume it."
// Reusing the wrapper is also what keeps this from becoming a second,
// independent implementation of the engine sub-project D already built --
// exactly the "two formulas that happen to agree today" defect class this
// codebase's own conventions call out repeatedly.
//
// raw is bound as a plain string SYMBOL rather than interpolated into the
// expression source, so arbitrary path text -- quotes, backslashes,
// non-ASCII -- can never be misparsed as expression syntax. ExprEvalOptions
// is reused for the options (metering, expr.PathNative, the converted
// rules), the SAME options every other phase-3 evaluation in this package
// gets -- this is exactly the "one place all four settings are assembled"
// ExprEvalOptions' own doc comment already claims, now also true for
// symbol construction, not just rendering.
func mapPathParamValue(raw string, lim ExprLimits, pathMap []protocol.PathMapRule) (expr.Value, error) {
	syms := expr.MapSymbols{"src": expr.String(raw)}
	v, err := expr.Eval("apply_path_mapping(src)", syms, expr.TPath, ExprEvalOptions(lim, pathMap)...)
	if err != nil {
		return expr.Value{}, fmt.Errorf("mapping %q: %w", raw, err)
	}
	return v, nil
}

// errUnresolvedValue reports that a phase-3 evaluation produced a
// still-unresolved placeholder instead of a concrete value.
//
// This should be unreachable in the sense exprsyms.go's own doc comment
// promises -- "every symbol this file binds is CONCRETE" -- with exactly
// ONE documented exception, the same one that comment calls out: a
// declared value whose submitted text failed to parse as its declared type
// (expr.ValueFromText's fallback -- an invalid CHUNK[INT] range is the
// concrete case exprsyms_test.go pins) stays Unresolved rather than
// panicking there, so an expression that reads it can carry an unresolved
// value all the way to coerceTop successfully: coerce()'s own
// coerceUnresolved narrows an unresolved value's CONSTRAINT and hands back
// another unresolved value, it does not manufacture a concrete one, so
// Eval(target=TString) or Eval(target=TargetArgItem) can return a
// STILL-unresolved Value with no error at all.
//
// Every call site in this file that reaches a Value payload accessor
// (AsStr, AsList) checks IsUnresolved first and calls this rather than
// touching the payload: the accessors panic on the wrong code (by design --
// value.go's own mustBe doc comment: "a silent zero would flow into a
// rendered command line unnoticed"), and Value.String()'s own rendering of
// an unresolved value ("<unresolved[T]>", a DIAGNOSTIC form) must not leak
// into a real command line either. Both failure modes are worse than the
// clear, positioned error this returns.
func errUnresolvedValue(v expr.Value) error {
	return fmt.Errorf(
		"value is unresolved (%s): the submitted text could not be parsed as its declared type", v.Type,
	)
}

// resolveFormatStringExpr resolves one format-string field -- a command, an
// embedded file's data, or a variable value -- against syms, implementing
// section 1.3.2 exactly as this file's own doc comment describes: a LONE
// reference evaluates once against expr.TString and its coerced payload IS
// the result; anything else walks fmtstring.Segments, evaluating each
// reference against expr.TAny and rendering it with Value.String() (a null
// result rendering as the empty string, per RFC 0005), concatenated with the
// literal text around it.
//
// The returned error carries no field context (no "command" or "variable
// X" prefix) -- that is the caller's job, exactly as it is in fmtres.go's
// ResolveAction/ResolveEmbeddedFiles/ResolveVars, so one field's error names
// itself only once rather than once per layer.
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

// evalEmbeddedRef evaluates one EMBEDDED reference (expr.TAny, converted to
// text) -- the half of section 1.3.2's rule shared, unchanged, by every
// caller in this file: resolveFormatStringExpr's own segment loop and
// resolveArgItemExpr's non-lone branch both need exactly this, so it is
// factored out rather than duplicated.
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
		// None/null result is treated as the empty string" -- unlike
		// Value.String()'s own "null" rendering, which is a DIAGNOSTIC form
		// (that method's own doc comment) never meant for this position.
		return "", nil
	}
	return v.String(), nil
}

// resolveArgItemExpr resolves one Args entry, implementing section 1.3.2's
// list-item rule via TargetArgItem: a LONE reference evaluates once against
// it, and its result decides how many actual arguments this ONE template
// entry becomes -- null drops it (zero arguments), a list[string] flattens
// inline (one argument per element), anything else is exactly one argument.
// Anything other than a lone reference -- literal text, an embedded
// reference, several references -- resolves through the exact same path as
// every other format-string field (resolveFormatStringExpr) and is always
// exactly one argument, matching section 1.3.2's "a string is one argument"
// baseline case.
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
// so the caller's own "is this a lone reference" check does not nest a
// second decision tree inside it (golangci-lint's nestif).
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
// comment for why one entry may become zero, one, or several actual
// arguments). All other fields (TimeoutSeconds, Cancelation) are copied
// unchanged. A nil action returns (nil, nil).
//
// syms is the phase-3 symbol table (TaskSymbols or EnvSymbols); pathMap is
// the assignment's own PathMap field, forwarded to ExprEvalOptions to build
// this call's options -- there is deliberately NO way to call this function
// without metering, the worker's native path flavor, and the session's real
// path-mapping rules all applying: FIX ROUND 1 (Task 4 review) replaced a
// variadic opts ...expr.Option parameter with this one, because a variadic
// tail compiles fine when the caller passes NOTHING, silently handing every
// evaluation expr.Eval's own unconfigured defaults (10x the operations, 5x
// the memory this package's own named limits choose) with no compiler error,
// no lint, and no test failure to catch it. Taking the raw ingredient
// (pathMap) rather than a pre-built []expr.Option closes that gap
// completely, not merely narrows it: there is no longer an options value a
// caller could construct, empty or otherwise, that skips metering.
//
// ResolveActionExpr returns a descriptive error -- naming the offending
// field -- if any reference is malformed, evaluates to an error, or names a
// symbol syms does not provide. The input action is never mutated.
//
// budget is EXPR sub-project E4c's Task 4 addition: charges one position for
// the command and one for each Args entry, BEFORE resolving it, so a budget
// already exhausted by an earlier action/environment in this same assignment
// stops the walk here without doing that entry's own evaluation work. See
// [AssignmentBudget]'s own doc comment for the full account; omitting it
// (every pre-Task-4 call site) charges a fresh, throwaway budget no single
// call can exhaust.
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
// resolveFormatStringExpr (TargetString -- an embedded file's body is
// always exactly one string, never subject to TargetArgItem's list-item
// rule). All other fields (Name, Filename, Runnable, EndOfLine) are copied
// unchanged. A nil slice returns (nil, nil).
//
// See [ResolveActionExpr] for syms/pathMap/budget. ResolveEmbeddedFilesExpr
// returns a descriptive error -- naming the offending file -- when a file's
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
// See [ResolveActionExpr] for syms/pathMap/budget. ResolveVarsExpr returns a
// descriptive error -- naming the offending variable key -- if any value is
// malformed, evaluates to an error, or names a symbol syms does not
// provide. The input map is never mutated.
//
// Keys are iterated in MAP ORDER, which is not deterministic across calls --
// unlike exprcheck.go's checker walk, which sorts (checkEnvironmentExpressions'
// own reason: reproducible ERROR pointers for a template author). This
// function has no author-facing position to keep stable; which variable
// happens to trip an already-near-exhausted budget varying run to run does
// not change the OUTCOME (the assignment still fails, with a budget-exceeded
// error either way), only which key's name appears in it.
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
