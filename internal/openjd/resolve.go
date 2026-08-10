// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import (
	"fmt"

	"github.com/uberware/sqi/internal/openjd/expr"
	"github.com/uberware/sqi/internal/openjd/fmtstring"
)

// ResolveParameterSpaceParams returns a new *StepParameterSpace with every
// {{Param.<name>}} and {{RawParam.<name>}} reference in RangeExpr and
// RangeList entries substituted with the corresponding value from jobParams.
//
// Both "Param.<name>" and "RawParam.<name>" resolve to the same bound string
// value — path mapping is worker-side and out of scope at submit time.
//
// tmpl is the job template ps was parsed from. It exists on this signature
// for exactly two things, both gated on tmpl.hasExtension("EXPR") (tmpl may
// be nil, which is treated the same as an extension-less template):
//
//   - deciding whether a whole-field RangeExpr that is a LONE {{...}}
//     expression (fmtstring.LoneRef — the entire field is one reference, no
//     surrounding text) is section 1.3.12's extended range form and should be
//     EVALUATED into a value rather than substituted as plain text;
//   - building the phase-2 ScopeJob symbol table (symbolsFor) that
//     evaluation runs against, with jobParams already bound concrete —
//     Submitter.prepareTemplate's step 2d re-check runs the identical
//     construction over the identical map, so a range expression this
//     package resolves and the one checkTemplateExpressions already accepted
//     read the same job-parameter values the same way.
//
// This is the smallest signature change that gets the caller (submit.go's
// expandStepTaskParams, the function's only production call site) both
// facts: tmpl already carries the extension declaration AND the parameter
// definitions symbolsFor needs, so no second parameter (a bool, or a
// pre-built symbol table) is needed alongside it. A nil tmpl, or one that
// does not declare EXPR, takes exactly the base-spec substitution path this
// function has always taken — see [resolveRangeExprField]'s own doc comment
// for why that matters and how it is proven.
//
// The input ps is never mutated; a fresh struct (with a new slice of
// TaskParamDefinition values) is always returned on success.
//
// If ps is nil, (nil, nil) is returned immediately.
//
// On a malformed reference, an unknown variable, or (EXPR-declared, whole-
// field expression only) an evaluation error, a [ValidationError] is
// accumulated with a pointer of the form
//
//	/parameterSpace/taskParameterDefinitions/<i>/range
//	/parameterSpace/taskParameterDefinitions/<i>/range/<j>   (RangeList entry j)
//
// All task-parameter definitions are inspected before returning so callers
// receive a complete error list in one round-trip. On any error (nil, errs)
// is returned.
func ResolveParameterSpaceParams(tmpl *JobTemplate, ps *StepParameterSpace, jobParams map[string]string) (*StepParameterSpace, ValidationErrors) {
	if ps == nil {
		return nil, nil
	}

	exprEnabled := tmpl != nil && tmpl.hasExtension("EXPR")

	// The phase-2 ScopeJob symbol table (section 1.3.12's range field sits at
	// the same ScopeJob position host requirements do — see exprcheck.go's
	// checkStepExpressions), built only when EXPR is declared: step and env
	// are nil, so only Param.<name>/RawParam.<name> are bound, each to its
	// concrete jobParams value. Left nil (never read) when EXPR is not
	// declared, which is the point — see resolveRangeExprField.
	var syms expr.MapSymbols
	if exprEnabled {
		syms = symbolsFor(tmpl, nil, nil, ScopeJob, jobParams)
	}

	// Build a fmtstring scope: each job-param name → value, exposed as both
	// "Param.<name>" and "RawParam.<name>". Built unconditionally — every
	// RangeList entry, and every RangeExpr that is not a whole-field
	// expression (EXPR-declared or not), keeps resolving through it.
	scope := make(fmtstring.MapScope, len(jobParams)*2)
	for name, value := range jobParams {
		scope["Param."+name] = value
		scope["RawParam."+name] = value
	}

	var errs ValidationErrors
	newDefs := make([]TaskParamDefinition, len(ps.TaskParameterDefinitions))

	for i, def := range ps.TaskParameterDefinitions {
		newDef := def // shallow copy — Name, Type, Chunks, Combination are unchanged

		if def.RangeExpr != nil {
			ptr := fmt.Sprintf("/parameterSpace/taskParameterDefinitions/%d/range", i)
			newRangeExpr, newRangeList, rerr := resolveRangeExprField(exprEnabled, *def.RangeExpr, def.Type, syms, scope)
			if rerr != nil {
				errs = append(errs, ValidationError{
					Pointer: ptr,
					Message: rerr.Error(),
				})
				// Do not assign; keep processing to accumulate.
			} else {
				newDef.RangeExpr = newRangeExpr
				newDef.RangeList = newRangeList
			}
		}

		if len(def.RangeList) > 0 {
			newList := make([]string, len(def.RangeList))
			for j, entry := range def.RangeList {
				eptr := fmt.Sprintf("/parameterSpace/taskParameterDefinitions/%d/range/%d", i, j)
				resolved, err := fmtstring.Resolve(entry, scope)
				if err != nil {
					errs = append(errs, ValidationError{
						Pointer: eptr,
						Message: err.Error(),
					})
					newList[j] = entry // placeholder; result discarded on error
				} else {
					newList[j] = resolved
				}
			}
			newDef.RangeList = newList
		}

		newDefs[i] = newDef
	}

	if len(errs) > 0 {
		return nil, errs
	}

	out := *ps // shallow copy — Combination pointer is shared but never modified
	out.TaskParameterDefinitions = newDefs
	return &out, nil
}

// resolveRangeExprField resolves one task-parameter definition's whole-field
// range value (TaskParamDefinition.RangeExpr) for a definition of type typ.
//
// exprEnabled gates everything, and is the whole reason this function exists
// rather than inlining the old two-line body: when it is false, this is
// EXACTLY the base-spec substitution ResolveParameterSpaceParams has always
// performed — fmtstring.Resolve run over raw, with the result written back
// into a *string returned as rangeExpr. THAT BRANCH IS BYTE FOR BYTE
// UNCHANGED from before this task; TestResolveParameterSpaceParams_
// BaseSpecUnchanged (resolve_test.go) proves it with a range body that is
// valid EXPR syntax but not a valid base-spec dotted-identifier reference —
// if this function (or its caller) were ever rerouted to the EXPR-aware
// branch for a non-EXPR template, that test would wrongly succeed with a
// computed value instead of failing with "not a valid dotted identifier".
//
// When exprEnabled is true, raw is inspected first: only when it is a LONE
// {{...}} expression (fmtstring.LoneRef — the whole field is one reference,
// nothing else) is it section 1.3.12's extended form, evaluated against syms
// and converted into a range list (see evalRangeExprList). Any other shape —
// literal text with no {{}} at all ("1-100:2"), or a {{...}} reference
// embedded in surrounding text ("1-{{Param.End}}") — is not a whole-field
// list expression and keeps taking the exact same fmtstring.Resolve path the
// non-EXPR branch does: declaring EXPR does not change how an ordinary
// base-spec RangeExpr resolves, only what a WHOLE-FIELD lone expression
// means.
//
// Returns exactly one of rangeExpr (non-nil) or rangeList (non-nil) on
// success — the caller assigns both straight onto the new definition, so a
// list result must come with a nil rangeExpr to clear the old one and let
// expandTaskParam (expand.go) take the list branch, per the design spec's
// section 2.
func resolveRangeExprField(
	exprEnabled bool, raw string, typ TaskParamType, syms expr.MapSymbols, scope fmtstring.Scope,
) (rangeExpr *string, rangeList []string, err error) {
	if exprEnabled {
		if body, ok := fmtstring.LoneRef(raw); ok {
			list, err := evalRangeExprList(body, typ, syms)
			if err != nil {
				return nil, nil, err
			}
			return nil, list, nil
		}
	}

	resolved, err := fmtstring.Resolve(raw, scope)
	if err != nil {
		return nil, nil, err
	}
	return &resolved, nil, nil
}

// evalRangeExprList evaluates body — the inner source of a lone whole-field
// {{...}} range expression — against syms and returns its value as range-list
// entries, per the design spec's section 1.3.12 extended-range table:
//
//	INT / CHUNK[INT]   range_expr | list[int]
//	FLOAT              list[float]
//	STRING             list[string]
//	PATH               list[path]
//
// The evaluation TARGET is always list[<the field's own element type>]
// (rangeExprElemType), never a bare range_expr type or expr.TAny. This is
// what keeps this function on the correct side of the design spec's section
// 2.1 trap, stated in full there: internal/openjd's OWN <IntRangeExpr>
// reader (parseIntRangeExpr, expand.go, via internal/openjd/intrange with
// Policy{PositiveStepOnly, AscendingOnly}) applies a policy that deliberately
// DIFFERS from internal/openjd/expr's (the zero Policy) in three ways this
// repo has decided to preserve — it rejects start > end, rejects a negative
// step, and expands in first-seen rather than increasing order. Stringifying
// an expression-produced range_expr value back into <IntRangeExpr> text and
// re-parsing it through that reader would re-admit it under the STRICTER
// policy, silently rejecting or reordering a range the expression language
// legitimately produced.
//
// coerce() (coerce.go) already implements range_expr -> list[int] as one of
// section 1.2.3's three list rules — coerceList calls the unexported
// rangeInts directly on the VALUE, with no detour through text at all — so
// targeting list[int] here reuses the exact same conversion sub-project C1
// built for the language's own list(range_expr) function, rather than this
// package writing a second, independent implementation of "take the
// integers out of a range_expr". One code path, not two: a range_expr result
// and a literal list[int] result (e.g. from range(), which returns list[int]
// directly — funcslist.go) are indistinguishable by the time this function's
// caller sees them.
//
// Every element is rendered to text with Value.String() — the same
// conversion coerceScalar's own CodeString case performs (coerce a value to
// expr.TString and the result IS String()'s output), which is the identical
// rendering EXPR sub-project E4a already established as definitive for a
// value reaching a template's output text (fmtres/expres.go's
// resolveFormatStringExpr, LONE-reference branch: evaluate against TString,
// return AsStr()). That answers this wave's own section 2.3 question for a
// computed FLOAT with no submitted carry: String() falls through to
// formatFloat's shortest round-tripping decimal (value.go), which is
// SPEC-CORRECT, not merely convenient reuse — the base specification states
// this rendering directly (wiki/2026-02-Expression-Language.md, section
// 1.3.4, "Float Value Pass-Through": "When an operation is performed on a
// float value ... string interpolation uses the shortest decimal string
// representation", worked example "{{Param.V + 1}} outputs \"4.5\"") and E2's
// carry ruling for a value WITH submitted text says nothing about a value
// that never had any. Confirmed for this task's own two computed cases —
// Param.Scale=2.5: Scale*2 -> "5.0", Scale+0.5 -> "3.0" — by a throwaway
// evaluation against this package before this comment was written; both
// match the brief's hypothesized values exactly, so there is no ruling to
// change here, only to record: the existing mechanism already produces the
// spec-correct answer, reused rather than reimplemented.
func evalRangeExprList(body string, typ TaskParamType, syms expr.MapSymbols) ([]string, error) {
	elemType := rangeExprElemType(typ)
	v, err := expr.Eval(body, syms, expr.ListOf(elemType), submissionLimits()...)
	if err != nil {
		return nil, err
	}
	if v.IsUnresolved() {
		// Unreachable in production: syms binds every ScopeJob symbol
		// concrete (jobParams is always the fully-bound map by the time this
		// package's one caller, submit.go's expandStepTaskParams, runs — see
		// Submitter.prepareTemplate's step 2c), and ScopeJob exposes no
		// symbol family that ever stays a placeholder. Reported as an error
		// rather than silently producing an empty or zero-valued list,
		// matching this codebase's convention of never letting an
		// unresolved value flow into output unnoticed (expr/value.go's
		// mustBe doc comment: "a silent zero would flow into a rendered
		// command line unnoticed").
		return nil, fmt.Errorf("range expression %q produced an unresolved value", body)
	}
	elems := v.AsList()
	out := make([]string, len(elems))
	for i, elem := range elems {
		out[i] = elem.String()
	}
	return out, nil
}

// rangeExprElemType maps a task-parameter's declared type to the element
// type evalRangeExprList targets when evaluating a whole-field range
// expression, per section 1.3.12's extended-range table (see that function's
// doc comment).
//
// INT and CHUNK[INT] share expr.TInt: expand.go's expandChunkInt still reads
// a CHUNK[INT] definition's RangeList as individual integer strings and
// groups them into chunks itself after this package hands them back —
// chunking is not this function's, evalRangeExprList's, or
// ResolveParameterSpaceParams' concern, only producing the flat per-value
// list is.
func rangeExprElemType(typ TaskParamType) expr.Type {
	switch typ {
	case TaskParamTypeFloat:
		return expr.TFloat
	case TaskParamTypeString:
		return expr.TString
	case TaskParamTypePath:
		return expr.TPath
	default: // TaskParamTypeInt, TaskParamTypeChunkInt
		return expr.TInt
	}
}
