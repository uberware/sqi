// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import (
	"fmt"
	"maps"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
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

// ─── extension gating ────────────────────────────────────────────────────────

// validateExtensions runs unconditionally (not gated by EnforceLimits) and:
//  1. Rejects every entry in t.Extensions that does not match the format pattern
//     [A-Z_0-9]{3,128}. The OpenJD spec defines extension names as uppercase
//     identifiers matching this pattern.
//  2. Rejects every entry that is not in supportedExtensions. Silently accepting
//     an unsupported extension would cause the template to mis-run, so this is
//     structural correctness rather than a quantitative limit.
//  3. Requires the TASK_CHUNKING extension to be declared when any step uses a
//     CHUNK[INT] task parameter.  Declaring TASK_CHUNKING without using
//     CHUNK[INT] is allowed.
func validateExtensions(t *JobTemplate) ValidationErrors {
	var errs ValidationErrors

	// Build a fast lookup set from the declared extensions and report any
	// unsupported ones. Format check runs first.
	declared := make(map[string]struct{}, len(t.Extensions))
	for i, ext := range t.Extensions {
		// Check format FIRST: extension names must match [A-Z_0-9]{3,128}
		if !extensionNameRE.MatchString(ext) {
			errs = append(errs, ValidationError{
				Pointer: fmt.Sprintf("/extensions/%d", i),
				Message: fmt.Sprintf("invalid extension name %q; must match [A-Z_0-9]{3,128}", ext),
			})
			continue // Skip unsupported-set check and declared-set addition for malformed names
		}

		// Check if the well-formed name is supported
		if _, ok := LookupExtension(ext); !ok {
			errs = append(errs, ValidationError{
				Pointer: fmt.Sprintf("/extensions/%d", i),
				Message: fmt.Sprintf("unsupported extension %q", ext),
			})
		}
		declared[ext] = struct{}{}
	}

	// If any step declares a CHUNK[INT] parameter, TASK_CHUNKING must appear
	// in the template's extensions list.
	if _, ok := declared["TASK_CHUNKING"]; !ok {
		for i, s := range t.Steps {
			if s.ParameterSpace == nil {
				continue
			}
			for _, tp := range s.ParameterSpace.TaskParameterDefinitions {
				if tp.Type == TaskParamTypeChunkInt {
					errs = append(errs, ValidationError{
						Pointer: fmt.Sprintf("/steps/%d/parameterSpace", i),
						Message: `CHUNK[INT] parameters require declaring the TASK_CHUNKING extension in the template's extensions list`,
					})
					break // one error per step is sufficient
				}
			}
		}
	}

	return errs
}

// ─── identifier pattern ───────────────────────────────────────────────────────

var identifierRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// extensionNameRE matches the OpenJD extension name format: uppercase letters,
// underscores, and digits, 3-128 characters. Must match [A-Z_0-9]{3,128}.
var extensionNameRE = regexp.MustCompile(`^[A-Z_0-9]{3,128}$`)

// ─── ValidateOptions ──────────────────────────────────────────────────────────

// ValidateOptions controls optional validation behavior passed to
// [ValidateWithOptions].
type ValidateOptions struct {
	// EnforceLimits gates quantitative limit checks: maximum name lengths,
	// element counts, reserved-name rules, etc.
	//
	// When false those checks are skipped — useful in operator environments
	// that predate strict limit enforcement and cannot yet update all templates.
	//
	// NOTE: the quantitative limit checks live in [validateLimits] (and the
	// helpers it calls). Every limit check MUST be guarded by opts.EnforceLimits
	// and belong to validateLimits — do not scatter gated checks elsewhere.
	// Resource-exhaustion guards (e.g. the [maxRangeValues] cap in
	// parseIntRangeExpr) are NOT limit checks: they always apply, regardless of
	// this flag.
	EnforceLimits bool
}

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
//
// Validate is a thin wrapper around [ValidateWithOptions] with
// [ValidateOptions.EnforceLimits] set to true.
func Validate(t *JobTemplate) ValidationErrors {
	return ValidateWithOptions(t, ValidateOptions{EnforceLimits: true})
}

// ValidateWithOptions is like [Validate] but accepts a [ValidateOptions] value
// so callers can control optional validation behavior — currently whether
// quantitative limit checks are enforced.
//
// Existing callers should continue to use [Validate]; this entry point is for
// the submission pipeline where the operator may disable limit enforcement via
// config.
func ValidateWithOptions(t *JobTemplate, opts ValidateOptions) ValidationErrors {
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

	// ── extensions (unconditional) ────────────────────────────────────────
	// Run unconditionally: silently accepting an unsupported extension would
	// mis-run the template.  NOT gated by opts.EnforceLimits.
	errs = append(errs, validateExtensions(t)...)

	// ── quantitative limits (gated) ───────────────────────────────────────
	// Every check below MUST stay behind opts.EnforceLimits.
	if opts.EnforceLimits {
		errs = append(errs, validateLimits(t)...)
	}

	return errs
}

// ─── reserved-name tables ─────────────────────────────────────────────────────

// reservedAmountMinimums maps the lowercase canonical reserved AMOUNT capability
// names from the OpenJD jobtemplate-2023-09 specification to their required
// minimum values. A host-requirement amount whose name matches (case-insensitively)
// one of these keys must have its Min and Max (when present) >= the mapped value.
//
// Spec reference: OpenJD jobtemplate-2023-09 §hostRequirements.amounts —
// Reserved Capability Names (AMOUNT).
// Read-only after initialization; never modified at runtime.
var reservedAmountMinimums = map[string]float64{
	"amount.worker.vcpu":         1, // at least 1 vCPU
	"amount.worker.memory":       0, // MiB; >= 0
	"amount.worker.gpu":          0, // GPU count; >= 0
	"amount.worker.gpu.memory":   0, // MiB VRAM per device; >= 0
	"amount.worker.disk.scratch": 0, // GiB scratch space; >= 0
}

// reservedAttributeAllowed maps the lowercase canonical reserved ATTRIBUTE
// capability names from the OpenJD jobtemplate-2023-09 specification to the
// set of allowed string values. Every entry in anyOf and allOf for a matching
// attribute must be present in the corresponding set.
//
// Attribute names AND values are both matched CASE-INSENSITIVELY, per OpenJD
// jobtemplate-2023-09 (capability names: "comparisons ... must be
// case-insensitive"; attribute values: "This comparison is case-insensitive").
// The allowed-value sets are stored as lowercase tokens; values are lowercased
// before lookup.
//
// Spec reference: OpenJD jobtemplate-2023-09 §hostRequirements.attributes —
// Reserved Attribute Names.
// Read-only after initialization; never modified at runtime.
var reservedAttributeAllowed = map[string]map[string]bool{
	"attr.worker.os.family": {"linux": true, "windows": true, "macos": true},
	"attr.worker.cpu.arch":  {"x86_64": true, "arm64": true},
}

// ─── quantitative limits ──────────────────────────────────────────────────────

// Quantitative limits from the OpenJD jobtemplate-2023-09 specification
// (base spec, no extensions). All of these are enforced only when
// [ValidateOptions.EnforceLimits] is true.
const (
	// maxJobParameterDefinitions caps parameterDefinitions. The spec range is
	// 1–50, but the field is optional so only the upper bound is enforced here.
	maxJobParameterDefinitions = 50
	// maxTaskParameterDefinitions caps a step's taskParameterDefinitions (1–16).
	maxTaskParameterDefinitions = 16
	// maxTaskParamValues caps the number of values a single task parameter may
	// expand to (1–1024).
	maxTaskParamValues = 1024
	// maxJobNameLen caps the job name length.
	maxJobNameLen = 128
	// maxStepNameLen caps each step name length.
	maxStepNameLen = 64
	// maxEnvNameLen caps each environment (job- and step-level) name length.
	maxEnvNameLen = 64
	// maxHostRequirements caps the combined count of amounts + attributes in a
	// step's hostRequirements (1–50 when present).
	maxHostRequirements = 50
	// maxCapabilityNameLen caps each amount/attribute capability name (1–100).
	maxCapabilityNameLen = 100
	// maxAttributeValues caps each attribute's anyOf/allOf element count (1–50).
	maxAttributeValues = 50
	// maxUILabelLen caps userInterface label length.
	maxUILabelLen = 256
	// maxUIGroupLabelLen caps userInterface groupLabel length.
	maxUIGroupLabelLen = 256
)

// validateLimits runs every quantitative limit check. It is only invoked when
// EnforceLimits is true (see [ValidateWithOptions]).
func validateLimits(t *JobTemplate) ValidationErrors {
	var errs ValidationErrors

	// Job name length (in characters/runes, not bytes — the spec limits and the
	// error message are expressed in characters).
	if n := utf8.RuneCountInString(t.Name); n > maxJobNameLen {
		errs = append(errs, ValidationError{
			Pointer: "/name",
			Message: fmt.Sprintf("name must be at most %d characters (got %d)", maxJobNameLen, n),
		})
	}

	// parameterDefinitions count (upper bound only; an empty list is allowed).
	if len(t.ParameterDefinitions) > maxJobParameterDefinitions {
		errs = append(errs, ValidationError{
			Pointer: "/parameterDefinitions",
			Message: fmt.Sprintf("at most %d parameter definitions are allowed (got %d)", maxJobParameterDefinitions, len(t.ParameterDefinitions)),
		})
	}

	// userInterface label length limits.
	errs = append(errs, validateUILimits(t.ParameterDefinitions)...)

	// Job environment name lengths.
	errs = append(errs, validateEnvNameLimits(t.JobEnvironments, "/jobEnvironments")...)

	for i, s := range t.Steps {
		base := fmt.Sprintf("/steps/%d", i)

		// Step name length (characters, not bytes).
		if n := utf8.RuneCountInString(s.Name); n > maxStepNameLen {
			errs = append(errs, ValidationError{
				Pointer: base + "/name",
				Message: fmt.Sprintf("name must be at most %d characters (got %d)", maxStepNameLen, n),
			})
		}

		// Step environment name lengths.
		errs = append(errs, validateEnvNameLimits(s.StepEnvironments, base+"/stepEnvironments")...)

		// Parameter space limits.
		if s.ParameterSpace != nil {
			errs = append(errs, validateParameterSpaceLimits(*s.ParameterSpace, base+"/parameterSpace")...)
		}

		// Host requirement limits.
		if s.HostRequirements != nil {
			errs = append(errs, validateHostRequirementLimits(*s.HostRequirements, base+"/hostRequirements")...)
		}
	}

	return errs
}

// validateEnvNameLimits checks the name length of each environment in envs.
func validateEnvNameLimits(envs []Environment, base string) ValidationErrors {
	var errs ValidationErrors
	for i, e := range envs {
		if n := utf8.RuneCountInString(e.Name); n > maxEnvNameLen {
			errs = append(errs, ValidationError{
				Pointer: fmt.Sprintf("%s/%d/name", base, i),
				Message: fmt.Sprintf("name must be at most %d characters (got %d)", maxEnvNameLen, n),
			})
		}
	}
	return errs
}

// validateParameterSpaceLimits checks the gated count and value-count limits for
// a step's parameter space, including INT range overlap detection.
func validateParameterSpaceLimits(ps StepParameterSpace, base string) ValidationErrors {
	var errs ValidationErrors

	if len(ps.TaskParameterDefinitions) > maxTaskParameterDefinitions {
		errs = append(errs, ValidationError{
			Pointer: base + "/taskParameterDefinitions",
			Message: fmt.Sprintf("at most %d task parameter definitions are allowed (got %d)", maxTaskParameterDefinitions, len(ps.TaskParameterDefinitions)),
		})
	}

	for j, tp := range ps.TaskParameterDefinitions {
		ptr := fmt.Sprintf("%s/taskParameterDefinitions/%d/range", base, j)

		// Number of values the parameter expands to.
		if n, counted := taskParamValueCount(tp); counted && n > maxTaskParamValues {
			errs = append(errs, ValidationError{
				Pointer: ptr,
				Message: fmt.Sprintf("at most %d values are allowed per task parameter (got %d)", maxTaskParamValues, n),
			})
		}

		// INT/CHUNK[INT] range expressions must not contain overlapping
		// sub-ranges (skipped for unresolved {{...}} expressions).
		if tp.RangeExpr != nil && (tp.Type == TaskParamTypeInt || tp.Type == TaskParamTypeChunkInt) {
			if overlap, err := intRangeHasOverlap(*tp.RangeExpr); err == nil && overlap {
				errs = append(errs, ValidationError{
					Pointer: ptr,
					Message: "range expression contains overlapping values; sub-ranges must not overlap",
				})
			}
		}
	}

	return errs
}

// taskParamValueCount returns the number of values a task parameter expands to
// and whether that count could be determined. INT/CHUNK[INT] range expressions
// that contain a format-string reference ("{{") cannot be counted before
// resolution and report counted=false.
func taskParamValueCount(tp TaskParamDefinition) (count int, counted bool) {
	if tp.RangeExpr != nil && (tp.Type == TaskParamTypeInt || tp.Type == TaskParamTypeChunkInt) {
		if strings.Contains(*tp.RangeExpr, "{{") {
			return 0, false
		}
		// Count arithmetically, without materializing the integers. An
		// unparseable expression (or one exceeding the hard resource-exhaustion
		// bound) is reported by the structural parse check; do not double-report
		// here.
		return intRangeExprCount(*tp.RangeExpr)
	}
	return len(tp.RangeList), true
}

// validateHostRequirementLimits checks the gated limits on a step's
// hostRequirements: combined count, presence, capability name lengths, and
// attribute anyOf/allOf element counts.
func validateHostRequirementLimits(hr HostRequirements, base string) ValidationErrors {
	var errs ValidationErrors

	combined := len(hr.Amounts) + len(hr.Attributes)
	switch {
	case combined == 0:
		errs = append(errs, ValidationError{
			Pointer: base,
			Message: "hostRequirements must declare at least one amount or attribute when present",
		})
	case combined > maxHostRequirements:
		errs = append(errs, ValidationError{
			Pointer: base,
			Message: fmt.Sprintf("at most %d host requirements are allowed (got %d)", maxHostRequirements, combined),
		})
	}

	for i, a := range hr.Amounts {
		ptr := fmt.Sprintf("%s/amounts/%d/name", base, i)
		errs = append(errs, validateCapabilityName(a.Name, ptr)...)
		errs = append(errs, validateCapabilityPrefix(a.Name, "amount.", ptr)...)
	}

	for i, a := range hr.Attributes {
		ptr := fmt.Sprintf("%s/attributes/%d", base, i)
		errs = append(errs, validateCapabilityName(a.Name, ptr+"/name")...)
		errs = append(errs, validateCapabilityPrefix(a.Name, "attr.", ptr+"/name")...)

		if len(a.AnyOf) > maxAttributeValues {
			errs = append(errs, ValidationError{
				Pointer: ptr + "/anyOf",
				Message: fmt.Sprintf("at most %d values are allowed (got %d)", maxAttributeValues, len(a.AnyOf)),
			})
		}
		if len(a.AllOf) > maxAttributeValues {
			errs = append(errs, ValidationError{
				Pointer: ptr + "/allOf",
				Message: fmt.Sprintf("at most %d values are allowed (got %d)", maxAttributeValues, len(a.AllOf)),
			})
		}
		if len(a.AnyOf)+len(a.AllOf) == 0 {
			errs = append(errs, ValidationError{
				Pointer: ptr,
				Message: "attribute must declare at least one of anyOf or allOf",
			})
		}
	}

	// Reserved-name checks (also gated by EnforceLimits via the call chain).
	errs = append(errs, validateReservedAmounts(hr.Amounts, base)...)
	errs = append(errs, validateReservedAttributes(hr.Attributes, base)...)

	return errs
}

// validateReservedAmounts checks that any amount with a reserved capability
// name (matched case-insensitively) has its Min and Max values (when present)
// >= the spec-mandated minimum. Amounts with non-reserved names are unconstrained.
//
// Min/Max values that contain an unresolved format-string reference ("{{") are
// skipped: the numeric value cannot be determined before job-parameter binding.
// Non-numeric values are already rejected by earlier structural checks, so a
// parse failure here is silently skipped to avoid double-reporting.
func validateReservedAmounts(amounts []AmountRequirement, base string) ValidationErrors {
	var errs ValidationErrors
	for i, a := range amounts {
		minReq, ok := reservedAmountMinimums[strings.ToLower(a.Name)]
		if !ok {
			continue // not a reserved capability name
		}
		ptr := fmt.Sprintf("%s/amounts/%d", base, i)
		errs = append(errs, checkReservedBound(a.Min, minReq, a.Name, ptr+"/min")...)
		errs = append(errs, checkReservedBound(a.Max, minReq, a.Name, ptr+"/max")...)
	}
	return errs
}

// checkReservedBound validates that a *string capability bound (Min or Max),
// when present and parseable as a number, is >= the reserved minimum. Nil
// bounds and bounds containing format-string references ("{{") are skipped.
// Non-finite values (NaN, ±Inf) are rejected as validation errors.
func checkReservedBound(val *string, minReq float64, capName, ptr string) ValidationErrors {
	if val == nil || strings.Contains(*val, "{{") {
		return nil
	}
	v, err := strconv.ParseFloat(*val, 64)
	if err != nil {
		return ValidationErrors{{
			Pointer: ptr,
			Message: fmt.Sprintf("value %q is not a valid number", *val),
		}}
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return ValidationErrors{{
			Pointer: ptr,
			Message: fmt.Sprintf("value %q is not a finite number", *val),
		}}
	}
	if v < minReq {
		return ValidationErrors{{
			Pointer: ptr,
			Message: fmt.Sprintf(
				"value %s is below the reserved minimum %g for capability %q",
				*val, minReq, capName,
			),
		}}
	}
	return nil
}

// validateReservedAttributes checks that any attribute with a reserved name
// (matched case-insensitively) has all anyOf and allOf values within the
// allowed set defined by the OpenJD spec. Attributes with non-reserved names
// are unconstrained.
//
// Attribute VALUES are compared CASE-INSENSITIVELY, per OpenJD
// jobtemplate-2023-09 ("This comparison is case-insensitive") and matching the
// matcher's EqualFold value comparison. The allowed-set keys are lowercase, so
// each value is lowercased before lookup.
func validateReservedAttributes(attrs []AttributeRequirement, base string) ValidationErrors {
	var errs ValidationErrors
	for i, a := range attrs {
		allowed, ok := reservedAttributeAllowed[strings.ToLower(a.Name)]
		if !ok {
			continue // not a reserved attribute name
		}
		ptr := fmt.Sprintf("%s/attributes/%d", base, i)
		allowedList := sortedMapKeys(allowed)
		for m, v := range a.AnyOf {
			if !allowed[strings.ToLower(v)] {
				errs = append(errs, ValidationError{
					Pointer: fmt.Sprintf("%s/anyOf/%d", ptr, m),
					Message: fmt.Sprintf(
						"value %q is not allowed for reserved attribute %q; allowed: %s",
						v, a.Name, allowedList,
					),
				})
			}
		}
		for m, v := range a.AllOf {
			if !allowed[strings.ToLower(v)] {
				errs = append(errs, ValidationError{
					Pointer: fmt.Sprintf("%s/allOf/%d", ptr, m),
					Message: fmt.Sprintf(
						"value %q is not allowed for reserved attribute %q; allowed: %s",
						v, a.Name, allowedList,
					),
				})
			}
		}
	}
	return errs
}

// sortedMapKeys returns the keys of a map[string]bool as a sorted,
// comma-separated string — used to produce deterministic error messages.
func sortedMapKeys(m map[string]bool) string {
	return strings.Join(slices.Sorted(maps.Keys(m)), ", ")
}

// validateCapabilityPrefix checks that a non-empty capability name uses the
// spec-mandated namespace prefix for its section — "amount." for amounts,
// "attr." for attributes. The match is case-insensitive, since OpenJD
// jobtemplate-2023-09 defines capability names as case-insensitive and the
// scheduler resolves them case-insensitively (see internal/scheduler/matcher.go).
// An empty name is left to [validateCapabilityName]'s "name is required" check.
func validateCapabilityPrefix(name, wantPrefix, ptr string) ValidationErrors {
	if name == "" {
		return nil
	}
	if !hasPrefixFold(name, wantPrefix) {
		return ValidationErrors{{
			Pointer: ptr,
			Message: fmt.Sprintf("capability name %q must begin with %q", name, wantPrefix),
		}}
	}
	return nil
}

// validateCapabilityName checks that an amount/attribute capability name has a
// length within the spec bounds (1–100 characters).
func validateCapabilityName(name, ptr string) ValidationErrors {
	switch n := utf8.RuneCountInString(name); {
	case n == 0:
		return ValidationErrors{{Pointer: ptr, Message: "name is required"}}
	case n > maxCapabilityNameLen:
		return ValidationErrors{{
			Pointer: ptr,
			Message: fmt.Sprintf("name must be at most %d characters (got %d)", maxCapabilityNameLen, n),
		}}
	}
	return nil
}

// validateUILimits enforces length caps on userInterface labels. Gated: callers
// run it only when EnforceLimits is set.
func validateUILimits(params []JobParameter) ValidationErrors {
	var errs ValidationErrors
	for i, p := range params {
		if p.UserInterface == nil {
			continue
		}
		base := fmt.Sprintf("/parameterDefinitions/%d/userInterface", i)
		if len(p.UserInterface.Label) > maxUILabelLen {
			errs = append(errs, ValidationError{
				Pointer: base + "/label",
				Message: fmt.Sprintf("label exceeds %d characters", maxUILabelLen),
			})
		}
		if len(p.UserInterface.GroupLabel) > maxUIGroupLabelLen {
			errs = append(errs, ValidationError{
				Pointer: base + "/groupLabel",
				Message: fmt.Sprintf("groupLabel exceeds %d characters", maxUIGroupLabelLen),
			})
		}
	}
	return errs
}

// ─── userInterface validation ─────────────────────────────────────────────────

// validControls is the set of OpenJD base-spec userInterface control values.
// Read-only after initialization.
var validControls = map[ControlType]struct{}{
	ControlLineEdit:      {},
	ControlMultilineEdit: {},
	ControlDropdownList:  {},
	ControlCheckBox:      {},
	ControlChipInput:     {},
	ControlHidden:        {},
	ControlSpinBox:       {},
}

// validateUserInterfaceControl checks control-specific constraints for a
// userInterface hint, extracted to keep [validateUserInterface] complexity <= 15.
func validateUserInterfaceControl(ui *ParameterUserInterface, p JobParameter, ctrlPtr string) ValidationErrors {
	var errs ValidationErrors
	switch ui.Control {
	case ControlDropdownList:
		if len(p.AllowedValues) == 0 {
			errs = append(errs, ValidationError{Pointer: ctrlPtr, Message: "DROPDOWN_LIST requires allowedValues"})
		}
	case ControlCheckBox:
		if len(p.AllowedValues) != 2 {
			errs = append(errs, ValidationError{Pointer: ctrlPtr, Message: "CHECK_BOX requires exactly two allowedValues"})
		}
	case ControlSpinBox:
		if p.Type != JobParamTypeInt && p.Type != JobParamTypeFloat {
			errs = append(errs, ValidationError{Pointer: ctrlPtr, Message: "SPIN_BOX is valid only on INT or FLOAT parameters"})
		}
	}
	return errs
}

// validateUserInterface checks a parameter's optional userInterface hints:
// the control must be a known value, and control/constraint combinations must
// be coherent (DROPDOWN_LIST/CHECK_BOX need allowedValues; SPIN_BOX is numeric;
// decimals/singleStepRemoval pair with their controls). Structural — always runs.
func validateUserInterface(p JobParameter, ptr string) ValidationErrors {
	ui := p.UserInterface
	if ui == nil {
		return nil
	}
	var errs ValidationErrors
	ctrlPtr := ptr + "/userInterface/control"

	if ui.Control == "" {
		errs = append(errs, ValidationError{Pointer: ctrlPtr, Message: "required"})
		return errs
	}
	if _, ok := validControls[ui.Control]; !ok {
		errs = append(errs, ValidationError{
			Pointer: ctrlPtr,
			Message: fmt.Sprintf("unknown control %q", ui.Control),
		})
		return errs
	}

	errs = append(errs, validateUserInterfaceControl(ui, p, ctrlPtr)...)

	if ui.Decimals != nil && (ui.Control != ControlSpinBox || p.Type != JobParamTypeFloat) {
		errs = append(errs, ValidationError{
			Pointer: ptr + "/userInterface/decimals",
			Message: "decimals is valid only with SPIN_BOX on a FLOAT parameter",
		})
	}
	if ui.SingleStepRemoval != nil && ui.Control != ControlChipInput {
		errs = append(errs, ValidationError{
			Pointer: ptr + "/userInterface/singleStepRemoval",
			Message: "singleStepRemoval is valid only with CHIP_INPUT",
		})
	}

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
		case JobParamTypeInt:
			errs = append(errs, validateIntParamConstraints(p, ptr)...)
		case JobParamTypeFloat:
			errs = append(errs, validateFloatParamConstraints(p, ptr)...)
		case JobParamTypeString, JobParamTypePath:
			errs = append(errs, validateStringParamConstraints(p, ptr)...)
		case "":
			errs = append(errs, ValidationError{Pointer: ptr + "/type", Message: "required"})
		default:
			errs = append(errs, ValidationError{
				Pointer: ptr + "/type",
				Message: fmt.Sprintf("unknown type %q; must be INT, FLOAT, STRING, or PATH", p.Type),
			})
		}

		// objectType and dataFlow are structural constraints checked
		// unconditionally (not gated by EnforceLimits).
		errs = append(errs, validatePathOnlyFields(p, ptr)...)

		// userInterface validation is also structural, always runs.
		errs = append(errs, validateUserInterface(p, ptr)...)
	}
	return errs
}

// validateIntParamConstraints checks that an INT parameter's constraint fields
// contain syntactically valid int64 literals and that min <= max when both are
// present.  This is structural correctness and is always enforced.
func validateIntParamConstraints(p JobParameter, ptr string) ValidationErrors {
	var errs ValidationErrors

	var minVal, maxVal int64
	var minOK, maxOK bool

	if p.MinValue != nil {
		v, err := strconv.ParseInt(*p.MinValue, 10, 64)
		if err != nil {
			errs = append(errs, ValidationError{
				Pointer: ptr + "/minValue",
				Message: fmt.Sprintf("minValue %q is not a valid INT", *p.MinValue),
			})
		} else {
			minVal, minOK = v, true
		}
	}

	if p.MaxValue != nil {
		v, err := strconv.ParseInt(*p.MaxValue, 10, 64)
		if err != nil {
			errs = append(errs, ValidationError{
				Pointer: ptr + "/maxValue",
				Message: fmt.Sprintf("maxValue %q is not a valid INT", *p.MaxValue),
			})
		} else {
			maxVal, maxOK = v, true
		}
	}

	if minOK && maxOK && minVal > maxVal {
		errs = append(errs, ValidationError{
			Pointer: ptr + "/minValue",
			Message: fmt.Sprintf("minValue %d must be <= maxValue %d", minVal, maxVal),
		})
	}

	for j, av := range p.AllowedValues {
		if _, err := strconv.ParseInt(av, 10, 64); err != nil {
			errs = append(errs, ValidationError{
				Pointer: fmt.Sprintf("%s/allowedValues/%d", ptr, j),
				Message: fmt.Sprintf("allowedValues[%d] %q is not a valid INT", j, av),
			})
		}
	}

	return errs
}

// parseFloatBound parses rawVal as a finite float64 for use as a constraint
// bound (minValue, maxValue, or an allowedValues entry). field is the
// constraint field label used in error messages (e.g. "minValue").
// Returns (value, true, nil) on success, or (0, false, errors) on failure.
func parseFloatBound(rawVal, boundPtr, field string) (v float64, ok bool, errs ValidationErrors) {
	v, err := strconv.ParseFloat(rawVal, 64)
	if err != nil {
		return 0, false, ValidationErrors{{
			Pointer: boundPtr,
			Message: fmt.Sprintf("%s %q is not a valid FLOAT", field, rawVal),
		}}
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, false, ValidationErrors{{
			Pointer: boundPtr,
			Message: fmt.Sprintf("%s %q must be a finite number", field, rawVal),
		}}
	}
	return v, true, nil
}

// validateFloatParamConstraints checks that a FLOAT parameter's constraint
// fields contain syntactically valid, finite float64 literals and that min <=
// max when both are present.  NaN and ±Inf are rejected as constraint values.
func validateFloatParamConstraints(p JobParameter, ptr string) ValidationErrors {
	var errs ValidationErrors
	var minVal, maxVal float64
	var minOK, maxOK bool

	if p.MinValue != nil {
		v, ok, ve := parseFloatBound(*p.MinValue, ptr+"/minValue", "minValue")
		errs = append(errs, ve...)
		if ok {
			minVal, minOK = v, true
		}
	}

	if p.MaxValue != nil {
		v, ok, ve := parseFloatBound(*p.MaxValue, ptr+"/maxValue", "maxValue")
		errs = append(errs, ve...)
		if ok {
			maxVal, maxOK = v, true
		}
	}

	if minOK && maxOK && minVal > maxVal {
		errs = append(errs, ValidationError{
			Pointer: ptr + "/minValue",
			Message: fmt.Sprintf("minValue %v must be <= maxValue %v", minVal, maxVal),
		})
	}

	for j, av := range p.AllowedValues {
		avPtr := fmt.Sprintf("%s/allowedValues/%d", ptr, j)
		avField := fmt.Sprintf("allowedValues[%d]", j)
		_, _, ve := parseFloatBound(av, avPtr, avField)
		errs = append(errs, ve...)
	}

	return errs
}

// validateStringParamConstraints checks that STRING and PATH parameter length
// constraints are consistent: minLength <= maxLength when both are present.
// AllowedValues entries are free strings and require no syntax check.
func validateStringParamConstraints(p JobParameter, ptr string) ValidationErrors {
	var errs ValidationErrors

	if p.MinLength != nil && p.MaxLength != nil && *p.MinLength > *p.MaxLength {
		errs = append(errs, ValidationError{
			Pointer: ptr + "/minLength",
			Message: fmt.Sprintf("minLength %d must be <= maxLength %d", *p.MinLength, *p.MaxLength),
		})
	}

	return errs
}

// validatePathOnlyFields checks that objectType and dataFlow, if set, carry
// legal values and only appear on PATH-typed parameters.  These are structural
// correctness checks; they are always enforced regardless of EnforceLimits.
func validatePathOnlyFields(p JobParameter, ptr string) ValidationErrors {
	isPath := p.Type == JobParamTypePath
	var errs ValidationErrors
	errs = append(errs, validatePathOnlyField(
		p.ObjectType, ptr+"/objectType", "objectType", isPath,
		[]PathObjectType{PathObjectTypeFile, PathObjectTypeDirectory}, "must be FILE or DIRECTORY",
	)...)
	errs = append(errs, validatePathOnlyField(
		p.DataFlow, ptr+"/dataFlow", "dataFlow", isPath,
		[]PathDataFlow{PathDataFlowNone, PathDataFlowIn, PathDataFlowOut, PathDataFlowInOut}, "must be NONE, IN, OUT, or INOUT",
	)...)
	return errs
}

// validatePathOnlyField validates one PATH-only field (objectType or dataFlow):
// when set, its value must be one of valid, and the field may appear only on a
// PATH-typed parameter. valueHint describes the allowed values for the
// invalid-value message.
func validatePathOnlyField[T ~string](value T, ptr, field string, isPath bool, valid []T, valueHint string) ValidationErrors {
	if value == "" {
		return nil
	}
	var errs ValidationErrors
	if !slices.Contains(valid, value) {
		errs = append(errs, ValidationError{
			Pointer: ptr,
			Message: fmt.Sprintf("invalid %s %q; %s", field, value, valueHint),
		})
	}
	if !isPath {
		errs = append(errs, ValidationError{
			Pointer: ptr,
			Message: field + " may only be set on PATH parameters",
		})
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
		if e.Script != nil {
			if e.Script.Actions.OnEnter == nil && e.Script.Actions.OnExit == nil {
				errs = append(errs, ValidationError{
					Pointer: ptr + "/script/actions",
					Message: "at least one of onEnter or onExit must be defined",
				})
			}
			errs = append(errs, validateEmbeddedFiles(e.Script.EmbeddedFiles, ptr+"/script/embeddedFiles")...)
		}
	}
	return errs
}

// ─── embedded file validation ─────────────────────────────────────────────────

// validateEmbeddedFiles validates the type field of each embedded file.
// ptr is the JSON-pointer base for the embeddedFiles array itself, e.g.
// "/steps/0/script/embeddedFiles" or "/jobEnvironments/0/script/embeddedFiles".
//
// This check is structural correctness — it runs unconditionally, NOT gated
// behind EnforceLimits.
func validateEmbeddedFiles(files []EmbeddedFile, ptr string) ValidationErrors {
	var errs ValidationErrors
	for j, f := range files {
		typePtr := fmt.Sprintf("%s/%d/type", ptr, j)
		switch f.Type {
		case EmbeddedFileTypeText:
			// valid
		case "":
			errs = append(errs, ValidationError{
				Pointer: typePtr,
				Message: "required; must be TEXT",
			})
		default:
			errs = append(errs, ValidationError{
				Pointer: typePtr,
				Message: fmt.Sprintf("unsupported embedded file type %q; expected TEXT", f.Type),
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

	// step script embedded files
	if s.Script != nil {
		errs = append(errs, validateEmbeddedFiles(s.Script.EmbeddedFiles, base+"/script/embeddedFiles")...)
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
	// The upper bound (>16) is a quantitative limit enforced by validateLimits
	// (gated behind EnforceLimits).

	// Validate each parameter definition and collect names. chunked records the
	// set of CHUNK[INT] parameter names for combination-association checks.
	paramNames := make(map[string]struct{}, len(ps.TaskParameterDefinitions))
	chunked := make(map[string]struct{})
	chunkCount := 0
	for i, tp := range ps.TaskParameterDefinitions {
		ptr := fmt.Sprintf("%s/taskParameterDefinitions/%d", base, i)
		errs = append(errs, validateTaskParam(tp, ptr, paramNames)...)
		if tp.Type == TaskParamTypeChunkInt {
			chunked[tp.Name] = struct{}{}
			chunkCount++
		}
	}

	// At most one CHUNK[INT] parameter is allowed per step (OpenJD). This is a
	// structural-correctness check; always enforced.
	if chunkCount > 1 {
		errs = append(errs, ValidationError{
			Pointer: base + "/taskParameterDefinitions",
			Message: "at most one CHUNK[INT] task parameter is allowed per step",
		})
	}

	// Validate combination expression references.
	if ps.Combination != nil {
		errs = append(errs, validateCombination(*ps.Combination, paramNames, chunked, base+"/combination")...)
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

	errs = append(errs, validateTaskParamRangeAndChunks(tp, base)...)

	return errs
}

// validateTaskParamRangeAndChunks validates the range field and, for CHUNK[INT]
// parameters, the chunks definition. It is extracted from [validateTaskParam]
// to keep that function's cyclomatic complexity within bounds.
func validateTaskParamRangeAndChunks(tp TaskParamDefinition, base string) ValidationErrors {
	var errs ValidationErrors

	// Range must be present
	if tp.RangeExpr == nil && len(tp.RangeList) == 0 {
		errs = append(errs, ValidationError{Pointer: base + "/range", Message: "required"})
	}

	// INT and CHUNK[INT] range expressions must be parseable — but only when
	// they contain no format-string references (those will be resolved against
	// bound job parameters before expansion; parsability is re-checked then).
	if tp.RangeExpr != nil && (tp.Type == TaskParamTypeInt || tp.Type == TaskParamTypeChunkInt) {
		if !strings.Contains(*tp.RangeExpr, "{{") {
			if err := validateIntRangeExpr(*tp.RangeExpr); err != nil {
				errs = append(errs, ValidationError{
					Pointer: base + "/range",
					Message: err.Error(),
				})
			}
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
// valid, that every identifier it references names a declared parameter, and
// that no CHUNK[INT] parameter is associated (zipped) with other parameters.
//
// chunked is the set of declared CHUNK[INT] parameter names.
func validateCombination(expr string, paramNames, chunked map[string]struct{}, ptr string) ValidationErrors {
	var errs ValidationErrors

	// Parse once; reuse both the identifier list and the tree to avoid a second
	// parse for the CHUNK[INT] association check.
	names, tree, err := combinationIdentifiers(expr)
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

	// A CHUNK[INT] parameter must not be associated with other parameters: it
	// may not appear inside an association group together with any other
	// parameter or expression. It may still be combined via '*'.
	if len(chunked) > 0 {
		if name := findChunkInAssoc(tree, chunked); name != "" {
			errs = append(errs, ValidationError{
				Pointer: ptr,
				Message: fmt.Sprintf("CHUNK[INT] parameter %q must not be associated with other parameters", name),
			})
		}
	}

	return errs
}

// findChunkInAssoc walks the combination tree and returns the name of the first
// CHUNK[INT] parameter found inside an association group (where it would be
// zipped with sibling expressions), or "" if none. Any chunked identifier that
// appears within a combAssoc subtree is considered associated.
func findChunkInAssoc(node combNode, chunked map[string]struct{}) string {
	switch n := node.(type) {
	case combIdent:
		return ""
	case combProduct:
		for _, child := range n.Children {
			if name := findChunkInAssoc(child, chunked); name != "" {
				return name
			}
		}
		return ""
	case combAssoc:
		// Any chunked identifier anywhere within an association group is being
		// zipped with the group's other comma-separated expressions.
		for _, name := range collectIdents(n) {
			if _, ok := chunked[name]; ok {
				return name
			}
		}
		return ""
	}
	return ""
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
