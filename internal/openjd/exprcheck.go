// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import (
	"fmt"
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
func checkFormatString(s, ptr string, scope Scope, syms expr.MapSymbols, target expr.Type) ValidationErrors {
	if body, ok := fmtstring.LoneRef(s); ok {
		e, err := expr.Parse(body)
		if err != nil {
			return ValidationErrors{{Pointer: ptr, Message: err.Error()}}
		}
		if errs := checkHostOnlyFunctions(e, scope, ptr); len(errs) != 0 {
			return errs
		}
		if _, err := e.Eval(syms, target); err != nil {
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
		if _, err := e.Eval(syms, expr.TAny); err != nil {
			errs = append(errs, ValidationError{Pointer: ptr, Message: err.Error()})
		}
	}
	return errs
}
