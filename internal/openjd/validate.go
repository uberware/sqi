// SPDX-License-Identifier: AGPL-3.0-only

package openjd

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// ─── Validation errors ────────────────────────────────────────────────────────

// ValidationError is a single validation failure with a JSON-pointer path to
// the offending field.
type ValidationError struct {
	// Pointer is a JSON Pointer (RFC 6901) to the offending field, e.g.
	// "/steps/0/dependencies/0/dependsOn".
	Pointer string
	// Message is a human-readable description of the problem.
	Message string
}

func (e ValidationError) Error() string {
	return fmt.Sprintf("%s: %s", e.Pointer, e.Message)
}

// ValidationErrors is the result of [Validate]: a slice of zero or more errors.
type ValidationErrors []ValidationError

func (e ValidationErrors) Error() string {
	if len(e) == 0 {
		return "openjd: no validation errors"
	}
	msgs := make([]string, len(e))
	for i, err := range e {
		msgs[i] = err.Error()
	}
	return strings.Join(msgs, "; ")
}

// ─── identifier pattern ───────────────────────────────────────────────────────

var identifierRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ─── Validate ─────────────────────────────────────────────────────────────────

// Validate performs semantic validation of a parsed [JobTemplate] and returns
// all detected problems.  An empty slice means the template is valid.
//
// Validations performed:
//   - specificationVersion must equal [SpecVersion]
//   - name must not be empty
//   - at least one step is required
//   - job parameter names must be unique and match the identifier pattern
//   - job parameter type must be INT, FLOAT, STRING, or PATH
//   - step names must be unique and non-empty
//   - each step dependency must reference a declared step name
//   - step dependency graph must be acyclic
//   - task parameter names within a step must be unique
//   - task parameter types must be INT, FLOAT, STRING, PATH, or CHUNK[INT]
//   - INT/CHUNK[INT] range expressions must be parseable
//   - combination expression must reference only declared task parameter names
func Validate(t *JobTemplate) ValidationErrors {
	var errs ValidationErrors

	// ── specificationVersion ──────────────────────────────────────────────
	if t.SpecificationVersion == "" {
		errs = append(errs, ValidationError{
			Pointer: "/specificationVersion",
			Message: "required; must be " + SpecVersion,
		})
	} else if t.SpecificationVersion != SpecVersion {
		errs = append(errs, ValidationError{
			Pointer: "/specificationVersion",
			Message: fmt.Sprintf("unsupported version %q; expected %q", t.SpecificationVersion, SpecVersion),
		})
	}

	// ── name ─────────────────────────────────────────────────────────────
	if strings.TrimSpace(t.Name) == "" {
		errs = append(errs, ValidationError{Pointer: "/name", Message: "required"})
	}

	// ── parameterDefinitions ─────────────────────────────────────────────
	errs = append(errs, validateJobParams(t.ParameterDefinitions)...)

	// ── jobEnvironments ───────────────────────────────────────────────────
	errs = append(errs, validateEnvironments(t.JobEnvironments, "/jobEnvironments")...)

	// ── steps ─────────────────────────────────────────────────────────────
	if len(t.Steps) == 0 {
		errs = append(errs, ValidationError{Pointer: "/steps", Message: "at least one step is required"})
	}

	// Build step name set for dependency resolution.
	stepNames := make(map[string]struct{}, len(t.Steps))
	for i, s := range t.Steps {
		if strings.TrimSpace(s.Name) == "" {
			errs = append(errs, ValidationError{
				Pointer: fmt.Sprintf("/steps/%d/name", i),
				Message: "required",
			})
			continue
		}
		if _, dup := stepNames[s.Name]; dup {
			errs = append(errs, ValidationError{
				Pointer: fmt.Sprintf("/steps/%d/name", i),
				Message: fmt.Sprintf("duplicate step name %q", s.Name),
			})
		}
		stepNames[s.Name] = struct{}{}
	}

	for i, s := range t.Steps {
		errs = append(errs, validateStep(s, i, stepNames)...)
	}

	// ── acyclicity ────────────────────────────────────────────────────────
	errs = append(errs, validateNoCycles(t.Steps)...)

	return errs
}

// ─── job parameter validation ─────────────────────────────────────────────────

func validateJobParams(params []JobParameter) ValidationErrors {
	var errs ValidationErrors
	seen := make(map[string]struct{}, len(params))
	for i, p := range params {
		ptr := fmt.Sprintf("/parameterDefinitions/%d", i)

		if !identifierRE.MatchString(p.Name) {
			errs = append(errs, ValidationError{
				Pointer: ptr + "/name",
				Message: fmt.Sprintf("invalid identifier %q; must match [A-Za-z_][A-Za-z0-9_]*", p.Name),
			})
		} else if _, dup := seen[p.Name]; dup {
			errs = append(errs, ValidationError{
				Pointer: ptr + "/name",
				Message: fmt.Sprintf("duplicate parameter name %q", p.Name),
			})
		} else {
			seen[p.Name] = struct{}{}
		}

		switch p.Type {
		case JobParamTypeInt, JobParamTypeFloat, JobParamTypeString, JobParamTypePath:
			// ok
		case "":
			errs = append(errs, ValidationError{Pointer: ptr + "/type", Message: "required"})
		default:
			errs = append(errs, ValidationError{
				Pointer: ptr + "/type",
				Message: fmt.Sprintf("unknown type %q; must be INT, FLOAT, STRING, or PATH", p.Type),
			})
		}
	}
	return errs
}

// ─── environment validation ───────────────────────────────────────────────────

func validateEnvironments(envs []Environment, base string) ValidationErrors {
	var errs ValidationErrors
	seen := make(map[string]struct{}, len(envs))
	for i, e := range envs {
		ptr := fmt.Sprintf("%s/%d", base, i)
		if strings.TrimSpace(e.Name) == "" {
			errs = append(errs, ValidationError{Pointer: ptr + "/name", Message: "required"})
		} else if _, dup := seen[e.Name]; dup {
			errs = append(errs, ValidationError{
				Pointer: ptr + "/name",
				Message: fmt.Sprintf("duplicate environment name %q", e.Name),
			})
		} else {
			seen[e.Name] = struct{}{}
		}
		if e.Script != nil && e.Script.Actions.OnEnter == nil && e.Script.Actions.OnExit == nil {
			errs = append(errs, ValidationError{
				Pointer: ptr + "/script/actions",
				Message: "at least one of onEnter or onExit must be defined",
			})
		}
	}
	return errs
}

// ─── step validation ──────────────────────────────────────────────────────────

func validateStep(s StepTemplate, idx int, stepNames map[string]struct{}) ValidationErrors {
	var errs ValidationErrors
	base := fmt.Sprintf("/steps/%d", idx)

	// dependencies
	for j, dep := range s.Dependencies {
		ptr := fmt.Sprintf("%s/dependencies/%d/dependsOn", base, j)
		if dep.DependsOn == "" {
			errs = append(errs, ValidationError{Pointer: ptr, Message: "required"})
			continue
		}
		if _, ok := stepNames[dep.DependsOn]; !ok {
			errs = append(errs, ValidationError{
				Pointer: ptr,
				Message: fmt.Sprintf("references unknown step %q", dep.DependsOn),
			})
		}
		if dep.DependsOn == s.Name {
			errs = append(errs, ValidationError{
				Pointer: ptr,
				Message: "a step cannot depend on itself",
			})
		}
	}

	// step environments
	errs = append(errs, validateEnvironments(s.StepEnvironments, base+"/stepEnvironments")...)

	// parameter space
	if s.ParameterSpace != nil {
		errs = append(errs, validateParameterSpace(*s.ParameterSpace, base+"/parameterSpace")...)
	}

	return errs
}

// ─── parameter space validation ───────────────────────────────────────────────

func validateParameterSpace(ps StepParameterSpace, base string) ValidationErrors {
	var errs ValidationErrors

	if len(ps.TaskParameterDefinitions) == 0 {
		errs = append(errs, ValidationError{
			Pointer: base + "/taskParameterDefinitions",
			Message: "at least one task parameter definition is required",
		})
		return errs
	}
	if len(ps.TaskParameterDefinitions) > 16 {
		errs = append(errs, ValidationError{
			Pointer: base + "/taskParameterDefinitions",
			Message: "at most 16 task parameter definitions are allowed",
		})
	}

	// Validate each parameter definition and collect names.
	paramNames := make(map[string]struct{}, len(ps.TaskParameterDefinitions))
	for i, tp := range ps.TaskParameterDefinitions {
		ptr := fmt.Sprintf("%s/taskParameterDefinitions/%d", base, i)
		errs = append(errs, validateTaskParam(tp, ptr, paramNames)...)
	}

	// Validate combination expression references.
	if ps.Combination != nil {
		errs = append(errs, validateCombination(*ps.Combination, paramNames, base+"/combination")...)
	}

	return errs
}

func validateTaskParam(tp TaskParamDefinition, base string, seen map[string]struct{}) ValidationErrors {
	var errs ValidationErrors

	if !identifierRE.MatchString(tp.Name) {
		errs = append(errs, ValidationError{
			Pointer: base + "/name",
			Message: fmt.Sprintf("invalid identifier %q; must match [A-Za-z_][A-Za-z0-9_]*", tp.Name),
		})
	} else if _, dup := seen[tp.Name]; dup {
		errs = append(errs, ValidationError{
			Pointer: base + "/name",
			Message: fmt.Sprintf("duplicate task parameter name %q", tp.Name),
		})
	} else {
		seen[tp.Name] = struct{}{}
	}

	switch tp.Type {
	case TaskParamTypeInt, TaskParamTypeFloat, TaskParamTypeString, TaskParamTypePath, TaskParamTypeChunkInt:
		// ok
	case "":
		errs = append(errs, ValidationError{Pointer: base + "/type", Message: "required"})
	default:
		errs = append(errs, ValidationError{
			Pointer: base + "/type",
			Message: fmt.Sprintf("unknown type %q; must be INT, FLOAT, STRING, PATH, or CHUNK[INT]", tp.Type),
		})
	}

	// Range must be present
	if tp.RangeExpr == nil && len(tp.RangeList) == 0 {
		errs = append(errs, ValidationError{Pointer: base + "/range", Message: "required"})
	}

	// INT and CHUNK[INT] range expressions must be parseable
	if tp.RangeExpr != nil && (tp.Type == TaskParamTypeInt || tp.Type == TaskParamTypeChunkInt) {
		if _, err := parseIntRangeExpr(*tp.RangeExpr); err != nil {
			errs = append(errs, ValidationError{
				Pointer: base + "/range",
				Message: err.Error(),
			})
		}
	}

	// CHUNK[INT] must have a chunks definition with defaultTaskCount >= 1
	if tp.Type == TaskParamTypeChunkInt {
		if tp.Chunks == nil {
			errs = append(errs, ValidationError{
				Pointer: base + "/chunks",
				Message: "required for CHUNK[INT] parameters",
			})
		} else if tp.Chunks.DefaultTaskCount <= 0 {
			errs = append(errs, ValidationError{
				Pointer: base + "/chunks/defaultTaskCount",
				Message: "must be a positive integer",
			})
		}
	}

	return errs
}

// validateCombination checks that a combination expression is syntactically
// valid and that every identifier it references names a declared parameter.
func validateCombination(expr string, paramNames map[string]struct{}, ptr string) ValidationErrors {
	var errs ValidationErrors

	names, err := combinationIdentifiers(expr)
	if err != nil {
		errs = append(errs, ValidationError{Pointer: ptr, Message: err.Error()})
		return errs
	}

	for _, name := range names {
		if _, ok := paramNames[name]; !ok {
			errs = append(errs, ValidationError{
				Pointer: ptr,
				Message: fmt.Sprintf("references undeclared parameter %q", name),
			})
		}
	}

	// Every declared parameter must appear in the combination expression.
	for name := range paramNames {
		if !slices.Contains(names, name) {
			errs = append(errs, ValidationError{
				Pointer: ptr,
				Message: fmt.Sprintf("declared parameter %q not referenced in combination expression", name),
			})
		}
	}

	return errs
}

// ─── cycle detection ──────────────────────────────────────────────────────────

// validateNoCycles uses DFS topological sort to detect dependency cycles.
func validateNoCycles(steps []StepTemplate) ValidationErrors {
	// Build adjacency list: step name → names it depends on.
	deps := make(map[string][]string, len(steps))
	for _, s := range steps {
		if s.Name == "" {
			continue
		}
		for _, d := range s.Dependencies {
			deps[s.Name] = append(deps[s.Name], d.DependsOn)
		}
	}

	const (
		stateWhite = 0
		stateGray  = 1
		stateBlack = 2
	)
	state := make(map[string]int, len(steps))
	var cycleErr *ValidationError

	var visit func(name string)
	visit = func(name string) {
		if cycleErr != nil {
			return
		}
		switch state[name] {
		case stateGray:
			cycleErr = &ValidationError{
				Pointer: "/steps",
				Message: fmt.Sprintf("dependency cycle detected involving step %q", name),
			}
			return
		case stateBlack:
			return
		}
		state[name] = stateGray
		for _, dep := range deps[name] {
			visit(dep)
		}
		state[name] = stateBlack
	}

	for _, s := range steps {
		if s.Name == "" {
			continue
		}
		visit(s.Name)
		if cycleErr != nil {
			return ValidationErrors{*cycleErr}
		}
	}

	return nil
}
