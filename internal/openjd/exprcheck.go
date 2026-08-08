// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import (
	"strconv"
	"strings"

	"github.com/uberware/sqi/internal/openjd/expr"
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
// step may be nil for a job-level position; tmpl may carry no parameters;
// params may be nil for phase 1 (or when no concrete values are available yet).
//
// The type-mapping rules (PATH, LIST[PATH], CHUNK[INT], and the ParseType
// floor) are copied from test/conformance/exprcase.go's DeclaredSymbols, which
// implements the same section 1.2.2 tables for the conformance harness. The
// two copies are deliberately parallel: DeclaredSymbols works from an
// unparsed YAML document and is scope-blind (it binds every family
// regardless of position), while this one works from the parsed model and is
// scope-aware. They stay separate until sub-project H deletes the harness
// copy, at which point DeclaredSymbols is retired in favor of this path.
func symbolsFor(tmpl *JobTemplate, step *StepTemplate, scope Scope, params map[string]string) expr.MapSymbols {
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
		case "Task.File.", "Env.File.":
			bindEmbeddedFileSymbols(step, fam.Prefix, syms)
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
// embedded file in step's script.
//
// step is the only source of embedded files symbolsFor's signature gives it:
// there is no separate Environment parameter, so Task.File. (bound for
// ScopeStepScript) and Env.File. (bound for ScopeJobEnvironment and
// ScopeStepEnvironment) both read step.Script.EmbeddedFiles here, matching
// DeclaredSymbols' own stepSymbols, which binds both families from the same
// source for the same reason. This under-serves ScopeJobEnvironment in
// particular: a job environment has no associated StepTemplate, so step is
// nil there and no Env.File symbols are bound at all. Threading the
// Environment itself through is left to whichever later task wires a real
// caller to symbolsFor, which is the first one with an Environment value to
// pass.
func bindEmbeddedFileSymbols(step *StepTemplate, prefix string, syms expr.MapSymbols) {
	if step == nil || step.Script == nil {
		return
	}
	for _, f := range step.Script.EmbeddedFiles {
		if f.Name == "" {
			continue
		}
		syms[prefix+f.Name] = expr.Unresolved(expr.TPath)
	}
}
