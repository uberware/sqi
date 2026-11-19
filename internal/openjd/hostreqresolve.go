// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import (
	"fmt"
	"slices"
	"strings"
)

// resolveHostRequirements resolves every format string in a step's host
// requirements — amount and attribute names (Template Schemas §3.3), amount
// bounds and attribute values — against the step's stepScope, and then
// applies the host-requirement constraints to the RESOLVED value: a name's
// prefix, reserved scope and length, no two names alike, an amount bound that
// is a number.
//
// The scheduler compares what this returns with worker capabilities, so an
// unresolved "{{Param.Min}}" bound reaching it could never parse and the step
// could never be scheduled. hr is returned unchanged when it holds no format
// string, having already been validated as written. enforceLimits gates the
// size caps (name length, value counts) exactly as validation does.
func resolveHostRequirements(
	b *templateBudget, sc stepScope, hr *HostRequirements, stepIdx int, enforceLimits bool,
) (*HostRequirements, ValidationErrors) {
	if !hostRequirementsHaveFormatStrings(hr) {
		return hr, nil
	}
	base := fmt.Sprintf("/steps/%d/hostRequirements", stepIdx)
	r := hostReqResolver{b: b, sc: sc}
	out := &HostRequirements{
		Amounts:    make([]AmountRequirement, len(hr.Amounts)),
		Attributes: make([]AttributeRequirement, len(hr.Attributes)),
	}
	for i, a := range hr.Amounts {
		ptr := fmt.Sprintf("%s/amounts/%d", base, i)
		a.Name = r.resolve(a.Name, ptr+"/name")
		a.Min = r.resolvePtr(a.Min, ptr+"/min")
		a.Max = r.resolvePtr(a.Max, ptr+"/max")
		out.Amounts[i] = a
	}
	for i, a := range hr.Attributes {
		ptr := fmt.Sprintf("%s/attributes/%d", base, i)
		a.Name = r.resolve(a.Name, ptr+"/name")
		a.AnyOf = r.resolveAll(a.AnyOf, ptr+"/anyOf")
		a.AllOf = r.resolveAll(a.AllOf, ptr+"/allOf")
		out.Attributes[i] = a
	}
	r.errs = append(r.errs, b.errs()...)
	if len(r.errs) > 0 || b.deadline() != nil {
		return nil, r.errs
	}

	errs := validateHostRequirements(*out, base, true, nil)
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
	b    *templateBudget
	sc   stepScope
	errs ValidationErrors
}

// resolve resolves one host-requirement string as a string-valued format
// string, charging one position under EXPR, the way a range-list entry of a
// STRING task parameter is resolved.
func (r *hostReqResolver) resolve(s, ptr string) string {
	if !strings.Contains(s, "{{") || (r.sc.exprEnabled && !r.b.chargePositions(1, ptr)) {
		return s
	}
	out, err := resolveRangeListEntry(r.sc.exprEnabled, s, TaskParamTypeString, r.sc.syms, r.sc.scope, r.b.limits)
	if err != nil {
		if !r.b.recordDeadline(err) {
			r.errs = append(r.errs, ValidationError{Pointer: ptr, Message: err.Error()})
		}
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
	if hr == nil {
		return false
	}
	return slices.ContainsFunc(hostRequirementPositions(*hr, "", nil), func(p fmtPosition) bool {
		return strings.Contains(p.text, "{{")
	})
}
