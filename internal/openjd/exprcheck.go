// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

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
// concrete. That is phase 1. In phase 2, where params holds a value for a job
// parameter, symbolsFor binds the concrete value instead of the placeholder,
// so the same expression and evaluator either type-check against placeholders
// or run against submitted values.
//
// step may be nil for a job-level position; env may be nil where the position
// is not inside any environment's script; tmpl may carry no parameters;
// params may be nil for phase 1 (or when no concrete values are available).
//
// env identifies which environment's embedded files back Env.File. A template
// can declare many job and step environments, each with its own files, and
// Env.File.X means a different file depending on which one an expression sits
// inside. That cannot be derived from tmpl/step alone (a StepTemplate holds a
// slice of StepEnvironments), so the caller, which walks the template, must
// pass it. A nil env binds no Env.File symbols, the same way a nil step binds
// no Task.File or Task.Param symbols.
//
// This is the only server-side copy of the type-mapping rules (PATH,
// LIST[PATH], CHUNK[INT], and the ParseType floor) — see jobParamTypes for
// the one the worker shares, since it cannot import this package.
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

// SymbolsFor is the exported counterpart of symbolsFor, for
// internal/worker/fmtres's phase-3 agreement test, which checks that the
// worker's phase-3 table types a template's declared parameters identically to
// this package's phase-2 table. See symbolsFor for the contract (what
// step/env/params being nil means, and the scope gating).
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
// the mapping (see its doc comment for why RawParam is not uniformly string).
// The mapping lives in internal/openjd/expr because the worker-side phase-3
// symbol table (internal/worker/fmtres/exprsyms.go) needs the identical
// mapping and cannot import this package (internal/openjd imports
// internal/store, which the worker binary must not depend on). One copy keeps
// phase 2 and phase 3 from typing the same declared parameter differently.
func jobParamTypes(declared string) (paramType, rawType expr.Type) {
	return expr.JobParamTypes(declared)
}

// concreteJobParamValue converts a submitted job-parameter value into a Value
// of type t. Thin wrapper over expr.ValueFromText -- see that function's doc
// comment for the two distinct situations where it returns Unresolved(t)
// (parse failure vs. by-construction) and for the section 1.3.4 float-text
// rationale. Sharing the one definition in internal/openjd/expr keeps this
// package's phase 2 and the worker's phase 3 from drifting apart; see
// jobParamTypes.
//
// PathPOSIX is hardcoded: symbolsFor takes no PathFormat, and no caller in
// this package evaluates with a non-default path format. A caller that set
// expr.WithPathFormat(expr.PathWindows) would get a concrete Param.<path>
// value in a different flavor from the evaluator's own path literals.
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
// The caller picks which files: Task.File. is bound from a step's own script
// (step.Script.EmbeddedFiles), Env.File. from the environment's script the
// caller identifies (env.Script.EmbeddedFiles). These are different sources,
// unlike DeclaredSymbols' scope-blind stepSymbols, which binds both families
// from a step's script because it has no environment context. Passing the
// wrong slice would bind Env.File.<name> from a step's task-script files.
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
// path-mapping rules, which exist only once a session is running.
// internal/openjd/expr registers it with no scope model of its own, so this
// set is the only thing enforcing the restriction.
//
// The restriction is the specification's: the wiki's FunctionLibrary entry
// for with_host_context() is a library "with host-only functions like
// apply_path_mapping() enabled"
// (third_party/openjd-specifications/wiki/2026-02-Expression-Language.md:527).
// That "like" is why this is a set rather than a name comparison. RFC 0005's
// "Host-Context Function Availability" section
// (third_party/openjd-specifications/rfcs/0005-expression-language.md:1163)
// says the same at more length, but the RFC is the proposal behind the
// specification, not the specification; do not cite it as normative.
// apply_path_mapping is the only entry because it is the only function that
// reads session state.
var hostOnlyFunctions = map[string]struct{}{
	"apply_path_mapping": {},
}

// checkHostOnlyFunctions rejects e when scope is not a host context and e
// calls a member of hostOnlyFunctions, per Scope.IsHostContext. It is called
// after a successful expr.Parse and before Eval, so the call never runs in a
// scope where the state it needs does not exist.
//
// It walks Expression.CalledFunctions(), which collects every function and
// method name the expression calls, sorted and de-duplicated, including names
// reached through nested calls and comprehension bodies. Its loop-variable
// exclusion (mirroring Names') does not weaken this check: a call to a
// comprehension's own loop variable cannot reach a registry function of the
// same name (see its doc comment and TestCalledFunctions).
func checkHostOnlyFunctions(e *expr.Expression, scope Scope, ptr string) ValidationErrors {
	if scope.IsHostContext() {
		return nil
	}
	// Fast path: this runs on every expression of every non-host position.
	// CallsAny stops early and allocates nothing but its stack;
	// CalledFunctions builds a map, a slice and a sorted result, so it runs
	// only when there is something to report.
	if !e.CallsAny(hostOnlyFunctions) {
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
// against: a format string that is exactly one reference with no surrounding
// text (fmtstring.LoneRef) is transparent and inherits target, the field's
// declared type. Anything else (a literal, an embedded reference, or a
// reference alongside other text) evaluates each reference unconstrained
// (expr.TAny), because the result is converted to a string whatever the
// reference produces.
//
// Each reference is parsed on its own (expr.Parse) rather than evaluated in
// one step (expr.Eval) so that checkHostOnlyFunctions can run between parse
// and eval: a host-only call is rejected before it executes.
//
// Every Eval call runs under [templateBudget.evalOptions]: the section
// 1.3.9/1.3.10 limits (expr.WithMemoryLimit, expr.WithOperationLimit) plus the
// submission deadline, taken from b. They are tighter than expr.Eval's
// execution-time defaults because this function runs at template validation
// time, synchronously from POST /api/v1/jobs. See [templateBudget.evalOptions]
// for why the options are not a variadic parameter.
//
// b supplies those limits and also receives a wall-clock deadline breach,
// which is recorded on the budget instead of returned as a ValidationError,
// stops the rest of the walk and reaches [ValidateWithBudget] as an error (see
// [templateBudget.recordDeadline]). b may be nil (unit tests pass nil); then
// nothing is diverted and the default submission limits apply.
func checkFormatString(
	b *templateBudget, s, ptr string, scope Scope, syms expr.MapSymbols, target expr.Type,
) ValidationErrors {
	opts := b.evalOptions()
	if body, ok := fmtstring.LoneRef(s); ok {
		e, err := expr.Parse(body)
		if err != nil {
			return ValidationErrors{{Pointer: ptr, Message: err.Error()}}
		}
		if errs := checkHostOnlyFunctions(e, scope, ptr); len(errs) != 0 {
			return errs
		}
		if _, err := e.Eval(syms, target, opts...); err != nil {
			if b.recordDeadline(err) {
				return nil
			}
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
			if b.recordDeadline(err) {
				// Stop, rather than reporting and moving to the next segment:
				// the remaining segments would each evaluate past a deadline
				// that has already passed.
				return errs
			}
			errs = append(errs, ValidationError{Pointer: ptr, Message: err.Error()})
		}
	}
	return errs
}

// checkLetBindings evaluates a let: block's bindings in declaration order,
// inserting each one's value into syms so the next binding can reference it,
// and returns one error per binding that fails.
//
// It mutates syms. Section 3.6 says "Later bindings can reference names from
// earlier bindings in the same let block", and because the symbol table holds
// values rather than types, inserting the evaluated result gives section
// 3.6.1's "the type of the binding is the natural result type of the
// expression" and phase-2 concreteness at once, through the same code path
// every other position uses.
//
// A failed binding is not inserted, so a later position reports an unknown
// name rather than a second copy of the same fault. Evaluation continues to
// the next binding either way, so one malformed line does not hide the rest
// of the block.
//
// Self-reference needs no check: "x = x + 1" fails because x is inserted only
// after its own expression evaluates.
//
// Section 3.6 also forbids a binding that "shadows a previous binding in the
// same let block or any enclosing scope". One lookup covers both cases: when
// a nested block's bindings are checked, the enclosing scope's names are
// already in syms, so "a name already present in syms may not be bound"
// applies to both. Because the check is keyed off syms, it would also reject
// shadowing a spec-defined symbol (Param, Task, Session, Env, RawParam);
// section 3.6.1's lowercase-first <UserIdentifier> rule keeps those
// namespaces disjoint, but this check does not rely on it.
//
// # The maxLetBindings guard
//
// Evaluation stops after [maxLetBindings] bindings. That is a
// resource-exhaustion guard, not a diagnostic: [validateLetElementCounts] has
// already reported the over-count (it runs unconditionally, before the walk,
// and is not gated by EnforceLimits), so a second error here would duplicate
// it at a different pointer.
//
// The guard lives here, where the cost is incurred, so it holds regardless of
// upstream ordering and of EnforceLimits, and also for unit tests that call
// this function directly. Phase 2 (checkExpressionsAtSubmit in submit.go)
// does not bypass validateLetElementCounts: prepareTemplate runs
// ValidateWithOptions first and returns on any error.
//
// let is the only construct in this checker that retains values across
// evaluations. Every other position goes through checkFormatString, which
// discards each result, so a per-Eval budget ([ExprLimits.evalOptions]) is
// enough there. Here each binding may allocate just under the per-evaluation
// memory limit and keep it, so without the guard N bindings would hold N
// budgets of live memory. A template-wide operation budget would not catch
// that, because building a large string costs almost no operations.
//
// Truncating rather than abandoning the walk means a template with too many
// bindings still gets every other expression error reported in the same
// pass, alongside the count error.
//
// Every Eval call runs under [templateBudget.evalOptions], as
// [checkFormatString]'s do, so operator-configured limits reach every let
// binding through the same single point as every format string.
//
// b is the walk's [templateBudget], may be nil, and carries a wall-clock
// deadline breach out of this function as it does out of
// [checkFormatString]. The difference: an ordinary binding failure appends
// and continues, while a deadline breach returns at once. See
// [templateBudget.recordDeadline].
func checkLetBindings(
	b *templateBudget, lets []string, base string, scope Scope, syms expr.MapSymbols,
) ValidationErrors {
	if len(lets) > maxLetBindings {
		lets = lets[:maxLetBindings]
	}
	opts := b.evalOptions()

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
			if b.recordDeadline(err) {
				// Return rather than continue: past the deadline there is no time to
				// spend on the remaining bindings, and the breach is reported
				// through b rather than as a per-binding ValidationError.
				return errs
			}
			errs = append(errs, ValidationError{Pointer: ptr, Message: err.Error()})
			continue
		}
		syms[name] = v
	}
	return errs
}

// ─── submission-time limits ─────────────────────────────────────────────────

// defaultSubmissionOperations and defaultSubmissionMemoryBytes are the default
// per-evaluation limits (sections 1.3.9/1.3.10) for every submission-time
// evaluation, checkFormatString's and checkLetBindings' alike. They are
// tighter than expr.Eval's execution-time defaults (10,000,000 operations /
// 100,000,000 bytes) because checkTemplateExpressions runs at template
// validation time, synchronously from POST /api/v1/jobs, not on a worker.
// The specification's operation count does not bound wall-clock time: rule 3
// prices string work at ceil(len/256), so a 10,000-character .upper() costs
// about 41 operations, and a loop can make a quarter of a million such calls
// within 10,000,000 operations.
//
// Both defaults are sized to avoid rejecting a legitimate template:
//
//   - Phase 1 (checkTemplateExpressions with params == nil, from
//     ValidateWithOptions) binds every Param./RawParam./Task.Param. symbol as
//     an unresolved placeholder, which costs a handful of operations whatever
//     the submitted value will be. Real cost at this phase can only come from
//     computation written into the template text: a large string or list
//     literal, or a loop over one.
//   - Phase 2 (checkExpressionsAtSubmit, submit.go, after job parameters are
//     bound) re-runs the same walk with Param./RawParam. bound to the
//     submitted values, so a large submitted value costs real bytes and
//     per-character operations, as a literal would, and is bounded by the
//     same limits. Phase 2 is the phase that can be expensive per evaluation.
//     For example, `[Param.S.upper() for i in range(10)]` with a 900,000-byte
//     Param.S exceeds the 1 MB memory limit, because the bound value and its
//     .upper() copy are both live.
//   - defaultSubmissionOperations (10,000) leaves about two orders of
//     magnitude of headroom over a realistic submission-time expression (a
//     few Param references, arithmetic, string formatting, or a
//     comprehension over a small literal list cost tens to low hundreds of
//     operations).
//   - defaultSubmissionMemoryBytes (1,000,000 bytes) is generous for real
//     template text (a job name, command or args entry is well under a few
//     KB) and catches a large single-string literal long before limits.go's
//     fixed maxStringBytes (10,000,000 bytes). maxStringBytes bounds one
//     produced string, while this limit bounds the sum of live values and
//     recurses into containers, so it is the only bound on a list of
//     individually legal strings. The same limit applies to a large concrete
//     parameter value in phase 2, through the same checkFormatString call and
//     [templateBudget.evalOptions].
//
// A template whose expressions need more than this, against placeholders or
// against submitted values, is rejected as a submission-time liability.
//
// These are defaults. The enforced values come from [ExprLimits]
// (exprlimits.go), set by internal/config through
// [ValidateOptions.ExprLimits] / [SubmitterOptions.ExprLimits] and carried to
// every evaluation by the walk's [templateBudget]. internal/config's ceilings
// let an operator raise either by one order of magnitude; both ceilings are
// policy limits, not derived from a fixed guard.
const (
	defaultSubmissionOperations  int64 = 10_000
	defaultSubmissionMemoryBytes int64 = 1_000_000
)

// ─── template-wide budget ───────────────────────────────────────────────────

// defaultTemplatePositions and defaultTemplateRetainedBytes are the two
// dimensions of the budget checkTemplateExpressions enforces across one whole
// walk. defaultSubmissionOperations/defaultSubmissionMemoryBytes (above) bound
// what one expression may cost; these bound what the entire template may
// cost in the scope checker, in let: blocks and in the resolver.
//
// Operations are not tracked here. With a position cap and a per-position
// operation cap, the cumulative operation ceiling is at most
// defaultTemplatePositions * defaultSubmissionOperations (10,000 x 10,000 =
// 10^8). That bound holds only because internal/openjd/expr refuses a bulk
// operation on its arithmetic count before allocating (meter.reserve). A
// charge levied after the value exists would not bound the work:
// "[0] * 10000000" would build ten million elements before reporting
// 10,000,001 operations, leaving limits.go's maxElements as the real bound.
//
// The operation ceiling does not bound wall-clock time, because operations
// are not uniform in cost: section 1.3.10 rule 3 prices 256 bytes of string
// work at one operation, so byte-heavy work over a ~900 KB string (the
// largest defaultSubmissionMemoryBytes allows to be live), such as a regex or
// a case mapping, costs tens of milliseconds for a few thousand operations.
// Wall-clock time is bounded by the submission deadline
// ([ExprLimits.Deadline]).
const (
	// defaultTemplatePositions caps the number of format-string and
	// let-binding positions one call to checkTemplateExpressions may check:
	// one unit per checkFormatString call and one per let: binding attempted
	// (letPositions, below, mirrors checkLetBindings' maxLetBindings
	// truncation, so the charge matches the work done rather than the count
	// the template declared).
	//
	// 10,000 allows an average of 100 positions per step at maxSteps (100).
	// Nothing in this package caps Action.Args, EmbeddedFiles or
	// Environment.Variables, so no other constant bounds a step's position
	// count. maxSteps applies only when opts.EnforceLimits is set, while this
	// budget always applies; with enforce_limits: false the step count is
	// unbounded and this cap's absolute value is the only bound on the walk.
	//
	// The sizing assumes a generous hand-authored render-pipeline step:
	//
	//	command + timeout                                            2
	//	args (a renderer's CLI flags)                               20
	//	let (step template + step script, 5 bindings each)          10
	//	host requirements (3 amounts x min/max, 2 attributes x 4)   14
	//	range (one RangeExpr)                                         1
	//	one step environment (5 vars, 2 files, onEnter+onExit
	//	  command+3 args each, its own script let of 5)             20
	//	step's own script embedded files                             3
	//	                                                     --------
	//	                                                             70 / step
	//
	// x100 steps = 7,000, plus about 40 job-level positions (the job name and
	// a couple of job environments), for about 7,040. 10,000 gives that shape
	// ~1.4x headroom and a more modest one (a command, 15 args, 3 variables
	// and 2 embedded files per step: 2,100) ~4.8x. A 100-step template that
	// uses every category above at that level in every step is close to the
	// cap, and one with about twice that, or maximal host requirements
	// (maxHostRequirements = 50) in most steps, is rejected; the operator
	// setting is [ExprLimits.TemplatePositions].
	//
	// One step's task-parameter range positions can reach 16,384 within every
	// structural cap (maxTaskParameterDefinitions x maxTaskParamValues), so
	// this budget is what catches that construction; see
	// TestCheckTemplateExpressions_TemplateWideBudget_CatchesWhatThePreWalkGuardAdmits.
	//
	// This cap is not a wall-clock bound, and no value of it can be one while
	// an operation's cost varies by orders of magnitude between scalar
	// arithmetic and a regex over a megabyte. internal/openjd/expr reserves
	// bulk range_expr expansions (counting a multi-sub-range one, comparing
	// against one on the right of ==, coercing one to list[int]) from
	// arithmetic bounds before performing them (reserveRangeExprExpansion,
	// rangeexpr.go), so bulk materialization is bounded; byte-heavy string
	// work is bounded only by the deadline. The cases are enumerated in
	// internal/openjd/expr/reservework_internal_test.go. Treat any worst case
	// stated for this cap as a floor.
	//
	// The worker's cap is tied to this one.
	// internal/worker/fmtres's DefaultAssignmentPositions bounds the same
	// quantity per assignment in phase 3, and an assignment's positions are a
	// subset of its template's, so a worker cap below this value would fail
	// every task of a job this package accepted.
	// TestTemplateBudget_WorkerCapIsNotTighter (exprcheck_budget_test.go)
	// fails if the two drift apart.
	defaultTemplatePositions int64 = 10_000

	// defaultTemplateRetainedBytes caps the cumulative section 1.3.9 size
	// (expr.SizeOf) of every value every let: block in the template adds to
	// its symbol table, summed across the whole call, not reset per block or
	// per step. Every other position discards its result (checkFormatString),
	// so only let bindings are charged; see
	// checkStepExpressions/checkEnvironmentExpressions for where each of the
	// three let: positions (step template, step script, environment script)
	// is charged.
	//
	// 10,000,000 (10 MB) matches the worker's defaultLetRetainedBytes
	// (internal/worker/fmtres), so the server and the worker bound retained
	// let bytes with the same figure. It also bounds what maxLetBindings
	// does not: that cap bounds one block's binding count, not the bytes the
	// bindings retain. Because the counter is cumulative, many individually
	// small blocks that are large together also trip it.
	//
	// The charge is applied once per let: block, after checkLetBindings
	// finishes evaluating it (a before/after delta or a stepLetSymbols diff
	// at the call sites), not per binding. A single block can therefore
	// transiently retain up to maxLetBindings x defaultSubmissionMemoryBytes
	// = 50 x 1,000,000 = 50 MB before the counter sees it and rejects the
	// template. The worker instead stops within a block once its running
	// total crosses its limit.
	//
	// The counter measures cumulative allocation across the walk, not peak
	// live retention: a template whose blocks each hold a few MB but which
	// declares many of them is charged for their sum.
	defaultTemplateRetainedBytes int64 = 10_000_000
)

// templateBudget is one phase's allowance against defaultTemplatePositions
// and defaultTemplateRetainedBytes. Phase 1 (ValidateWithOptions, params ==
// nil) and phase 2 (checkExpressionsAtSubmit, boundParams concrete) each get
// their own budget: they are separate walks with separate symbol tables, and
// a shared budget would make phase 2's verdict depend on what phase 1 spent,
// with no way for the submitter to see why.
//
// Once either dimension trips, err is set exactly once and every further
// charge call is a no-op that returns false. Callers gate further work on
// ok() or on a charge call's return value, so the walk stops parsing and
// evaluating once the budget is spent.
//
// The budget also carries the operator-configured limits ([ExprLimits],
// exprlimits.go) for its walk, because b is already in scope at every
// evaluation site; see [ExprLimits.evalOptions] and
// [templateBudget.evalOptions]. limits is normalized by [newTemplateBudget]
// and is never the zero value on a live budget.
type templateBudget struct {
	positions int64
	retained  int64
	limits    ExprLimits
	// evalOpts is limits.evalOptions() built once, in [newTemplateBudget], and
	// read through [templateBudget.evalOptions] at every metered position.
	//
	// Caching is safe because limits is fixed for the walk's life: every
	// constructor (validate.go's ValidateWithBudget, submit.go's
	// prepareTemplate for both phase-2 budgets) finishes assembling its
	// ExprLimits, including the per-request [ExprLimits.Deadline], before
	// calling newTemplateBudget, and nothing writes limits afterwards. Code
	// that mutates limits mid-walk must rebuild this field.
	//
	// Sharing is safe because expr treats an option slice as read-only: the
	// options are applied to a fresh evalCtx per evaluation and the slice is
	// never appended to.
	evalOpts []expr.Option
	err      *ValidationError
	// deadlineErr holds the first [expr.ErrDeadlineExceeded] any evaluation in
	// this walk returned. It is an error, not a *ValidationError; see
	// [templateBudget.recordDeadline].
	deadlineErr error
}

// newTemplateBudget returns a fresh budget bounded by lim, with every unset
// field of lim replaced by its default ([ExprLimits.orDefaults]) -- so
// newTemplateBudget(ExprLimits{}) is exactly the default allowance.
func newTemplateBudget(lim ExprLimits) *templateBudget {
	lim = lim.orDefaults()
	return &templateBudget{limits: lim, evalOpts: lim.evalOptions()}
}

// templateBudgetOrFresh returns budget[0] when the caller supplied one (e.g.
// a budget shared by every step's ResolveParameterSpaceParams call for one
// submission), or a new [templateBudget] otherwise.
//
// The trailing variadic parameter lets callers with no budget to share
// (unit tests in this package and direct callers of
// [ResolveParameterSpaceParams]) omit it.
//
// budget[0] == nil is treated as "no budget supplied": a nil budget must
// never reach chargePositions/chargeRetainedBytes, which write through the
// pointer unconditionally.
//
// A budget created here carries default limits, because this function has no
// access to operator configuration. A caller that wants configured limits
// must supply a budget, as every production path does (validate.go's
// ValidateWithOptions and submit.go's prepareTemplate both build one from
// their options).
func templateBudgetOrFresh(budget []*templateBudget) *templateBudget {
	if len(budget) > 0 && budget[0] != nil {
		return budget[0]
	}
	return newTemplateBudget(ExprLimits{})
}

// ok reports whether the walk may still do real work: neither per-walk
// dimension has tripped and no evaluation has hit the wall-clock deadline.
//
// A nil budget is ok. Production always supplies a budget; only this
// package's unit tests pass nil (see [checkFormatString]), and get no
// template-wide bound and no deadline.
func (b *templateBudget) ok() bool { return b == nil || (b.err == nil && b.deadlineErr == nil) }

// deadline returns the wall-clock breach this walk recorded, or nil. It is
// nil-safe so a caller holding an optional budget can ask without a guard.
//
// Every caller that can observe a budget must consult it. A recorded deadline
// stops the walk without adding a ValidationError, so a caller that tests
// only len(errs) would report a partial result as success.
// [ValidateWithBudget], [checkExpressionsAtSubmit] and
// [ResolveParameterSpaceParams]' caller each check it.
func (b *templateBudget) deadline() error {
	if b == nil {
		return nil
	}
	return b.deadlineErr
}

// limitsOrDefaults returns the budget's [ExprLimits], or the package defaults
// when b is nil.
//
// Use it instead of reading b.limits directly, which would panic on the nil
// budget that [checkFormatString], [checkLetBindings] and [stepLetSymbols]
// accept; every other budget method is nil-safe.
func (b *templateBudget) limitsOrDefaults() ExprLimits {
	if b == nil {
		return ExprLimits{}.orDefaults()
	}
	return b.limits
}

// evalOptions returns the [expr.Option] tail every evaluation in this walk runs
// under: the section 1.3.9/1.3.10 limits this budget carries, plus the
// wall-clock deadline when one is set.
//
// checkFormatString, checkLetBindings and stepLetSymbols derive their options
// from the budget they receive rather than taking an opts parameter. A
// variadic `opts ...expr.Option` would compile and pass tests when a caller
// passed nothing, silently replacing the operator-configured limits with
// expr.Eval's much looser defaults and dropping [ExprLimits.Deadline], which
// rides on the same slice. The worker (internal/worker/fmtres) has the same
// shape.
//
// A nil budget (passed only by this package's unit tests) gets the default
// submission options, matching [templateBudget.limitsOrDefaults].
//
// A non-nil budget returns the slice [newTemplateBudget] cached; see the
// evalOpts field for why caching and sharing it are safe. The evalOpts == nil
// arm makes a budget built as a bare struct literal, rather than through the
// constructor, behave like a nil budget instead of an unmetered evaluation.
func (b *templateBudget) evalOptions() []expr.Option {
	if b == nil || b.evalOpts == nil {
		return b.limitsOrDefaults().evalOptions()
	}
	return b.evalOpts
}

// recordDeadline reports whether err is a wall-clock deadline breach and, if
// it is, records it on the budget so the rest of the walk stops.
//
// This is the one place the 503/422 distinction is drawn. Every other
// evaluation failure in this checker becomes a ValidationError, a verdict
// that the template is invalid, which a caller reports as a 422. A deadline
// means this server ran out of time and the same body could validate on an
// idle machine, so the answer is a 503. Converting it to a ValidationError
// would make acceptance depend on machine load.
//
// Recording it also stops the walk: it makes [templateBudget.ok] false, and
// every caller in this file gates further work on ok(). A local return at
// each conversion site would not be enough, because [checkLetBindings]
// appends and continues on a binding failure.
//
// The first deadline error wins. A nil budget records nothing and reports
// false, so a unit-test caller sees the error as a ValidationError.
func (b *templateBudget) recordDeadline(err error) bool {
	if !errors.Is(err, expr.ErrDeadlineExceeded) {
		return false
	}
	if b == nil {
		return false
	}
	if b.deadlineErr == nil {
		b.deadlineErr = err
	}
	return true
}

// chargePositions charges n additional walk positions, naming ptr as the
// position that exhausted the budget if this call is the one that does. It
// returns whether the caller may still do the work those positions
// represent -- false either because this call itself tripped the budget or
// because an earlier call (either dimension) already had.
func (b *templateBudget) chargePositions(n int64, ptr string) bool {
	// !b.ok() rather than b.err != nil: the budget also carries a
	// wall-clock deadline breach, which must stop the walk exactly as an
	// exhausted dimension does even though it is not a ValidationError.
	if !b.ok() {
		return false
	}
	b.positions += n
	if b.positions > b.limits.TemplatePositions {
		b.err = &ValidationError{
			Pointer: ptr,
			Message: fmt.Sprintf(
				"template-wide expression budget exceeded: at most %d expression positions may be "+
					"checked in one validation pass (reached %d at %s)",
				b.limits.TemplatePositions, b.positions, ptr,
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
	if !b.ok() {
		return false
	}
	if n <= 0 {
		return true
	}
	b.retained += n
	if b.retained > b.limits.TemplateRetainedBytes {
		b.err = &ValidationError{
			Pointer: ptr,
			Message: fmt.Sprintf(
				"template-wide expression budget exceeded: let bindings may retain at most %d bytes "+
					"across the whole template (reached %d at %s)",
				b.limits.TemplateRetainedBytes, b.retained, ptr,
			),
		}
		return false
	}
	return true
}

// errs returns the ValidationError recording why the budget tripped, or nil
// if it never did. checkTemplateExpressions appends it to the errors the walk
// collected before the budget stopped it.
func (b *templateBudget) errs() ValidationErrors {
	if b.err == nil {
		return nil
	}
	return ValidationErrors{*b.err}
}

// letPositions is the number of positions one let: block costs the budget:
// min(n, maxLetBindings), mirroring checkLetBindings' truncation so the
// charge matches the bindings it evaluates, not the count the template
// declared.
func letPositions(n int) int64 {
	if n > maxLetBindings {
		return int64(maxLetBindings)
	}
	return int64(n)
}

// templateExprRetainedBytes is the section 1.3.9 size of every value a
// symbol table holds, summed. It is a copy of internal/worker/fmtres's
// tableRetainedBytes: internal/openjd cannot depend on internal/worker, and
// internal/worker cannot depend on internal/openjd (which imports
// internal/store), so both build on the package they share, through
// expr.SizeOf.
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
// It runs only when tmpl declares the EXPR extension; a nil tmpl or one that
// does not is a no-op. Without EXPR, validate.go's base-spec path
// (validateFormatString, gated on the same declaration) covers these
// positions with base-spec semantics; running the expression evaluator there
// would be redundant, and wrong for a body the base-spec reader rejects but
// EXPR's grammar parses differently.
//
// ValidateWithOptions also skips this call for a parameter space over
// parameterSpaceOverCaps' two count caps, a cost guard whose verdict phase 2
// does not inherit (see that call site). Unit tests that call this function
// directly bypass that guard.
//
// params is nil for phase 1 (every job-parameter symbol unresolved, called
// from ValidateWithOptions) and holds concrete values for phase 2 (called
// from submit.go's checkExpressionsAtSubmit once job parameters are bound);
// see symbolsFor.
//
// The walk mirrors validate.go's traversal (ValidateWithOptions ->
// validateStep -> validateEnvironments/validateParameterSpace/
// validateHostRequirements -> validateAction/validateScriptRefs) position
// for position:
//
//	job name                          ScopeJob                TargetString
//	host requirement values           ScopeJob + step let     TargetString
//	task-parameter range entries      ScopeJob + step let     §1.3.12 per-type
//	task-parameter chunk sizing       ScopeJob + step let     expr.TInt (checkChunkExpressions)
//	environment variable values       job/step env            TargetString
//	env + step embedded-file data     job/step env / step     TargetString
//	action command                    matching env / step     TargetString
//	action args entries               matching env / step     TargetArgItem
//	action timeout                    matching env / step     TargetInt
//
// Two rows need explanation:
//
//   - "ScopeJob + step let" is the scope ScopeJob with the enclosing step
//     template's let-bound names merged into its symbol table, which is what
//     section 3.6.2 row 1 grants (rangeScopeSymbols). The scope is
//     unchanged: a task-level symbol is still out of reach at both positions.
//   - A task-parameter range position targets section 1.3.12's per-type type
//     (rangeExprElemType for a range-list entry, rangeExprFieldType for the
//     whole field), and resolve.go targets the same types at the same
//     positions. See checkParameterSpaceExpressions.
//
// Action timeout is wired here but is inert for a real template:
// decodeAction (parse.go) decodes "timeout" as a strict integer, so no
// format-string body can reach this position unless that decoder changes.
func checkTemplateExpressions(tmpl *JobTemplate, params map[string]string, budget ...*templateBudget) ValidationErrors {
	if tmpl == nil || !tmpl.hasExtension("EXPR") {
		return nil
	}

	// b is the caller's budget, or a fresh default one (templateBudgetOrFresh).
	// ValidateWithBudget (phase 1) and prepareTemplate (phase 2, via
	// checkExpressionsAtSubmit) each pass their own, so the two phases have
	// separate allowances (see templateBudget).
	b := templateBudgetOrFresh(budget)
	var errs ValidationErrors

	if b.chargePositions(1, "/name") {
		errs = append(errs, checkFormatString(
			b, tmpl.Name, "/name", ScopeJob, symbolsFor(tmpl, nil, nil, ScopeJob, params), TargetString,
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
// hostRequirements, parameterSpace and script, the other positions of this
// function. stepLet holds exactly those bound names (see stepLetSymbols), not
// the whole post-let table, which also carries Job.Name, Step.Name, Param.*
// and RawParam.* entries that ScopeStepTemplate exposes but ScopeJob (below)
// does not. Propagating the whole table would make a bare Step.Name legal in
// a host requirement or task-parameter range; see jobSyms' comment below.
func checkStepExpressions(b *templateBudget, tmpl *JobTemplate, s StepTemplate, idx int, params map[string]string) ValidationErrors {
	var errs ValidationErrors
	base := fmt.Sprintf("/steps/%d", idx)

	// stepLetSymbols runs whatever b's state: it is bounded on its own
	// (maxLetBindings bindings, each under the per-evaluation memory limit,
	// via checkLetBindings). b is charged after it runs; stepLet is already
	// the diff stepLetSymbols computed, so templateExprRetainedBytes(stepLet)
	// is the net bytes the block added.
	stepLet, letErrs := stepLetSymbols(b, tmpl, &s, params, base)
	errs = append(errs, letErrs...)
	b.chargePositions(letPositions(len(s.Let)), base+"/let")
	b.chargeRetainedBytes(templateExprRetainedBytes(stepLet), base+"/let")

	if s.Script != nil && b.ok() {
		// syms is a clone of stepLet: the step script's own let: block
		// (section 3.6.2 row 2) is evaluated over it next, and
		// checkLetBindings mutates the table it is handed. Using stepLet
		// itself would leak the script's bound names into
		// stepEnvironments/hostRequirements/parameterSpace below, which
		// section 3.6.2 does not grant.
		syms := maps.Clone(stepLet)
		maps.Copy(syms, symbolsFor(tmpl, &s, nil, ScopeStepScript, params))
		// checkLetBindings only adds keys (the shadow check rejects rebinding
		// one already present), so the before/after delta of the whole
		// table's retained bytes is this block's net contribution.
		before := templateExprRetainedBytes(syms)
		errs = append(errs, checkLetBindings(
			b, s.Script.Let, base+"/script/let", ScopeStepScript, syms,
		)...)
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
	// do not exist while its host is being selected or its task-parameter
	// space is being defined, so step is not passed to symbolsFor here;
	// ScopeJob's symbol families never include them (scope.go's
	// scopeFamilies).
	//
	// Section 3.6.2 row 1 changes the table, not the scope: jobSyms gains the
	// step's let-bound names (stepLet, merged in below) while keeping
	// ScopeJob's fixed/family set as symbolsFor(..., ScopeJob, ...) built it,
	// so a let name bound by the step template is visible here and a
	// task-level symbol is not.
	//
	// Known gap: ScopeJob's fixed symbols (scopeFixed) are nil, so a bare
	// Job.Name or Step.Name is rejected at both positions, which section
	// 7.3.1 does not permit. 7.3.1's symbol table says Step.Name is
	// "Available within the Step Template scope: stepEnvironments,
	// hostRequirements, parameterSpace, and script", and Job.Name is
	// "Available in every Format String in the Job Template, except the name
	// field of the Job Template itself". The rejections:
	//
	//	/steps/0/hostRequirements/attributes/0/anyOf/0: col 1: unknown symbol "Step.Name"
	//	/steps/0/hostRequirements/attributes/0/anyOf/0: col 1: unknown symbol "Job.Name"
	//	/steps/0/parameterSpace/taskParameterDefinitions/0/range/0: col 1: unknown symbol "Step.Name"
	//
	// Section 3.6.2 governs where let names are visible and is silent on the
	// fixed symbols, so it does not justify the rejection. Correcting it
	// means splitting scopeFixed(ScopeJob) or adding a distinct scope for
	// these two positions, which can change conformance scoring; see
	// docs/openjd-conformance.md. No fixture covers it.
	jobSyms := rangeScopeSymbols(tmpl, stepLet, params)

	if b.ok() && s.ParameterSpace != nil {
		errs = append(errs, checkParameterSpaceExpressions(b, *s.ParameterSpace, base+"/parameterSpace", jobSyms)...)
	}
	if b.ok() && s.HostRequirements != nil {
		errs = append(errs, checkHostRequirementExpressions(b, *s.HostRequirements, base+"/hostRequirements", jobSyms)...)
	}

	return errs
}

// stepLetSymbols evaluates a step template's own let: block (section 3.6.2
// row 1) at ScopeStepTemplate and returns exactly the names it bound: the
// set difference between the ScopeStepTemplate table before and after
// checkLetBindings runs. The whole post-let table also carries Job.Name,
// Step.Name, Param.* and RawParam.* entries that ScopeStepTemplate exposes
// and the narrower positions below it do not; propagating it would make a
// bare Step.Name legal in a host requirement or a task-parameter range.
//
// base is the step's JSON pointer prefix; each binding's errors are reported
// at base + "/let/<i>". Pass "" for a caller whose pointers are already
// relative to the step (resolve.go's newStepScope, whose caller in submit.go
// prefixes "/steps/<i>").
//
// The checker (checkStepExpressions) and the resolver
// (ResolveParameterSpaceParams) both call this so their tables cannot
// differ. If they did, a step-level let: ["base = 10"] plus
// range: "{{ [base, base + 1] }}" could pass validation and the phase-2
// re-check and then fail in expandStepTaskParams with unknown symbol "base".
//
// b is the walk's [templateBudget] rather than its [ExprLimits] because
// checkLetBindings reports a wall-clock deadline breach through it. It may be
// nil, as for [checkFormatString] and [checkLetBindings]: a nil b gives the
// default limits and diverts nothing. No production caller passes nil.
func stepLetSymbols(
	b *templateBudget, tmpl *JobTemplate, s *StepTemplate, params map[string]string, base string,
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
	errs := checkLetBindings(
		b, s.Let, base+"/let", ScopeStepTemplate, stepTemplateSyms,
	)
	for k, v := range stepTemplateSyms {
		if _, existed := preLetKeys[k]; !existed {
			stepLet[k] = v
		}
	}
	return stepLet, errs
}

// rangeScopeSymbols builds the symbol table the two ScopeJob-scoped positions
// inside a step (its parameterSpace range fields and its hostRequirements
// values) are checked and resolved against: ScopeJob's fixed and family
// symbols plus the enclosing step template's let-bound names (stepLet).
//
// Section 3.6.2 row 1 changes the table, not the scope: a name the step
// template's let: bound is visible here, and a task-level symbol
// (Task.Param./Task.RawParam./Task.File.) is not, because ScopeJob's
// families never include them. See checkStepExpressions for the
// Job.Name/Step.Name gap.
//
// Shared with resolve.go for the same reason as stepLetSymbols.
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
// outerLet carries the enclosing step template's let-bound names (section
// 3.6.2 row 1: bound names are visible in the whole stepEnvironments
// subtree); it is nil for job environments. It is folded into baseSyms,
// which backs both the variables and (via scriptSyms) the script positions,
// because Variables is a descendant of the stepEnvironments element row 1
// names.
//
// An environment's own EnvironmentScript.let (row 4) is narrower: its bound
// names are visible only in actions and embeddedFiles, EnvironmentScript's
// only child elements. Variables sits on the parent Environment, a sibling of
// Script, so the environment's own let is bound into scriptSyms but never
// into baseSyms.
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

		// baseSyms is a clone of outerLet: outerLet is the same map across
		// every iteration of this loop, so writing this environment's
		// additions into it would leak into the next environment's table. A
		// nil outerLet (job environments) clones to nil, so the map is
		// allocated explicitly for the writes below.
		baseSyms := maps.Clone(outerLet)
		if baseSyms == nil {
			baseSyms = expr.MapSymbols{}
		}
		// symbolsFor needs this environment's own script to bind Env.File.:
		// a template may declare many environments, each with its own files.
		maps.Copy(baseSyms, symbolsFor(tmpl, step, &e, scope, params))

		// Sorted so errors do not depend on map order, as in
		// validateScriptRefs. Checked against baseSyms, before this
		// environment's own EnvironmentScript.let is bound (see the function
		// doc).
		for _, k := range slices.Sorted(maps.Keys(e.Variables)) {
			varPtr := ptr + "/variables/" + k
			if !b.chargePositions(1, varPtr) {
				break
			}
			errs = append(errs, checkFormatString(
				b, e.Variables[k], varPtr, scope, baseSyms, TargetString,
			)...)
		}

		// scriptSyms is a clone of baseSyms so the environment's own let:
		// bindings never reach baseSyms. With the current order (the
		// variables loop above runs first and nothing reads baseSyms
		// afterwards) the clone has no observable effect; it keeps the
		// separation if the variables loop moves below this line or baseSyms
		// gains a later reader.
		scriptSyms := maps.Clone(baseSyms)
		if e.Script != nil && b.ok() {
			// Before/after delta, as in checkStepExpressions' script let:
			// checkLetBindings only adds keys, so the baseline cancels out.
			before := templateExprRetainedBytes(scriptSyms)
			errs = append(errs, checkLetBindings(
				b, e.Script.Let, ptr+"/script/let", scope, scriptSyms,
			)...)
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
// environment's variable values are a different position, checked by
// checkEnvironmentExpressions against its own, possibly narrower, symbol
// table (see that function for why the two cannot share one table).
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
			b, f.Data, ptr, scope, syms, TargetString,
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
// list[string] flattens inline.
//
// The timeout check below cannot be reached from a real template.
// decodeAction (parse.go) decodes "timeout" with a strict integer parse
// (intFieldStrict/scalarToInt) for every template, EXPR or not, so
// a.TimeoutSeconds is always a resolved int and its strconv.Itoa rendering
// has no "{{" references. A timeout such as
// "{{ len(apply_path_mapping(Param.Val)) }}"
// (EXPR/job_templates/7.3--apply-path-mapping-in-timeout.invalid.yaml) fails
// in openjd.Parse with "openjd: timeout must be an integer", before this
// function runs. Making the position live means changing decodeAction to
// accept a format string for "timeout", for base-spec templates too. The call
// stays, unconditional when TimeoutSet, so the position is wired if that
// decoder changes.
func checkActionExpressions(b *templateBudget, a Action, ptr string, scope Scope, syms expr.MapSymbols) ValidationErrors {
	var errs ValidationErrors
	cmdPtr := ptr + "/command"
	if b.chargePositions(1, cmdPtr) {
		errs = append(errs, checkFormatString(
			b, a.Command, cmdPtr, scope, syms, TargetString,
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
			b, arg, argPtr, scope, syms, TargetArgItem,
		)...)
	}
	if a.TimeoutSet && b.ok() {
		timeoutPtr := ptr + "/timeout"
		if b.chargePositions(1, timeoutPtr) {
			errs = append(errs, checkFormatString(
				b, strconv.Itoa(a.TimeoutSeconds), timeoutPtr, scope, syms, TargetInt,
			)...)
		}
	}
	return errs
}

// checkHostRequirementExpressions checks a step's host requirement values --
// every amount's min/max and every attribute's anyOf/allOf entries -- against
// the ScopeJob symbol table syms. validate.go's validateHostRequirements is
// the parallel base-spec check.
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
// complexity within the repo's lint budget.
func checkHostRequirementAmount(b *templateBudget, a AmountRequirement, amtPtr string, syms expr.MapSymbols) ValidationErrors {
	errs := checkCapabilityName(b, a.Name, amtPtr+"/name", syms)
	if a.Min != nil && b.ok() {
		minPtr := amtPtr + "/min"
		if b.chargePositions(1, minPtr) {
			errs = append(errs, checkFormatString(
				b, *a.Min, minPtr, ScopeJob, syms, TargetString,
			)...)
		}
	}
	if a.Max != nil && b.ok() {
		maxPtr := amtPtr + "/max"
		if b.chargePositions(1, maxPtr) {
			errs = append(errs, checkFormatString(
				b, *a.Max, maxPtr, ScopeJob, syms, TargetString,
			)...)
		}
	}
	return errs
}

// checkHostRequirementAttribute checks one attribute's anyOf/allOf entries --
// extracted for the same reason as checkHostRequirementAmount, above.
func checkHostRequirementAttribute(b *templateBudget, a AttributeRequirement, attrPtr string, syms expr.MapSymbols) ValidationErrors {
	errs := checkCapabilityName(b, a.Name, attrPtr+"/name", syms)
	for k, v := range a.AnyOf {
		if !b.ok() {
			break
		}
		p := fmt.Sprintf("%s/anyOf/%d", attrPtr, k)
		if b.chargePositions(1, p) {
			errs = append(errs, checkFormatString(
				b, v, p, ScopeJob, syms, TargetString,
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
				b, v, p, ScopeJob, syms, TargetString,
			)...)
		}
	}
	return errs
}

// checkCapabilityName checks an amount's or attribute's name, a format string
// (Template Schemas §3.3). A name with no expression in it is not charged a
// position: there is nothing to evaluate.
func checkCapabilityName(b *templateBudget, name, ptr string, syms expr.MapSymbols) ValidationErrors {
	if !strings.Contains(name, "{{") || !b.chargePositions(1, ptr) {
		return nil
	}
	return checkFormatString(b, name, ptr, ScopeJob, syms, TargetString)
}

// checkParameterSpaceExpressions checks a step's task-parameter range
// entries in both of the field's shapes: each range-list entry and the
// whole-field range expression. Both target section 1.3.12's types: a
// RangeList entry targets rangeExprElemType(tp.Type) (int, float, string or
// path) and a whole-field RangeExpr targets rangeExprFieldType(tp.Type)
// (int | string | range_expr | list[int] for INT/CHUNK[INT], list[<elem>]
// otherwise).
//
// resolve.go calls the same two functions at the same two positions
// (resolveRangeListEntry and evalRangeExprField), so the checker's verdict
// and rejection message match the resolver's coercion.
// resolve_agreement_test.go checks this: a range the checker accepts must
// resolve and expand without error, a range it rejects must never reach
// expansion, and assertRowRejected compares messages as well as verdicts.
//
// The typed targets do not weaken scope checking: an out-of-scope symbol
// fails at symbol lookup, before target coercion runs.
func checkParameterSpaceExpressions(b *templateBudget, ps StepParameterSpace, base string, syms expr.MapSymbols) ValidationErrors {
	var errs ValidationErrors
	for i, tp := range ps.TaskParameterDefinitions {
		if !b.ok() {
			break
		}
		ptr := fmt.Sprintf("%s/taskParameterDefinitions/%d", base, i)
		elemType := rangeListItemType(tp.Type)
		for j, v := range tp.RangeList {
			p := fmt.Sprintf("%s/range/%d", ptr, j)
			if !b.chargePositions(1, p) {
				break
			}
			errs = append(errs, checkFormatString(
				b, v, p, ScopeJob, syms, elemType,
			)...)
		}
		if tp.RangeExpr != nil && b.ok() {
			p := ptr + "/range"
			if b.chargePositions(1, p) {
				errs = append(errs, checkFormatString(
					b, *tp.RangeExpr, p, ScopeJob, syms, rangeExprFieldType(tp.Type),
				)...)
			}
		}
		errs = append(errs, checkChunkExpressions(b, tp.Chunks, ptr, syms)...)
	}
	return errs
}

// rangeExprFieldType maps a task-parameter's declared type to the target
// checkParameterSpaceExpressions verifies a whole-field RangeExpr's lone
// {{...}} reference against, per section 1.3.12's extended-range table:
//
//	INT / CHUNK[INT]   int | string | range_expr | list[int]
//	FLOAT              list[float]
//	STRING             list[string]
//	PATH               list[path]
//
// resolve.go's evalRangeExprField targets the same type from this function,
// so the checker and resolver produce identical rejection messages for the
// same input. They differ only in what they do with the result: the checker
// discards it, and the resolver dispatches on which union member it landed
// in (see evalRangeExprField).
//
// int and string are members because section 1.3.12 leaves the INT row
// "(unchanged, but see RangeString note below)", and that note extends the
// RangeString with an expression evaluating to a range_expr or a list[int]
// "in addition to the original <IntRangeExpr> grammar". A RangeString is a
// Format String (Template Schemas §3.4.1.1.1), so a lone {{...}} whose
// result is an int or a string is the ordinary base-spec form: range text,
// which parseIntRangeExpr then reads. The conformance suite states the
// target (EXPR/jobs/expr1.2.3--union-target-type.test.yaml:12, "INT task
// 'range': int | string | range_expr | list[int] (match-first)") and
// exercises all four members, including "{{ Param.StrFrames }}" with a
// STRING job parameter "1-3" expanding to tasks 1, 2, 3 and
// "{{ Param.IntCount + 1 }}" with IntCount = 7 expanding to the single task
// 8. A narrower union (range_expr | list[int]) would make declaring EXPR
// reject range: "{{Param.Frames}}" with a STRING Frames, the shape most
// presets in presets/sqi/ use, while the same template without EXPR expands.
//
// Member order does not matter here: expr.UnionOf sorts members into a
// canonical order, and section 1.2.3's match-first rule is implemented by
// coerce.go's directUnionMember, which compares whole types. Order matters
// in evalRangeExprField's dispatch on the result; see that function.
//
// FLOAT/STRING/PATH stay a plain list: their rows extend the field with a
// <ListExpressionString>, which section 1.3.12 defines as "a format string
// containing an expression that evaluates to a list". Only INT/CHUNK[INT]
// have the <IntRangeExpr> text form the RangeString note adds to.
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

// checkChunkExpressions checks a CHUNK[INT] definition's two @fmtstring sizing
// fields as job-scope positions, like the range field beside them:
// Template Schemas' "Job creation" row resolves both then. One position each,
// charged to the template-wide budget like every other position.
func checkChunkExpressions(b *templateBudget, c *TaskChunks, ptr string, syms expr.MapSymbols) ValidationErrors {
	if c == nil {
		return nil
	}
	fields := []struct {
		raw  *string
		name string
	}{
		{c.DefaultTaskCountExpr, "defaultTaskCount"},
		{c.TargetRuntimeSecondsExpr, "targetRuntimeSeconds"},
	}
	var errs ValidationErrors
	for _, f := range fields {
		if f.raw == nil || !b.ok() {
			continue
		}
		p := ptr + "/chunks/" + f.name
		if b.chargePositions(1, p) {
			errs = append(errs, checkFormatString(b, *f.raw, p, ScopeJob, syms, expr.TInt)...)
		}
	}
	return errs
}
