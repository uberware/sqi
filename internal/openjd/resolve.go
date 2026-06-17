// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import (
	"fmt"

	"github.com/uberware/sqi/internal/openjd/fmtstring"
)

// ResolveParameterSpaceParams returns a new *StepParameterSpace with every
// {{Param.<name>}} and {{RawParam.<name>}} reference in RangeExpr and
// RangeList entries substituted with the corresponding value from jobParams.
//
// Both "Param.<name>" and "RawParam.<name>" resolve to the same bound string
// value — path mapping is worker-side and out of scope at submit time.
//
// The input ps is never mutated; a fresh struct (with a new slice of
// TaskParamDefinition values) is always returned on success.
//
// If ps is nil, (nil, nil) is returned immediately.
//
// On a malformed reference or an unknown variable, a [ValidationError] is
// accumulated with a pointer of the form
//
//	/parameterSpace/taskParameterDefinitions/<i>/range
//	/parameterSpace/taskParameterDefinitions/<i>/range/<j>   (RangeList entry j)
//
// All task-parameter definitions are inspected before returning so callers
// receive a complete error list in one round-trip. On any error (nil, errs)
// is returned.
func ResolveParameterSpaceParams(ps *StepParameterSpace, jobParams map[string]string) (*StepParameterSpace, ValidationErrors) {
	if ps == nil {
		return nil, nil
	}

	// Build a fmtstring scope: each job-param name → value, exposed as both
	// "Param.<name>" and "RawParam.<name>".
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
			resolved, err := fmtstring.Resolve(*def.RangeExpr, scope)
			if err != nil {
				errs = append(errs, ValidationError{
					Pointer: ptr,
					Message: err.Error(),
				})
				// Do not assign to newDef.RangeExpr; keep processing to accumulate.
			} else {
				newDef.RangeExpr = &resolved
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
