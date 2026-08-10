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
// tmpl is the job template ps was parsed from. It exists on this signature
// for exactly two things, both gated on tmpl.hasExtension("EXPR") (tmpl may
// be nil, which is treated the same as an extension-less template):
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
// On a malformed reference, an unknown variable, or (EXPR-declared only) an
// expression evaluation error — at the whole-field RangeExpr, an individual
// RangeList entry, or an embedded reference within either — a
// [ValidationError] is accumulated with a pointer of the form
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
		} else if len(def.RangeList) > 0 {
			// else if, not a second independent if: the parser guarantees
			// exactly one of RangeExpr/RangeList is set per definition (see
			// TaskParamDefinition's own doc comment), so this branch is
			// unreachable when the RangeExpr branch above ran — but if that
			// invariant were ever violated, a plain "if" here would silently
			// overwrite whatever the RangeExpr branch just wrote into
			// newDef.RangeList using the ORIGINAL def.RangeList, while leaving
			// RangeExpr cleared (nil) from the branch above: a corrupted
			// parameter space (list AND nil range field) with no error raised.
			// else if makes that combination structurally impossible instead
			// of merely unlikely.
			newList := make([]string, len(def.RangeList))
			for j, entry := range def.RangeList {
				eptr := fmt.Sprintf("/parameterSpace/taskParameterDefinitions/%d/range/%d", i, j)
				resolved, err := resolveRangeListEntry(exprEnabled, entry, def.Type, syms, scope)
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
// evalRangeExprList's list-producing path. That is deliberate, and it is NOT
// a second occurrence of section 2.1's trap (see evalRangeExprList's own doc
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
// evalRangeExprList evaluates it as a VALUE (list[elemType] or, for a
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
			list, err := evalRangeExprList(body, typ, syms)
			if err != nil {
				return nil, nil, err
			}
			return nil, list, nil
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
		// rendering evalRangeExprList's elements already use and is total
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
// production for the same reason evalRangeExprList's identical guard is (see
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
