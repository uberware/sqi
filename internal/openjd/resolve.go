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
// as an extension-less template). They exist on this signature for exactly
// three things, all gated on tmpl.hasExtension("EXPR"):
//
//   - deciding whether a whole-field RangeExpr that is a LONE {{...}}
//     expression (fmtstring.LoneRef — the entire field is one reference, no
//     surrounding text) is section 1.3.12's extended range form and should be
//     EVALUATED into a value rather than substituted as plain text — and,
//     for every other shape (a RangeList entry, or a RangeExpr/RangeString
//     that is literal text or has a reference embedded in surrounding text),
//     whether each embedded {{...}} is read as an EXPRESSION (evaluated,
//     expr.TAny, rendered to text) rather than a bare dotted-identifier
//     lookup (resolveFormatStringExpr, resolveRangeListEntry);
//   - building the phase-2 ScopeJob symbol table that evaluation runs
//     against, with jobParams already bound concrete —
//     Submitter.prepareTemplate's step 2d re-check runs the identical
//     construction over the identical map, so a range expression this
//     package resolves and the one checkTemplateExpressions already accepted
//     read the same job-parameter values the same way;
//   - evaluating step's OWN let: block and merging the names it binds into
//     that table, which is what section 3.6.2 row 1 grants a step template's
//     let: at the parameterSpace position. Both halves are shared with the
//     checker rather than reimplemented (exprcheck.go's stepLetSymbols and
//     rangeScopeSymbols), because them being built separately is exactly how
//     they drifted: before EXPR sub-project E4b's whole-branch review this
//     function passed no step at all, so a step-level let: ["base = 10"] with
//     range: "{{ [base, base + 1] }}" validated at upload, passed the phase-2
//     re-check, and then failed HERE with unknown symbol "base" — a symbol
//     the checker had just certified.
//
// A nil tmpl, or one that does not declare EXPR, takes exactly the base-spec
// substitution path this function has always taken — see
// [resolveRangeExprField]'s own doc comment for why that matters and how it
// is proven. step is read only on the EXPR path, so a base-spec caller may
// pass nil for it freely.
//
// The input ps is never mutated; a fresh struct (with a new slice of
// TaskParamDefinition values) is always returned on success.
//
// If ps is nil, (nil, nil) is returned immediately.
//
// On a malformed reference, an unknown variable, or (EXPR-declared only) an
// expression evaluation error — at the whole-field RangeExpr, an individual
// RangeList entry, or an embedded reference within either — a
// [ValidationError] is accumulated with a pointer of the form
//
//	/parameterSpace/taskParameterDefinitions/<i>/range
//	/parameterSpace/taskParameterDefinitions/<i>/range/<j>   (RangeList entry j)
//
// A malformed or unevaluatable entry in step's own let: block is reported the
// same way, at pointer /let/<i> — the checker has already rejected it by the
// time production reaches here, but reporting it directly beats letting every
// range expression that referenced the failed binding fail with a misleading
// "unknown symbol" instead.
//
// All task-parameter definitions are inspected before returning so callers
// receive a complete error list in one round-trip UNLESS the template-wide
// budget (below) trips first, in which case the walk stops early -- see that
// paragraph.
//
// budget is EXPR sub-project E4c's Task 4 addition: an optional
// [templateBudget], shared with THE SAME SUBMISSION's checkTemplateExpressions
// call (submit.go's prepareTemplate builds one and threads it through Submit's
// step loop to every call this function makes for that one submission). Every
// task-parameter range position this function resolves -- a RangeList entry,
// a whole-field RangeExpr, and the step's own let: block -- is ALSO one of
// checkTemplateExpressions' own positions (checkParameterSpaceExpressions,
// exprcheck.go), so before this task NEITHER walk's per-call budget saw the
// other's cost: a template that individually fit under checkTemplateExpressions'
// cap and under a hypothetical resolver-only cap could still, when both walks
// run back to back for the same submission (exactly what Submit does), spend
// twice the position count neither budget alone would catch -- see
// TestPhase2Budget_ResolverSharesCheckerBudget (resolve_budget_test.go) for
// the construction that proves it, mirroring EXPR sub-project E4b's own
// finding that these positions are now evaluated TWICE. Omitting budget (as
// every pre-Task-4 caller does) gives this call its
// own fresh, throwaway allowance -- see [templateBudgetOrFresh] -- which is
// harmless: no legitimate single call comes close to either dimension's cap on
// its own.
//
// The budget is consulted, and charged, ONLY on the EXPR-enabled path: a
// template that does not declare EXPR takes the exact base-spec substitution
// this function has always performed, byte for byte, with no budget
// interaction of any kind -- matching every other EXPR-only behavior this
// function gates on exprEnabled.
func ResolveParameterSpaceParams(
	tmpl *JobTemplate, step *StepTemplate, ps *StepParameterSpace, jobParams map[string]string,
	budget ...*templateBudget,
) (*StepParameterSpace, ValidationErrors) {
	if ps == nil {
		return nil, nil
	}

	exprEnabled := tmpl != nil && tmpl.hasExtension("EXPR")
	b := templateBudgetOrFresh(budget)

	var errs ValidationErrors

	// If a budget shared with this submission's checkTemplateExpressions call
	// is ALREADY exhausted (by that call, or by an earlier step's own
	// ResolveParameterSpaceParams call in the same submission), stop before
	// doing any of this step's real work -- evaluating its own let: block
	// still costs a real expr.Eval per binding, and there is nothing left to
	// spend it on.
	if exprEnabled && !b.ok() {
		return nil, b.errs()
	}

	// The phase-2 symbol table, built only when EXPR is declared, and built by
	// the CHECKER'S OWN two helpers so the two layers cannot disagree about
	// what is in scope: ScopeJob's fixed and family symbols (section 1.3.12's
	// range field sits at the same ScopeJob position host requirements do —
	// see exprcheck.go's checkStepExpressions) plus, per section 3.6.2 row 1,
	// the names step's own let: block binds. Left nil (never read) when EXPR
	// is not declared, which is the point — see resolveRangeExprField.
	var syms expr.MapSymbols
	if exprEnabled {
		stepLet, letErrs := stepLetSymbols(tmpl, step, jobParams, "")
		errs = append(errs, letErrs...)

		// Charged exactly like checkStepExpressions charges the identical
		// stepLetSymbols call (exprcheck.go): one position per binding
		// actually attempted (letPositions mirrors checkLetBindings' own
		// maxLetBindings truncation), and the net retained bytes stepLet
		// itself IS -- stepLetSymbols already returns exactly the diff, so
		// there is no before/after subtraction to do here.
		var letCount int
		if step != nil {
			letCount = len(step.Let)
		}
		b.chargePositions(letPositions(letCount), "/let")
		b.chargeRetainedBytes(templateExprRetainedBytes(stepLet), "/let")

		syms = rangeScopeSymbols(tmpl, stepLet, jobParams)
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

	newDefs := make([]TaskParamDefinition, len(ps.TaskParameterDefinitions))

	for i, def := range ps.TaskParameterDefinitions {
		if exprEnabled && !b.ok() {
			// The budget tripped partway through -- on this step's own let:
			// block, or an earlier definition in this same step, or an
			// earlier STEP's call sharing this budget. Stop resolving; the
			// remaining definitions are left zero-valued, which is fine --
			// b.errs() below makes errs non-empty, so the whole result is
			// discarded on the way out (the "if len(errs) > 0" branch just
			// below).
			break
		}
		newDef, defErrs := resolveTaskParamDefinition(b, exprEnabled, i, def, syms, scope)
		errs = append(errs, defErrs...)
		newDefs[i] = newDef
	}

	errs = append(errs, b.errs()...)

	if len(errs) > 0 {
		return nil, errs
	}

	out := *ps // shallow copy — Combination pointer is shared but never modified
	out.TaskParameterDefinitions = newDefs
	return &out, nil
}

// resolveTaskParamDefinition resolves ONE task-parameter definition's range
// field -- either shape, RangeExpr or RangeList -- charging b (when
// exprEnabled) exactly as ResolveParameterSpaceParams' own inline loop body
// did before EXPR sub-project E4c's Task 4 extracted it: this split exists
// only to keep ResolveParameterSpaceParams' own cyclomatic complexity within
// the repo's lint budget (see CLAUDE.md's "Lint is strict" convention;
// exprcheck.go's checkHostRequirementAmount/checkHostRequirementAttribute
// split for the identical reason), not because the two range shapes needed
// separating for any behavioral reason -- the "else if, not a second
// independent if" invariant comment on the RangeList branch, below, is the
// same one that lived inline before this split.
//
// Returns the definition's own copy (Name/Type/Chunks/Combination unchanged,
// Range* fields resolved when successful) and any errors resolving it
// produced. On a budget-exhausted charge, the copy is returned unresolved and
// with no errors of its own -- the caller's own b.errs() call, once the whole
// walk stops, is what turns that into a reported failure.
func resolveTaskParamDefinition(
	b *templateBudget, exprEnabled bool, i int, def TaskParamDefinition, syms expr.MapSymbols, scope fmtstring.Scope,
) (TaskParamDefinition, ValidationErrors) {
	newDef := def // shallow copy — Name, Type, Chunks, Combination are unchanged

	if def.RangeExpr != nil {
		ptr := fmt.Sprintf("/parameterSpace/taskParameterDefinitions/%d/range", i)
		if exprEnabled && !b.chargePositions(1, ptr) {
			return newDef, nil
		}
		newRangeExpr, newRangeList, rerr := resolveRangeExprField(exprEnabled, *def.RangeExpr, def.Type, syms, scope)
		if rerr != nil {
			// Do not assign newDef.RangeExpr/RangeList on error -- returning
			// the untouched copy alongside the error is what lets the
			// CALLER'S loop keep accumulating every other definition's
			// errors in the same pass (unless the budget stops it first).
			return newDef, ValidationErrors{{Pointer: ptr, Message: rerr.Error()}}
		}
		newDef.RangeExpr = newRangeExpr
		newDef.RangeList = newRangeList
		return newDef, nil
	}

	if len(def.RangeList) == 0 {
		// else if, not a second independent if, in the pre-split inline form:
		// the parser guarantees exactly one of RangeExpr/RangeList is set per
		// definition (see TaskParamDefinition's own doc comment), so this
		// early return only fires when RangeExpr is nil too -- an empty,
		// range-less definition. Kept as an explicit early return rather than
		// an else so a caller reading top to bottom sees "RangeExpr wins, or
		// there is nothing to resolve" before the RangeList branch, matching
		// the original's own ordering guarantee: this function can never
		// silently overwrite a RangeExpr-branch result with the ORIGINAL
		// (unresolved) def.RangeList the way a bare "if" instead of "else if"
		// would have.
		return newDef, nil
	}

	newList, errs := resolveRangeListDefinition(b, exprEnabled, i, def, syms, scope)
	newDef.RangeList = newList
	return newDef, errs
}

// resolveRangeListDefinition resolves every entry of one task-parameter
// definition's RangeList -- split out of resolveTaskParamDefinition for the
// same complexity-budget reason that function's own doc comment gives.
func resolveRangeListDefinition(
	b *templateBudget, exprEnabled bool, i int, def TaskParamDefinition, syms expr.MapSymbols, scope fmtstring.Scope,
) ([]string, ValidationErrors) {
	var errs ValidationErrors
	newList := make([]string, len(def.RangeList))
	for j, entry := range def.RangeList {
		if exprEnabled && !b.ok() {
			break
		}
		eptr := fmt.Sprintf("/parameterSpace/taskParameterDefinitions/%d/range/%d", i, j)
		if exprEnabled && !b.chargePositions(1, eptr) {
			break
		}
		resolved, err := resolveRangeListEntry(exprEnabled, entry, def.Type, syms, scope)
		if err != nil {
			errs = append(errs, ValidationError{Pointer: eptr, Message: err.Error()})
			newList[j] = entry // placeholder; result discarded on error
		} else {
			newList[j] = resolved
		}
	}
	return newList, errs
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
// nothing else) is it section 1.3.12's whole-field form, evaluated against
// syms by evalRangeExprField. That evaluation returns EITHER a range list (a
// list or range_expr result) OR range TEXT (an int or string result, which
// for an INT/CHUNK[INT] field is the section's own "in addition to the
// original <IntRangeExpr> grammar" case) — see that function. Any other shape —
// literal text with no {{}} at all ("1-100:2"), or a {{...}} reference
// embedded in surrounding text ("1-{{Param.End}}") — is NOT a whole-field
// list expression, but as of this task it is still a section 1.3.2 format
// string, and §1.3.12 says exactly that of the RangeString: "it is a format
// string, it can now contain an expression". So it is resolved through
// resolveFormatStringExpr (below), the same embedded-reference machinery
// checkFormatString (exprcheck.go) already checks it with, evaluating each
// {{...}} segment at expr.TAny and rendering with Value.String() — NOT
// through fmtstring.Resolve, which only understands a bare dotted-identifier
// reference and rejects anything else as "not a valid dotted identifier".
//
// Before this task, that mismatch was exactly the checker/resolver drift
// EXPR sub-project E4b Task 1's review found: a template with
// range: "1-{{ Param.End * 2 }}" and extensions: [EXPR] validated cleanly
// (checkParameterSpaceExpressions checks the WHOLE RangeExpr field, lone or
// not, through checkFormatString) and then failed at submit here, because
// this function still routed the non-lone case to base-spec
// fmtstring.Resolve. Now both agree: both walk the field as segments and
// evaluate each embedded reference as an expression.
//
// The result of this branch is still TEXT, assigned back into rangeExpr,
// exactly as the base-spec branch below does — NOT a value handed to
// evalRangeExprField's list-producing path. That is deliberate, and it is NOT
// a second occurrence of section 2.1's trap (see evalRangeExprField's own doc
// comment for the trap's full statement) — but NOT for the reason an earlier
// version of this comment gave. An embedded reference here CAN legitimately
// evaluate to a genuine range_expr-typed value — "{{ range_expr(\"10-15:2,1-5\") }},7"
// is a real, reachable input, and Value.String() renders a range_expr value
// back to ITS OWN <IntRangeExpr> text (rangeexpr.go: the value's payload IS
// the text range_expr() was called with, unmodified) — so "there is no
// range_expr-typed VALUE here to mis-stringify" is FALSE; one can be, and
// String() does stringify it. Confirmed, and pinned by
// TestResolveParameterSpaceParams_NonLoneRangeExprEmbeddedRangeExprValue:
// resolving "{{ range_expr(\"10-15:2,1-5\") }},7" produces RangeExpr
// "10-15:2,1-5,7", which ExpandParameterSpace's parseIntRangeExpr then
// expands under internal/openjd's OWN first-seen policy to
// [10,12,14,1,2,3,4,5,7] — the SPEC's own ascending, deduplicated order
// (section 3.4.1.1.1) for that same composed text would be
// [1,2,3,4,5,7,10,12,14], so this really does reorder relative to the
// expression language.
//
// The ruling (coordinator, EXPR sub-project E4b Task 2 fix round 1): KEEP
// this behavior — it is correct, not a defect. Section 2.1's rule is scoped
// to the LONE whole-field form, where the expression IS the range and
// evalRangeExprField evaluates it as a VALUE (list[elemType] or, for a
// genuine range_expr, range_expr's own list[int] coercion — never
// re-entering <IntRangeExpr> text at all, which is what section 2.1 actually
// forbids). It does not govern a reference EMBEDDED in surrounding range
// syntax, which is a different position: section 1.3.2 states plainly that
// an embedded reference evaluates at expr.TAny and converts to a STRING,
// full stop, regardless of what type that value happens to be — a
// range_expr is not special-cased out of that rule, and treating it as
// special would mean this branch reads the embedded value's TYPE to decide
// how to render it, which format-string interpolation never does anywhere
// else in this codebase (worker fmtres's evalEmbeddedRef renders every
// TAny result the same way, range_expr included). The COMPOSED result —
// literal text plus the range_expr's own rendered text plus more literal
// text — is, structurally, an ordinary base-spec <RangeString>: text a human
// author could have typed by hand, no different in kind from "1-10" in the
// arithmetic example above. Base-spec text is parsed under
// internal/openjd's OWN <IntRangeExpr> policy (parseIntRangeExpr,
// range.go) — that policy's three deliberate divergences from the
// expression language's (start > end rejected, negative step rejected,
// first-seen rather than ascending order — see this repo's CLAUDE.md, which
// records and preserves all three on purpose) apply here exactly as they do
// to any other literal range string, INCLUDING one assembled partly from an
// embedded range_expr's own text. This wave does not change that policy or
// its divergences; it only makes the checker and resolver AGREE about which
// text reaches it.
//
// The resulting text is precisely what a base-spec author would have typed
// by hand, and — for INT/CHUNK[INT] — legitimately belongs to expand.go's
// parseIntRangeExpr afterward, the same as any other literal <IntRangeExpr>
// string. Declaring EXPR does not change how an ordinary base-spec RangeExpr
// resolves in VALUE, only in how its embedded references are evaluated (as
// expressions, not dotted-identifier lookups) — and, per section 1.3.12,
// only what a WHOLE-FIELD LONE expression means.
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
			// evalRangeExprField returns this function's own two-result shape
			// directly, so there is nothing to adapt: exactly one of the two
			// is non-nil either way.
			return evalRangeExprField(body, typ, syms)
		}

		resolved, err := resolveFormatStringExpr(raw, syms, expr.TAny, submissionLimits()...)
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

// resolveRangeListEntry resolves one RangeList entry (a single FLOAT|STRING|
// PATH range value, or occasionally a literal INT/CHUNK[INT] value —
// TaskParamDefinition.RangeList's own doc comment), following the exact same
// exprEnabled gate resolveRangeExprField's non-EXPR branch does: when
// exprEnabled is false this is EXACTLY fmtstring.Resolve run over entry, byte
// for byte unchanged from before this task.
//
// When exprEnabled is true, entry resolves through resolveFormatStringExpr
// with target rangeExprElemType(typ) — matching
// checkParameterSpaceExpressions's own checkFormatString call for a RangeList
// entry exactly (design spec §3): a LONE {{...}} reference is evaluated once
// against the task parameter's own declared element type (so an int/float/
// path value that needs narrowing, e.g. an integral float for an INT entry,
// gets the SAME §1.2.3 coercion the checker just verified would succeed —
// not merely a widening to text), and anything else (literal text, or a
// reference embedded in surrounding text) evaluates each embedded reference
// at expr.TAny and concatenates. Either way, the entry's TEXT (Value.String())
// is what is stored back into RangeList, and expand.go (validateIntList,
// validateFloatList, PATH's own non-empty check, and so on) is what parses
// that text into the parameter's actual type, exactly as it already does for
// a literal (non-EXPR) entry.
//
// Before this task, this function targeted TargetString regardless of typ,
// which is what let it drift from the checker the moment the checker started
// targeting the element type: the checker would accept an INT entry like
// "{{ Param.Scale * 2 }}" with Scale=2.5 (float -> int is a legal §1.2.3
// coercion and 5.0 is integral), while this function, still at TargetString,
// rendered that same entry "5.0" — text validateIntList rejects outright
// ("invalid integer \"5.0\""). Checker accepts, expansion fails: the exact
// drift class resolveRangeExprField (above) closes for the whole-field
// position, reopened here by tightening only one side. Threading typ through
// closes it: both this function and checkParameterSpaceExpressions now target
// rangeExprElemType(typ), so a checker accept and a resolver success are the
// same coercion, not two independent judgment calls that happened to agree
// before either side had a real target.
func resolveRangeListEntry(exprEnabled bool, entry string, typ TaskParamType, syms expr.MapSymbols, scope fmtstring.Scope) (string, error) {
	if exprEnabled {
		return resolveFormatStringExpr(entry, syms, rangeExprElemType(typ), submissionLimits()...)
	}
	return fmtstring.Resolve(entry, scope)
}

// resolveFormatStringExpr resolves one EXPR-aware format-string field —
// either a RangeList entry (loneTarget rangeExprElemType(typ): TInt, TFloat,
// TString or TPath, per the entry's own declared task-parameter type — see
// resolveRangeListEntry's own doc comment) or a whole-field RangeString that
// is not a lone reference (loneTarget expr.TAny, since resolveRangeExprField's
// caller already special-cased and consumed the lone case before this
// function ever runs for that position) — against syms.
//
// This mirrors internal/worker/fmtres/expres.go's resolveFormatStringExpr
// and internal/openjd/exprcheck.go's checkFormatString exactly, per section
// 1.3.2: a format string that is EXACTLY one reference with no surrounding
// text (fmtstring.LoneRef) is transparent and inherits loneTarget; anything
// else — literal text, an embedded reference, or several references — walks
// fmtstring.Segments and evaluates each reference unconstrained (expr.TAny),
// rendering with Value.String() and treating a null result as the empty
// string (RFC 0005), concatenated with the literal text around it.
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
		// Value.String(), NOT v.AsStr(): AsStr panics (Value.mustBe) for any
		// payload code other than string, and loneTarget is a caller-supplied
		// PARAMETER, not a constant — resolveRangeListEntry now passes
		// rangeExprElemType(typ) (design spec §3), which is TInt/TFloat/TPath
		// for an INT/FLOAT/PATH entry, not always TString. A parameter that
		// varies must not crash the process (inside a synchronous submit
		// handler, no less) the first time it does. String() is the same
		// rendering evalRangeExprField's elements already use and is total
		// over every Value — the safe choice here costs nothing and is what
		// makes threading rangeExprElemType through loneTarget safe at all.
		// See TestResolveFormatStringExpr_NonStringLoneTargetDoesNotPanic.
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
			// RFC 0005: within a format string, the target type is string?
			// and a None/null result is treated as the empty string — unlike
			// Value.String()'s own "null" rendering, which is a DIAGNOSTIC
			// form never meant for this position (see
			// internal/worker/fmtres/expres.go's evalEmbeddedRef, which this
			// mirrors).
			continue
		}
		b.WriteString(v.String())
	}
	return b.String(), nil
}

// errUnresolvedRangeValue reports an EXPR evaluation that produced a
// still-unresolved value at a range-field position — unreachable in
// production for the same reason evalRangeExprField's identical guard is (see
// that function's doc comment): syms binds every ScopeJob symbol concrete by
// the time this package's one caller runs. Reported rather than letting an
// unresolved value's diagnostic "<unresolved[T]>" rendering (Value.String())
// leak into a range value unnoticed — this codebase's standing convention
// (expr/value.go's mustBe doc comment).
func errUnresolvedRangeValue(v expr.Value) error {
	return fmt.Errorf(
		"value is unresolved (%s): the submitted text could not be parsed as its declared type", v.Type,
	)
}

// evalRangeExprField evaluates body — the inner source of a lone whole-field
// {{...}} range expression — against syms, and returns EITHER range text
// (rangeText non-nil) OR range-list entries (rangeList non-nil), never both,
// per section 1.3.12's extended-range table. That is deliberately
// resolveRangeExprField's own (rangeExpr, rangeList) return shape, so its lone
// branch is a bare `return` of this call with nothing to adapt:
//
//	INT / CHUNK[INT]   int | string | range_expr | list[int]
//	FLOAT              list[float]
//	STRING             list[string]
//	PATH               list[path]
//
// The evaluation TARGET is rangeExprFieldType(typ) — literally the CHECKER'S
// OWN function (exprcheck.go), not a second target chosen to match it. That
// is what makes the two layers' accept/reject verdicts, and their rejection
// MESSAGES, identical for the same input. Before EXPR sub-project E4b's
// whole-branch review they were separately chosen and separately too narrow
// in the same direction, so they agreed only by coincidence — the checker
// said "cannot be coerced to list[int] | range_expr" and this function said
// "cannot be coerced to list[int]" for one and the same template.
//
// THE TWO INT MEMBERS THAT ARE NOT LISTS. Section 1.3.12 leaves the INT row
// "(unchanged, but see RangeString note below)", and that note extends the
// RangeString with an expression evaluating to a range_expr or a list[int]
// "IN ADDITION TO the original <IntRangeExpr> grammar". A RangeString is
// itself a Format String (Template Schemas §3.4.1.1.1), so a lone {{...}}
// evaluating to an int or a string is still that original grammar: the
// expression produced range TEXT, not a range value. Those two are therefore
// rendered with Value.String() and handed back as rangeText, which
// resolveRangeExprField assigns to RangeExpr and expand.go's
// parseIntRangeExpr then reads under internal/openjd's own base-spec policy —
// exactly as it reads a hand-typed "1-100:2", and exactly consistent with the
// embedded-reference ruling in resolveRangeExprField's doc comment: text a
// human could have typed by hand is base-spec text.
//
// THE DISPATCH ORDER BELOW IS LOAD-BEARING, in a way the union's own member
// order is not (expr.UnionOf sorts members, and section 1.2.3's match-first
// rule is implemented order-independently — see rangeExprFieldType). A list
// and a range_expr result MUST be recognized BEFORE the text fallback. Send a
// lone {{ range_expr("10-15:2,1-5") }} down the text path instead and it
// would be re-parsed by parseIntRangeExpr, which is precisely the design
// spec's section 2.1 trap. That trap, stated in full: internal/openjd's OWN
// <IntRangeExpr> reader (parseIntRangeExpr, expand.go, via
// internal/openjd/intrange with Policy{PositiveStepOnly, AscendingOnly})
// applies a policy that deliberately DIFFERS from internal/openjd/expr's (the
// zero Policy) in three ways this repo has decided to preserve — it rejects
// start > end, rejects a negative step, and expands in first-seen rather than
// increasing order. Re-admitting an expression-produced range_expr under the
// STRICTER policy would silently reject, or silently reorder, a range the
// expression language legitimately produced.
// TestRangeCheckerResolverAgreement_LoneRangeExprPolicy is the detector for
// exactly that mistake: it pins [1,2,3,4,5,10,12,14] (ascending, expr's
// policy) for that call, where the text path would give [10,12,14,1,2,3,4,5].
//
// expr.Coerce (coerce.go) already implements range_expr -> list[int] as one
// of section 1.2.3's three list rules — coerceList calls the unexported
// rangeInts directly on the VALUE, with no detour through text at all — so
// the range_expr arm reuses the exact conversion sub-project C1 built for the
// language's own list(range_expr) function, rather than this package writing
// a second, independent implementation of "take the integers out of a
// range_expr". One code path, not two: a range_expr result and a literal
// list[int] result (e.g. from range(), which returns list[int] directly —
// funcslist.go) are indistinguishable by the time this function's caller sees
// them.
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
func evalRangeExprField(
	body string, typ TaskParamType, syms expr.MapSymbols,
) (rangeText *string, rangeList []string, err error) {
	v, err := expr.Eval(body, syms, rangeExprFieldType(typ), submissionLimits()...)
	if err != nil {
		return nil, nil, err
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
		return nil, nil, fmt.Errorf("range expression %q produced an unresolved value", body)
	}

	// Order matters — see the doc comment. Note also that neither of the two
	// list-producing arms may be reached with AsList() called unguarded: that
	// method panics (Value.mustBe) for any payload other than CodeList, and
	// since this task widened the INT target an int or a string result really
	// does arrive here. The switch IS the guard.
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
// INT and CHUNK[INT] share expr.TInt: expand.go's expandChunkInt still reads
// a CHUNK[INT] definition's RangeList as individual integer strings and
// groups them into chunks itself after this package hands them back —
// chunking is not this function's, evalRangeExprField's, or
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
