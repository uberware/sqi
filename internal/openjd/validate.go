// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import (
	"fmt"
	"maps"
	"math"
	"regexp"
	"slices"
	"sort"
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
//  2. Rejects every entry that is not in the extension registry. Silently accepting
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

// ─── path translation validation ─────────────────────────────────────────────

// hasExtension reports whether name appears in the template's extensions list.
func (t *JobTemplate) hasExtension(name string) bool {
	return slices.Contains(t.Extensions, name)
}

// validateDelivery checks per-delivery settings for a single PathDelivery.
// Extracted from validatePathTranslation to keep cyclomatic complexity in bounds.
func validateDelivery(d PathDelivery, ptr string) ValidationErrors {
	switch d.Kind {
	case DeliverySwapInPlace, DeliveryTranslationFile, DeliveryStageLocally:
		// no per-delivery settings to validate
	case DeliveryCommandFlags:
		if !strings.Contains(d.Pattern, "{src}") || !strings.Contains(d.Pattern, "{dest}") {
			return ValidationErrors{{Pointer: ptr, Message: "command_flags pattern must contain {src} and {dest}"}}
		}
	case DeliveryEnvironment:
		if d.Variable == "" {
			return ValidationErrors{{Pointer: ptr, Message: "environment delivery requires a non-empty variable"}}
		}
	default:
		return ValidationErrors{{Pointer: ptr, Message: fmt.Sprintf("unknown delivery %q", string(d.Kind))}}
	}
	return nil
}

// validatePathTranslation enforces the SQI_PATH_TRANSLATION extension/block
// coupling and per-delivery settings. Structural (not gated by EnforceLimits).
func validatePathTranslation(t *JobTemplate) ValidationErrors {
	declared := t.hasExtension("SQI_PATH_TRANSLATION")

	if t.PathTranslation != nil && !declared {
		return ValidationErrors{{
			Pointer: "/SQI_PATH_TRANSLATION",
			Message: "SQI_PATH_TRANSLATION block requires declaring the SQI_PATH_TRANSLATION extension in extensions",
		}}
	}
	if !declared {
		return nil
	}
	if t.PathTranslation == nil {
		return ValidationErrors{{
			Pointer: "/extensions",
			Message: "the SQI_PATH_TRANSLATION extension requires a SQI_PATH_TRANSLATION block",
		}}
	}
	if len(t.PathTranslation.Deliveries) == 0 {
		return ValidationErrors{{
			Pointer: "/SQI_PATH_TRANSLATION/deliveries",
			Message: "SQI_PATH_TRANSLATION requires at least one delivery",
		}}
	}
	var errs ValidationErrors
	for i, d := range t.PathTranslation.Deliveries {
		ptr := fmt.Sprintf("/SQI_PATH_TRANSLATION/deliveries/%d", i)
		errs = append(errs, validateDelivery(d, ptr)...)
	}
	return errs
}

// validateChunkBounds enforces the SQI_CHUNK_BOUNDS extension's constraints when
// it is declared: TASK_CHUNKING must also be declared, and every CHUNK[INT] task
// parameter must be CONTIGUOUS (rangeConstraint is required, so this only rejects
// the literal NONCONTIGUOUS value), because .Start/.End are undefined across the
// gaps of a NONCONTIGUOUS chunk.
// Runs unconditionally (not gated by EnforceLimits).
func validateChunkBounds(t *JobTemplate) ValidationErrors {
	if !t.hasExtension("SQI_CHUNK_BOUNDS") {
		return nil
	}
	var errs ValidationErrors
	if !t.hasExtension("TASK_CHUNKING") {
		errs = append(errs, ValidationError{
			Pointer: "/extensions",
			Message: "SQI_CHUNK_BOUNDS requires the TASK_CHUNKING extension to also be declared",
		})
	}
	for i, s := range t.Steps {
		if s.ParameterSpace == nil {
			continue
		}
		for j, tp := range s.ParameterSpace.TaskParameterDefinitions {
			if tp.Type != TaskParamTypeChunkInt {
				continue
			}
			if tp.Chunks != nil && tp.Chunks.RangeConstraint == "NONCONTIGUOUS" {
				errs = append(errs, ValidationError{
					Pointer: fmt.Sprintf("/steps/%d/parameterSpace/taskParameterDefinitions/%d", i, j),
					Message: "SQI_CHUNK_BOUNDS requires CHUNK[INT] parameters to be CONTIGUOUS; .Start/.End are undefined for a NONCONTIGUOUS chunk",
				})
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

// fileFilterPatternRE matches the grammar of
// <FileDialogFilterPatternStringValue> (§2.8): "*", "*.*", or "*." followed
// by one or more legal extension characters. Legal extension characters are
// any unicode character except the Cc category, path separators ("\" and
// "/"), wildcard characters ("*", "?", "[", "]"), and characters commonly
// disallowed in paths ("#", "%", "&", "{", "}", "<", ">", "$", "!", "'",
// "\"", ":", "@", "`", "|", "="). This also subsumes the spec's 1-character
// minimum length: an empty string does not match any alternative.
var fileFilterPatternRE = regexp.MustCompile(`^(\*|\*\.\*|\*\.[^\p{Cc}\\/*?\[\]#%&{}<>$!'":@` + "`" + `|=]+)$`)

// ─── ValidateOptions ──────────────────────────────────────────────────────────

// ValidateOptions controls optional validation behavior passed to
// [ValidateWithOptions].
type ValidateOptions struct {
	// EnforceLimits gates quantitative limit checks: maximum name lengths,
	// element counts, etc.
	//
	// When false those checks are skipped — useful in operator environments
	// that predate strict limit enforcement and cannot yet update all templates.
	//
	// NOTE: the quantitative limit checks live in [validateLimits] (and the
	// helpers it calls). Every limit check MUST be guarded by opts.EnforceLimits
	// and belong to validateLimits — do not scatter gated checks elsewhere.
	// Structural correctness checks belong in the always-run path instead:
	// [validateHostRequirements] is the host-requirement half, deliberately
	// separate from [validateHostRequirementLimits]. Reserved-capability value
	// checks ([validateReservedAmounts], [validateReservedAttributes]) are
	// value-domain correctness, not a size or count cap, so they run from
	// [validateHostRequirements] unconditionally too.
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
	errs = append(errs, validatePathTranslation(t)...)
	errs = append(errs, validateChunkBounds(t)...)

	// ── quantitative limits (gated) ───────────────────────────────────────
	// Every check below MUST stay behind opts.EnforceLimits.
	if opts.EnforceLimits {
		errs = append(errs, validateLimits(t)...)
	}

	return errs
}

// ─── reserved-name tables ─────────────────────────────────────────────────────

// NOTE: there is deliberately no reservedAmountMinimums map here. sqi once
// enforced the spec's per-capability "Minimum Value" table (vcpu >= 1, and so
// on) as a floor on a requirement's own min/max. That was a misreading: the
// table gives the default used when min is OMITTED ("If not provided, then the
// default is 0 unless the specific host capability defines a minimum"), not a
// lower bound on an explicitly supplied value. The schema types min as
// <nonnegativefloat> and max as <positivefloat>, so "amount.worker.vcpu min: 0"
// is valid — see the conformance fixture 3.3.1--amount-min-zero-valid.yaml.
// Those bounds are enforced by [validateAmountBounds].

// reservedCapabilityScopes are the identifiers the spec reserves as the first
// segment after a capability namespace ("amount."/"attr."). §3.3.1.1: "This
// specification has reserved specific values of the first <Identifier> after
// 'amount.' for use in this and future revisions. The reserved values are:
// 'worker', 'job', 'step', and 'task'". A name under one of these scopes must
// be one the spec (or, see sqiReservedScopeNames, sqi itself) actually defines.
// Read-only after initialization.
var reservedCapabilityScopes = map[string]bool{
	"worker": true, "job": true, "step": true, "task": true,
}

// specDefinedCapabilities are the full capability names the OpenJD 2023-09
// spec defines under a reserved scope. Read-only after initialization.
var specDefinedCapabilities = map[string]bool{
	"amount.worker.vcpu":         true,
	"amount.worker.memory":       true,
	"amount.worker.gpu":          true,
	"amount.worker.gpu.memory":   true,
	"amount.worker.disk.scratch": true,
	"attr.worker.os.family":      true,
	"attr.worker.cpu.arch":       true,
}

// sqiReservedScopeNames are capability names sqi defines under the reserved
// "worker" scope. These are a KNOWN DIVERGENCE from strict conformance: the
// spec reserves that scope for its own future use, and a vendor is meant to
// namespace its capabilities behind a vendor prefix instead
// ("sqi:attr.worker.tag.nuke", which [validateCapabilityPrefix] accepts).
//
// They are permitted because they predate this check and are load-bearing:
// attr.worker.tag.* backs worker capability tags (every shipped DCC preset
// gates on one), amount.worker.usagepool.* backs usage pools, and
// attr.worker.computelocation backs compute locations. Enforcing the spec
// strictly here would invalidate sqi's own presets. Migrating them behind a
// vendor prefix is the conformant fix and a breaking change; it is not made
// here. Prefixes end in "." to match a family; bare entries match exactly.
var sqiReservedScopeNames = []string{
	"amount.worker.usagepool.",
	"attr.worker.tag.",
	"attr.worker.os.version",
	"attr.worker.computelocation",
}

// validateReservedScope reports an error when a capability name sits under a
// spec-reserved scope but is not a name the spec (or sqi, see
// [sqiReservedScopeNames]) defines — for example "amount.worker.custom".
//
// A vendor-prefixed name is exempt: it lives in the vendor's own namespace, so
// "mycompany:amount.worker.custom" is that vendor's capability rather than a
// claim on the spec's reserved scope.
func validateReservedScope(name, wantPrefix, ptr string) ValidationErrors {
	local := strings.ToLower(name)
	if vendor, rest, ok := strings.Cut(local, ":"); ok && identifierRE.MatchString(vendor) {
		return nil // vendor namespace; not the spec's reserved scope
	} else if ok {
		local = rest
	}
	if !strings.HasPrefix(local, wantPrefix) {
		return nil // prefix problems are reported by validateCapabilityPrefix
	}
	scope, _, _ := strings.Cut(strings.TrimPrefix(local, wantPrefix), ".")
	if !reservedCapabilityScopes[scope] {
		return nil // a custom scope is unconstrained
	}
	if specDefinedCapabilities[local] {
		return nil
	}
	for _, allowed := range sqiReservedScopeNames {
		if local == allowed || (strings.HasSuffix(allowed, ".") && strings.HasPrefix(local, allowed)) {
			return nil
		}
	}
	return ValidationErrors{{
		Pointer: ptr,
		Message: fmt.Sprintf(
			"capability name %q uses the reserved scope %q but is not a defined capability; "+
				"use a custom scope or a vendor prefix", name, scope,
		),
	}}
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

	// maxAttributeValueLen caps each <AttributeCapabilityValue> string
	// (spec §3.3.2.2: max length 100 characters).
	maxAttributeValueLen = 100
	// maxUILabelLen caps userInterface label length in characters (runes).
	// The spec's <UserInterfaceLabelStringValue> is 1-64 characters.
	maxUILabelLen = 64
	// maxUIGroupLabelLen caps userInterface groupLabel length in characters (runes).
	maxUIGroupLabelLen = 64
	// maxFileFilterLabelLen caps a fileFilters/fileFilterDefault entry's label
	// length in characters (runes). Same <UserInterfaceLabelStringValue> bound
	// (1-64 characters) as maxUILabelLen (§2.6).
	maxFileFilterLabelLen = maxUILabelLen
	// maxFileFilters caps the number of entries in a PATH parameter's
	// fileFilters list. Spec: "Maximum of 20 filters".
	maxFileFilters = 20
	// maxFileFilterPatternLen caps a single pattern string's length in
	// characters (runes). The spec's <FileDialogFilterPatternStringValue> is
	// 1-20 characters; the 1-character minimum is structural (subsumed by
	// [fileFilterPatternRE]) so only the maximum is gated here.
	maxFileFilterPatternLen = 20
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
// hostRequirements: combined count, capability name lengths, and attribute
// anyOf/allOf element counts. Structural correctness (presence, capability
// name well-formedness and prefix, and attribute anyOf/allOf presence) and
// reserved-capability value checks (reserved amount minimums, reserved
// attribute allowed values) live in [validateHostRequirements] instead and
// always run — see the invariant documented on [ValidateOptions].
func validateHostRequirementLimits(hr HostRequirements, base string) ValidationErrors {
	var errs ValidationErrors

	if combined := len(hr.Amounts) + len(hr.Attributes); combined > maxHostRequirements {
		errs = append(errs, ValidationError{
			Pointer: base,
			Message: fmt.Sprintf("at most %d host requirements are allowed (got %d)", maxHostRequirements, combined),
		})
	}

	for i, a := range hr.Amounts {
		ptr := fmt.Sprintf("%s/amounts/%d/name", base, i)
		errs = append(errs, validateCapabilityNameLength(a.Name, ptr)...)
	}

	for i, a := range hr.Attributes {
		ptr := fmt.Sprintf("%s/attributes/%d", base, i)
		errs = append(errs, validateCapabilityNameLength(a.Name, ptr+"/name")...)

		if len(a.AnyOf) > maxAttributeValues {
			errs = append(errs, ValidationError{
				Pointer: ptr + "/anyOf",
				Message: fmt.Sprintf("at most %d values are allowed (got %d)", maxAttributeValues, len(a.AnyOf)),
			})
		}
		for j, v := range append(append([]string{}, a.AnyOf...), a.AllOf...) {
			if utf8.RuneCountInString(v) > maxAttributeValueLen {
				field, idx := "anyOf", j
				if j >= len(a.AnyOf) {
					field, idx = "allOf", j-len(a.AnyOf)
				}
				errs = append(errs, ValidationError{
					Pointer: fmt.Sprintf("%s/%s/%d", ptr, field, idx),
					Message: fmt.Sprintf("value must be at most %d characters (got %d)",
						maxAttributeValueLen, utf8.RuneCountInString(v)),
				})
			}
		}
		if len(a.AllOf) > maxAttributeValues {
			errs = append(errs, ValidationError{
				Pointer: ptr + "/allOf",
				Message: fmt.Sprintf("at most %d values are allowed (got %d)", maxAttributeValues, len(a.AllOf)),
			})
		}
	}

	return errs
}

// validateHostRequirements checks the structural correctness of a step's host
// requirements: that the block declares something, that capability names are
// non-empty and correctly prefixed, that each attribute constrains something,
// and — for reserved capability names — that the value asked for is within
// the spec-mandated domain (reserved amount minimums, reserved attribute
// allowed values). These are correctness checks, not size caps: a template
// asking for vcpu: 0 is malformed, not oversized. So they always run -- see
// the invariant documented on [ValidateOptions]. The gated size caps
// (combined count, name length, anyOf/allOf element counts) live in
// [validateHostRequirementLimits] instead.
func validateHostRequirements(hr HostRequirements, base string) ValidationErrors {
	var errs ValidationErrors

	if len(hr.Amounts)+len(hr.Attributes) == 0 {
		errs = append(errs, ValidationError{
			Pointer: base,
			Message: "hostRequirements must declare at least one amount or attribute when present",
		})
	}

	for i, a := range hr.Amounts {
		ptr := fmt.Sprintf("%s/amounts/%d/name", base, i)
		errs = append(errs, validateCapabilityNameRequired(a.Name, ptr)...)
		errs = append(errs, validateCapabilityPrefix(a.Name, "amount.", ptr)...)
		errs = append(errs, validateReservedScope(a.Name, "amount.", ptr)...)
	}

	for i, a := range hr.Attributes {
		ptr := fmt.Sprintf("%s/attributes/%d", base, i)
		errs = append(errs, validateCapabilityNameRequired(a.Name, ptr+"/name")...)
		errs = append(errs, validateCapabilityPrefix(a.Name, "attr.", ptr+"/name")...)
		errs = append(errs, validateReservedScope(a.Name, "attr.", ptr+"/name")...)

		if len(a.AnyOf)+len(a.AllOf) == 0 {
			errs = append(errs, ValidationError{
				Pointer: ptr,
				Message: "attribute must declare at least one of anyOf or allOf",
			})
		}
	}

	// Amount bound checks and reserved-attribute value checks: pure
	// value-domain correctness, with no size or count component, so they
	// belong here rather than in the gated validateHostRequirementLimits.
	errs = append(errs, validateAmountBounds(hr.Amounts, base)...)
	errs = append(errs, validateReservedAttributes(hr.Attributes, base)...)

	return errs
}

// validateReservedAmounts checks that any amount with a reserved capability
// name (matched case-insensitively) has its Min and Max values (when present)
// >= the spec-mandated minimum. Amounts with non-reserved names are unconstrained.
//
// Min/Max values that contain an unresolved format-string reference ("{{") are
// skipped: the numeric value cannot be determined before job-parameter binding.
// Nothing upstream of this check rejects a non-numeric Min/Max: the decoder
// (decodeAmountBound) accepts any scalar (string, number, or boolean) without
// requiring it to parse as a number, so a non-numeric bound is reported here,
// by [checkReservedBound], not silently skipped.
func validateAmountBounds(amounts []AmountRequirement, base string) ValidationErrors {
	var errs ValidationErrors
	for i, a := range amounts {
		ptr := fmt.Sprintf("%s/amounts/%d", base, i)

		if a.Min == nil && a.Max == nil {
			errs = append(errs, ValidationError{
				Pointer: ptr,
				Message: "at least one of min or max must be provided",
			})
			continue
		}

		minV, minOK, minErrs := parseAmountBound(a.Min, ptr+"/min")
		errs = append(errs, minErrs...)
		maxV, maxOK, maxErrs := parseAmountBound(a.Max, ptr+"/max")
		errs = append(errs, maxErrs...)

		if minOK && minV < 0 {
			errs = append(errs, ValidationError{
				Pointer: ptr + "/min",
				Message: fmt.Sprintf("min must be non-negative (got %g)", minV),
			})
		}
		if maxOK && maxV <= 0 {
			errs = append(errs, ValidationError{
				Pointer: ptr + "/max",
				Message: fmt.Sprintf("max must be positive (got %g)", maxV),
			})
		}
		if minOK && maxOK && minV > maxV {
			errs = append(errs, ValidationError{
				Pointer: ptr + "/min",
				Message: fmt.Sprintf("min %g must not exceed max %g", minV, maxV),
			})
		}
	}
	return errs
}

// checkReservedBound validates that a *string capability bound (Min or Max)
// is >= the reserved minimum. Nil bounds and bounds containing format-string
// references ("{{") are skipped. A bound that fails to parse as a number, or
// that parses to a non-finite value (NaN, ±Inf), is reported as a validation
// error rather than skipped.
func parseAmountBound(val *string, ptr string) (value float64, ok bool, errs ValidationErrors) {
	if val == nil || strings.Contains(*val, "{{") {
		return 0, false, nil
	}
	v, err := strconv.ParseFloat(*val, 64)
	if err != nil {
		return 0, false, ValidationErrors{{
			Pointer: ptr,
			Message: fmt.Sprintf("value %q is not a valid number", *val),
		}}
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, false, ValidationErrors{{
			Pointer: ptr,
			Message: fmt.Sprintf("value %q is not a finite number", *val),
		}}
	}
	return v, true, nil
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
// An empty name is left to [validateCapabilityNameRequired]'s "name is
// required" check.
//
// A single optional vendor prefix may precede the namespace. The spec's format
// is "[<Identifier>:]amount.<Identifier>[.<Identifier>]*" (§3.3.1.1, and
// §3.3.2.1 for "attr."), so "mycompany:amount.licenses" is valid and lets a
// studio namespace capabilities of its own. The prefix is stripped before the
// namespace check; it must itself be a well-formed <Identifier>, and only one
// is permitted — "a:b:amount.x" is rejected because "b:amount.x" does not begin
// with the namespace.
//
// Reserved-value checks ([validateReservedAmounts], [validateReservedAttributes])
// deliberately key on the FULL name, so a vendor-prefixed capability is not
// treated as reserved: "mycompany:amount.worker.vcpu" names the vendor's own
// capability, not the spec's. The spec does not settle this, and no conformance
// fixture covers it.
func validateCapabilityPrefix(name, wantPrefix, ptr string) ValidationErrors {
	if name == "" {
		return nil
	}
	malformed := ValidationErrors{{
		Pointer: ptr,
		Message: fmt.Sprintf(
			"capability name %q must begin with %q, optionally preceded by a single \"<identifier>:\" vendor prefix",
			name, wantPrefix,
		),
	}}

	local := name
	if vendor, rest, ok := strings.Cut(name, ":"); ok {
		if !identifierRE.MatchString(vendor) {
			return malformed
		}
		local = rest
	}
	if !hasPrefixFold(local, wantPrefix) {
		return malformed
	}
	return nil
}

// validateCapabilityNameRequired checks that an amount/attribute capability
// name is present. Structural correctness: always runs, regardless of
// EnforceLimits — see the invariant documented on [ValidateOptions].
func validateCapabilityNameRequired(name, ptr string) ValidationErrors {
	if utf8.RuneCountInString(name) == 0 {
		return ValidationErrors{{Pointer: ptr, Message: "name is required"}}
	}
	return nil
}

// validateCapabilityNameLength checks that an amount/attribute capability name
// is within the spec bound (at most 100 characters). Gated: only runs when
// EnforceLimits is set (see [validateHostRequirementLimits]).
func validateCapabilityNameLength(name, ptr string) ValidationErrors {
	if n := utf8.RuneCountInString(name); n > maxCapabilityNameLen {
		return ValidationErrors{{
			Pointer: ptr,
			Message: fmt.Sprintf("name must be at most %d characters (got %d)", maxCapabilityNameLen, n),
		}}
	}
	return nil
}

// validateUILimits enforces length caps on userInterface labels and the
// fileFilters quantitative caps (label length, filter count). Gated: callers
// run it only when EnforceLimits is set.
func validateUILimits(params []JobParameter) ValidationErrors {
	var errs ValidationErrors
	for i, p := range params {
		paramPtr := fmt.Sprintf("/parameterDefinitions/%d", i)
		if p.UserInterface != nil {
			base := paramPtr + "/userInterface"
			if utf8.RuneCountInString(p.UserInterface.Label) > maxUILabelLen {
				errs = append(errs, ValidationError{
					Pointer: base + "/label",
					Message: fmt.Sprintf("label exceeds %d characters", maxUILabelLen),
				})
			}
			if utf8.RuneCountInString(p.UserInterface.GroupLabel) > maxUIGroupLabelLen {
				errs = append(errs, ValidationError{
					Pointer: base + "/groupLabel",
					Message: fmt.Sprintf("groupLabel exceeds %d characters", maxUIGroupLabelLen),
				})
			}
		}
		errs = append(errs, validateFileFilterLimits(p, paramPtr)...)
	}
	return errs
}

// validateFileFilterLimits checks the gated quantitative caps on a PATH
// parameter's file filters: at most [maxFileFilters] entries, each entry's
// (including fileFilterDefault's) label at most [maxFileFilterLabelLen]
// characters, and each pattern (including fileFilterDefault's) at most
// [maxFileFilterPatternLen] characters. Structural correctness (label
// required, control pairing, pattern grammar) lives in [validateFileFilters]
// instead and always runs -- see the invariant documented on
// [ValidateOptions].
func validateFileFilterLimits(p JobParameter, ptr string) ValidationErrors {
	var errs ValidationErrors
	if len(p.FileFilters) > maxFileFilters {
		errs = append(errs, ValidationError{
			Pointer: ptr + "/fileFilters",
			Message: fmt.Sprintf("at most %d file filters are allowed (got %d)", maxFileFilters, len(p.FileFilters)),
		})
	}
	for i, f := range p.FileFilters {
		errs = append(errs, validatePathFileFilterLimits(f, fmt.Sprintf("%s/fileFilters/%d", ptr, i))...)
	}
	if p.FileFilterDefault != nil {
		errs = append(errs, validatePathFileFilterLimits(*p.FileFilterDefault, ptr+"/fileFilterDefault")...)
	}
	return errs
}

// validatePathFileFilterLimits checks the gated quantitative caps on a
// single [PathFileFilter]: label length and each pattern's length. Extracted
// from [validateFileFilterLimits] to keep its complexity in bounds and
// reused for both fileFilters entries and fileFilterDefault.
func validatePathFileFilterLimits(f PathFileFilter, ptr string) ValidationErrors {
	var errs ValidationErrors
	if n := utf8.RuneCountInString(f.Label); n > maxFileFilterLabelLen {
		errs = append(errs, ValidationError{
			Pointer: ptr + "/label",
			Message: fmt.Sprintf("label must be at most %d characters (got %d)", maxFileFilterLabelLen, n),
		})
	}
	for i, pattern := range f.Patterns {
		if n := utf8.RuneCountInString(pattern); n > maxFileFilterPatternLen {
			errs = append(errs, ValidationError{
				Pointer: fmt.Sprintf("%s/patterns/%d", ptr, i),
				Message: fmt.Sprintf("pattern must be at most %d characters (got %d)", maxFileFilterPatternLen, n),
			})
		}
	}
	return errs
}

// ─── userInterface validation ─────────────────────────────────────────────────

// controlsByType is the OpenJD base-spec userInterface control vocabulary,
// scoped per parameter type as the spec defines it. The spec does NOT share one
// vocabulary across types: LINE_EDIT is valid on STRING and invalid on PATH,
// which needs a CHOOSE_* dialog instead. Read-only after initialization.
//
// The *_LIST control variants belong to the EXPR extension and are deliberately
// absent -- sqi does not implement EXPR.
var controlsByType = map[JobParamType]map[ControlType]struct{}{
	JobParamTypeString: {
		ControlLineEdit:      {},
		ControlMultilineEdit: {},
		ControlDropdownList:  {},
		ControlCheckBox:      {},
		ControlHidden:        {},
	},
	JobParamTypePath: {
		ControlChooseInputFile:  {},
		ControlChooseOutputFile: {},
		ControlChooseDirectory:  {},
		ControlDropdownList:     {},
		ControlHidden:           {},
	},
	JobParamTypeInt: {
		ControlSpinBox:      {},
		ControlDropdownList: {},
		ControlHidden:       {},
	},
	JobParamTypeFloat: {
		ControlSpinBox:      {},
		ControlDropdownList: {},
		ControlHidden:       {},
	},
}

// allowedControlsFor returns the sorted control names valid for a parameter
// type, for use in error messages. A template author who writes LINE_EDIT on a
// PATH needs to be told CHOOSE_INPUT_FILE exists, not merely that they are wrong.
func allowedControlsFor(t JobParamType) string {
	set, ok := controlsByType[t]
	if !ok {
		return ""
	}
	names := make([]string, 0, len(set))
	for c := range set {
		names = append(names, string(c))
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
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
	}
	return errs
}

// validateUserInterface checks a parameter's optional userInterface hints:
// the control must be a known value, and control/constraint combinations must
// be coherent (DROPDOWN_LIST/CHECK_BOX need allowedValues; SPIN_BOX is numeric;
// decimals pairs with its control). Structural — always runs.
func validateUserInterface(p JobParameter, ptr string) ValidationErrors {
	ui := p.UserInterface
	if ui == nil {
		return nil
	}
	var errs ValidationErrors
	ctrlPtr := ptr + "/userInterface/control"

	// control is optional: the schema marks it "@optional" on every parameter
	// type, so a template may supply only a label or groupLabel and leave the
	// control to the submitting application. Checks that depend on a control
	// are skipped when it is absent; those that do not — decimals, below — are
	// still enforced.
	if ui.Control != "" {
		allowed, known := controlsByType[p.Type]
		if !known {
			return errs // unknown parameter type is reported by validateJobParams
		}
		if _, ok := allowed[ui.Control]; !ok {
			errs = append(errs, ValidationError{
				Pointer: ctrlPtr,
				Message: fmt.Sprintf("control %q is not valid on a %s parameter; allowed: %s",
					ui.Control, p.Type, allowedControlsFor(p.Type)),
			})
			return errs
		}

		errs = append(errs, validateUserInterfaceControl(ui, p, ctrlPtr)...)
	}

	if ui.Decimals != nil && (ui.Control != ControlSpinBox || p.Type != JobParamTypeFloat) {
		errs = append(errs, ValidationError{
			Pointer: ptr + "/userInterface/decimals",
			Message: "decimals is valid only with SPIN_BOX on a FLOAT parameter",
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

		// fileFilters / fileFilterDefault are also structural, always runs.
		errs = append(errs, validateFileFilters(p, ptr)...)
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

// fileFilterControls is the set of userInterface controls that fileFilters and
// fileFilterDefault are valid alongside, per OpenJD jobtemplate-2023-09 §2.7:
// "Can be provided when the uiControl is CHOOSE_INPUT_FILE or
// CHOOSE_OUTPUT_FILE".
var fileFilterControls = map[ControlType]struct{}{
	ControlChooseInputFile:  {},
	ControlChooseOutputFile: {},
}

// validateFileFilters checks the PATH-only file chooser filters: they are
// valid only on PATH parameters whose userInterface.control is
// CHOOSE_INPUT_FILE or CHOOSE_OUTPUT_FILE, and each filter (including
// fileFilterDefault) must declare a label and at least one pattern.
// Structural -- always runs. The quantitative caps (label length, filter
// count) live in [validateFileFilterLimits] instead, gated behind
// EnforceLimits -- see the invariant documented on [ValidateOptions].
func validateFileFilters(p JobParameter, ptr string) ValidationErrors {
	var errs ValidationErrors
	if len(p.FileFilters) == 0 && p.FileFilterDefault == nil {
		return nil
	}
	// Point at whichever field was actually declared, so a
	// fileFilterDefault-only parameter doesn't get an error pointing at
	// the sibling fileFilters field it never set.
	field := "fileFilterDefault"
	if len(p.FileFilters) > 0 {
		field = "fileFilters"
	}
	if p.Type != JobParamTypePath {
		errs = append(errs, ValidationError{
			Pointer: ptr + "/" + field,
			Message: "fileFilters and fileFilterDefault are valid only on PATH parameters",
		})
		return errs
	}
	if p.UserInterface == nil {
		errs = append(errs, ValidationError{
			Pointer: ptr + "/" + field,
			Message: "fileFilters and fileFilterDefault require userInterface.control to be CHOOSE_INPUT_FILE or CHOOSE_OUTPUT_FILE",
		})
	} else if _, ok := fileFilterControls[p.UserInterface.Control]; !ok {
		errs = append(errs, ValidationError{
			Pointer: ptr + "/" + field,
			Message: fmt.Sprintf("fileFilters and fileFilterDefault require userInterface.control to be CHOOSE_INPUT_FILE or CHOOSE_OUTPUT_FILE (got %q)", p.UserInterface.Control),
		})
	}
	for i, f := range p.FileFilters {
		errs = append(errs, validatePathFileFilter(f, fmt.Sprintf("%s/fileFilters/%d", ptr, i))...)
	}
	if p.FileFilterDefault != nil {
		errs = append(errs, validatePathFileFilter(*p.FileFilterDefault, ptr+"/fileFilterDefault")...)
	}
	return errs
}

// validatePathFileFilter checks the structural correctness of a single
// [PathFileFilter]: label is required, at least one pattern is required, and
// each pattern must match the <FileDialogFilterPatternStringValue> grammar
// (§2.8): "*", "*.*", or "*." followed by one or more legal extension
// characters. Extracted from [validateFileFilters] to keep its complexity in
// bounds and reused for both fileFilters entries and fileFilterDefault.
func validatePathFileFilter(f PathFileFilter, ptr string) ValidationErrors {
	var errs ValidationErrors
	if f.Label == "" {
		errs = append(errs, ValidationError{
			Pointer: ptr + "/label",
			Message: "required",
		})
	}
	if len(f.Patterns) == 0 {
		errs = append(errs, ValidationError{
			Pointer: ptr + "/patterns",
			Message: "at least one pattern is required",
		})
	}
	for i, pattern := range f.Patterns {
		if !fileFilterPatternRE.MatchString(pattern) {
			errs = append(errs, ValidationError{
				Pointer: fmt.Sprintf("%s/patterns/%d", ptr, i),
				Message: fmt.Sprintf("pattern %q is not a valid file filter pattern; must be \"*\", \"*.*\", or \"*.\" followed by one or more legal extension characters", pattern),
			})
		}
	}
	return errs
}

// validateAction checks the spec-required fields on a single action. The spec
// marks command required with a minimum length of 1 character. An action with
// no command is accepted by parse, expands into tasks, and then runs nothing --
// the step reports success having done no work -- so this is structural
// correctness and always runs, never gated behind EnforceLimits.
func validateAction(a Action, ptr string) ValidationErrors {
	if a.Command == "" {
		return ValidationErrors{{
			Pointer: ptr + "/command",
			Message: "required; must be at least 1 character",
		}}
	}
	return nil
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
		if e.Script == nil && len(e.Variables) == 0 {
			errs = append(errs, ValidationError{
				Pointer: ptr,
				Message: "at least one of script or variables must be provided",
			})
		}
		if e.Script != nil {
			if e.Script.Actions.OnEnter == nil {
				errs = append(errs, ValidationError{
					Pointer: ptr + "/script/actions/onEnter",
					Message: "required",
				})
			} else {
				errs = append(errs, validateAction(*e.Script.Actions.OnEnter, ptr+"/script/actions/onEnter")...)
			}
			if e.Script.Actions.OnExit != nil {
				errs = append(errs, validateAction(*e.Script.Actions.OnExit, ptr+"/script/actions/onExit")...)
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

	// step script -- required by spec; without it the step has no action and
	// scheduler/assign.go silently omits it, so the step runs nothing.
	if s.Script == nil {
		errs = append(errs, ValidationError{Pointer: base + "/script", Message: "required"})
	} else {
		errs = append(errs, validateEmbeddedFiles(s.Script.EmbeddedFiles, base+"/script/embeddedFiles")...)
		errs = append(errs, validateAction(s.Script.Actions.OnRun, base+"/script/actions/onRun")...)
	}

	// step environments
	errs = append(errs, validateEnvironments(s.StepEnvironments, base+"/stepEnvironments")...)

	// parameter space
	if s.ParameterSpace != nil {
		errs = append(errs, validateParameterSpace(*s.ParameterSpace, base+"/parameterSpace")...)
	}

	// host requirements (structural; the size caps stay in validateLimits)
	if s.HostRequirements != nil {
		errs = append(errs, validateHostRequirements(*s.HostRequirements, base+"/hostRequirements")...)
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
		} else {
			errs = append(errs, validateChunks(*tp.Chunks, base)...)
		}
	}

	return errs
}

// validateChunks validates a CHUNK[INT] parameter's chunks definition. It is
// extracted from [validateTaskParamRangeAndChunks] to keep that function's
// cyclomatic complexity within bounds.
func validateChunks(c TaskChunks, base string) ValidationErrors {
	var errs ValidationErrors

	if c.DefaultTaskCount <= 0 {
		errs = append(errs, ValidationError{
			Pointer: base + "/chunks/defaultTaskCount",
			Message: "must be a positive integer",
		})
	}

	switch c.RangeConstraint {
	case "":
		errs = append(errs, ValidationError{
			Pointer: base + "/chunks/rangeConstraint",
			Message: "required; must be CONTIGUOUS or NONCONTIGUOUS",
		})
	case "CONTIGUOUS", "NONCONTIGUOUS":
		// valid
	default:
		errs = append(errs, ValidationError{
			Pointer: base + "/chunks/rangeConstraint",
			Message: fmt.Sprintf("invalid value %q; must be CONTIGUOUS or NONCONTIGUOUS", c.RangeConstraint),
		})
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
