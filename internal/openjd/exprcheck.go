// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import (
	"fmt"
	"maps"
	"slices"
	"strconv"

	"github.com/uberware/sqi/internal/openjd/expr"
	"github.com/uberware/sqi/internal/openjd/fmtstring"
)

// Target types for checkFormatString, section 1.3.2. Declared once as
// package-level values so call sites read as "the name field" rather than
// reconstructing the type at every call.
var (
	// TargetString is the target for a name, command, variable value or
	// embedded-file body -- any plain string-typed field.
	TargetString = expr.TString
	// TargetInt is the target for a timeout.
	TargetInt = expr.TInt
	// TargetArgItem is the target for a single entry of an args list, per
	// section 1.3.2's list-item rule: a string is one argument, None drops the
	// argument, and a list[string] flattens inline. UnionOf/OptionalOf's
	// normalization collapses OptionalOf(TString)'s own union with the
	// list[string] member into one flat three-member union rather than a
	// union nested inside a union.
	TargetArgItem = expr.UnionOf(expr.OptionalOf(expr.TString), expr.ListOf(expr.TString))
)

// symbolsFor builds the typed EXPR symbol table for a template position,
// gated by scope: only the fixed symbols and symbol families the scope
// exposes (Scope, scopeFixed, scopeFamilies -- scope.go) are bound.
//
// Every symbol is bound as an unresolved placeholder of its declared type --
// the specification's static-type-checking model, where type checking and
// evaluation are the same code path, differing only in which symbols are
// concrete. That is "phase 1". "Phase 2" is the only difference: where params
// holds a value for a job parameter, symbolsFor binds the concrete value
// instead of the placeholder, so the SAME expression, evaluated against the
// SAME evaluator, either type-checks against placeholders or runs for real
// against submitted values.
//
// step may be nil for a job-level position; env may be nil where the position
// is not inside any environment's script; tmpl may carry no parameters;
// params may be nil for phase 1 (or when no concrete values are available
// yet).
//
// env identifies WHICH environment's embedded files back Env.File. -- a
// template can declare many job and step environments, each with its own
// files, and Env.File.X means a different file depending on which one an
// expression sits inside. That is not reachable from tmpl/step alone (a
// StepTemplate holds a slice of StepEnvironments, not "the one this position
// is inside"), so the caller -- which walks the template and knows exactly
// which environment it is currently inside -- must say so explicitly. A nil
// env binds no Env.File symbols, the same way a nil step binds no Task.File
// or Task.Param symbols.
//
// The type-mapping rules (PATH, LIST[PATH], CHUNK[INT], and the ParseType
// floor) are copied from test/conformance/exprcase.go's DeclaredSymbols, which
// implements the same section 1.2.2 tables for the conformance harness. The
// two copies are deliberately parallel: DeclaredSymbols works from an
// unparsed YAML document and is scope-blind (it binds every family
// regardless of position), while this one works from the parsed model and is
// scope-aware. They stay separate until sub-project H deletes the harness
// copy, at which point DeclaredSymbols is retired in favor of this path.
func symbolsFor(
	tmpl *JobTemplate, step *StepTemplate, env *Environment, scope Scope, params map[string]string,
) expr.MapSymbols {
	syms := expr.MapSymbols{}

	for _, sym := range scopeFixed(scope) {
		syms[sym.Name] = expr.Unresolved(sym.Type)
	}

	var boundJobParams, boundTaskParams bool
	for _, fam := range scopeFamilies(scope) {
		switch fam.Prefix {
		case "Param.", "RawParam.":
			if !boundJobParams {
				bindJobParamSymbols(tmpl, params, syms)
				boundJobParams = true
			}
		case "Task.Param.", "Task.RawParam.":
			if !boundTaskParams {
				bindTaskParamSymbols(step, syms)
				boundTaskParams = true
			}
		case "Task.File.":
			if step != nil && step.Script != nil {
				bindEmbeddedFileSymbols(step.Script.EmbeddedFiles, fam.Prefix, syms)
			}
		case "Env.File.":
			if env != nil && env.Script != nil {
				bindEmbeddedFileSymbols(env.Script.EmbeddedFiles, fam.Prefix, syms)
			}
		}
	}

	return syms
}

// SymbolsFor is the exported counterpart of symbolsFor, for the one caller
// outside this package that needs to drive the SAME phase-1/phase-2
// symbol-table construction this package uses internally:
// internal/worker/fmtres's phase-3 "agreement" test (EXPR sub-project E4a),
// which proves the worker's own phase-3 table types a template's declared
// parameters identically to this package's phase-2 table -- the design
// spec's own claim that phases 1-3 are "the same walk with a different
// table" gets its sharpest test here, and that test needs the REAL
// phase-2 table, not a hand-reconstructed approximation of it.
//
// This is a direct, unwrapped call -- see symbolsFor's own doc comment for
// the full contract (what step/env/params being nil means, and the scope
// gating).
func SymbolsFor(
	tmpl *JobTemplate, step *StepTemplate, env *Environment, scope Scope, params map[string]string,
) expr.MapSymbols {
	return symbolsFor(tmpl, step, env, scope, params)
}

// bindJobParamSymbols binds Param.<name> and RawParam.<name> for every job
// parameter tmpl declares. Where params supplies a value for that name, the
// symbol is bound to the concrete value (phase 2) rather than a placeholder
// (phase 1) -- the only difference between the two evaluation phases.
func bindJobParamSymbols(tmpl *JobTemplate, params map[string]string, syms expr.MapSymbols) {
	if tmpl == nil {
		return
	}
	for _, def := range tmpl.ParameterDefinitions {
		if def.Name == "" {
			continue
		}
		paramType, rawType := jobParamTypes(string(def.Type))
		if raw, ok := params[def.Name]; ok {
			syms["Param."+def.Name] = concreteJobParamValue(paramType, raw)
			syms["RawParam."+def.Name] = concreteJobParamValue(rawType, raw)
			continue
		}
		syms["Param."+def.Name] = expr.Unresolved(paramType)
		syms["RawParam."+def.Name] = expr.Unresolved(rawType)
	}
}

// jobParamTypes maps a declared job-parameter type to the expression types of
// Param.<name> and RawParam.<name>, per section 1.2.2's job-parameter table.
//
// This is a thin wrapper over expr.JobParamTypes, the shared definition of
// the mapping -- see that function's doc comment for the full rationale
// (including why RawParam is NOT uniformly string) and for why the mapping
// lives in internal/openjd/expr rather than here: EXPR sub-project E4a's
// worker-side phase-3 symbol table (internal/worker/fmtres/exprsyms.go)
// needs the identical mapping and cannot import this package (internal/
// openjd pulls in internal/store, which the worker binary must never depend
// on), so a single copy in the one package both sides already share is what
// keeps phase 2 and phase 3 from typing the same declared parameter two
// different ways.
//
// (test/conformance/exprcase.go keeps its OWN separate copy, deliberately --
// see symbolsFor's doc comment for why that third copy is not folded in
// here too.)
func jobParamTypes(declared string) (paramType, rawType expr.Type) {
	return expr.JobParamTypes(declared)
}

// concreteJobParamValue converts a submitted job-parameter value into a Value
// of type t. Thin wrapper over expr.ValueFromText -- see that function's doc
// comment for the two distinct situations where it returns Unresolved(t)
// (parse failure vs. by-construction) and for the section 1.3.4 float-text
// rationale. Symbolic drift between this package's phase 2 and the worker's
// phase 3 is exactly what sharing the one definition in internal/openjd/expr
// prevents; see jobParamTypes' doc comment just above for the fuller
// argument.
//
// PathPOSIX is hardcoded here rather than threading a PathFormat from the
// caller. That is harmless today -- nothing calls symbolsFor with a non-
// default path format yet -- but a future caller that also sets
// expr.WithPathFormat(expr.PathWindows) for the same evaluation would get a
// mismatched flavor for a concrete Param.<path> value: this Value and the
// evaluator's own path literals would disagree. Left as a known gap rather
// than fixed now, since symbolsFor has no PathFormat input to thread through
// yet and inventing one without a caller to drive it would be speculative.
func concreteJobParamValue(t expr.Type, raw string) expr.Value {
	return expr.ValueFromText(t, raw, expr.PathPOSIX)
}

// bindTaskParamSymbols binds Task.Param.<name> and Task.RawParam.<name> for
// every task parameter step's parameter space declares. Task parameters are
// per-task, not per-template, so symbolsFor never has a concrete value for
// them -- every entry is an unresolved placeholder regardless of phase.
func bindTaskParamSymbols(step *StepTemplate, syms expr.MapSymbols) {
	if step == nil || step.ParameterSpace == nil {
		return
	}
	for _, def := range step.ParameterSpace.TaskParameterDefinitions {
		if def.Name == "" {
			continue
		}
		t := taskParamType(def.Type)
		syms["Task.Param."+def.Name] = expr.Unresolved(t)
		syms["Task.RawParam."+def.Name] = expr.Unresolved(t)
	}
}

// taskParamType maps a declared task-parameter type per section 1.2.2's task
// table. Thin wrapper over expr.TaskParamType -- see jobParamTypes' doc
// comment just above for why the mapping lives there rather than here.
func taskParamType(declared TaskParamType) expr.Type {
	return expr.TaskParamType(string(declared))
}

// bindEmbeddedFileSymbols binds prefix+<name> as an unresolved path for every
// embedded file in files.
//
// The caller picks which files: Task.File. is bound from a step's OWN script
// (step.Script.EmbeddedFiles), Env.File. from the specific environment's
// script the caller identifies (env.Script.EmbeddedFiles) -- they are
// DIFFERENT sources, unlike DeclaredSymbols' scope-blind stepSymbols, which
// binds both families from a step's script alone because it has no separate
// environment context to draw on. Conflating the two here previously bound
// Env.File.<name> from a step's task-script files rather than the
// environment's own -- fixed by requiring the caller to pass the right slice.
func bindEmbeddedFileSymbols(files []EmbeddedFile, prefix string, syms expr.MapSymbols) {
	for _, f := range files {
		if f.Name == "" {
			continue
		}
		syms[prefix+f.Name] = expr.Unresolved(expr.TPath)
	}
}

// hostOnlyFunctions is the set of function names restricted to a host-context
// scope (SESSION and TASK) because they need runtime resources that do not
// exist at submission time. apply_path_mapping needs the session's
// path-mapping rules, which are established only once a session is running;
// internal/openjd/expr registers it FLAT, with no scope model of its own, so
// this set is the only thing enforcing the restriction.
//
// The restriction itself is the SPECIFICATION's: the wiki's FunctionLibrary
// entry for with_host_context() is a library "with host-only functions like
// apply_path_mapping() enabled"
// (third_party/openjd-specifications/wiki/2026-02-Expression-Language.md:514).
//
// That "like" is the whole reason this is a SET rather than a name
// comparison, and the specification is the only source needed for it. RFC
// 0005's "Host-Context Function Availability" section
// (third_party/openjd-specifications/rfcs/0005-expression-language.md:1022)
// says the same thing at more length -- "Certain functions ... For example,
// apply_path_mapping()" -- but that section exists ONLY in the RFC, which is
// the proposal behind the specification and not the specification itself; do
// not cite it as normative. apply_path_mapping is the only entry today
// because it is the only function that reads session state, and a second
// entrant costs one map entry rather than a rewritten conditional.
var hostOnlyFunctions = map[string]struct{}{
	"apply_path_mapping": {},
}

// checkHostOnlyFunctions rejects e when scope is not a host context and e
// calls a member of hostOnlyFunctions, per Scope.IsHostContext. It is called
// after a successful expr.Parse and BEFORE Eval: the call must not run at
// all in a scope where the state it needs does not exist, not merely fail
// once it tries to read that state.
//
// It walks Expression.CalledFunctions() rather than inspecting the parse tree
// directly -- CalledFunctions already collects every function and method name
// the expression calls, sorted and de-duplicated, including names reached
// through nested calls and comprehension bodies (its own narrow
// loop-variable exclusion mirrors Names' and does not weaken this check: a
// direct call to a comprehension's own loop variable is excluded because
// that call cannot possibly reach a REGISTRY function of the same name, host-
// only or not -- see its doc comment and TestCalledFunctions).
func checkHostOnlyFunctions(e *expr.Expression, scope Scope, ptr string) ValidationErrors {
	if scope.IsHostContext() {
		return nil
	}
	var errs ValidationErrors
	for _, name := range e.CalledFunctions() {
		if _, ok := hostOnlyFunctions[name]; !ok {
			continue
		}
		errs = append(errs, ValidationError{
			Pointer: ptr,
			Message: fmt.Sprintf(
				"%s() is only available in a host-context scope (SESSION or TASK), not %s",
				name, scope,
			),
		})
	}
	return errs
}

// checkFormatString parses and evaluates every EXPR reference in s against
// syms, at the position ptr, reporting a ValidationError for every parse or
// type error the evaluator finds.
//
// Section 1.3.2's target-type rule decides what a reference is checked
// against: a format string that is EXACTLY one reference with no surrounding
// text (fmtstring.LoneRef) is transparent and inherits target -- the field's
// own declared type. Anything else -- a literal, an embedded reference, or a
// reference alongside other text -- evaluates each reference unconstrained
// (expr.TAny) because the result is converted to a string regardless of what
// the reference itself produces, so a type that would be wrong for target is
// still fine here.
//
// Each reference is parsed on its own (expr.Parse) rather than evaluated in
// one step (expr.Eval) so that checkHostOnlyFunctions can run BETWEEN parse
// and eval: a host-only call is rejected before it is ever executed, in a
// scope where the state it would read does not exist.
//
// opts is forwarded verbatim to every Eval call. It is how a caller supplies
// section 1.3.9/1.3.10 limits (expr.WithMemoryLimit, expr.WithOperationLimit);
// checkTemplateExpressions's callers pass submissionLimits (below) --
// deliberately tighter than expr.Eval's own execution-time defaults, because
// this function runs at TEMPLATE VALIDATION time, reachable synchronously from
// POST /api/v1/jobs once the EXPR extension is registered. Tests that call
// checkFormatString directly pass no opts, which is fine: the package
// defaults apply, and no committed test's expression does enough real work to
// approach even submissionLimits' much tighter budget.
func checkFormatString(
	s, ptr string, scope Scope, syms expr.MapSymbols, target expr.Type, opts ...expr.Option,
) ValidationErrors {
	if body, ok := fmtstring.LoneRef(s); ok {
		e, err := expr.Parse(body)
		if err != nil {
			return ValidationErrors{{Pointer: ptr, Message: err.Error()}}
		}
		if errs := checkHostOnlyFunctions(e, scope, ptr); len(errs) != 0 {
			return errs
		}
		if _, err := e.Eval(syms, target, opts...); err != nil {
			return ValidationErrors{{Pointer: ptr, Message: err.Error()}}
		}
		return nil
	}

	segs, err := fmtstring.Segments(s)
	if err != nil {
		return ValidationErrors{{Pointer: ptr, Message: err.Error()}}
	}

	var errs ValidationErrors
	for _, seg := range segs {
		if !seg.IsRef {
			continue
		}
		e, err := expr.Parse(seg.Ref)
		if err != nil {
			errs = append(errs, ValidationError{Pointer: ptr, Message: err.Error()})
			continue
		}
		if hostErrs := checkHostOnlyFunctions(e, scope, ptr); len(hostErrs) != 0 {
			errs = append(errs, hostErrs...)
			continue
		}
		if _, err := e.Eval(syms, expr.TAny, opts...); err != nil {
			errs = append(errs, ValidationError{Pointer: ptr, Message: err.Error()})
		}
	}
	return errs
}

// checkLetBindings evaluates a let: block's bindings in declaration order,
// inserting each one's value into syms so the next binding can reference it,
// and returns one error per binding that fails.
//
// It MUTATES syms. That is the whole mechanism: section 3.6 says "Later
// bindings can reference names from earlier bindings in the same let block",
// and because E2's symbol table holds VALUES rather than types, inserting the
// evaluated result gives section 3.6.1's "the type of the binding is the
// natural result type of the expression" and phase-2 concreteness at once,
// through the same code path E2 already uses for every other position.
//
// A failed binding is NOT inserted, so a later position reports an unknown name
// rather than a second copy of the same fault. Evaluation continues to the next
// binding either way -- one malformed line should not hide the rest of the
// block.
//
// Self-reference needs no check: "x = x + 1" fails because x is inserted only
// after its own expression evaluates.
//
// Section 3.6 also forbids a binding that "shadows a previous binding in the
// same let block or any enclosing scope". That reads as two rules but is
// implemented as one: by the time a nested block's bindings are checked, the
// enclosing scope's names are already present in syms -- exactly because of
// the mutate-in-place mechanism above -- so "a name already present in syms
// may not be bound" covers both cases through a single lookup. The check is
// keyed off syms itself, not off some parallel list of names this call has
// seen, so it equally rejects shadowing a spec-defined symbol (Param, Task,
// Session, Env, RawParam) if one were ever present in syms under a name a
// let binding could produce -- unreachable today only because 3.6.1's
// lowercase-first <UserIdentifier> rule keeps those two namespaces disjoint,
// not because this check assumes it.
//
// # The maxLetBindings guard
//
// Evaluation STOPS after [maxLetBindings] bindings. That is a
// resource-exhaustion guard, not a diagnostic: [validateLetElementCounts] has
// already REPORTED the over-count (it runs unconditionally, before the walk,
// and is not gated by EnforceLimits -- see its comment), so a second error
// here would only duplicate it at a different pointer.
//
// The guard lives here rather than as a short-circuit in ValidateWithOptions
// because this function is the only place the cost is actually incurred, so
// bounding it here is independent of upstream ordering, of EnforceLimits, and
// of exprExpressionWalkEnabled's status gate -- and it also covers the unit
// tests that call this leaf directly.
//
// An earlier revision of this comment justified the placement by claiming
// phase 2 (checkExpressionsAtSubmit, submit.go) reaches checkTemplateExpressions
// without going through validateLetElementCounts. That is FALSE and was
// retracted: prepareTemplate runs ValidateWithOptions first and returns on any
// error, and validateLetElementCounts runs unconditionally within it, so an
// over-cap block aborts before phase 2 is ever reached. A short-circuit
// upstream would have bounded both production phases too. The placement is
// still the better one, for the independence reasons above -- not for a
// reachability reason that does not exist.
//
// let is the first construct in this checker that RETAINS values across
// evaluations. Every other position goes through checkFormatString, which
// discards each result -- which is why a per-Eval budget (submissionLimits,
// below) was sufficient there. Here, syms[name] = v accumulates one live value
// per binding in a table no per-Eval limit measures: each binding may
// legitimately allocate just under submissionMemoryLimit and KEEP it, so N
// bindings cost N budgets of live memory with nothing counting the sum.
// Measured through the real Parse + ValidateWithOptions path before this
// guard existed: 2,000 bindings of `a<i> = "x" * 900000` -- a 57 KB template
// body -- allocated 1,725 MB in 335 ms and still returned the same 2 errors,
// because sqi had already declared the block invalid and then evaluated every
// binding anyway. At the 4 MiB request-body cap (internal/api/jobs.go) that
// extrapolates to ~190,000 bindings and an out-of-memory kill. A
// template-wide OPERATION budget (sub-project E4) would not catch it: the
// construction spends almost no operations.
//
// Truncating rather than abandoning the whole walk is also the friendlier
// answer: a template with 51 bindings still gets every OTHER expression error
// in the template reported in the same pass, alongside the count error, so
// one over-long let: block does not hide the rest of the template's faults.
//
// opts is forwarded verbatim to every Eval call, exactly as
// [checkFormatString]'s identically-named tail is: it is how a caller supplies
// section 1.3.9/1.3.10 limits, and all three walk call sites pass
// submissionLimits(). checkLetBindings used to hardcode submissionLimits()
// internally, which was behaviorally identical but a trap for sub-project E4,
// which owns the specification's CONFIGURABLE limits: threading an
// operator-supplied budget through checkTemplateExpressions reaches every
// format-string position via checkFormatString and would have silently missed
// every let binding -- the one position where budgets ACCUMULATE. One opts
// tail per evaluator, so E4 has one place to thread rather than two to
// remember.
func checkLetBindings(
	lets []string, base string, scope Scope, syms expr.MapSymbols, opts ...expr.Option,
) ValidationErrors {
	if len(lets) > maxLetBindings {
		lets = lets[:maxLetBindings]
	}

	var errs ValidationErrors
	for i, raw := range lets {
		ptr := fmt.Sprintf("%s/%d", base, i)

		name, src, err := parseLetBinding(raw)
		if err != nil {
			errs = append(errs, ValidationError{Pointer: ptr, Message: err.Error()})
			continue
		}

		if _, taken := syms[name]; taken {
			errs = append(errs, ValidationError{
				Pointer: ptr,
				Message: fmt.Sprintf(
					"let binding %q shadows a name already in scope; section 3.6 forbids shadowing "+
						"an earlier binding in the same block or in any enclosing scope", name,
				),
			})
			continue
		}

		e, err := expr.Parse(src)
		if err != nil {
			errs = append(errs, ValidationError{Pointer: ptr, Message: err.Error()})
			continue
		}
		if hostErrs := checkHostOnlyFunctions(e, scope, ptr); len(hostErrs) != 0 {
			errs = append(errs, hostErrs...)
			continue
		}

		v, err := e.Eval(syms, expr.TAny, opts...)
		if err != nil {
			errs = append(errs, ValidationError{Pointer: ptr, Message: err.Error()})
			continue
		}
		syms[name] = v
	}
	return errs
}

// ─── submission-time limits ─────────────────────────────────────────────────

// submissionOperationLimit and submissionMemoryLimit bound every submission-time
// evaluation (section 1.3.9/1.3.10) -- checkFormatString's and
// checkLetBindings' alike -- tighter than expr.Eval's own execution-time
// defaults
// (10,000,000 operations / 100,000,000 bytes).
//
// Why tighter: checkTemplateExpressions runs at TEMPLATE VALIDATION time --
// reachable synchronously from POST /api/v1/jobs the moment the EXPR
// extension is registered (sub-project H) -- not at task execution time on a
// worker, where a multi-second evaluation is merely one task among many. The
// specification's own operation-count budget does not bound WALL CLOCK: rule
// 3 prices string work at ceil(len/256), so a single 10,000-character
// .upper() call costs about 41 operations -- cheap enough that a loop can
// perform a quarter of a million such calls before the DEFAULT 10,000,000-op
// budget trips, while the underlying byte-processing work is genuinely
// expensive. Confirmed directly: EXPR/job_templates/expr1.3.10--string-
// operation-limit-exceeded.invalid.yaml (a 117,700-iteration comprehension,
// each iteration building and upper-casing a 10,000-character string) took
// 15.6s to evaluate at the default limits -- correctly rejected, but
// catastrophically slow for a synchronous request path.
//
// Why THESE numbers, not tighter or looser -- both picked to avoid the
// opposite failure, a false rejection of a legitimate template that would
// have worked fine at run time:
//
//   - Phase 1 (checkTemplateExpressions with params == nil, called from
//     ValidateWithOptions) binds every Param./RawParam./Task.Param. symbol as
//     Unresolved -- an opaque placeholder that carries no data to process, so
//     referencing it costs a handful of operations regardless of what the
//     SUBMITTED value will eventually be. The only way an evaluation can
//     accumulate real cost at THIS phase is literal computation baked
//     directly into the template text -- a large string literal, a large
//     literal list, or a loop over one -- which is exactly the class of
//     construct sections 1.3.9/1.3.10 exist to bound, not something a
//     legitimate template needs at a submission-time position (a job name, a
//     host requirement value, a command).
//   - Phase 2 (checkExpressionsAtSubmit, submit.go, sub-project E2's Task 10,
//     called after job parameters are bound) re-runs the SAME walk with
//     Param./RawParam. symbols now bound to their concrete submitted values --
//     so referencing one is NO LONGER free: a large submitted value costs
//     real bytes and real per-character operations, same as a literal would.
//     Measured directly: `[Param.S.upper() for i in range(10)]` with a
//     900,000-byte Param.S costs ~520us at phase 1 (nothing to touch, params
//     is nil) versus ~23ms at phase 2 (roughly 40x) -- and correctly trips
//     submissionMemoryLimit before completing ("1800128 bytes of live values
//     exceeds the limit of 1000000"), because the doubled live string (the
//     bound value plus its .upper() copy) alone exceeds the 1MB budget. A
//     submitted parameter value large enough to threaten the budget is
//     bounded by the SAME fixed limits below, not a separate analysis --
//     symmetric with the literal-computation case Phase 1 bounds, just
//     reached through a submitted value instead of template text. Phase 2 is
//     therefore the phase that can be genuinely EXPENSIVE per evaluation, not
//     Phase 1 -- keep that in mind before assuming Phase 1's "cheap
//     placeholder" reasoning extends to it.
//   - submissionOperationLimit (10,000) leaves roughly two orders of
//     magnitude of headroom over any realistic submission-time expression --
//     a handful of Param references, arithmetic, string formatting, or a
//     comprehension over a small literal list (tens of elements) all land in
//     the tens-to-low-hundreds of operations -- while bounding the
//     pathological case above to ~119 loop iterations before it trips
//     (10,000 / 84 operations-per-iteration), well under 20ms measured
//     directly (see the report for Task 9's timings).
//   - submissionMemoryLimit (1,000,000 bytes = 1MB) is similarly generous for
//     real template text (a job name, command, or args entry is realistically
//     well under a few KB) while bounding any single large literal
//     allocation far below limits.go's fixed, non-configurable maxStringBytes
//     floor (10,000,000 bytes) -- so a large literal is caught by THIS limit
//     first, before it can allocate anywhere close to that floor. The same
//     budget also catches a large CONCRETE parameter value at phase 2, per
//     the measurement above -- one limit, enforced identically at both
//     phases because both go through the same checkFormatString/submissionLimits
//     call.
//
// A template whose expressions need more than this to type-check against
// UNRESOLVED placeholders (phase 1) or to evaluate against submitted values
// (phase 2) was already relying on computation heavy enough to be a
// submission-time liability; rejecting it here is the intended outcome, not
// a false positive.
const (
	submissionOperationLimit int64 = 10_000
	submissionMemoryLimit    int64 = 1_000_000
)

// submissionLimits builds the expr.Option slice every checkTemplateExpressions
// call site passes to checkFormatString and to checkLetBindings, through the
// identical opts tail both take. A function (not a package-level slice) so
// each call gets its own slice header -- cheap, and avoids any question of the
// underlying array being mutated by a caller.
func submissionLimits() []expr.Option {
	return []expr.Option{
		expr.WithOperationLimit(submissionOperationLimit),
		expr.WithMemoryLimit(submissionMemoryLimit),
	}
}

// ─── template-wide budget ───────────────────────────────────────────────────

// maxTemplateExprPositions and maxTemplateExprRetainedBytes are the two
// dimensions of the per-CALL budget checkTemplateExpressions enforces across
// its own walk -- design spec §3, §3.1 ("EXPR E4c -- The template-wide
// cumulative budget"). Both are cumulative across the WHOLE walk, not
// per-position: submissionOperationLimit/submissionMemoryLimit (above) bound
// what ONE expression may cost; these two bound what the ENTIRE template may
// cost, closing the gap three separate sub-projects (E2, E3, E4b) each found
// independently and each fixed only locally -- see the design spec's §1.1
// table.
//
// A THIRD dimension, operations, is deliberately NOT tracked here. With a
// position cap of maxTemplateExprPositions and the existing per-position
// operation cap (submissionOperationLimit, above), the cumulative operation
// ceiling is bounded at maxTemplateExprPositions * submissionOperationLimit
// (10,000 x 10,000 = 10^8) -- no operation counter needs to cross into
// internal/openjd. State this plainly rather than implying a precise
// operation accounting that does not exist: the derived ceiling is only as
// tight as maxTemplateExprPositions is chosen, and it is an upper BOUND, not
// a measurement -- nothing here counts a single operation.
//
// THAT DERIVATION WAS FALSE BY A FACTOR OF A THOUSAND until fix round 2
// (whole-branch review, Critical 1), and the correction lives in
// internal/openjd/expr, not here. Every Cost.ResultElements/ResultBytes
// charge is levied by chargeResult AFTER the produced value exists, so
// "[0] * 10000000" materialized ten million elements (1.1 GB, 108 ms) and
// only THEN reported 10,000,001 operations against a limit of 10,000. The
// per-position cap did not bound the work; limits.go's fixed maxElements
// floor did, three orders of magnitude higher, putting the real ceiling at
// maxTemplateExprPositions x maxElements ~= 10^11. meter.reserve now refuses
// such an operation on its ARITHMETIC count before allocating, which is what
// makes the multiplication above an actual bound rather than a hope. An
// EARLIER revision of this comment claimed the ceiling held "BY
// CONSTRUCTION" while resting on that same false premise; the construction
// it named is only now the construction that exists.
//
// WHAT THE OPERATION CEILING STILL DOES NOT BOUND is WALL TIME, because an
// operation's cost is not uniform: section 1.3.10 rule 3 prices 256 bytes of
// string work at ONE operation, so a regex or a case mapping over a ~900 KB
// string (the largest submissionMemoryLimit allows to be live) spends ~3,500
// operations and ~50 ms, four orders of magnitude more wall time per
// operation than scalar arithmetic. See the measured figures on
// maxTemplateExprPositions below. Bounding THAT needs a template-wide
// operation budget sized in wall time, which is sub-project E4d's
// operator-configuration question, not this constant's.
const (
	// maxTemplateExprPositions caps the number of format-string/let-binding
	// positions ONE call to checkTemplateExpressions may check -- one unit
	// per checkFormatString call and one per let: binding actually attempted
	// (letPositions, below, mirrors checkLetBindings' own maxLetBindings
	// truncation so the count charged here matches the work actually done,
	// not the possibly-larger count the template declared).
	//
	// 10,000 is chosen the way maxSteps was (validate.go, "WHAT THIS DOES
	// BOUND"): with a WORKED calculation, not a round guess, and with real
	// headroom above it -- fix round 1 (post-implementation review) found an
	// earlier value of 2,000 had a crossover of exactly 20 positions per
	// step against maxSteps (100), and its own doc comment's illustrative
	// example ("a handful of let bindings ... a few args") landed on the
	// ACCEPTED side while a plausible worked example landed on the
	// REJECTED side -- a comment whose own example straddles the cap is not
	// a justification.
	//
	// THE CROSSOVER: maxTemplateExprPositions / maxSteps = 100 positions
	// available per step, on average, before a template is rejected purely
	// on count. There is no cap on Action.Args, EmbeddedFiles, or
	// Environment.Variables in this package (checked directly; none
	// exists), so a real template's per-step position count is not bounded
	// by any OTHER constant this cap can lean on.
	//
	// THAT RATIO ASSUMES BOTH TERMS ARE IN FORCE, AND ONLY ONE ALWAYS IS --
	// stated explicitly by fix round 2 (whole-branch review, MINOR 3),
	// because E4d will size an operator knob from it. THIS budget is
	// always-on: it is a resource-exhaustion guard, reached whether or not
	// opts.EnforceLimits is set. maxSteps is NOT -- it sits inside
	// validateLimits, behind EnforceLimits (validate.go). Under the
	// operator opt-out (enforce_limits: false) the denominator simply
	// vanishes: step count is unbounded, "positions per step" stops meaning
	// anything, and the only thing still bounding the walk is this cap's own
	// absolute value. Not a defect -- the cap holds either way, which is the
	// property that matters -- but the JUSTIFICATION above is conditional in
	// a way the number it justifies is not.
	//
	// THE WORKED CALCULATION -- one step, generous but plausible for a real,
	// hand-authored render-pipeline step (not maximal against every
	// structural cap at once, which is a different, adversarial question
	// parameterSpaceOverCaps and this budget's own byte dimension answer
	// separately):
	//
	//	command + timeout                                            2
	//	args (a renderer's CLI flags)                               20
	//	let (step template + step script, 5 bindings each)          10
	//	host requirements (3 amounts x min/max, 2 attributes x 4)   14
	//	range (one RangeExpr -- the idiomatic form, not a raw list)   1
	//	one step environment (5 vars, 2 files, onEnter+onExit
	//	  command+3 args each, its own script let of 5)             20
	//	step's own script embedded files                             3
	//	                                                     --------
	//	                                                             70 / step
	//
	// x100 steps (maxSteps) = 7,000, plus job-level positions (the job name,
	// and up to a couple of job environments at a similar weight to one
	// step environment above) ~= 40, for a total around 7,040.
	//
	// 10,000 gives that calculation ~1.4x headroom on its own, ~4.8x
	// headroom over a MORE MODEST real-world shape (a command, 15 args, 3
	// environment variables and 2 embedded files per step, no let/host-
	// requirements/range use at all -- 21 x 100 = 2,100 -- the exact shape
	// fix round 1's review used to show 2,000 was too tight), and stays at
	// 61% of the hard ceiling this cap exists to stay under: a SINGLE
	// step's task-parameter range positions can reach 16,384 by
	// construction at the existing per-field caps
	// (maxTaskParameterDefinitions x maxTaskParamValues, EXPR sub-project
	// E4b's own finding, design spec §1.1) -- a construction that sits
	// WITHIN every structural cap Task 1/2 of this sub-project added
	// (parameterSpaceOverCaps, maxSteps), so this budget is the only thing
	// that still catches it. 10,000 still catches it, in the low tens of
	// thousands of positions rather than at 16,384 -- see
	// TestCheckTemplateExpressions_TemplateWideBudget_E4bConstruction.
	//
	// WHAT THIS STILL EXCLUDES, stated plainly rather than left implicit: a
	// 100-step template that uses EVERY category above at generous levels
	// in EVERY step simultaneously (the full ~70-position shape, not a
	// realistic subset of it, repeated 100 times with no step lighter than
	// another) sits close to this cap's floor and a template meaningfully
	// past that shape -- more like TWO of everything above, or maximal
	// per-field host requirements (maxHostRequirements = 50) used in most
	// steps -- is still rejected. That is a real, standing tradeoff this
	// wave accepts on purpose: per the reviewer, "a false rejection post-H
	// is worse than a bound E4d can tighten later," and E4d (operator
	// configuration of these limits, "Not in this wave") is where that
	// tradeoff gets a knob rather than a recompiled constant.
	//
	// THE COST TRADEOFF THIS RAISE ACCEPTS, restated in fix round 2
	// (whole-branch review, Critical 1) with END-TO-END MEASUREMENTS rather
	// than the per-expression estimate it carried before. An earlier
	// revision claimed "10,000 x ~6ms =~ 60s ... a categorical improvement
	// over E2's ~9 minutes", extrapolated from `("x" * 900000).upper()`.
	// Both halves were wrong: that expression is not the worst case, and
	// the construction the reviewer actually ran -- one step, 10,000 args
	// entries of `{{ [0] * 10000000 }}` -- cost 9m09s and 2.4 GB of peak
	// heap, which is E2's own figure to within 2%.
	//
	// MEASURED on this repository's development machine: 10,000 args
	// positions in one step, timing checkTemplateExpressions alone, scaled
	// x10 from a 1,000-position run.
	//
	//	payload                                    before         after
	//	{{ len(range_expr("1-5e6,6e6-9e6")) }}     ~6450s        ~50ms
	//	{{ [0] * 10000000 }}                        ~660s, 2.3GB  ~20ms
	//	{{ ("x" * 900000).title() }}                ~571s         ~571s
	//	{{ re_findall("x", "x" * 900000) }}         ~496s         ~496s
	//	{{ ("x" * 900000).upper() }}                 ~58s          ~58s
	//
	// (Row 1's range text is abbreviated to fit; it is
	// "1-5000000,6000000-9000000". Row 3 is 57.1 ms per position here; an
	// independent measurement on other hardware put the same payload at 71
	// ms, i.e. ~715 s -- treat ~9.5 minutes as the order, not the digit.)
	//
	// ROW 1 IS THE ONE THAT MOVED LAST, and it is why this table was wrong
	// once already. A range_expr with TWO OR MORE sub-ranges cannot be
	// counted arithmetically -- rangeExprCount expands it to count it -- so
	// len() over one spent 645 ms and 687 MB per position while charging
	// TWO operations, invisible to every budget in the package, at ~107
	// MINUTES for a full template. An earlier revision of this comment
	// named row 3 as the worst case while row 1 was an order of magnitude
	// past it. Fixed in internal/openjd/expr by reserveRangeExprExpansion
	// (rangeexpr.go), which bounds the expansion from arithmetic bounds
	// instead of from an expansion.
	//
	// So meter.reserve and reserveRangeExprExpansion together remove the
	// BULK-MATERIALIZATION class from every path that reaches it -- rows 1
	// and 2, four to five orders of magnitude each, and with them the
	// multi-gigabyte heap -- while the WALL-CLOCK worst case remains
	// roughly 9.5 minutes, because it is now set by op-cheap, byte-heavy
	// work: a regex or a case mapping over the ~900 KB string
	// submissionMemoryLimit allows to be live spends ~3,500 of the 10,000
	// permitted operations and ~50 ms of CPU. Every per-Eval budget is
	// respected throughout.
	//
	// Recorded plainly rather than smoothed over: this cap is NOT a
	// wall-time bound, and no value of it can be one while an operation's
	// cost varies by four orders of magnitude between scalar arithmetic and
	// a regex over a megabyte. Bounding wall time needs a template-wide
	// budget denominated in something closer to time, which is E4d's
	// operator-configuration question. The choice standing in the meantime
	// is fix round 1's: avoiding a false rejection of a legitimate template
	// matters more than tightening this particular worst case by lowering
	// the cap.
	//
	// THE WORKER'S CAP IS TIED TO THIS ONE, and the tie is asserted rather
	// than assumed. internal/worker/fmtres' MaxAssignmentPositions bounds the
	// same quantity for phase 3, on the worker, per assignment -- and an
	// assignment's positions are a SUBSET of its template's, so a worker cap
	// BELOW this value is reachable by a template this package ACCEPTED, and
	// fails every task in the job after submission instead of failing the one
	// request that could have reported it. That was the state until fix round
	// 2 (whole-branch review, IMPORTANT 1): 10,000 here, 5,000 there, no
	// stated relation and no test. Lowering THIS constant is not the way to
	// restore the relation -- see the paragraph above on why it was raised.
	// TestTemplateBudget_WorkerCapIsNotTighter (exprcheck_budget_test.go)
	// fails the build if the two ever drift apart again.
	maxTemplateExprPositions int64 = 10_000

	// maxTemplateExprRetainedBytes caps the cumulative section 1.3.9 size
	// (expr.SizeOf) of every value EVERY let: block in the template adds to
	// the symbol table it lands in -- summed across the WHOLE call, not
	// reset per block or per step. Every other position discards its result
	// (checkFormatString); let is the only construct that RETAINS one (see
	// checkLetBindings' own doc comment), so only let bindings contribute to
	// this counter -- see checkStepExpressions/checkEnvironmentExpressions
	// for where each of the three let: positions (step template, step
	// script, environment script) is charged.
	//
	// 10,000,000 (10 MB) is chosen to MATCH workerLetRetainedLimit
	// (internal/worker/fmtres/exprsyms.go), not by coincidence: design spec
	// §4 ("The asymmetry this wave should also close") requires the server
	// and the worker to agree that retained let bytes are bounded, and using
	// the identical figure is what makes that agreement legible rather than
	// two independently-tuned numbers that happen to land in the same
	// ballpark. It also SUBSUMES the server's own pre-existing gap:
	// checkLetBindings' 50-binding cap (maxLetBindings) bounds one BLOCK's
	// count but nothing bounded the BYTES those 50 could retain -- and
	// because this counter is cumulative rather than reset per table, many
	// blocks that are each individually compliant but cumulatively large
	// also trip it, which a per-table-only bound (the worker's own shape)
	// would not catch. See this task's report for the construction that
	// proves that.
	//
	// NOT identical to a dedicated per-table 10 MB bound, corrected by fix
	// round 1 (post-implementation review): the charge is applied ONCE per
	// let: block, AFTER checkLetBindings finishes evaluating it (the
	// before/after-delta or stepLetSymbols-diff technique the call sites
	// use), not per binding WITHIN it. A single block can therefore
	// transiently retain up to maxLetBindings x submissionMemoryLimit =
	// 50 x 1,000,000 = 50,000,000 bytes (50 MB) before this counter ever
	// sees it and rejects -- the true single-block ceiling this budget
	// enforces, not 10 MB. It IS still bounded, and the template as a whole
	// is still rejected the moment that block's full charge lands; it is
	// bounded by a larger number than a mid-block check (the worker's own
	// shape, which stops WITHIN a block once the running total crosses its
	// limit) would allow.
	//
	// This counter also measures CUMULATIVE ALLOCATION across the walk, not
	// PEAK LIVE RETENTION at any one instant -- intentional, and simpler to
	// reason about than trying to model when Go's garbage collector might
	// reclaim an earlier block's now-unreferenced values: a template whose
	// blocks never hold more than a few MB live at once but which declares
	// many such blocks in sequence is still charged for their sum, and is
	// still rejected once that sum crosses the limit, exactly as a template
	// that held all of it live simultaneously would be.
	maxTemplateExprRetainedBytes int64 = 10_000_000
)

// templateBudget is one phase's allowance against maxTemplateExprPositions
// and maxTemplateExprRetainedBytes. checkTemplateExpressions allocates a
// FRESH templateBudget at the top of every call -- see that function -- so
// phase 1 (ValidateWithOptions, params == nil) and phase 2
// (checkExpressionsAtSubmit, boundParams concrete) each get their own
// allowance, per design spec §3.1: they are separate calls with separate
// symbol tables, and a budget shared across them would make phase 2's
// verdict depend on what phase 1 already spent, with no way for the
// submitter to see why.
//
// Once either dimension trips, err is set exactly once and every further
// charge call becomes a cheap no-op that returns false -- callers gate
// further work on ok() or on a charge call's own return value so the walk
// stops doing real work (parsing, evaluating) once the budget is spent, not
// merely stops reporting once it is spent.
type templateBudget struct {
	positions int64
	retained  int64
	err       *ValidationError
}

func newTemplateBudget() *templateBudget { return &templateBudget{} }

// templateBudgetOrFresh returns budget[0] when the caller supplied one
// (EXPR sub-project E4c's Task 4: a shared budget threaded in from outside,
// e.g. submit.go's ONE phase-2 budget spanning both checkTemplateExpressions
// and every step's ResolveParameterSpaceParams call for the same submission),
// or a brand-new [templateBudget] otherwise -- the exact allowance
// checkTemplateExpressions has always allocated for itself.
//
// This is the mechanism that keeps every existing call site (validate.go's
// checkTemplateExpressions(t, nil), every direct unit-test call in this
// package, and every pre-Task-4 caller of [ResolveParameterSpaceParams])
// compiling and behaving byte for byte unchanged: a trailing variadic
// parameter with zero arguments supplied is indistinguishable, at the call
// site, from a function that never took the parameter at all. Only Task 4's
// two NEW callers -- submit.go's prepareTemplate/Submit -- ever pass a
// non-nil budget.
//
// budget[0] == nil (an explicit nil passed where a *templateBudget was
// expected) is treated the same as "no budget supplied": a nil budget must
// never reach chargePositions/chargeRetainedBytes, both of which write
// through the pointer unconditionally.
func templateBudgetOrFresh(budget []*templateBudget) *templateBudget {
	if len(budget) > 0 && budget[0] != nil {
		return budget[0]
	}
	return newTemplateBudget()
}

// ok reports whether the budget has not yet been exhausted.
func (b *templateBudget) ok() bool { return b.err == nil }

// chargePositions charges n additional walk positions, naming ptr as the
// position that exhausted the budget if this call is the one that does. It
// returns whether the caller may still do the work those positions
// represent -- false either because this call itself tripped the budget or
// because an earlier call (either dimension) already had.
func (b *templateBudget) chargePositions(n int64, ptr string) bool {
	if b.err != nil {
		return false
	}
	b.positions += n
	if b.positions > maxTemplateExprPositions {
		b.err = &ValidationError{
			Pointer: ptr,
			Message: fmt.Sprintf(
				"template-wide expression budget exceeded: at most %d expression positions may be "+
					"checked in one validation pass (reached %d at %s)",
				maxTemplateExprPositions, b.positions, ptr,
			),
		}
		return false
	}
	return true
}

// chargeRetainedBytes charges n additional retained bytes -- the net size a
// let: block's successful bindings just added to the table they landed in --
// naming ptr as the position that exhausted the budget if this call is the
// one that does. n <= 0 (an empty or fully-failed block) is a no-op that
// still reports the budget's current state, matching chargePositions' own
// contract.
func (b *templateBudget) chargeRetainedBytes(n int64, ptr string) bool {
	if b.err != nil {
		return false
	}
	if n <= 0 {
		return true
	}
	b.retained += n
	if b.retained > maxTemplateExprRetainedBytes {
		b.err = &ValidationError{
			Pointer: ptr,
			Message: fmt.Sprintf(
				"template-wide expression budget exceeded: let bindings may retain at most %d bytes "+
					"across the whole template (reached %d at %s)",
				maxTemplateExprRetainedBytes, b.retained, ptr,
			),
		}
		return false
	}
	return true
}

// errs returns the ONE ValidationError recording why the budget tripped, or
// nil if it never did. checkTemplateExpressions appends this to the errors
// the walk itself collected before the budget stopped it.
func (b *templateBudget) errs() ValidationErrors {
	if b.err == nil {
		return nil
	}
	return ValidationErrors{*b.err}
}

// letPositions is the number of bindings ONE let: block actually costs the
// budget: min(n, maxLetBindings), mirroring checkLetBindings' own truncation
// (validate.go) so the count charged here matches the work checkLetBindings
// actually does, not the (possibly larger) count the template declared.
func letPositions(n int) int64 {
	if n > maxLetBindings {
		return int64(maxLetBindings)
	}
	return int64(n)
}

// templateExprRetainedBytes is the section 1.3.9 size of every value a
// symbol table currently holds live, summed -- internal/worker/fmtres's
// tableRetainedBytes (EXPR sub-project E4a), copied here rather than shared:
// internal/openjd cannot depend on internal/worker (see jobParamTypes' doc
// comment, above, for the established reason internal/openjd/expr's mapping
// helpers live where they do rather than here) and internal/worker cannot
// depend on internal/openjd (it pulls in internal/store, which the worker
// binary must never depend on), so the one package both already share
// (internal/openjd/expr, via the exported expr.SizeOf) is the only thing
// this technique can be built on twice rather than shared once.
func templateExprRetainedBytes(syms expr.MapSymbols) int64 {
	var total int64
	for _, v := range syms {
		total += expr.SizeOf(v)
	}
	return total
}

// ─── template walk ─────────────────────────────────────────────────────────

// checkTemplateExpressions walks tmpl and checks every format-string-bearing
// position through the EXPR-aware evaluator (checkFormatString), building the
// right scope-aware symbol table (symbolsFor) and target type at each one.
//
// It runs ONLY when tmpl declares the EXPR extension; a nil tmpl or one that
// does not is a no-op. When EXPR is not declared, validate.go's base-spec
// path (validateFormatString, gated on the same declaration) already covers
// these positions with base-spec semantics -- running the expression
// evaluator over content that is guaranteed to be nothing but a bare dotted
// identifier would be redundant at best and, for a body the base-spec reader
// would reject as malformed but EXPR's grammar parses differently, wrong at
// worst.
//
// The EXPR declaration is a necessary but NOT a sufficient condition for the
// walk to happen: ValidateWithOptions additionally gates this call on the EXPR
// registry entry's status (exprExpressionWalkEnabled), because while EXPR is
// StatusInProgress the template has already been rejected by
// validateExtensions and this walk -- whose operation and byte budgets are per
// expression position, with no template-wide cap and no bound on the number of
// positions -- would only burn CPU on a verdict it cannot change. Callers that
// need the walk regardless (test/conformance's EXPR scoring) opt in via
// ValidateOptions.CheckEXPRExpressionsWhileUnsupported. Calling this function
// directly, as the unit tests in exprcheck_test.go do, bypasses that gate on
// purpose: it tests the checker, not the decision to invoke it.
//
// params is nil for phase 1 (every job-parameter symbol unresolved, called
// from ValidateWithOptions) and holds concrete values for phase 2 (called
// from submit.go's checkExpressionsAtSubmit, sub-project E2's Task 10, once
// job parameters are bound) -- see symbolsFor's own doc comment for what
// params changes.
//
// The walk mirrors validate.go's traversal (ValidateWithOptions ->
// validateStep -> validateEnvironments/validateParameterSpace/
// validateHostRequirements -> validateAction/validateScriptRefs) position
// for position, per the table in this sub-project's task brief:
//
//	job name                          ScopeJob                TargetString
//	host requirement values           ScopeJob + step let     TargetString
//	task-parameter range entries      ScopeJob + step let     §1.3.12 per-type
//	environment variable values       job/step env            TargetString
//	env + step embedded-file data     job/step env / step     TargetString
//	action command                    matching env / step     TargetString
//	action args entries               matching env / step     TargetArgItem
//	action timeout                    matching env / step     TargetInt
//
// Two rows of that table need their own sentence, because neither is the flat
// "one scope, one target" the other six are:
//
//   - "ScopeJob + step let" is the SCOPE ScopeJob with the enclosing step
//     template's let-bound names merged into its symbol table, which is what
//     section 3.6.2 row 1 grants and nothing more (rangeScopeSymbols). The
//     scope itself is unchanged: a task-level symbol is still out of reach at
//     both positions.
//   - a task-parameter range position does NOT target TargetString. It
//     targets section 1.3.12's real per-type type -- rangeExprElemType for an
//     individual range-list entry, rangeExprFieldType for the whole field --
//     and resolve.go targets the identical types at the identical positions.
//     See checkParameterSpaceExpressions.
//
// Host requirement values and task-parameter range entries are two of the
// three positions that had NO format-string scope validation at all before
// this task (validate.go's parallel checks are new in the same commit); the
// third, action timeout, is wired here too but is inert for a real template
// today -- decodeAction (parse.go) decodes "timeout" as a strict integer, so
// no format-string body can ever reach this position until that decoder is
// changed, which is a separate, later gap this task does not close.
func checkTemplateExpressions(tmpl *JobTemplate, params map[string]string, budget ...*templateBudget) ValidationErrors {
	if tmpl == nil || !tmpl.hasExtension("EXPR") {
		return nil
	}

	// b is this CALL's budget -- fresh every time UNLESS the caller threads
	// one in (templateBudgetOrFresh), which is what gives phase 1 and phase 2
	// their own separate allowance (design spec §3.1; see templateBudget's
	// own doc comment). ValidateWithOptions (phase 1) never passes one, so it
	// is unaffected by this parameter's addition -- Task 4 lets
	// checkExpressionsAtSubmit (phase 2) share this call's budget with the
	// SAME submission's ResolveParameterSpaceParams calls (resolve.go),
	// because both walk the SAME parameter-space range positions and
	// together represent one submission's total re-check cost -- see
	// submit.go's prepareTemplate/Submit.
	b := templateBudgetOrFresh(budget)
	var errs ValidationErrors

	if b.chargePositions(1, "/name") {
		errs = append(errs, checkFormatString(
			tmpl.Name, "/name", ScopeJob, symbolsFor(tmpl, nil, nil, ScopeJob, params), TargetString,
			submissionLimits()...,
		)...)
	}

	if b.ok() {
		errs = append(errs, checkEnvironmentExpressions(
			b, tmpl, nil, tmpl.JobEnvironments, "/jobEnvironments", ScopeJobEnvironment, params, nil,
		)...)
	}

	for i, s := range tmpl.Steps {
		if !b.ok() {
			break
		}
		errs = append(errs, checkStepExpressions(b, tmpl, s, i, params)...)
	}

	return append(errs, b.errs()...)
}

// checkStepExpressions checks the format-string positions of one step: its
// own let, its own script, its step environments, its parameter space's
// range entries, and its host requirement values.
//
// Section 3.6.2 row 1: a step template's own let: block is evaluated at
// ScopeStepTemplate, and its bound names become visible in stepEnvironments,
// hostRequirements, parameterSpace and script -- the other three positions of
// this function. stepLet holds EXACTLY those bound names, computed as the
// set-difference between stepTemplateSyms before and after checkLetBindings
// runs, deliberately NOT the whole (post-let) stepTemplateSyms table: that
// table also carries Job.Name, Step.Name, Param.* and RawParam.* entries
// which ScopeStepTemplate exposes but ScopeJob (below) does not. Propagating
// the whole table instead of just the diff would silently make a bare
// Step.Name legal in a host requirement or task-parameter range, which
// section 3.6.2 forbids -- see jobSyms' own comment below.
func checkStepExpressions(b *templateBudget, tmpl *JobTemplate, s StepTemplate, idx int, params map[string]string) ValidationErrors {
	var errs ValidationErrors
	base := fmt.Sprintf("/steps/%d", idx)

	// stepLetSymbols runs unconditionally, regardless of b -- it is bounded
	// on its own (maxLetBindings bindings, each under submissionMemoryLimit,
	// via checkLetBindings), so it is never itself the expensive part of a
	// step. b is charged with the position/byte cost AFTER it runs: stepLet
	// is already exactly the diff stepLetSymbols computed (its own doc
	// comment), so templateExprRetainedBytes(stepLet) is the net bytes this
	// block added with no before/after subtraction needed.
	stepLet, letErrs := stepLetSymbols(tmpl, &s, params, base)
	errs = append(errs, letErrs...)
	b.chargePositions(letPositions(len(s.Let)), base+"/let")
	b.chargeRetainedBytes(templateExprRetainedBytes(stepLet), base+"/let")

	if s.Script != nil && b.ok() {
		// syms starts as a CLONE of stepLet, not stepLet itself: the step
		// script's own let: block (section 3.6.2 row 2) is evaluated over
		// this table next, and checkLetBindings MUTATES the table it is
		// handed -- if that were stepLet itself, the script's own bound
		// names would leak into stepEnvironments/hostRequirements/
		// parameterSpace below, which section 3.6.2 does not grant them.
		syms := maps.Clone(stepLet)
		maps.Copy(syms, symbolsFor(tmpl, &s, nil, ScopeStepScript, params))
		// Unlike stepLet above, syms has no ready-made diff: it starts as a
		// clone carrying stepLet's names plus symbolsFor's own (Job.Name,
		// Param.*, ...), and checkLetBindings can only ADD keys (the shadow
		// check rejects rebinding one already present) -- so the before/after
		// delta of the WHOLE table's retained bytes is exactly this block's
		// own net contribution, with the baseline canceling out.
		before := templateExprRetainedBytes(syms)
		errs = append(errs, checkLetBindings(s.Script.Let, base+"/script/let", ScopeStepScript, syms, submissionLimits()...)...)
		b.chargePositions(letPositions(len(s.Script.Let)), base+"/script/let")
		b.chargeRetainedBytes(templateExprRetainedBytes(syms)-before, base+"/script/let")

		if b.ok() {
			errs = append(errs, checkScriptRefExpressions(
				b, s.Script.EmbeddedFiles, ScopeStepScript, syms, base+"/script",
			)...)
		}
		if b.ok() {
			errs = append(errs, checkActionExpressions(
				b, s.Script.Actions.OnRun, base+"/script/actions/onRun", ScopeStepScript, syms,
			)...)
		}
	}

	if b.ok() {
		errs = append(errs, checkEnvironmentExpressions(
			b, tmpl, &s, s.StepEnvironments, base+"/stepEnvironments", ScopeStepEnvironment, params, stepLet,
		)...)
	}

	// Host requirements and the task-parameter range both sit at ScopeJob: a
	// step's own task-level symbols (Task.Param./Task.RawParam./Task.File.)
	// do not exist yet while its host is being selected or its
	// task-parameter space is being defined, so step is deliberately NOT
	// passed to symbolsFor here -- ScopeJob's symbol families never include
	// them anyway (scope.go's scopeFamilies).
	//
	// What section 3.6.2 row 1 settles, and all it settles: it changes the
	// TABLE, not the scope. jobSyms gains the step's let-bound names (stepLet,
	// merged in below) while keeping ScopeJob's own fixed/family set exactly
	// as symbolsFor(..., ScopeJob, ...) built it, so a let name bound by the
	// step template IS visible here and a task-level symbol is not. Scope and
	// symbol table are separate things, and conflating them here was the
	// mistake this function existed to avoid before let: made it worth
	// restating.
	//
	// KNOWN GAP, pre-existing since sub-project E2, NOT settled by the above.
	// ScopeJob's fixed symbols (scopeFixed) are nil, so a bare Job.Name or
	// Step.Name is currently rejected at both positions -- and per section
	// 7.3.1 that is WRONG. 7.3.1's symbol table says Step.Name is "Available
	// within the Step Template scope: stepEnvironments, hostRequirements,
	// parameterSpace, and script", and Job.Name is "Available in every Format
	// String in the Job Template, except the name field of the Job Template
	// itself". Both of these positions are inside that grant. Reproduced at
	// HEAD, all three verbatim:
	//
	//	/steps/0/hostRequirements/attributes/0/anyOf/0: col 1: unknown symbol "Step.Name"
	//	/steps/0/hostRequirements/attributes/0/anyOf/0: col 1: unknown symbol "Job.Name"
	//	/steps/0/parameterSpace/taskParameterDefinitions/0/range/0: col 1: unknown symbol "Step.Name"
	//
	// Section 3.6.2 does NOT authorize that rejection and must not be cited
	// for it: 3.6.2 governs where LET NAMES are visible and is silent on the
	// fixed symbols. An earlier revision of this comment read the two as one
	// rule and certified "a bare Job.Name or Step.Name STAYS ILLEGAL at this
	// position" as correct; it is not, and the true statement about 3.6.2 did
	// not make it so.
	//
	// Deliberately NOT fixed here. Correcting it means either splitting
	// scopeFixed(ScopeJob) or introducing a distinct scope for these two
	// positions, which can move conformance scoring and deserves its own
	// analysis -- see docs/openjd-conformance.md. No fixture covers it today,
	// which is why it survived E2 and E3 unnoticed.
	jobSyms := rangeScopeSymbols(tmpl, stepLet, params)

	if b.ok() && s.ParameterSpace != nil {
		errs = append(errs, checkParameterSpaceExpressions(b, *s.ParameterSpace, base+"/parameterSpace", jobSyms)...)
	}
	if b.ok() && s.HostRequirements != nil {
		errs = append(errs, checkHostRequirementExpressions(b, *s.HostRequirements, base+"/hostRequirements", jobSyms)...)
	}

	return errs
}

// stepLetSymbols evaluates a step template's OWN let: block (section 3.6.2
// row 1) at ScopeStepTemplate and returns EXACTLY the names it bound --
// computed as the set-difference between the ScopeStepTemplate table before
// and after checkLetBindings runs, deliberately NOT the whole post-let table,
// which also carries Job.Name, Step.Name, Param.* and RawParam.* entries that
// ScopeStepTemplate exposes and the narrower positions below it do not.
// Propagating the whole table would silently make a bare Step.Name legal in a
// host requirement or a task-parameter range, which section 3.6.2 does not
// grant.
//
// base is the step's own JSON pointer prefix; each binding's errors are
// reported at base + "/let/<i>". Pass "" for a caller whose pointers are
// already relative to the step (resolve.go's ResolveParameterSpaceParams,
// whose one production caller prefixes "/steps/<i>" itself).
//
// This is shared, rather than duplicated, on purpose. It has TWO callers --
// the checker (checkStepExpressions) and the resolver
// (ResolveParameterSpaceParams) -- and them building the table differently is
// precisely the defect EXPR sub-project E4b's whole-branch review found: the
// checker merged stepLet and the resolver did not, so a step-level
// let: ["base = 10"] plus range: "{{ [base, base + 1] }}" validated at upload,
// passed the phase-2 re-check, and then died in expandStepTaskParams with
// unknown symbol "base" -- a symbol the checker had just certified. One
// function means the two tables cannot drift again without the compiler
// noticing.
func stepLetSymbols(
	tmpl *JobTemplate, s *StepTemplate, params map[string]string, base string,
) (expr.MapSymbols, ValidationErrors) {
	stepLet := expr.MapSymbols{}
	if s == nil {
		return stepLet, nil
	}

	stepTemplateSyms := symbolsFor(tmpl, s, nil, ScopeStepTemplate, params)
	preLetKeys := make(map[string]struct{}, len(stepTemplateSyms))
	for k := range stepTemplateSyms {
		preLetKeys[k] = struct{}{}
	}
	errs := checkLetBindings(s.Let, base+"/let", ScopeStepTemplate, stepTemplateSyms, submissionLimits()...)
	for k, v := range stepTemplateSyms {
		if _, existed := preLetKeys[k]; !existed {
			stepLet[k] = v
		}
	}
	return stepLet, errs
}

// rangeScopeSymbols builds the symbol table the two ScopeJob-scoped positions
// inside a step -- its parameterSpace range fields and its hostRequirements
// values -- are checked and resolved against: ScopeJob's own fixed and family
// symbols, plus the enclosing step template's let-bound names (stepLet), and
// nothing else.
//
// Section 3.6.2 row 1 changes the TABLE, not the scope: a name the step
// template's let: bound IS visible here, and a task-level symbol
// (Task.Param./Task.RawParam./Task.File.) still is not, because ScopeJob's
// families never included them. See checkStepExpressions' own comment for the
// pre-existing Job.Name/Step.Name gap this does not close.
//
// Shared with resolve.go for the same reason stepLetSymbols is.
func rangeScopeSymbols(tmpl *JobTemplate, stepLet expr.MapSymbols, params map[string]string) expr.MapSymbols {
	jobSyms := symbolsFor(tmpl, nil, nil, ScopeJob, params)
	maps.Copy(jobSyms, stepLet)
	return jobSyms
}

// checkEnvironmentExpressions checks the format-string positions of a list of
// environments (job-level or step-level): each one's variable values,
// embedded-file data, and onEnter/onExit actions. step is nil for job
// environments; scope selects ScopeJobEnvironment or ScopeStepEnvironment so
// symbolsFor binds the right fixed-symbol and family set for the level.
//
// outerLet carries the ENCLOSING step template's let-bound names (section
// 3.6.2 row 1: bound names are visible in the whole stepEnvironments
// subtree, not merely its script) -- nil for job environments, which have no
// enclosing step. It is folded into baseSyms, which backs BOTH variables and
// (via scriptSyms, below) the script positions, because Variables is a
// descendant of the stepEnvironments element that row 1 names as a whole.
//
// A single environment's OWN EnvironmentScript.let (row 4) is narrower: its
// bound names are visible only in actions and embeddedFiles, because those
// are EnvironmentScript's only child elements -- Variables sits on the
// parent Environment, a SIBLING of Script, not a descendant of it. That is
// why the environment's own let is bound into scriptSyms below but never
// into baseSyms, which the variables loop above it has already consulted.
func checkEnvironmentExpressions(
	b *templateBudget, tmpl *JobTemplate, step *StepTemplate, envs []Environment, base string, scope Scope,
	params map[string]string, outerLet expr.MapSymbols,
) ValidationErrors {
	var errs ValidationErrors
	for i, e := range envs {
		if !b.ok() {
			break
		}
		ptr := fmt.Sprintf("%s/%d", base, i)

		var envScriptFiles []EmbeddedFile
		if e.Script != nil {
			envScriptFiles = e.Script.EmbeddedFiles
		}

		// baseSyms is a CLONE of outerLet, not outerLet itself: outerLet is
		// the SAME map object across every iteration of this loop (it is
		// built once by the caller, not per environment), so writing this
		// environment's own symbolsFor additions -- or, further down,
		// letting checkLetBindings mutate a table derived from it -- directly
		// into outerLet would leak into environment i+1's table. A nil
		// outerLet (job environments) clones to nil, so the map is
		// allocated explicitly rather than left nil, which the writes just
		// below require.
		baseSyms := maps.Clone(outerLet)
		if baseSyms == nil {
			baseSyms = expr.MapSymbols{}
		}
		// symbolsFor needs THIS environment's own script to bind Env.File.:
		// a template may declare many environments, each with its own files.
		maps.Copy(baseSyms, symbolsFor(tmpl, step, &e, scope, params))

		// Sorted so the errors a template produces do not depend on map
		// order -- matching validateScriptRefs' own reason for sorting.
		// Checked against baseSyms, BEFORE this environment's own
		// EnvironmentScript.let is bound: see the function doc for why
		// Variables must not see it.
		for _, k := range slices.Sorted(maps.Keys(e.Variables)) {
			varPtr := ptr + "/variables/" + k
			if !b.chargePositions(1, varPtr) {
				break
			}
			errs = append(errs, checkFormatString(
				e.Variables[k], varPtr, scope, baseSyms, TargetString, submissionLimits()...,
			)...)
		}

		// scriptSyms is a clone of baseSyms, but -- unlike syms's clone of
		// stepLet in checkStepExpressions, or baseSyms's own clone of
		// outerLet just above -- mutation-tested and confirmed to have NO
		// OBSERVABLE EFFECT today, under either the unit tests or the full
		// conformance suite: baseSyms is allocated fresh every loop
		// iteration (nothing else in this call reads it afterward), and the
		// variables loop that consults it runs BEFORE this line, not after.
		// Removing this clone (aliasing scriptSyms := baseSyms) changes
		// nothing a test can currently see.
		//
		// Kept anyway, as ORDERING INSURANCE, not a live behavioral
		// guarantee: it is one reordering away from mattering (moving the
		// variables loop below this line, or adding a second consumer of
		// baseSyms after it) and cloning here costs one small map allocation
		// against that risk. A future edit that removes it should not read
		// this comment as evidence something depends on it today -- verify
		// by mutation before either keeping or removing it, the same way
		// this note was produced.
		scriptSyms := maps.Clone(baseSyms)
		if e.Script != nil && b.ok() {
			// Before/after delta, exactly as checkStepExpressions' own script
			// let does and for the same reason: scriptSyms has no ready-made
			// diff, and checkLetBindings can only ADD keys, so the baseline
			// cancels out of the subtraction.
			before := templateExprRetainedBytes(scriptSyms)
			errs = append(errs, checkLetBindings(e.Script.Let, ptr+"/script/let", scope, scriptSyms, submissionLimits()...)...)
			b.chargePositions(letPositions(len(e.Script.Let)), ptr+"/script/let")
			b.chargeRetainedBytes(templateExprRetainedBytes(scriptSyms)-before, ptr+"/script/let")
		}

		if b.ok() {
			errs = append(errs, checkScriptRefExpressions(b, envScriptFiles, scope, scriptSyms, ptr+"/script")...)
		}

		if e.Script != nil {
			if b.ok() && e.Script.Actions.OnEnter != nil {
				errs = append(errs, checkActionExpressions(
					b, *e.Script.Actions.OnEnter, ptr+"/script/actions/onEnter", scope, scriptSyms,
				)...)
			}
			if b.ok() && e.Script.Actions.OnExit != nil {
				errs = append(errs, checkActionExpressions(
					b, *e.Script.Actions.OnExit, ptr+"/script/actions/onExit", scope, scriptSyms,
				)...)
			}
		}
	}
	return errs
}

// checkScriptRefExpressions checks a script's embedded-file data -- the
// EXPR-aware counterpart of validate.go's validateScriptRefs. An
// environment's variable values are a DIFFERENT position, checked by
// checkEnvironmentExpressions directly against its own (possibly narrower)
// symbol table -- see that function's doc comment for why the two positions
// cannot share one symbol table once let: is in the picture.
func checkScriptRefExpressions(
	b *templateBudget, files []EmbeddedFile, scope Scope, syms expr.MapSymbols, scriptBase string,
) ValidationErrors {
	var errs ValidationErrors
	for i, f := range files {
		ptr := fmt.Sprintf("%s/embeddedFiles/%d/data", scriptBase, i)
		if !b.chargePositions(1, ptr) {
			break
		}
		errs = append(errs, checkFormatString(
			f.Data, ptr, scope, syms, TargetString,
			submissionLimits()...,
		)...)
	}
	return errs
}

// checkActionExpressions checks one action's command, args, and timeout --
// the EXPR-aware counterpart of validate.go's validateActionRefs, extended
// with the timeout position validateActionRefs never covered.
//
// args uses TargetArgItem, not TargetString: section 1.3.2's list-item rule
// for an args entry, where a string is one argument, None drops it, and a
// list[string] flattens inline -- this is the first real caller of
// TargetArgItem; exprcheck_test.go's direct checkFormatString calls establish
// the contract, this wires it into the walk.
//
// PLAIN STATEMENT, so nobody reads "timeout is validated" into this: the
// timeout call below is WIRED BUT UNREACHABLE FROM A REAL TEMPLATE. It is not
// a live check today, only the shape of one. decodeAction (parse.go) decodes
// "timeout" via a strict integer parse (intFieldStrict/scalarToInt) for EVERY
// template, EXPR or not -- there is no code path by which a.TimeoutSeconds
// ever holds anything but an already-resolved int, so re-rendering it with
// strconv.Itoa below and running it through checkFormatString ALWAYS produces
// a plain decimal literal with zero "{{" references, and checkFormatString is
// a guaranteed no-op on that input. Confirmed directly: parsing
// EXPR/job_templates/7.3--apply-path-mapping-in-timeout.invalid.yaml
// (timeout: "{{ len(apply_path_mapping(Param.Val)) }}") fails at
// openjd.Parse itself with "openjd: timeout must be an integer", before
// ValidateWithOptions -- and therefore this function -- ever runs; that
// fixture is one of this sub-project's thirteen target fixtures and it
// passes CONFORMANCE (correctly rejected) entirely incidentally, for a
// decode-time reason unrelated to scope checking. Making this position
// actually live requires changing decodeAction to accept a format-string
// body for "timeout", which has its own blast radius (every existing
// template's timeout becomes format-string-capable, base-spec included) and
// is deliberately NOT part of this task -- out of the file list
// (validate.go, exprcheck.go, baseline-expr.txt) this task was scoped to.
// The call stays here, unconditional when TimeoutSet, so the position is
// wired for the day that decoder changes without a second pass over the
// walk; until then, reading this comment is the only way to know it does
// nothing.
func checkActionExpressions(b *templateBudget, a Action, ptr string, scope Scope, syms expr.MapSymbols) ValidationErrors {
	var errs ValidationErrors
	cmdPtr := ptr + "/command"
	if b.chargePositions(1, cmdPtr) {
		errs = append(errs, checkFormatString(
			a.Command, cmdPtr, scope, syms, TargetString, submissionLimits()...,
		)...)
	}
	for i, arg := range a.Args {
		if !b.ok() {
			break
		}
		argPtr := fmt.Sprintf("%s/args/%d", ptr, i)
		if !b.chargePositions(1, argPtr) {
			break
		}
		errs = append(errs, checkFormatString(
			arg, argPtr, scope, syms, TargetArgItem, submissionLimits()...,
		)...)
	}
	if a.TimeoutSet && b.ok() {
		timeoutPtr := ptr + "/timeout"
		if b.chargePositions(1, timeoutPtr) {
			errs = append(errs, checkFormatString(
				strconv.Itoa(a.TimeoutSeconds), timeoutPtr, scope, syms, TargetInt, submissionLimits()...,
			)...)
		}
	}
	return errs
}

// checkHostRequirementExpressions checks a step's host requirement values --
// every amount's min/max and every attribute's anyOf/allOf entries -- against
// the ScopeJob symbol table syms. This is one of the two positions that had
// NO format-string scope validation at all before sub-project E2's Task 9;
// validate.go's validateHostRequirements gained the parallel base-spec check
// in the same commit.
func checkHostRequirementExpressions(b *templateBudget, hr HostRequirements, base string, syms expr.MapSymbols) ValidationErrors {
	var errs ValidationErrors
	for i, a := range hr.Amounts {
		if !b.ok() {
			break
		}
		errs = append(errs, checkHostRequirementAmount(b, a, fmt.Sprintf("%s/amounts/%d", base, i), syms)...)
	}
	for i, a := range hr.Attributes {
		if !b.ok() {
			break
		}
		errs = append(errs, checkHostRequirementAttribute(b, a, fmt.Sprintf("%s/attributes/%d", base, i), syms)...)
	}
	return errs
}

// checkHostRequirementAmount checks one amount's min/max, extracted from
// checkHostRequirementExpressions to keep that function's cyclomatic
// complexity within the repo's lint budget (see CLAUDE.md's "Lint is
// strict" convention).
func checkHostRequirementAmount(b *templateBudget, a AmountRequirement, amtPtr string, syms expr.MapSymbols) ValidationErrors {
	var errs ValidationErrors
	if a.Min != nil {
		minPtr := amtPtr + "/min"
		if b.chargePositions(1, minPtr) {
			errs = append(errs, checkFormatString(
				*a.Min, minPtr, ScopeJob, syms, TargetString, submissionLimits()...,
			)...)
		}
	}
	if a.Max != nil && b.ok() {
		maxPtr := amtPtr + "/max"
		if b.chargePositions(1, maxPtr) {
			errs = append(errs, checkFormatString(
				*a.Max, maxPtr, ScopeJob, syms, TargetString, submissionLimits()...,
			)...)
		}
	}
	return errs
}

// checkHostRequirementAttribute checks one attribute's anyOf/allOf entries --
// extracted for the same reason as checkHostRequirementAmount, above.
func checkHostRequirementAttribute(b *templateBudget, a AttributeRequirement, attrPtr string, syms expr.MapSymbols) ValidationErrors {
	var errs ValidationErrors
	for k, v := range a.AnyOf {
		if !b.ok() {
			break
		}
		p := fmt.Sprintf("%s/anyOf/%d", attrPtr, k)
		if b.chargePositions(1, p) {
			errs = append(errs, checkFormatString(
				v, p, ScopeJob, syms, TargetString, submissionLimits()...,
			)...)
		}
	}
	for k, v := range a.AllOf {
		if !b.ok() {
			break
		}
		p := fmt.Sprintf("%s/allOf/%d", attrPtr, k)
		if b.chargePositions(1, p) {
			errs = append(errs, checkFormatString(
				v, p, ScopeJob, syms, TargetString, submissionLimits()...,
			)...)
		}
	}
	return errs
}

// checkParameterSpaceExpressions checks a step's task-parameter range
// entries -- the other position with no format-string scope validation
// before Task 9 -- in both of the field's two shapes: each RANGE LIST entry
// and the whole-field RANGE EXPR alternative. Both now target section
// 1.3.12's real types (design spec §3) instead of the permissive
// TargetString/expr.TAny this function used before that section's positions
// had a consumer: a RangeList entry targets rangeExprElemType(tp.Type) (int,
// float, string or path) and a whole-field RangeExpr targets
// rangeExprFieldType(tp.Type) (int | string | range_expr | list[int] for
// INT/CHUNK[INT], list[<elem>] otherwise).
//
// resolve.go calls THE SAME TWO FUNCTIONS at the same two positions
// (resolveRangeListEntry and evalRangeExprField) -- not a separate target
// argued to be equivalent, the identical Type value. An earlier revision of
// this comment pointed at rangeExprFieldType "for why the checker's union
// differs from the resolver's plain list[elem] while still agreeing on every
// verdict"; they agreed only because both were equally wrong, and while they
// agreed on the verdict they reported DIFFERENT MESSAGES for it (validate:
// "cannot be coerced to list[int] | range_expr", submit: "cannot be coerced
// to list[int]"). One function per position is what makes both the verdict
// and the message one thing, and assertRowRejected
// (resolve_agreement_test.go) now asserts the messages match, not merely the
// verdicts.
//
// exprcheck.go and resolve.go MUST agree on both targets, or the checker's
// verdict and the resolver's actual coercion drift apart -- design spec §4's
// agreement instrument exists precisely to catch that: a range the checker
// accepts must resolve and expand without error, and a range the checker
// rejects must never reach expansion. A checker that accepted anything
// (expr.TAny/TargetString, this function's own shape before this task) could
// never disagree with a resolver, which is also why that permissive choice
// was never actually validating section 1.3.12's types in the first place.
//
// Neither target change weakens scope checking: an out-of-scope symbol fails
// at evaluation's symbol lookup, before target coercion ever runs, exactly as
// it did when the target was expr.TAny.
func checkParameterSpaceExpressions(b *templateBudget, ps StepParameterSpace, base string, syms expr.MapSymbols) ValidationErrors {
	var errs ValidationErrors
	for i, tp := range ps.TaskParameterDefinitions {
		if !b.ok() {
			break
		}
		ptr := fmt.Sprintf("%s/taskParameterDefinitions/%d", base, i)
		elemType := rangeExprElemType(tp.Type)
		for j, v := range tp.RangeList {
			p := fmt.Sprintf("%s/range/%d", ptr, j)
			if !b.chargePositions(1, p) {
				break
			}
			errs = append(errs, checkFormatString(
				v, p, ScopeJob, syms, elemType,
				submissionLimits()...,
			)...)
		}
		if tp.RangeExpr != nil && b.ok() {
			p := ptr + "/range"
			if b.chargePositions(1, p) {
				errs = append(errs, checkFormatString(
					*tp.RangeExpr, p, ScopeJob, syms, rangeExprFieldType(tp.Type),
					submissionLimits()...,
				)...)
			}
		}
	}
	return errs
}

// rangeExprFieldType maps a task-parameter's declared type to the target
// checkParameterSpaceExpressions verifies a whole-field RangeExpr's lone
// {{...}} reference against, per section 1.3.12's extended-range table
// (design spec §3):
//
//	INT / CHUNK[INT]   int | string | range_expr | list[int]
//	FLOAT              list[float]
//	STRING             list[string]
//	PATH               list[path]
//
// resolve.go's evalRangeExprField targets THE SAME TYPE, from this same
// function -- there is no longer a checker target and a separate resolver
// target to reconcile, and the two layers therefore produce byte-identical
// rejection messages for the same input. What differs between them is only
// what each does with the RESULT: the checker discards it, while the resolver
// dispatches on which union member it landed in (see evalRangeExprField).
//
// WHY int AND string ARE MEMBERS, which is the correction this row needed.
// Section 1.3.12 leaves the INT row "(unchanged, but see RangeString note
// below)", and that note extends the RangeString with an expression
// evaluating to a range_expr or a list[int] "in addition to the original
// <IntRangeExpr> grammar" -- IN ADDITION TO, not instead of. A RangeString is
// itself a Format String (Template Schemas §3.4.1.1.1), so a lone {{...}}
// whose result is an int or a string is still the ordinary base-spec form:
// the expression produced range TEXT, which parseIntRangeExpr then reads. The
// conformance suite states the required target verbatim --
// EXPR/jobs/expr1.2.3--union-target-type.test.yaml:12, "INT task 'range':
// int | string | range_expr | list[int] (match-first)" -- and exercises all
// four members, including "{{ Param.StrFrames }}" with a STRING job parameter
// "1-3" expanding to tasks 1, 2, 3 and "{{ Param.IntCount + 1 }}" with
// IntCount = 7 expanding to the single task 8.
//
// A narrower union (range_expr | list[int], this function's shape before EXPR
// sub-project E4b's whole-branch review) inverted the section's own promise:
// it made DECLARING the extension REMOVE capability at this field, rejecting
// range: "{{Param.Frames}}" with a STRING Frames -- the exact shape all six
// of this repo's own reference render presets use (presets/sqi/*.yaml) --
// while the identical template WITHOUT extensions: [EXPR] expanded correctly.
//
// The union's member ORDER is not load-bearing here: expr.UnionOf sorts
// members into a canonical order anyway, and section 1.2.3's match-first rule
// is implemented by coerce.go's directUnionMember, which compares whole types
// and so is order-independent. Order IS load-bearing one layer down, in
// evalRangeExprField's dispatch on the result -- see that function.
//
// FLOAT/STRING/PATH stay a plain list: their rows extend the field with a
// <ListExpressionString>, which section 1.3.12 defines as "a format string
// containing an expression that evaluates to a list", full stop. Only the INT
// row carries the RangeString note, because only INT/CHUNK[INT] ever had the
// <IntRangeExpr> text form to be "in addition to".
//
// TestCheckParameterSpaceExpressions_RangeTargetTypes pins the resulting
// accept/reject verdicts, not the internal Type value.
func rangeExprFieldType(typ TaskParamType) expr.Type {
	elem := rangeExprElemType(typ)
	switch typ {
	case TaskParamTypeInt, TaskParamTypeChunkInt:
		return expr.UnionOf(expr.TInt, expr.TString, expr.TRangeExpr, expr.ListOf(elem))
	default:
		return expr.ListOf(elem)
	}
}
