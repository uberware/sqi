// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import (
	"fmt"
	"strings"

	"github.com/uberware/sqi/internal/openjd/expr"
	"github.com/uberware/sqi/internal/openjd/fmtstring"
)

// resolveHostRequirements resolves every format string in a step's host
// requirements — amount and attribute names (Template Schemas §3.3), amount
// bounds and attribute values — against the bound job parameters, and then
// applies the host-requirement constraints to the RESOLVED value: a name's
// prefix, reserved scope and length, no two names alike, an amount bound
// that is a number.
//
// The scheduler compares what this returns with worker capabilities, so an
// unresolved "{{Param.Min}}" bound reaching it could never parse and the step
// could never be scheduled. hr is returned unchanged when it holds no format
// string, having already been validated as written.
//
// Under EXPR each format string is evaluated against the same ScopeJob symbol
// table the checker uses for these positions (the step's own let: bindings
// included); otherwise it is base-spec substitution. enforceLimits gates the
// size caps (name length, value counts) exactly as validation does.
func resolveHostRequirements(
	b *templateBudget, tmpl *JobTemplate, step *StepTemplate, stepIdx int,
	jobParams map[string]string, enforceLimits bool,
) (*HostRequirements, ValidationErrors) {
	hr := step.HostRequirements
	if hr == nil || !hostRequirementsHaveFormatStrings(hr) {
		return hr, nil
	}
	base := fmt.Sprintf("/steps/%d/hostRequirements", stepIdx)
	r := hostReqResolver{b: b, exprEnabled: tmpl.hasExtension("EXPR")}
	if r.exprEnabled {
		stepLet, letErrs := stepLetSymbols(b, tmpl, step, jobParams, "")
		if len(letErrs) > 0 {
			return nil, letErrs
		}
		r.syms = rangeScopeSymbols(tmpl, stepLet, jobParams)
	}
	r.scope = make(fmtstring.MapScope, len(jobParams)*2)
	for name, value := range jobParams {
		r.scope["Param."+name] = value
		r.scope["RawParam."+name] = value
	}

	out := &HostRequirements{
		Amounts:    make([]AmountRequirement, len(hr.Amounts)),
		Attributes: make([]AttributeRequirement, len(hr.Attributes)),
	}
	for i, a := range hr.Amounts {
		ptr := fmt.Sprintf("%s/amounts/%d", base, i)
		ra := a
		ra.Name = r.resolve(a.Name, ptr+"/name")
		ra.Min = r.resolvePtr(a.Min, ptr+"/min")
		ra.Max = r.resolvePtr(a.Max, ptr+"/max")
		out.Amounts[i] = ra
	}
	for i, a := range hr.Attributes {
		ptr := fmt.Sprintf("%s/attributes/%d", base, i)
		ra := a
		ra.Name = r.resolve(a.Name, ptr+"/name")
		ra.AnyOf = r.resolveAll(a.AnyOf, ptr+"/anyOf")
		ra.AllOf = r.resolveAll(a.AllOf, ptr+"/allOf")
		out.Attributes[i] = ra
	}
	if len(r.errs) > 0 || !b.ok() {
		return nil, r.errs
	}

	errs := validateHostRequirements(*out, base, true)
	if enforceLimits {
		errs = append(errs, validateHostRequirementLimits(*out, base)...)
	}
	if len(errs) > 0 {
		return nil, errs
	}
	return out, nil
}

// hostReqResolver resolves the format strings of one step's host requirements,
// collecting an error per position that fails.
type hostReqResolver struct {
	b           *templateBudget
	exprEnabled bool
	syms        expr.MapSymbols
	scope       fmtstring.MapScope
	errs        ValidationErrors
}

func (r *hostReqResolver) resolve(s, ptr string) string {
	if !strings.Contains(s, "{{") {
		return s
	}
	var out string
	var err error
	if r.exprEnabled {
		if !r.b.chargePositions(1, ptr) {
			return s
		}
		out, err = resolveFormatStringExpr(s, r.syms, expr.TString, r.b.limits.evalOptions()...)
		if err != nil && r.b.recordDeadline(err) {
			return s
		}
	} else {
		out, err = fmtstring.Resolve(s, r.scope)
	}
	if err != nil {
		r.errs = append(r.errs, ValidationError{Pointer: ptr, Message: err.Error()})
		return s
	}
	return out
}

func (r *hostReqResolver) resolvePtr(s *string, ptr string) *string {
	if s == nil {
		return nil
	}
	v := r.resolve(*s, ptr)
	return &v
}

func (r *hostReqResolver) resolveAll(vs []string, ptr string) []string {
	if vs == nil {
		return nil
	}
	out := make([]string, len(vs))
	for k, v := range vs {
		out[k] = r.resolve(v, fmt.Sprintf("%s/%d", ptr, k))
	}
	return out
}

// hostRequirementsHaveFormatStrings reports whether any name or value of hr
// holds a format-string reference.
func hostRequirementsHaveFormatStrings(hr *HostRequirements) bool {
	has := func(s string) bool { return strings.Contains(s, "{{") }
	for _, a := range hr.Amounts {
		if has(a.Name) || (a.Min != nil && has(*a.Min)) || (a.Max != nil && has(*a.Max)) {
			return true
		}
	}
	for _, a := range hr.Attributes {
		if has(a.Name) || strings.Contains(strings.Join(a.AnyOf, "\x00"), "{{") || strings.Contains(strings.Join(a.AllOf, "\x00"), "{{") {
			return true
		}
	}
	return false
}
