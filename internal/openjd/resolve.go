// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import (
	"fmt"
	"strings"

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
// tmpl is the job template ps was parsed from, and step is the step template
// ps belongs to (nil is allowed for both, and a nil tmpl is treated the same
// as an extension-less template). They are used for three things, all gated
// on tmpl.hasExtension("EXPR"):
//
//   - deciding whether a whole-field RangeExpr that is a lone {{...}}
//     expression (fmtstring.LoneRef: the entire field is one reference) is
//     section 1.3.12's extended range form and is evaluated into a value
//     rather than substituted as text, and, for every other shape (a
//     RangeList entry, or a RangeExpr that is literal text or has a reference
//     embedded in surrounding text), reading each embedded {{...}} as an
//     expression (evaluated at expr.TAny and rendered to text) rather than a
//     bare dotted-identifier lookup (resolveFormatStringExpr,
//     resolveRangeListEntry);
//   - building the phase-2 ScopeJob symbol table that evaluation runs
//     against, with jobParams bound concrete. Submitter.prepareTemplate's
//     re-check builds the same table from the same map, so the resolver and
//     checkTemplateExpressions read the same job-parameter values the same
//     way;
//   - evaluating step's own let: block and merging the names it binds into
//     that table, as section 3.6.2 row 1 grants a step template's let: at the
//     parameterSpace position. Both are shared with the checker
//     (exprcheck.go's stepLetSymbols and rangeScopeSymbols) so the two cannot
//     differ; see stepLetSymbols.
//
// A nil tmpl, or one that does not declare EXPR, takes the base-spec
// substitution path; see [resolveRangeExprField]. step is read only on the
// EXPR path, so a base-spec caller may pass nil for it.
//
// The input ps is never mutated; a fresh struct (with a new slice of
// TaskParamDefinition values) is returned on success.
//
// If ps is nil, (nil, nil) is returned immediately.
//
// On a malformed reference, an unknown variable, or (EXPR-declared only) an
// expression evaluation error, at the whole-field RangeExpr, a RangeList
// entry, or an embedded reference within either, a [ValidationError] is
// accumulated with a pointer of the form
//
//	/parameterSpace/taskParameterDefinitions/<i>/range
//	/parameterSpace/taskParameterDefinitions/<i>/range/<j>   (RangeList entry j)
//
// A malformed or unevaluatable entry in step's own let: block is reported the
// same way, at pointer /let/<i>, rather than leaving every range expression
// that referenced the failed binding to fail with "unknown symbol".
//
// A wall-clock deadline ([ExprLimits.Deadline], carried on budget) is not
// reported that way. It is recorded on the budget
// ([templateBudget.recordDeadline]), which stops the walk and is read back by
// the caller through [templateBudget.deadline]; this function then returns a
// nil parameter space, because a partly resolved one would look complete.
// Every evaluation site (step's let: block, the whole-field range, each
// RangeList entry) routes through recordDeadline first, so a server-side stop
// never reaches the API as an invalid template.
//
// All task-parameter definitions are inspected before returning, so callers
// get a complete error list in one pass, unless the budget trips first and
// the walk stops early.
//
// budget is an optional [templateBudget], meant to be shared by every step's
// call for one submission so a many-step submission's resolver cost is
// bounded together. It must not be the budget checkTemplateExpressions spent
// for the same submission: the resolver charges the same range positions and
// let bytes the checker already charged, so sharing one budget would halve
// the effective cap and a template phase 1 accepted could be rejected in
// phase 2 on accumulated cost alone. Submit follows the same rule (see
// prepareTemplate); TestSubmit_ValidateAcceptsMustNotRejectOnBudgetAlone
// (resolve_budget_test.go) and
// TestPhase2Budget_CheckerAndResolverHaveIndependentBudgets cover it.
//
// Omitting budget gives this call its own fresh allowance
// ([templateBudgetOrFresh]).
//
// The budget is consulted and charged only on the EXPR path: a template that
// does not declare EXPR takes the base-spec substitution with no budget
// interaction.
func ResolveParameterSpaceParams(
	tmpl *JobTemplate, step *StepTemplate, ps *StepParameterSpace, jobParams map[string]string,
	budget ...*templateBudget,
) (*StepParameterSpace, ValidationErrors) {
	if ps == nil {
		// A step with no parameterSpace costs this budget nothing, not even
		// its let: block, which is otherwise charged in newStepScope. The
		// checker still charges that block (checkStepExpressions runs
		// whether or not the step declares a parameterSpace), so the block
		// is still bounded.
		return nil, nil
	}

	b := templateBudgetOrFresh(budget)
	// If an earlier step's call already exhausted the budget, stop before
	// evaluating this step's let: block, which costs an expr.Eval per
	// binding.
	if tmpl != nil && tmpl.hasExtension("EXPR") && !b.ok() {
		return nil, b.errs()
	}
	sc, letErrs := newStepScope(b, tmpl, step, jobParams)
	return resolveParameterSpace(b, sc, ps, letErrs)
}

// stepScope is the scope a step's fields resolved at Job creation (its
// parameterSpace and its hostRequirements) resolve against: under EXPR the
// phase-2 symbol table, otherwise the base-spec Param./RawParam. scope.
// Building it evaluates the step's let: block, so a submission builds it once
// per step and shares it between the two.
type stepScope struct {
	exprEnabled bool
	syms        expr.MapSymbols // nil unless exprEnabled
	scope       fmtstring.MapScope
}

// newStepScope builds a step's stepScope, charging b for its let: block.
//
// The symbol table is built only when EXPR is declared, and built by the
// checker's own two helpers so the two layers cannot disagree about what is in
// scope: ScopeJob's fixed and family symbols (section 1.3.12's range field sits
// at the same ScopeJob position host requirements do; see exprcheck.go's
// checkStepExpressions) plus, per section 3.6.2 row 1, the names the step's own
// let: block binds. It is left nil, and never read, when EXPR is not declared;
// see resolveRangeExprField.
func newStepScope(
	b *templateBudget, tmpl *JobTemplate, step *StepTemplate, jobParams map[string]string,
) (stepScope, ValidationErrors) {
	sc := stepScope{exprEnabled: tmpl != nil && tmpl.hasExtension("EXPR")}
	var errs ValidationErrors
	if sc.exprEnabled {
		stepLet, letErrs := stepLetSymbols(b, tmpl, step, jobParams, "")
		errs = letErrs

		// Charged as checkStepExpressions charges the same stepLetSymbols
		// call (exprcheck.go): one position per binding attempted
		// (letPositions mirrors checkLetBindings' maxLetBindings
		// truncation), and stepLet's retained bytes, since stepLetSymbols
		// returns only the bindings it added.
		var letCount int
		if step != nil {
			letCount = len(step.Let)
		}
		b.chargePositions(letPositions(letCount), "/let")
		b.chargeRetainedBytes(templateExprRetainedBytes(stepLet), "/let")

		sc.syms = rangeScopeSymbols(tmpl, stepLet, jobParams)
	}

	// Each job-param name → value, exposed as both "Param.<name>" and
	// "RawParam.<name>". Built unconditionally: every RangeList entry, and
	// every RangeExpr that is not a whole-field expression (EXPR-declared or
	// not), resolves through it.
	sc.scope = make(fmtstring.MapScope, len(jobParams)*2)
	for name, value := range jobParams {
		sc.scope["Param."+name] = value
		sc.scope["RawParam."+name] = value
	}
	return sc, errs
}

// resolveParameterSpace is ResolveParameterSpaceParams' body for a step whose
// stepScope is already built; errs carries any error building it reported.
func resolveParameterSpace(
	b *templateBudget, sc stepScope, ps *StepParameterSpace, errs ValidationErrors,
) (*StepParameterSpace, ValidationErrors) {
	exprEnabled, syms, scope := sc.exprEnabled, sc.syms, sc.scope
	newDefs := make([]TaskParamDefinition, len(ps.TaskParameterDefinitions))

	for i, def := range ps.TaskParameterDefinitions {
		if exprEnabled && !b.ok() {
			// The budget tripped (on this step's let: block, an earlier
			// definition, or an earlier step sharing the budget). The
			// remaining definitions stay zero-valued; b.errs() below makes
			// errs non-empty, so the result is discarded.
			break
		}
		newDef, defErrs := resolveTaskParamDefinition(b, exprEnabled, i, def, syms, scope)
		errs = append(errs, defErrs...)
		chunks, chunkErrs := resolveChunkFields(b, exprEnabled, i, def.Chunks, syms, scope)
		errs = append(errs, chunkErrs...)
		newDef.Chunks = chunks
		newDefs[i] = newDef
	}

	errs = append(errs, b.errs()...)

	// A wall-clock deadline stopped the walk. It is not a ValidationError
	// (templateBudget.recordDeadline), so errs may be empty while newDefs is
	// partly zero-valued. Return no space, which would otherwise look like a
	// complete resolution; the caller reads b.deadline().
	if b.deadline() != nil {
		return nil, errs
	}

	if len(errs) > 0 {
		return nil, errs
	}

	out := *ps // shallow copy — Combination pointer is shared but never modified
	out.TaskParameterDefinitions = newDefs
	return &out, nil
}

// resolveTaskParamDefinition resolves one task-parameter definition's range
// field (RangeExpr or RangeList), charging b when exprEnabled. It is split
// out of resolveParameterSpace to keep that function's cyclomatic complexity
// within the lint limit.
//
// It returns the definition's copy (Name/Type/Chunks/Combination unchanged,
// Range* fields resolved on success) and any errors resolving it produced.
// When a charge finds the budget exhausted, the copy is returned unresolved
// with no errors of its own; the caller's b.errs() reports the failure.
//
// A wall-clock deadline is the other such case, which is why this function
// takes the whole budget rather than b.limits. Both evaluation failure paths
// (the whole-field one here and resolveRangeListDefinition's per-entry one)
// route the error through [templateBudget.recordDeadline] first, so a
// deadline (a 503) is recorded on the budget instead of becoming a
// ValidationError (a 422), and b.ok() turns false so the caller stops. See
// TestResolveParameterSpaceParams_DeadlineAtRangeExprPosition and its
// RangeList counterpart.
func resolveTaskParamDefinition(
	b *templateBudget, exprEnabled bool, i int, def TaskParamDefinition, syms expr.MapSymbols, scope fmtstring.Scope,
) (TaskParamDefinition, ValidationErrors) {
	newDef := def // shallow copy — Name, Type, Combination are unchanged; the caller resolves Chunks

	if def.RangeExpr != nil {
		ptr := fmt.Sprintf("/parameterSpace/taskParameterDefinitions/%d/range", i)
		if exprEnabled && !b.chargePositions(1, ptr) {
			return newDef, nil
		}
		newRangeExpr, newRangeList, rerr := resolveRangeExprField(exprEnabled, *def.RangeExpr, def.Type, syms, scope, b.limits)
		if rerr != nil {
			if b.recordDeadline(rerr) {
				// A wall-clock stop, not a verdict on this definition. It is
				// recorded on the budget, so it reaches the caller as an error
				// (a 503) rather than a ValidationError (a 422), and the
				// caller's !b.ok() guard ends the definition loop. See
				// [templateBudget.recordDeadline].
				return newDef, nil
			}
			// Leave newDef's range fields untouched on error; the caller's
			// loop goes on collecting other definitions' errors in the same
			// pass (unless the budget stops it).
			return newDef, ValidationErrors{{Pointer: ptr, Message: rerr.Error()}}
		}
		newDef.RangeExpr = newRangeExpr
		newDef.RangeList = newRangeList
		return newDef, nil
	}

	if len(def.RangeList) == 0 {
		// The parser sets at most one of RangeExpr/RangeList per definition
		// (see TaskParamDefinition), so this fires only for a definition with
		// no range at all. The RangeExpr branch above returns first, so its
		// result is never overwritten by the unresolved def.RangeList.
		return newDef, nil
	}

	newList, errs := resolveRangeListDefinition(b, exprEnabled, i, def, syms, scope)
	newDef.RangeList = newList
	return newDef, errs
}

// resolveRangeListDefinition resolves every entry of one task-parameter
// definition's RangeList, split out of resolveTaskParamDefinition for the
// same complexity reason.
//
// It records a deadline on the budget as that function does. This loop
// otherwise collects one ValidationError per bad entry and continues, which
// for a wall-clock stop would report a 422 and keep spending time past the
// deadline.
func resolveRangeListDefinition(
	b *templateBudget, exprEnabled bool, i int, def TaskParamDefinition, syms expr.MapSymbols, scope fmtstring.Scope,
) ([]string, ValidationErrors) {
	var errs ValidationErrors
	newList := make([]string, 0, len(def.RangeList))
	for j, entry := range def.RangeList {
		if exprEnabled && !b.ok() {
			break
		}
		eptr := fmt.Sprintf("/parameterSpace/taskParameterDefinitions/%d/range/%d", i, j)
		if exprEnabled && !b.chargePositions(1, eptr) {
			break
		}
		items, err := resolveRangeListElement(exprEnabled, entry, def.Type, syms, scope, b.limits, maxTasksPerStep-len(newList))
		if err != nil {
			if b.recordDeadline(err) {
				// Recorded on the budget, and the loop returns rather than
				// collecting the error and continuing.
				return newList, errs
			}
			errs = append(errs, ValidationError{Pointer: eptr, Message: err.Error()})
			continue // result discarded on error
		}
		newList = append(newList, items...)
	}
	// A range list must hold at least one element after null items are
	// dropped and list items flattened (§1.3.12: the list's element-count
	// limits apply again after resolution; the maximum is re-checked with the
	// other parameter-space limits on the resolved space).
	if len(errs) == 0 && len(def.RangeList) > 0 && len(newList) == 0 {
		errs = append(errs, ValidationError{
			Pointer: fmt.Sprintf("/parameterSpace/taskParameterDefinitions/%d/range", i),
			Message: "range list is empty once its null elements are dropped",
		})
	}
	return newList, errs
}

// resolveRangeListElement resolves one range-list element to the elements it
// contributes. Under EXPR an element that is exactly "{{<expr>}}" is a list
// item (§1.3.12, §1.3.2) and contributes zero, one or several; any other
// element resolves to one string.
//
// room is how many more values the list may hold: every value becomes a task,
// so a list item that would carry the list past maxTasksPerStep is refused
// before its values are written out.
func resolveRangeListElement(
	exprEnabled bool, entry string, typ TaskParamType, syms expr.MapSymbols, scope fmtstring.Scope, lim ExprLimits, room int,
) ([]string, error) {
	if body, ok := fmtstring.LoneRef(entry); ok && exprEnabled {
		return resolveRangeListItem(body, typ, syms, lim, room)
	}
	resolved, err := resolveRangeListEntry(exprEnabled, entry, typ, syms, scope, lim)
	if err != nil {
		return nil, err
	}
	return []string{resolved}, nil
}

// rangeListItemType is the target of a range-list element that is exactly
// "{{<expr>}}" (§1.3.12): the list-item type T? | list[T] of §1.3.2, where T
// is the parameter's element type — int for INT and CHUNK[INT], string,
// path — so a null result drops the element and a list result flattens. FLOAT
// also admits int, float? | int | list[float], so an int result is kept as
// written, as the <floatstring> it stands for would be, while a list's members
// are converted to float. exprcheck.go checks the same position against this
// same type, so the checker and the resolver cannot disagree about it.
func rangeListItemType(typ TaskParamType) expr.Type {
	elem := rangeExprElemType(typ)
	if typ == TaskParamTypeFloat {
		return expr.UnionOf(expr.OptionalOf(elem), expr.TInt, expr.ListOf(elem))
	}
	return expr.UnionOf(expr.OptionalOf(elem), expr.ListOf(elem))
}

// resolveRangeListItem evaluates one lone range-list expression against its
// list-item type and returns the elements it contributes: none for null, one
// per member for a list, and one otherwise.
func resolveRangeListItem(body string, typ TaskParamType, syms expr.MapSymbols, lim ExprLimits, room int) ([]string, error) {
	e, err := expr.Parse(body)
	if err != nil {
		return nil, err
	}
	v, err := e.Eval(syms, rangeListItemType(typ), lim.evalOptions()...)
	if err != nil {
		return nil, err
	}
	switch {
	case v.IsUnresolved():
		return nil, errUnresolvedRangeValue(v)
	case v.IsNull():
		return nil, nil
	case v.Type.Code == expr.CodeList:
		elems := v.AsList()
		if len(elems) > room {
			return nil, fmt.Errorf("the range list resolves to more than %d values, the most a step may have tasks", maxTasksPerStep)
		}
		out := make([]string, len(elems))
		for k, elem := range elems {
			if elem.IsUnresolved() {
				return nil, errUnresolvedRangeValue(elem)
			}
			out[k] = elem.String()
		}
		return out, nil
	}
	return []string{v.String()}, nil
}

// resolveRangeExprField resolves one task-parameter definition's whole-field
// range value (TaskParamDefinition.RangeExpr) for a definition of type typ.
//
// When exprEnabled is false, this is the base-spec substitution:
// fmtstring.Resolve run over raw, with the result returned as rangeExpr.
// TestResolveParameterSpaceParams_BaseSpecUnchanged (resolve_test.go) covers
// that branch with a range body that is valid EXPR syntax but not a valid
// base-spec dotted-identifier reference, so routing a non-EXPR template to
// the EXPR branch would make it pass instead of failing with "not a valid
// dotted identifier".
//
// When exprEnabled is true, a raw that is a lone {{...}} expression
// (fmtstring.LoneRef) is section 1.3.12's whole-field form, evaluated against
// syms by evalRangeExprField, which returns either a range list (a list or
// range_expr result) or range text (an int or string result); see that
// function. Any other shape (literal text such as "1-100:2", or a reference
// embedded in surrounding text such as "1-{{Param.End}}") is still a section
// 1.3.2 format string ("it is a format string, it can now contain an
// expression", §1.3.12), so it is resolved through resolveFormatStringExpr,
// the same embedded-reference handling checkFormatString (exprcheck.go)
// checks it with: each {{...}} segment evaluated at expr.TAny and rendered
// with Value.String(). fmtstring.Resolve would reject anything but a bare
// dotted identifier, so range: "1-{{ Param.End * 2 }}" would pass the checker
// and then fail here.
//
// That branch's result is text, assigned to rangeExpr as in the base-spec
// branch. An embedded reference can evaluate to a range_expr value, and
// Value.String() renders it in canonical form (Template Schemas §3.4.1.1.1):
// "{{ range_expr(\"10-15:2,1-5\") }},7" resolves to RangeExpr "1-5,10-14:2,7"
// (TestResolveParameterSpaceParams_NonLoneRangeExprEmbeddedRangeExprValue),
// which parseIntRangeExpr then expands in first-seen order to
// [1,2,3,4,5,10,12,14,7], where the expression language's ascending order
// would give [1,2,3,4,5,7,10,12,14]. This is intended. Section 1.3.2 says an
// embedded reference evaluates at expr.TAny and converts to a string whatever
// its type, and format-string interpolation never inspects the value's type
// to choose a rendering (the worker's fmtres evalEmbeddedRef renders every
// result the same way). The composed text is an ordinary base-spec
// <RangeString>, parsed under internal/openjd's <IntRangeExpr> policy
// (parseIntRangeExpr, range.go), whose three differences from the expression
// language's (start > end rejected, negative step rejected, first-seen order)
// apply to it as to any literal range string and must be kept. Declaring EXPR
// changes how embedded references are evaluated and what a lone whole-field
// expression means, not how range text is parsed.
//
// It returns exactly one of rangeExpr (non-nil) or rangeList (non-nil) on
// success. The caller assigns both onto the new definition, so a list result
// comes with a nil rangeExpr, which clears the old one and sends
// expandTaskParam (expand.go) down the list branch.
//
// lim is the walk's operator-configured [ExprLimits], from the caller's
// [templateBudget], so this position is metered as the checker's
// checkParameterSpaceExpressions call for the same field is.
func resolveRangeExprField(
	exprEnabled bool, raw string, typ TaskParamType, syms expr.MapSymbols, scope fmtstring.Scope, lim ExprLimits,
) (rangeExpr *string, rangeList []string, err error) {
	if exprEnabled {
		if body, ok := fmtstring.LoneRef(raw); ok {
			// evalRangeExprField returns this function's two-result shape:
			// exactly one of the two is non-nil.
			return evalRangeExprField(body, typ, syms, lim)
		}

		resolved, err := resolveFormatStringExpr(raw, syms, expr.TAny, lim.evalOptions()...)
		if err != nil {
			return nil, nil, err
		}
		return &resolved, nil, nil
	}

	resolved, err := fmtstring.Resolve(raw, scope)
	if err != nil {
		return nil, nil, err
	}
	return &resolved, nil, nil
}

// resolveRangeListEntry resolves one RangeList entry (a single FLOAT, STRING
// or PATH range value, or a literal INT/CHUNK[INT] value; see
// TaskParamDefinition.RangeList). When exprEnabled is false this is
// fmtstring.Resolve run over entry.
//
// When exprEnabled is true, entry resolves through resolveFormatStringExpr
// with target rangeExprElemType(typ), as checkParameterSpaceExpressions checks
// it: a lone {{...}} reference is evaluated against the task parameter's
// element type (so a value that needs narrowing, such as an integral float
// for an INT entry, gets the §1.2.3 coercion the checker verified), and
// anything else evaluates each embedded reference at expr.TAny and
// concatenates. Either way the entry's text (Value.String()) is stored back
// into RangeList, and expand.go (validateIntList, validateFloatList, PATH's
// non-empty check, and so on) parses it into the parameter's type, as for a
// literal entry.
//
// The target must be the element type, not TargetString: the checker accepts
// an INT entry "{{ Param.Scale * 2 }}" with Scale=2.5 (float -> int is a legal
// §1.2.3 coercion and 5.0 is integral), while TargetString would render it
// "5.0", which validateIntList rejects ("invalid integer \"5.0\"").
func resolveRangeListEntry(
	exprEnabled bool, entry string, typ TaskParamType, syms expr.MapSymbols, scope fmtstring.Scope, lim ExprLimits,
) (string, error) {
	if exprEnabled {
		return resolveFormatStringExpr(entry, syms, rangeExprElemType(typ), lim.evalOptions()...)
	}
	return fmtstring.Resolve(entry, scope)
}

// resolveFormatStringExpr resolves one EXPR-aware format-string field against
// syms: either a RangeList entry (loneTarget rangeExprElemType(typ): TInt,
// TFloat, TString or TPath; see resolveRangeListEntry) or a whole-field
// RangeString that is not a lone reference (loneTarget expr.TAny, since
// resolveRangeExprField handles the lone case itself).
//
// It mirrors internal/worker/fmtres/expres.go's resolveFormatStringExpr and
// exprcheck.go's checkFormatString, per section 1.3.2: a format string that is
// exactly one reference with no surrounding text (fmtstring.LoneRef) inherits
// loneTarget; anything else walks fmtstring.Segments and evaluates each
// reference at expr.TAny, rendering with Value.String() and treating a null
// result as the empty string (RFC 0005), concatenated with the literal text.
func resolveFormatStringExpr(s string, syms expr.MapSymbols, loneTarget expr.Type, opts ...expr.Option) (string, error) {
	if body, ok := fmtstring.LoneRef(s); ok {
		e, err := expr.Parse(body)
		if err != nil {
			return "", err
		}
		v, err := e.Eval(syms, loneTarget, opts...)
		if err != nil {
			return "", err
		}
		if v.IsUnresolved() {
			return "", errUnresolvedRangeValue(v)
		}
		// Value.String(), not v.AsStr(): AsStr panics (Value.mustBe) for any
		// payload other than string, and loneTarget may be TInt/TFloat/TPath
		// (resolveRangeListEntry passes rangeExprElemType(typ)). String() is
		// total over every Value and is the rendering evalRangeExprField's
		// elements use. See
		// TestResolveFormatStringExpr_NonStringLoneTargetDoesNotPanic.
		return v.String(), nil
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
		e, err := expr.Parse(seg.Ref)
		if err != nil {
			return "", err
		}
		v, err := e.Eval(syms, expr.TAny, opts...)
		if err != nil {
			return "", err
		}
		if v.IsUnresolved() {
			return "", errUnresolvedRangeValue(v)
		}
		if v.IsNull() {
			// RFC 0005: within a format string the target type is string?
			// and a null result is the empty string, not Value.String()'s
			// diagnostic "null" (as in internal/worker/fmtres/expres.go's
			// evalEmbeddedRef).
			continue
		}
		b.WriteString(v.String())
	}
	return b.String(), nil
}

// errUnresolvedRangeValue reports an EXPR evaluation that produced a
// still-unresolved value at a range-field position. Submit binds every
// ScopeJob symbol concrete before resolving (see evalRangeExprField's
// matching guard), so this guards against an unresolved value's diagnostic
// "<unresolved[T]>" rendering (Value.String()) reaching a range value
// unnoticed (see expr/value.go's mustBe doc comment).
func errUnresolvedRangeValue(v expr.Value) error {
	return fmt.Errorf(
		"value is unresolved (%s): the submitted text could not be parsed as its declared type", v.Type,
	)
}

// evalRangeExprField evaluates body, the inner source of a lone whole-field
// {{...}} range expression, against syms and returns either range text
// (rangeText non-nil) or range-list entries (rangeList non-nil), never both,
// per section 1.3.12's extended-range table. This is
// resolveRangeExprField's (rangeExpr, rangeList) return shape:
//
//	INT / CHUNK[INT]   int | string | range_expr | list[int]
//	FLOAT              list[float]
//	STRING             list[string]
//	PATH               list[path]
//
// The evaluation target is rangeExprFieldType(typ), the checker's own
// function (exprcheck.go), so the two layers' verdicts and rejection messages
// are identical for the same input.
//
// The two INT members that are not lists: section 1.3.12 leaves the INT row
// "(unchanged, but see RangeString note below)", and that note extends the
// RangeString with an expression evaluating to a range_expr or a list[int]
// "in addition to the original <IntRangeExpr> grammar". A RangeString is a
// Format String (Template Schemas §3.4.1.1.1), so a lone {{...}} evaluating
// to an int or a string produced range text, not a range value. Those results
// are rendered with Value.String() and returned as rangeText, which
// resolveRangeExprField assigns to RangeExpr and parseIntRangeExpr (range.go)
// then reads under internal/openjd's base-spec policy, as it reads a
// hand-typed "1-100:2".
//
// The dispatch order below matters, unlike the union's member order
// (expr.UnionOf sorts members, and section 1.2.3's match-first rule is
// order-independent; see rangeExprFieldType). A list or range_expr result
// must be recognized before the text fallback. Sent down the text path, a
// lone {{ range_expr("10-15:2,1-5") }} would be re-parsed by
// parseIntRangeExpr, whose policy (Policy{PositiveStepOnly, AscendingOnly}
// via internal/openjd/intrange) differs from internal/openjd/expr's zero
// Policy in three ways that must be kept: it rejects start > end, rejects a
// negative step, and expands in first-seen rather than increasing order. The
// stricter policy would reject or reorder a range the expression language
// produced. TestRangeCheckerResolverAgreement_LoneRangeExprPolicy expects
// [1,2,3,4,5,10,12,14] (ascending) for that call, where the text path would
// give [10,12,14,1,2,3,4,5].
//
// expr.Coerce (coerce.go) implements range_expr -> list[int] as one of
// section 1.2.3's list rules, on the value, so the range_expr arm uses the
// same conversion as the language's list(range_expr). A range_expr result and
// a list[int] result (e.g. from range(), funcslist.go) are indistinguishable
// to this function's caller.
//
// Every element is rendered with Value.String(), the same conversion
// coerceScalar's CodeString case performs and the rendering the worker uses
// for a value reaching a template's output text (fmtres/expres.go's
// resolveFormatStringExpr, lone-reference branch). A computed FLOAT with no
// submitted text renders as formatFloat's shortest round-tripping decimal
// (value.go), as the specification states
// (wiki/2026-02-Expression-Language.md, section 1.3.4, "Float Value
// Pass-Through": "When an operation is performed on a float value ... string
// interpolation uses the shortest decimal string representation", example
// "{{Param.V + 1}} outputs \"4.5\""). With Param.Scale=2.5: Scale*2 -> "5.0",
// Scale+0.5 -> "3.0".
func evalRangeExprField(
	body string, typ TaskParamType, syms expr.MapSymbols, lim ExprLimits,
) (rangeText *string, rangeList []string, err error) {
	v, err := expr.Eval(body, syms, rangeExprFieldType(typ), lim.evalOptions()...)
	if err != nil {
		return nil, nil, err
	}
	if v.IsUnresolved() {
		// Not expected in production: Submit binds job parameters
		// (prepareTemplate) before resolving, so syms binds every ScopeJob
		// symbol concrete, and ScopeJob has no symbol family that stays a
		// placeholder. Reported as an error rather than producing an empty
		// list, so an unresolved value never reaches output unnoticed (see
		// expr/value.go's mustBe doc comment).
		return nil, nil, fmt.Errorf("range expression %q produced an unresolved value", body)
	}

	// Order matters; see the doc comment. The switch also guards AsList(),
	// which panics (Value.mustBe) for any payload other than CodeList: the
	// INT target's int and string members let an int or string result reach
	// here.
	switch v.Type.Code {
	case expr.CodeList:
		return nil, listValueTexts(v), nil
	case expr.CodeRangeExpr:
		lv, cerr := expr.Coerce(v, expr.ListOf(expr.TInt))
		if cerr != nil {
			return nil, nil, cerr
		}
		return nil, listValueTexts(lv), nil
	default:
		text := v.String()
		return &text, nil, nil
	}
}

// listValueTexts renders a list value's elements to the text form a
// RangeList entry carries. See evalRangeExprField's doc comment for why
// Value.String() is the right rendering and what it means for a computed
// FLOAT.
func listValueTexts(v expr.Value) []string {
	elems := v.AsList()
	out := make([]string, len(elems))
	for i, elem := range elems {
		out[i] = elem.String()
	}
	return out
}

// rangeExprElemType maps a task-parameter's declared type to the element
// type rangeExprFieldType wraps, and the type a single RangeList entry is
// evaluated against, per section 1.3.12's extended-range table (see
// evalRangeExprField's doc comment).
//
// INT and CHUNK[INT] share expr.TInt: expand.go's expandChunkInt reads a
// CHUNK[INT] definition's RangeList as individual integer strings and groups
// them into chunks itself, so resolution only produces the flat per-value
// list.
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

// resolveChunkFields resolves a CHUNK[INT] definition's two @fmtstring sizing
// fields, which -- like range -- are resolved at job creation against the
// bound job parameters. c is returned as-is when neither field is a format
// string; otherwise a fresh copy comes back with each resolved field set and its Expr
// cleared. A budget or deadline stop leaves the Expr in place and reports
// nothing here: the caller's b.errs()/b.deadline() discards the whole result.
func resolveChunkFields(
	b *templateBudget, exprEnabled bool, i int, c *TaskChunks, syms expr.MapSymbols, scope fmtstring.Scope,
) (*TaskChunks, ValidationErrors) {
	if c == nil || (c.DefaultTaskCountExpr == nil && c.TargetRuntimeSecondsExpr == nil) {
		return c, nil
	}
	out := *c
	base := fmt.Sprintf("/parameterSpace/taskParameterDefinitions/%d/chunks", i)
	var errs ValidationErrors
	if raw := c.DefaultTaskCountExpr; raw != nil {
		n, ok, e := resolveChunkInt(b, exprEnabled, *raw, base+"/defaultTaskCount", 1, syms, scope)
		errs = append(errs, e...)
		if ok {
			out.DefaultTaskCount, out.DefaultTaskCountExpr = n, nil
		}
	}
	if raw := c.TargetRuntimeSecondsExpr; raw != nil {
		n, ok, e := resolveChunkInt(b, exprEnabled, *raw, base+"/targetRuntimeSeconds", 0, syms, scope)
		errs = append(errs, e...)
		if ok {
			out.TargetRuntimeSeconds, out.TargetRuntimeSecondsExpr = &n, nil
		}
	}
	return &out, errs
}

// resolveChunkInt resolves one sizing format string to an integer of at least
// minimum. On the EXPR path it is one charged position evaluated toward
// expr.TInt, and a wall-clock stop is diverted onto the budget (ok=false, no
// error) as resolveTaskParamDefinition diverts one; on the base-spec
// path it is plain substitution.
func resolveChunkInt(
	b *templateBudget, exprEnabled bool, raw, ptr string, minimum int, syms expr.MapSymbols, scope fmtstring.Scope,
) (n int, ok bool, errs ValidationErrors) {
	if exprEnabled && !b.chargePositions(1, ptr) {
		return 0, false, nil
	}
	// An INT range-list entry is resolved as a sizing field must be:
	// toward expr.TInt under EXPR, plain substitution otherwise.
	text, err := resolveRangeListEntry(exprEnabled, raw, TaskParamTypeInt, syms, scope, b.limits)
	if err != nil {
		if b.recordDeadline(err) {
			return 0, false, nil
		}
		return 0, false, ValidationErrors{{Pointer: ptr, Message: err.Error()}}
	}
	n64, convErr := expr.ParseIntString(text)
	if convErr != nil {
		return 0, false, ValidationErrors{{Pointer: ptr, Message: fmt.Sprintf("resolved to %q, which is not an integer", text)}}
	}
	n = int(n64)
	if n < minimum {
		return 0, false, ValidationErrors{{Pointer: ptr, Message: fmt.Sprintf("resolved to %d; must be at least %d", n, minimum)}}
	}
	return n, true, nil
}
