// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import (
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
// Copied from test/conformance/exprcase.go's jobParamTypes: PATH and
// LIST[PATH] are the two rows where the raw form differs from the resolved
// form, because the raw value may be a path for another operating system that
// cannot be parsed locally. An unrecognized spelling floors to "any" rather
// than leaving the name unbound -- an unbound name would make a valid
// template fail as "unknown symbol", which is worse than typing it loosely.
func jobParamTypes(declared string) (paramType, rawType expr.Type) {
	switch declared {
	case "PATH":
		return expr.TPath, expr.TString
	case "LIST[PATH]":
		return expr.ListOf(expr.TPath), expr.ListOf(expr.TString)
	}
	t, err := expr.ParseType(strings.ToLower(declared))
	if err != nil {
		return expr.TAny, expr.TAny
	}
	return t, t
}

// concreteJobParamValue converts a submitted job-parameter value into a Value
// of type t. It falls back to Unresolved(t) if raw does not parse as t:
// symbolsFor is not a validator, so a value that fails to parse here is
// reported as still-unknown rather than causing symbolsFor to panic or lie
// about what is bound. Validating submitted parameter values against their
// declared type is bind.go's job, upstream of this call.
//
// The CodePath case hardcodes PathPOSIX rather than taking a PathFormat from
// the caller. That is harmless today -- nothing calls symbolsFor yet -- but a
// future caller that also sets expr.WithPathFormat(expr.PathWindows) for the
// same evaluation would get a mismatched flavor for a concrete Param.<path>
// value: this Value and the evaluator's own path literals would disagree.
// Left as a known gap rather than fixed now, since symbolsFor has no
// PathFormat input to thread through yet and inventing one without a caller
// to drive it would be speculative.
func concreteJobParamValue(t expr.Type, raw string) expr.Value {
	switch t.Code {
	case expr.CodeInt:
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return expr.Unresolved(t)
		}
		return expr.Int(n)
	case expr.CodeFloat:
		f, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return expr.Unresolved(t)
		}
		return expr.Float(f)
	case expr.CodeString:
		return expr.String(raw)
	case expr.CodePath:
		return expr.Path(raw, expr.PathPOSIX)
	default:
		return expr.Unresolved(t)
	}
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
// table. Copied from test/conformance/exprcase.go's taskParamType:
// CHUNK[INT] is range_expr, NOT list[int], so that a frame range need not be
// expanded. An unrecognized spelling floors to "any", for the same reason
// jobParamTypes does.
func taskParamType(declared TaskParamType) expr.Type {
	if declared == TaskParamTypeChunkInt {
		return expr.TRangeExpr
	}
	t, err := expr.ParseType(strings.ToLower(string(declared)))
	if err != nil {
		return expr.TAny
	}
	return t
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

// hostOnlyFunctions is the set of function names the specification restricts
// to a host-context scope (SESSION and TASK) -- section "Host-Context
// Function Availability" -- because they need runtime resources that do not
// exist at submission time. apply_path_mapping needs the session's
// path-mapping rules, which are established only once a session is running;
// internal/openjd/expr registers it FLAT, with no scope model of its own, so
// this set is the only thing enforcing the restriction.
//
// The specification does NOT commit to a single such function:
// FunctionLibrary.with_host_context() returns a library "with host-only
// functions LIKE apply_path_mapping() enabled" (wiki line 515), and the
// availability section itself says "Certain functions ... For example,
// apply_path_mapping()". apply_path_mapping is the only entry today because it
// is the only function that reads session state -- declaring the gate as a
// SET rather than a single name comparison means a second entrant costs one
// map entry, not a rewritten conditional.
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

// ─── submission-time limits ─────────────────────────────────────────────────

// submissionOperationLimit and submissionMemoryLimit bound checkFormatString's
// evaluations (section 1.3.9/1.3.10), tighter than expr.Eval's own
// execution-time defaults (10,000,000 operations / 100,000,000 bytes).
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
//   - Phase 1 (checkTemplateExpressions with params == nil) binds every
//     Param./RawParam./Task.Param. symbol as Unresolved -- an opaque
//     placeholder that carries no data to process, so referencing it costs a
//     handful of operations regardless of what the SUBMITTED value will
//     eventually be. The only way an evaluation can accumulate real cost is
//     literal computation baked directly into the template text -- a large
//     string literal, a large literal list, or a loop over one -- which is
//     exactly the class of construct sections 1.3.9/1.3.10 exist to bound,
//     not something a legitimate template needs at a submission-time
//     position (a job name, a host requirement value, a command).
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
//     first, before it can allocate anywhere close to that floor.
//
// A template whose expressions need more than this to type-check against
// UNRESOLVED placeholders was already relying on literal computation heavy
// enough to be a submission-time liability; rejecting it here is the
// intended outcome, not a false positive.
const (
	submissionOperationLimit int64 = 10_000
	submissionMemoryLimit    int64 = 1_000_000
)

// submissionLimits builds the expr.Option slice every checkTemplateExpressions
// call site passes to checkFormatString. A function (not a package-level
// slice) so each call gets its own slice header -- cheap, and avoids any
// question of the underlying array being mutated by a caller.
func submissionLimits() []expr.Option {
	return []expr.Option{
		expr.WithOperationLimit(submissionOperationLimit),
		expr.WithMemoryLimit(submissionMemoryLimit),
	}
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
// params is nil for phase 1 (every job-parameter symbol unresolved) and
// holds concrete values for phase 2 (Task 10's caller, once one exists) --
// see symbolsFor's own doc comment for what params changes.
//
// The walk mirrors validate.go's traversal (ValidateWithOptions ->
// validateStep -> validateEnvironments/validateParameterSpace/
// validateHostRequirements -> validateAction/validateScriptRefs) position
// for position, per the table in this sub-project's task brief:
//
//	job name                          ScopeJob             TargetString
//	host requirement values           ScopeJob             TargetString
//	task-parameter range entries      ScopeJob             TargetString
//	environment variable values       job/step env         TargetString
//	env + step embedded-file data     job/step env / step  TargetString
//	action command                    matching env / step  TargetString
//	action args entries               matching env / step  TargetArgItem
//	action timeout                    matching env / step  TargetInt
//
// Host requirement values and task-parameter range entries are two of the
// three positions that had NO format-string scope validation at all before
// this task (validate.go's parallel checks are new in the same commit); the
// third, action timeout, is wired here too but is inert for a real template
// today -- decodeAction (parse.go) decodes "timeout" as a strict integer, so
// no format-string body can ever reach this position until that decoder is
// changed, which is a separate, later gap this task does not close.
func checkTemplateExpressions(tmpl *JobTemplate, params map[string]string) ValidationErrors {
	if tmpl == nil || !tmpl.hasExtension("EXPR") {
		return nil
	}

	var errs ValidationErrors

	errs = append(errs, checkFormatString(
		tmpl.Name, "/name", ScopeJob, symbolsFor(tmpl, nil, nil, ScopeJob, params), TargetString,
		submissionLimits()...,
	)...)

	errs = append(errs, checkEnvironmentExpressions(
		tmpl, nil, tmpl.JobEnvironments, "/jobEnvironments", ScopeJobEnvironment, params,
	)...)

	for i, s := range tmpl.Steps {
		errs = append(errs, checkStepExpressions(tmpl, s, i, params)...)
	}

	return errs
}

// checkStepExpressions checks the format-string positions of one step: its
// own script, its step environments, its parameter space's range entries,
// and its host requirement values.
func checkStepExpressions(tmpl *JobTemplate, s StepTemplate, idx int, params map[string]string) ValidationErrors {
	var errs ValidationErrors
	base := fmt.Sprintf("/steps/%d", idx)

	if s.Script != nil {
		syms := symbolsFor(tmpl, &s, nil, ScopeStepScript, params)
		errs = append(errs, checkScriptRefExpressions(
			s.Script.EmbeddedFiles, nil, ScopeStepScript, syms, base+"/script", base,
		)...)
		errs = append(errs, checkActionExpressions(
			s.Script.Actions.OnRun, base+"/script/actions/onRun", ScopeStepScript, syms,
		)...)
	}

	errs = append(errs, checkEnvironmentExpressions(
		tmpl, &s, s.StepEnvironments, base+"/stepEnvironments", ScopeStepEnvironment, params,
	)...)

	// Host requirements and the task-parameter range both sit at ScopeJob: a
	// step's own parameters and files do not exist yet while its host is
	// being selected or its task-parameter space is being defined, so step
	// is deliberately NOT passed to symbolsFor here -- ScopeJob's symbol
	// families never include Task.Param./Task.RawParam./Task.File. anyway
	// (scope.go's scopeFamilies), so the omission changes nothing observable,
	// but omitting it says so at the call site rather than relying on that.
	jobSyms := symbolsFor(tmpl, nil, nil, ScopeJob, params)

	if s.ParameterSpace != nil {
		errs = append(errs, checkParameterSpaceExpressions(*s.ParameterSpace, base+"/parameterSpace", jobSyms)...)
	}
	if s.HostRequirements != nil {
		errs = append(errs, checkHostRequirementExpressions(*s.HostRequirements, base+"/hostRequirements", jobSyms)...)
	}

	return errs
}

// checkEnvironmentExpressions checks the format-string positions of a list of
// environments (job-level or step-level): each one's variable values,
// embedded-file data, and onEnter/onExit actions. step is nil for job
// environments; scope selects ScopeJobEnvironment or ScopeStepEnvironment so
// symbolsFor binds the right fixed-symbol and family set for the level.
func checkEnvironmentExpressions(
	tmpl *JobTemplate, step *StepTemplate, envs []Environment, base string, scope Scope, params map[string]string,
) ValidationErrors {
	var errs ValidationErrors
	for i, e := range envs {
		ptr := fmt.Sprintf("%s/%d", base, i)

		var envScriptFiles []EmbeddedFile
		if e.Script != nil {
			envScriptFiles = e.Script.EmbeddedFiles
		}
		// symbolsFor needs THIS environment's own script to bind Env.File.:
		// a template may declare many environments, each with its own files.
		syms := symbolsFor(tmpl, step, &e, scope, params)
		errs = append(errs, checkScriptRefExpressions(
			envScriptFiles, e.Variables, scope, syms, ptr+"/script", ptr,
		)...)

		if e.Script != nil {
			if e.Script.Actions.OnEnter != nil {
				errs = append(errs, checkActionExpressions(
					*e.Script.Actions.OnEnter, ptr+"/script/actions/onEnter", scope, syms,
				)...)
			}
			if e.Script.Actions.OnExit != nil {
				errs = append(errs, checkActionExpressions(
					*e.Script.Actions.OnExit, ptr+"/script/actions/onExit", scope, syms,
				)...)
			}
		}
	}
	return errs
}

// checkScriptRefExpressions checks a script's embedded-file data and (for an
// environment) its variable values -- the EXPR-aware counterpart of
// validate.go's validateScriptRefs. vars may be nil for a step script.
func checkScriptRefExpressions(
	files []EmbeddedFile, vars map[string]string, scope Scope, syms expr.MapSymbols, scriptBase, varsBase string,
) ValidationErrors {
	var errs ValidationErrors
	for i, f := range files {
		errs = append(errs, checkFormatString(
			f.Data, fmt.Sprintf("%s/embeddedFiles/%d/data", scriptBase, i), scope, syms, TargetString,
			submissionLimits()...,
		)...)
	}
	// Sorted so the errors a template produces do not depend on map order --
	// matching validateScriptRefs' own reason for sorting.
	for _, k := range slices.Sorted(maps.Keys(vars)) {
		errs = append(errs, checkFormatString(
			vars[k], varsBase+"/variables/"+k, scope, syms, TargetString, submissionLimits()...,
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
// timeout is checked via a decimal re-rendering of the already-decoded int
// (a.TimeoutSeconds) rather than a raw format-string body, because
// decodeAction (parse.go) accepts only a strict integer for "timeout" today --
// there is no field on Action that could carry an unresolved "{{ ... }}"
// body for a real template to reach this call with. The call is still made,
// unconditionally when TimeoutSet, so the position is wired for the day that
// decoder changes; until then it is a no-op (a plain decimal string parses to
// zero format-string references).
func checkActionExpressions(a Action, ptr string, scope Scope, syms expr.MapSymbols) ValidationErrors {
	var errs ValidationErrors
	errs = append(errs, checkFormatString(
		a.Command, ptr+"/command", scope, syms, TargetString, submissionLimits()...,
	)...)
	for i, arg := range a.Args {
		errs = append(errs, checkFormatString(
			arg, fmt.Sprintf("%s/args/%d", ptr, i), scope, syms, TargetArgItem, submissionLimits()...,
		)...)
	}
	if a.TimeoutSet {
		errs = append(errs, checkFormatString(
			strconv.Itoa(a.TimeoutSeconds), ptr+"/timeout", scope, syms, TargetInt, submissionLimits()...,
		)...)
	}
	return errs
}

// checkHostRequirementExpressions checks a step's host requirement values --
// every amount's min/max and every attribute's anyOf/allOf entries -- against
// the ScopeJob symbol table syms. This is one of the two positions that had
// NO format-string scope validation at all before sub-project E2's Task 9;
// validate.go's validateHostRequirements gained the parallel base-spec check
// in the same commit.
func checkHostRequirementExpressions(hr HostRequirements, base string, syms expr.MapSymbols) ValidationErrors {
	var errs ValidationErrors
	for i, a := range hr.Amounts {
		amtPtr := fmt.Sprintf("%s/amounts/%d", base, i)
		if a.Min != nil {
			errs = append(errs, checkFormatString(
				*a.Min, amtPtr+"/min", ScopeJob, syms, TargetString, submissionLimits()...,
			)...)
		}
		if a.Max != nil {
			errs = append(errs, checkFormatString(
				*a.Max, amtPtr+"/max", ScopeJob, syms, TargetString, submissionLimits()...,
			)...)
		}
	}
	for i, a := range hr.Attributes {
		attrPtr := fmt.Sprintf("%s/attributes/%d", base, i)
		for k, v := range a.AnyOf {
			errs = append(errs, checkFormatString(
				v, fmt.Sprintf("%s/anyOf/%d", attrPtr, k), ScopeJob, syms, TargetString,
				submissionLimits()...,
			)...)
		}
		for k, v := range a.AllOf {
			errs = append(errs, checkFormatString(
				v, fmt.Sprintf("%s/allOf/%d", attrPtr, k), ScopeJob, syms, TargetString,
				submissionLimits()...,
			)...)
		}
	}
	return errs
}

// checkParameterSpaceExpressions checks a step's task-parameter RANGE LIST
// entries -- the other position with no format-string scope validation
// before Task 9. Each entry is checked at ScopeJob/TargetString, matching
// validate.go's validateRangeListValues.
//
// RangeExpr (the whole-field alternative form, e.g. "1-100:2" or, under EXPR,
// a list-valued expression per section 1.3.11) is deliberately NOT checked
// here, for the same reason validateRangeListValues' doc comment gives: its
// target type is not TargetString uniformly -- an EXPR RangeExpr may
// legitimately evaluate to list[float]/list[path]/list[string], and checking
// it against TargetString would reject the passing
// expr1.3.11--*-range-expression.yaml fixtures for a type mismatch that is
// not a real defect.
func checkParameterSpaceExpressions(ps StepParameterSpace, base string, syms expr.MapSymbols) ValidationErrors {
	var errs ValidationErrors
	for i, tp := range ps.TaskParameterDefinitions {
		ptr := fmt.Sprintf("%s/taskParameterDefinitions/%d", base, i)
		for j, v := range tp.RangeList {
			errs = append(errs, checkFormatString(
				v, fmt.Sprintf("%s/range/%d", ptr, j), ScopeJob, syms, TargetString,
				submissionLimits()...,
			)...)
		}
	}
	return errs
}
