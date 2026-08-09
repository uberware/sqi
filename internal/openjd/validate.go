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
	"unicode"
	"unicode/utf8"

	"github.com/uberware/sqi/internal/openjd/fmtstring"
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
	for i, name := range t.Extensions {
		// Check format FIRST: extension names must match [A-Z_0-9]{3,128}
		if !extensionNameRE.MatchString(name) {
			errs = append(errs, ValidationError{
				Pointer: fmt.Sprintf("/extensions/%d", i),
				Message: fmt.Sprintf("invalid extension name %q; must match [A-Z_0-9]{3,128}", name),
			})
			continue // Skip unsupported-set check and declared-set addition for malformed names
		}

		// Two-part gate: the name must be registered AND the entry must be
		// supported. A registered-but-unsupported extension gets its own
		// message — reusing "unsupported extension" here would say the name is
		// unknown, which is false and sends a reader to the wrong place.
		entry, known := LookupExtension(name)
		switch {
		case !known:
			errs = append(errs, ValidationError{
				Pointer: fmt.Sprintf("/extensions/%d", i),
				Message: fmt.Sprintf("unsupported extension %q", name),
			})
		case entry.Status != StatusSupported:
			errs = append(errs, ValidationError{
				Pointer: fmt.Sprintf("/extensions/%d", i),
				Message: fmt.Sprintf("extension %q is registered but not yet supported (status %q)",
					name, entry.Status),
			})
		}
		declared[name] = struct{}{}
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

// letPosition is one of the four places a let: block can occupy in a
// template, as yielded by [walkLetPositions].
type letPosition struct {
	// letSet records whether the key was present (distinguishing a declared
	// empty list from an omitted key -- see the LetSet field doc comments in
	// model.go).
	letSet bool
	// let is the raw "name = expression" strings, unparsed.
	let []string
	// ptr is the JSON pointer to this let: key, for error reporting.
	ptr string
}

// walkLetPositions visits the four positions a let: block can occupy --
// step template, step script, step environment script, job environment
// script -- and calls fn once per position, present or not.
//
// Shared by [validateLetExtension] and [validateLetElementCounts] (both of
// which apply at these same four positions) so the two rules cannot drift
// apart the first time a fifth position appears. A closure was chosen over a
// second pair of accumulator slices because each rule's logic is a few lines
// and independent of the other's; threading two ValidationErrors slices
// through one walk would couple them for no benefit.
func walkLetPositions(t *JobTemplate, fn func(letPosition)) {
	for i, s := range t.Steps {
		fn(letPosition{s.LetSet, s.Let, fmt.Sprintf("/steps/%d/let", i)})
		if s.Script != nil {
			fn(letPosition{s.Script.LetSet, s.Script.Let, fmt.Sprintf("/steps/%d/script/let", i)})
		}
		for j, e := range s.StepEnvironments {
			if e.Script != nil {
				fn(letPosition{e.Script.LetSet, e.Script.Let, fmt.Sprintf("/steps/%d/stepEnvironments/%d/script/let", i, j)})
			}
		}
	}
	for i, e := range t.JobEnvironments {
		if e.Script != nil {
			fn(letPosition{e.Script.LetSet, e.Script.Let, fmt.Sprintf("/jobEnvironments/%d/script/let", i)})
		}
	}
}

// validateLetExtension rejects a let: block on a template that does not declare
// the EXPR extension. Template Schemas 3.6 defines <LetBindings> as "Available
// when using the EXPR extension", and without this check a let: block is
// silently ignored -- the template is accepted and its bindings never evaluated,
// which is strictly worse than refusing it (see docs/openjd-conformance.md on
// why rejecting an unimplemented opt-in extension is the correct failure mode).
//
// This is the ONE let rule that must fire on the base-spec path.
// checkTemplateExpressions returns at its first line when EXPR is absent, so
// every other let rule lives in exprcheck.go and this one cannot.
//
// Runs unconditionally (not gated by EnforceLimits).
func validateLetExtension(t *JobTemplate) ValidationErrors {
	if t.hasExtension("EXPR") {
		return nil
	}
	var errs ValidationErrors
	walkLetPositions(t, func(p letPosition) {
		if p.letSet {
			errs = append(errs, ValidationError{
				Pointer: p.ptr,
				Message: `"let" requires the EXPR extension to be declared`,
			})
		}
	})
	return errs
}

// maxLetBindings caps the number of bindings in a single let: block.
// Template Schemas 3.6: "Maximum number of items: 50".
//
// Two places read it, and they do different jobs:
// [validateLetElementCounts] REPORTS an over-count as a validation error, and
// checkLetBindings (exprcheck.go) ENFORCES it by refusing to evaluate past the
// cap. Reporting is not guarding -- ValidateWithOptions does not short-circuit
// on the report -- so both are required.
const maxLetBindings = 50

// validateLetElementCounts enforces Template Schemas 3.6's element-count
// bounds on every let: block, regardless of whether EXPR is declared: "If
// defined, then there must be at least one element in this list" and
// "Maximum number of items: 50." These read like quantitative caps, but they
// are structural -- a let: block with 0 or 51 bindings is malformed, not
// merely large or small, so (like [validateLetExtension]) this runs
// unconditionally and is NOT gated by EnforceLimits (see
// docs/openjd-conformance.md on why a structural check must not vanish under
// EnforceLimits: false).
//
// The empty-list rule is expressible only because LetSet distinguishes a
// declared-but-empty list ("let: []") from an omitted key -- len(Let) == 0 is
// true for both, so this reuses [requireNonEmptyIfSet], the same
// set/omitted-vs-empty helper every other optional-list-with-a-minimum field
// in this file uses.
//
// A base-spec "let: []" therefore produces TWO errors at the same pointer --
// [validateLetExtension]'s `"let" requires the EXPR extension` and this
// function's `must contain at least one binding when provided` -- and that is
// intended, not a missing short-circuit: both statements are independently
// true, and a template that fixed only the one it was shown would come back
// still invalid for the other.
//
// DELIBERATE ASYMMETRY, do not harmonize: the structurally identical
// [maxJobParameterDefinitions] cap runs inside [validateLimits], behind
// opts.EnforceLimits, while this one runs unconditionally. The specification
// itself does not draw that line -- sections 3.6 and 2 both list the min- and
// max-item bounds together under "Constraints" -- so the difference is sqi's,
// and it is load-bearing. [maxLetBindings] is not only reported here, it is
// ENFORCED by checkLetBindings (exprcheck.go), which stops evaluating a block
// at the cap; that is the only bound on a construct whose per-binding cost
// ACCUMULATES in a symbol table no per-evaluation budget measures. Moving this
// check behind EnforceLimits would leave an operator who disables limits with
// the over-count neither reported nor -- were the guard ever re-derived from
// this check rather than from the constant -- bounded.
func validateLetElementCounts(t *JobTemplate) ValidationErrors {
	var errs ValidationErrors
	walkLetPositions(t, func(p letPosition) {
		errs = append(errs, requireNonEmptyIfSet(p.letSet, len(p.let), p.ptr, "binding")...)
		if p.letSet && len(p.let) > maxLetBindings {
			errs = append(errs, ValidationError{
				Pointer: p.ptr,
				Message: fmt.Sprintf("at most %d let bindings are allowed (got %d)", maxLetBindings, len(p.let)),
			})
		}
	})
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

	// CheckEXPRExpressionsWhileUnsupported runs the EXPR extension's
	// format-string expression walk ([checkTemplateExpressions]) even though
	// the EXPR registry entry is not [StatusSupported].
	//
	// Default (false) is production's setting, and it is the SAFE one. While
	// EXPR is StatusInProgress, [validateExtensions] rejects every
	// EXPR-declaring template outright, so the walk's result cannot change the
	// verdict — it can only burn CPU. And the walk is expensive: its
	// operation and byte budgets are PER EXPRESSION POSITION with no
	// template-wide cap and no bound on the number of positions, so a template
	// of N expression positions costs N budgets. Measured before this gate
	// existed, an 84 KB template of ~2,000 args entries took 11.3 s of CPU and
	// returned exactly one error — the status-gate rejection it would have
	// returned for free. Since POST /api/v1/jobs accepts a 4 MiB body and (with
	// auth off, the default) accepts it anonymously, that is a cheap way to buy
	// minutes of server CPU per request. Gating the walk on the registry status
	// restores the pre-E2 cost: a template production always rejects is
	// rejected without evaluating anything.
	//
	// Set to true ONLY by test/conformance's EXPR scoring path, which
	// deliberately discounts the status-gate error so a fixture is judged on
	// whatever OTHER errors sqi finds; without this field the conformance
	// suite would stop exercising the checker entirely.
	//
	// TEMPORARY SHAPE. This field exists only for the window in which EXPR is
	// registered but not yet supported. Sub-project H flips the registry entry
	// to StatusSupported, at which point [exprExpressionWalkEnabled] returns
	// true unconditionally, this field never changes an outcome, and H must
	// DELETE it along with the conformance harness's use of it — the walk
	// becomes unconditional again.
	//
	// H MUST NOT flip the status before sub-project E4's template-wide
	// expression budget exists. This gate is currently the ONLY thing bounding
	// the walk; after the flip the walk is unconditional, an accepted template
	// walks TWICE (phase 1 here, phase 2 in submit.go's
	// checkExpressionsAtSubmit), and there is no early rejection left to fall
	// back on. See TestConformance_EXPRNotSupported's failure message, which
	// carries H's checklist.
	CheckEXPRExpressionsWhileUnsupported bool
}

// exprExpressionWalkEnabled reports whether [checkTemplateExpressions] should
// run at all, given the EXPR registry entry's status and the caller's opt-in.
//
// The walk runs when EXPR is [StatusSupported] — the post-sub-project-H steady
// state, where an EXPR template is accepted and its expressions therefore
// decide the verdict — or when the caller explicitly opted in via
// [ValidateOptions.CheckEXPRExpressionsWhileUnsupported].
//
// This is NOT the extension status gate. That gate is [validateExtensions]',
// it is unconditional, and it is untouched by this function: an EXPR template
// is still rejected with the same error at the same pointer whether or not the
// walk runs. This only decides whether sqi spends CPU computing errors that
// cannot change that rejection.
func exprExpressionWalkEnabled(optIn bool) bool {
	if entry, ok := LookupExtension("EXPR"); ok && entry.Status == StatusSupported {
		return true
	}
	return optIn
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

	// exprDeclared gates which format-string checker runs at every position
	// that carries one: the base-spec single-dotted-identifier path
	// (validateFormatString, below) when the template does not declare EXPR,
	// or the expression-evaluator path (checkTemplateExpressions, an
	// unconditional call further down) when it does. Computed once so both
	// paths agree on the same declaration.
	exprDeclared := t.hasExtension("EXPR")

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
	errs = append(errs, validateNoControlChars(t.Name, "/name")...)
	if !exprDeclared {
		errs = append(errs, validateFormatString(t.Name, "/name", ScopeJob, nil)...)
	}
	for _, f := range t.UnknownFields {
		errs = append(errs, ValidationError{
			Pointer: "/" + f,
			Message: "unknown field; the job template schema defines no such key",
		})
	}
	errs = append(errs, requireNonEmptyIfSet(t.ExtensionsSet, len(t.Extensions), "/extensions", "extension")...)
	errs = append(errs, validateDescriptionText(t.Description, "/description")...)

	// ── parameterDefinitions ─────────────────────────────────────────────
	// @optional, but a declared list must hold at least one parameter.
	errs = append(errs, requireNonEmptyIfSet(t.ParameterDefinitionsSet, len(t.ParameterDefinitions),
		"/parameterDefinitions", "parameter")...)
	errs = append(errs, validateJobParams(t.ParameterDefinitions)...)

	// ── jobEnvironments ───────────────────────────────────────────────────
	errs = append(errs, validateEnvironments(t.JobEnvironments, "/jobEnvironments", ScopeJobEnvironment, exprDeclared)...)

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
		errs = append(errs, validateNoControlChars(s.Name, fmt.Sprintf("/steps/%d/name", i))...)
		errs = append(errs, validateDescriptionText(s.Description, fmt.Sprintf("/steps/%d/description", i))...)
		if _, dup := stepNames[s.Name]; dup {
			errs = append(errs, ValidationError{
				Pointer: fmt.Sprintf("/steps/%d/name", i),
				Message: fmt.Sprintf("duplicate step name %q", s.Name),
			})
		}
		stepNames[s.Name] = struct{}{}
	}

	for i, s := range t.Steps {
		errs = append(errs, validateStep(s, i, stepNames, exprDeclared)...)
		errs = append(errs, validateStepEnvCollisions(s, i, t.JobEnvironments)...)
	}

	// ── acyclicity ────────────────────────────────────────────────────────
	errs = append(errs, validateNoCycles(t.Steps)...)

	// ── extensions (unconditional) ────────────────────────────────────────
	// Run unconditionally: silently accepting an unsupported extension would
	// mis-run the template.  NOT gated by opts.EnforceLimits.
	errs = append(errs, validateExtensions(t)...)
	errs = append(errs, validatePathTranslation(t)...)
	errs = append(errs, validateChunkBounds(t)...)
	errs = append(errs, validateLetExtension(t)...)
	errs = append(errs, validateLetElementCounts(t)...)
	// checkTemplateExpressions is phase 1: params is nil, so every symbol is
	// an unresolved placeholder rather than a submitted value (Task 10 adds a
	// phase-2 caller with concrete parameters). It no-ops for a template that
	// does not declare EXPR -- see its own doc comment.
	//
	// Gated on the EXPR registry entry's STATUS, not on the template's own
	// declaration: while EXPR is StatusInProgress, validateExtensions (above)
	// has already rejected this template unconditionally, so the walk cannot
	// change the verdict and its per-position budgets are pure attack surface.
	// See [ValidateOptions.CheckEXPRExpressionsWhileUnsupported] for the cost
	// measurement and for why sub-project H deletes this gate.
	if exprExpressionWalkEnabled(opts.CheckEXPRExpressionsWhileUnsupported) {
		errs = append(errs, checkTemplateExpressions(t, nil)...)
	}

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

// attributeValueRE matches a <AttributeCapabilityValue> (spec §3.3.2.2):
// latin alphanumerics plus underscore and hyphen, starting with a letter or
// underscore. Length is capped separately by validateHostRequirementLimits.
var attributeValueRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*$`)

// validateAttributeValue checks one attribute anyOf/allOf value against the
// spec grammar. A value the scheduler can never match — empty, punctuation, or
// digit-leading — leaves the step permanently unschedulable rather than failing
// loudly, so this is structural correctness.
func validateAttributeValue(v, ptr string) ValidationErrors {
	if strings.Contains(v, "{{") {
		return nil
	}
	if !attributeValueRE.MatchString(v) {
		return ValidationErrors{{
			Pointer: ptr,
			Message: fmt.Sprintf("attribute value %q must start with a letter or underscore and contain only letters, digits, underscores, and hyphens", v),
		}}
	}
	return nil
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
		for _, set := range []struct {
			field  string
			values []string
		}{{"anyOf", a.AnyOf}, {"allOf", a.AllOf}} {
			for j, v := range set.values {
				if n := utf8.RuneCountInString(v); n > maxAttributeValueLen {
					errs = append(errs, ValidationError{
						Pointer: fmt.Sprintf("%s/%s/%d", ptr, set.field, j),
						Message: fmt.Sprintf("value must be at most %d characters (got %d)",
							maxAttributeValueLen, n),
					})
				}
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
//
// It also checks that every amount min/max and attribute anyOf/allOf value
// is a well-scoped format string -- a new check as of sub-project E2's Task
// 9. Host requirements had NO format-string scope validation before this: a
// reference like {{Session.WorkingDirectory}} here was accepted and resolved
// to nothing at run time. exprDeclared skips this half when the template
// declares EXPR; checkTemplateExpressions covers the same position with the
// real evaluator instead.
func validateHostRequirements(hr HostRequirements, base string, exprDeclared bool) ValidationErrors {
	var errs ValidationErrors

	if len(hr.Amounts)+len(hr.Attributes) == 0 {
		errs = append(errs, ValidationError{
			Pointer: base,
			Message: "hostRequirements must declare at least one amount or attribute when present",
		})
	}

	// Capability names are case-insensitive (spec §3.3.1.1/§3.3.2.1), so two
	// entries differing only in case name the SAME capability. Declaring both
	// is a contradiction the scheduler cannot resolve.
	seenAmounts := make(map[string]struct{}, len(hr.Amounts))
	for i, a := range hr.Amounts {
		ptr := fmt.Sprintf("%s/amounts/%d/name", base, i)
		errs = append(errs, validateCapabilityNameRequired(a.Name, ptr)...)
		errs = append(errs, validateCapabilityPrefix(a.Name, "amount.", ptr)...)
		errs = append(errs, validateReservedScope(a.Name, "amount.", ptr)...)
		if a.Name != "" {
			key := strings.ToLower(a.Name)
			if _, dup := seenAmounts[key]; dup {
				errs = append(errs, ValidationError{
					Pointer: ptr,
					Message: fmt.Sprintf("duplicate amount capability name %q (names are case-insensitive)", a.Name),
				})
			}
			seenAmounts[key] = struct{}{}
		}
	}

	seenAttrs := make(map[string]struct{}, len(hr.Attributes))
	for i, a := range hr.Attributes {
		ptr := fmt.Sprintf("%s/attributes/%d", base, i)
		errs = append(errs, validateCapabilityNameRequired(a.Name, ptr+"/name")...)
		errs = append(errs, validateCapabilityPrefix(a.Name, "attr.", ptr+"/name")...)
		errs = append(errs, validateReservedScope(a.Name, "attr.", ptr+"/name")...)
		if a.Name != "" {
			key := strings.ToLower(a.Name)
			if _, dup := seenAttrs[key]; dup {
				errs = append(errs, ValidationError{
					Pointer: ptr + "/name",
					Message: fmt.Sprintf("duplicate attribute capability name %q (names are case-insensitive)", a.Name),
				})
			}
			seenAttrs[key] = struct{}{}
		}
		for k, v := range a.AnyOf {
			errs = append(errs, validateAttributeValue(v, fmt.Sprintf("%s/anyOf/%d", ptr, k))...)
		}
		for k, v := range a.AllOf {
			errs = append(errs, validateAttributeValue(v, fmt.Sprintf("%s/allOf/%d", ptr, k))...)
		}
		// A reserved attribute with a fixed value set names a single property of
		// the host — an OS family cannot be both linux and windows — so an allOf
		// demanding more than one of its values can never be satisfied.
		if _, reserved := reservedAttributeAllowed[strings.ToLower(a.Name)]; reserved && len(a.AllOf) > 1 {
			errs = append(errs, ValidationError{
				Pointer: ptr + "/allOf",
				Message: fmt.Sprintf("%q holds a single value, so allOf cannot require %d of them", a.Name, len(a.AllOf)),
			})
		}

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

	if !exprDeclared {
		errs = append(errs, validateHostRequirementFormatStrings(hr, base)...)
	}

	return errs
}

// validateHostRequirementFormatStrings checks that every amount min/max and
// attribute anyOf/allOf value is a well-scoped format string (ScopeJob --
// host requirements are resolved at submission time, before any session or
// task exists). Split from [validateHostRequirements] to keep that function's
// cyclomatic complexity within bounds.
//
// This is a new check as of sub-project E2's Task 9: host requirements had NO
// format-string scope validation before it, so a reference like
// {{Session.WorkingDirectory}} here was accepted and resolved to nothing at
// run time. The caller skips this entirely when the template declares EXPR;
// checkTemplateExpressions covers the same position with the real evaluator
// instead. Called unconditionally regardless of whether a value looks like a
// bare number: validateFormatString is a no-op on a literal with no "{{"
// reference.
func validateHostRequirementFormatStrings(hr HostRequirements, base string) ValidationErrors {
	var errs ValidationErrors
	for i, a := range hr.Amounts {
		amtPtr := fmt.Sprintf("%s/amounts/%d", base, i)
		if a.Min != nil {
			errs = append(errs, validateFormatString(*a.Min, amtPtr+"/min", ScopeJob, nil)...)
		}
		if a.Max != nil {
			errs = append(errs, validateFormatString(*a.Max, amtPtr+"/max", ScopeJob, nil)...)
		}
	}
	for i, a := range hr.Attributes {
		attrPtr := fmt.Sprintf("%s/attributes/%d", base, i)
		for k, v := range a.AnyOf {
			errs = append(errs, validateFormatString(v, fmt.Sprintf("%s/anyOf/%d", attrPtr, k), ScopeJob, nil)...)
		}
		for k, v := range a.AllOf {
			errs = append(errs, validateFormatString(v, fmt.Sprintf("%s/allOf/%d", attrPtr, k), ScopeJob, nil)...)
		}
	}
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
		errs = append(errs, validateCheckBoxValues(p, ctrlPtr)...)
	case ControlLineEdit, ControlMultilineEdit, ControlSpinBox:
		// DROPDOWN_LIST is the control for a fixed value set; a free-entry or
		// numeric-stepper control paired with allowedValues is contradictory.
		if len(p.AllowedValues) > 0 {
			errs = append(errs, ValidationError{
				Pointer: ctrlPtr,
				Message: fmt.Sprintf("%s cannot be used when allowedValues is provided; use DROPDOWN_LIST", ui.Control),
			})
		}
	case ControlChooseInputFile, ControlChooseOutputFile, ControlChooseDirectory:
		errs = append(errs, validateChooseControl(ui, p, ctrlPtr)...)
	}
	return errs
}

// validateParamValueConstraints checks that a parameter's own declared
// constraints are satisfied by the values it ships. A template that declares
// allowedValues or a min/max bound and then supplies a default outside it is
// self-contradictory: the default can never be accepted at submission, so the
// parameter is unusable without an explicit value. Structural — always runs.
//
// Covers, per the parameter definitions in §2.1-2.4:
//   - default must be one of allowedValues when that list is given
//   - default and every allowedValues entry must satisfy minLength/maxLength
//     (STRING, PATH) or minValue/maxValue (INT, FLOAT)
//   - an INT value must actually be an integer, and an INT/FLOAT value a number
func validateParamValueConstraints(p JobParameter, ptr string) ValidationErrors {
	var errs ValidationErrors

	if p.Default != nil && len(p.AllowedValues) > 0 &&
		!strings.Contains(*p.Default, "{{") &&
		!slices.Contains(p.AllowedValues, *p.Default) {
		errs = append(errs, ValidationError{
			Pointer: ptr + "/default",
			Message: fmt.Sprintf("default %q is not one of allowedValues", *p.Default),
		})
	}

	if p.Default != nil {
		errs = append(errs, validateParamValueInBounds(p, *p.Default, ptr+"/default")...)
	}
	for i, v := range p.AllowedValues {
		errs = append(errs, validateParamValueInBounds(p, v, fmt.Sprintf("%s/allowedValues/%d", ptr, i))...)
	}
	return errs
}

// validateParamValueInBounds checks one concrete value against the parameter's
// declared bounds. Values carrying an unresolved format-string reference are
// skipped: they cannot be evaluated before job-parameter binding.
func validateParamValueInBounds(p JobParameter, v, ptr string) ValidationErrors {
	if strings.Contains(v, "{{") {
		return nil
	}
	var errs ValidationErrors
	switch p.Type {
	case JobParamTypeString, JobParamTypePath:
		n := utf8.RuneCountInString(v)
		// <JobParameterStringValue> is capped at 1024 characters (spec §2.5)
		// regardless of any maxLength the parameter declares.
		if n > maxJobParamStringValueLen {
			errs = append(errs, ValidationError{
				Pointer: ptr,
				Message: fmt.Sprintf("must be at most %d characters (got %d)", maxJobParamStringValueLen, n),
			})
		}
		if p.MinLength != nil && n < *p.MinLength {
			errs = append(errs, ValidationError{
				Pointer: ptr,
				Message: fmt.Sprintf("value is shorter than minLength %d (got %d)", *p.MinLength, n),
			})
		}
		if p.MaxLength != nil && n > *p.MaxLength {
			errs = append(errs, ValidationError{
				Pointer: ptr,
				Message: fmt.Sprintf("value is longer than maxLength %d (got %d)", *p.MaxLength, n),
			})
		}
	case JobParamTypeInt, JobParamTypeFloat:
		errs = append(errs, validateNumericValueInBounds(p, v, ptr)...)
	}
	return errs
}

// validateNumericValueInBounds is the INT/FLOAT half of
// [validateParamValueInBounds], split out to keep the switch's complexity down.
func validateNumericValueInBounds(p JobParameter, v, ptr string) ValidationErrors {
	if p.Type == JobParamTypeInt {
		if _, err := strconv.ParseInt(v, 10, 64); err != nil {
			return ValidationErrors{{
				Pointer: ptr,
				Message: fmt.Sprintf("value %q is not a valid integer", v),
			}}
		}
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return ValidationErrors{{
			Pointer: ptr,
			Message: fmt.Sprintf("value %q is not a valid number", v),
		}}
	}

	var errs ValidationErrors
	if lo, ok := parseNumericBound(p.MinValue); ok && f < lo {
		errs = append(errs, ValidationError{
			Pointer: ptr,
			Message: fmt.Sprintf("value %s is below minValue %s", v, *p.MinValue),
		})
	}
	if hi, ok := parseNumericBound(p.MaxValue); ok && f > hi {
		errs = append(errs, ValidationError{
			Pointer: ptr,
			Message: fmt.Sprintf("value %s is above maxValue %s", v, *p.MaxValue),
		})
	}
	return errs
}

// parseNumericBound reads a *string bound as a float, reporting false when it
// is absent, unresolved, or unparseable — the bound itself is validated
// elsewhere, so this only decides whether a comparison is possible.
func parseNumericBound(b *string) (float64, bool) {
	if b == nil || strings.Contains(*b, "{{") {
		return 0, false
	}
	f, err := strconv.ParseFloat(*b, 64)
	return f, err == nil
}

const (
	// maxIdentifierLen caps an <Identifier> (spec §7.1: 64 characters).
	maxIdentifierLen = 64
	// maxDescriptionLen caps a <Description> (spec §7.2: 2048 characters).
	maxDescriptionLen = 2048
	// maxJobParamStringValueLen caps a <JobParameterStringValue> (spec §2.5).
	maxJobParamStringValueLen = 1024
	// maxEmbeddedFilenameLen caps an embedded file's filename (spec §6.1.1: 64).
	maxEmbeddedFilenameLen = 64
)

// validateFormatString checks one format string: that its interpolation
// expressions parse, and that every reference is in scope where it appears.
//
// The legal reference PREFIXES for scope are derived from the shared Scope
// declaration (scope.go) rather than hand-maintained here (spec §7.3.1).
// Matching is exact-case because value references are case-sensitive —
// "param.X" is not "Param.X".
//
// A reference that is well-formed but out of scope resolves to nothing at run
// time, so the command silently receives an empty value rather than failing —
// which is why this is structural correctness and always runs.
func validateFormatString(s, ptr string, scope Scope, files map[string]struct{}) ValidationErrors {
	allowed := derivedPrefixes(scope)
	refs, err := fmtstring.References(s)
	if err != nil {
		return ValidationErrors{{
			Pointer: ptr,
			Message: fmt.Sprintf("malformed format string: %v", err),
		}}
	}
	var errs ValidationErrors
	for _, r := range refs {
		if !hasAnyPrefix(r, allowed) {
			errs = append(errs, ValidationError{
				Pointer: ptr,
				Message: fmt.Sprintf("reference %q is not available here; allowed: %s",
					r, strings.Join(allowed, ", ")),
			})
			continue
		}
		errs = append(errs, checkFileRef(r, ptr, files)...)
	}
	return errs
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// requireNonEmptyIfSet reports an error when an optional list was declared in
// the template but holds no entries. Several schema fields share this shape:
// omitting them is legal, but declaring an empty one is not.
func requireNonEmptyIfSet(set bool, n int, ptr, noun string) ValidationErrors {
	if !set || n > 0 {
		return nil
	}
	return ValidationErrors{{
		Pointer: ptr,
		Message: fmt.Sprintf("must contain at least one %s when provided", noun),
	}}
}

// validateNoControlChars rejects Unicode Cc control characters in a name-like
// string. The category covers both C0 (U+0000-U+001F, U+007F) and C1
// (U+0080-U+009F), so it catches the whole range the spec excludes. A control
// character in a name corrupts logs, terminal output, and any UI rendering it.
func validateNoControlChars(v, ptr string) ValidationErrors {
	for _, r := range v {
		if unicode.IsControl(r) {
			return ValidationErrors{{
				Pointer: ptr,
				Message: fmt.Sprintf("must not contain control characters (found U+%04X)", r),
			}}
		}
	}
	return nil
}

// validateDescriptionText checks a <Description> (spec §7.2): at most 2048
// characters, and no Cc control characters EXCEPT newline, carriage return, and
// horizontal tab, which a description is explicitly allowed to contain.
func validateDescriptionText(v, ptr string) ValidationErrors {
	if v == "" {
		return nil
	}
	if n := utf8.RuneCountInString(v); n > maxDescriptionLen {
		return ValidationErrors{{
			Pointer: ptr,
			Message: fmt.Sprintf("must be at most %d characters (got %d)", maxDescriptionLen, n),
		}}
	}
	for _, r := range v {
		if r == '\n' || r == '\r' || r == '\t' {
			continue
		}
		if unicode.IsControl(r) {
			return ValidationErrors{{
				Pointer: ptr,
				Message: fmt.Sprintf("must not contain control characters other than newline, carriage return, or tab (found U+%04X)", r),
			}}
		}
	}
	return nil
}

// validateIdentifierLen caps an <Identifier> at [maxIdentifierLen] characters.
// The character set itself is enforced by identifierRE at each use site.
func validateIdentifierLen(v, ptr string) ValidationErrors {
	if n := utf8.RuneCountInString(v); n > maxIdentifierLen {
		return ValidationErrors{{
			Pointer: ptr,
			Message: fmt.Sprintf("identifier must be at most %d characters (got %d)", maxIdentifierLen, n),
		}}
	}
	return nil
}

// validateSingleStepDelta checks a userInterface.singleStepDelta: the amount a
// SPIN_BOX moves per step. It is meaningful only on a SPIN_BOX, must be a
// positive number (a zero or negative step cannot advance the control), and on
// an INT parameter must itself be an integer — a fractional step on an integer
// field can never land on a legal value.
func validateSingleStepDelta(p JobParameter, ui *ParameterUserInterface, ptr string) ValidationErrors {
	if ui.SingleStepDelta == nil {
		return nil
	}
	v := *ui.SingleStepDelta
	if ui.Control != ControlSpinBox {
		return ValidationErrors{{
			Pointer: ptr,
			Message: fmt.Sprintf("singleStepDelta is valid only with the SPIN_BOX control (got %q)", ui.Control),
		}}
	}
	if strings.Contains(v, "{{") {
		return nil
	}
	if p.Type == JobParamTypeInt {
		if _, err := strconv.ParseInt(v, 10, 64); err != nil {
			return ValidationErrors{{
				Pointer: ptr,
				Message: fmt.Sprintf("singleStepDelta %q must be an integer on an INT parameter", v),
			}}
		}
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return ValidationErrors{{
			Pointer: ptr,
			Message: fmt.Sprintf("singleStepDelta %q is not a valid number", v),
		}}
	}
	if f <= 0 {
		return ValidationErrors{{
			Pointer: ptr,
			Message: fmt.Sprintf("singleStepDelta must be positive (got %s)", v),
		}}
	}
	return nil
}

// checkBoxValuePairs are the allowedValues pairs a CHECK_BOX may carry. The
// spec (§2.1.9.1) requires "two values, case-insensitive, one representing true
// and another representing false", and enumerates the valid pairs. Either order
// is accepted — the spec names no ordering requirement.
var checkBoxValuePairs = [][2]string{
	{"true", "false"}, {"yes", "no"}, {"on", "off"}, {"1", "0"},
}

// validateCheckBoxValues checks that a CHECK_BOX parameter's allowedValues are
// one of the spec's true/false pairs. Two arbitrary strings are not enough: a
// checkbox has to know which value means checked.
func validateCheckBoxValues(p JobParameter, ctrlPtr string) ValidationErrors {
	if len(p.AllowedValues) != 2 {
		return ValidationErrors{{Pointer: ctrlPtr, Message: "CHECK_BOX requires exactly two allowedValues"}}
	}
	a, b := strings.ToLower(p.AllowedValues[0]), strings.ToLower(p.AllowedValues[1])
	for _, pair := range checkBoxValuePairs {
		if (a == pair[0] && b == pair[1]) || (a == pair[1] && b == pair[0]) {
			return nil
		}
	}
	return ValidationErrors{{
		Pointer: ctrlPtr,
		Message: fmt.Sprintf(
			"CHECK_BOX allowedValues %v must be a true/false pair: [true false], [yes no], [on off], or [1 0]",
			p.AllowedValues,
		),
	}}
}

// validateUILabelText checks a declared userInterface label or groupLabel. Both
// fields are optional, but a declared one must carry real text: an empty string
// renders a nameless control, and a control character or line break corrupts the
// layout of any form built from the template.
//
// set reports whether the key was present at all, which the decoder tracks
// separately — the string alone cannot distinguish a missing label from
// `label: ""`.
func validateUILabelText(v string, set bool, ptr string) ValidationErrors {
	if !set {
		return nil
	}
	if v == "" {
		return ValidationErrors{{Pointer: ptr, Message: "must not be empty when provided"}}
	}
	return validateNoControlChars(v, ptr)
}

// validateLengthConstraintSanity checks that a parameter's own minLength and
// maxLength are usable: a negative bound is meaningless, and maxLength 0 (or
// below minLength) admits no value at all, making the parameter unsatisfiable.
func validateLengthConstraintSanity(p JobParameter, ptr string) ValidationErrors {
	var errs ValidationErrors
	if p.MinLength != nil && *p.MinLength < 0 {
		errs = append(errs, ValidationError{
			Pointer: ptr + "/minLength",
			Message: fmt.Sprintf("must not be negative (got %d)", *p.MinLength),
		})
	}
	if p.MaxLength != nil && *p.MaxLength < 1 {
		errs = append(errs, ValidationError{
			Pointer: ptr + "/maxLength",
			Message: fmt.Sprintf("must be at least 1 (got %d)", *p.MaxLength),
		})
	}
	if p.MinLength != nil && p.MaxLength != nil && *p.MinLength > *p.MaxLength {
		errs = append(errs, ValidationError{
			Pointer: ptr + "/minLength",
			Message: fmt.Sprintf("minLength %d exceeds maxLength %d", *p.MinLength, *p.MaxLength),
		})
	}
	return errs
}

// validateChooseControl checks the two coherence rules the spec states for the
// PATH parameter's CHOOSE_* controls (§2.2.9.1):
//
//   - None of them may be combined with allowedValues — each is documented
//     "Cannot be used when allowedValues is provided"; DROPDOWN_LIST is the
//     control for a fixed value set.
//   - The control must agree with objectType. The spec derives the DEFAULT
//     control from objectType (FILE picks a CHOOSE_*_FILE variant, otherwise
//     CHOOSE_DIRECTORY), and the conformance suite requires rejecting an
//     explicit control that contradicts it — a file dialog cannot pick a
//     directory. objectType defaults to DIRECTORY when absent (§2.2.7).
func validateChooseControl(ui *ParameterUserInterface, p JobParameter, ctrlPtr string) ValidationErrors {
	var errs ValidationErrors

	if len(p.AllowedValues) > 0 {
		errs = append(errs, ValidationError{
			Pointer: ctrlPtr,
			Message: fmt.Sprintf("%s cannot be used when allowedValues is provided; use DROPDOWN_LIST", ui.Control),
		})
	}

	objectType := p.ObjectType
	if objectType == "" {
		objectType = PathObjectTypeDirectory // spec default
	}
	wantFile := ui.Control == ControlChooseInputFile || ui.Control == ControlChooseOutputFile
	if wantFile && objectType != PathObjectTypeFile {
		errs = append(errs, ValidationError{
			Pointer: ctrlPtr,
			Message: fmt.Sprintf("%s requires objectType FILE (got %s)", ui.Control, objectType),
		})
	}
	if !wantFile && objectType != PathObjectTypeDirectory {
		errs = append(errs, ValidationError{
			Pointer: ctrlPtr,
			Message: fmt.Sprintf("%s requires objectType DIRECTORY (got %s)", ui.Control, objectType),
		})
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

		errs = append(errs, validateIdentifierLen(p.Name, ptr+"/name")...)
		errs = append(errs, validateDescriptionText(p.Description, ptr+"/description")...)
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

		errs = append(errs, validateUserInterface(p, ptr)...)
		errs = append(errs, validateParamValueConstraints(p, ptr)...)
		errs = append(errs, validateLengthConstraintSanity(p, ptr)...)
		errs = append(errs, requireNonEmptyIfSet(p.AllowedValuesSet, len(p.AllowedValues),
			ptr+"/allowedValues", "value")...)
		if ui := p.UserInterface; ui != nil {
			errs = append(errs, validateUILabelText(ui.Label, ui.LabelSet, ptr+"/userInterface/label")...)
			errs = append(errs, validateUILabelText(ui.GroupLabel, ui.GroupLabelSet, ptr+"/userInterface/groupLabel")...)
			errs = append(errs, validateSingleStepDelta(p, ui, ptr+"/userInterface/singleStepDelta")...)
		}

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
func validateAction(a Action, ptr string, scope Scope, files map[string]struct{}, exprDeclared bool) ValidationErrors {
	if a.Command == "" {
		return ValidationErrors{{
			Pointer: ptr + "/command",
			Message: "required; must be at least 1 character",
		}}
	}
	// A command or argument carrying a tab or line break cannot survive the
	// round trip to an OS process argv intact. (The EXPR extension relaxes this
	// for multi-line expressions; sqi does not implement EXPR.)
	errs := validateNoControlChars(a.Command, ptr+"/command")
	for i, arg := range a.Args {
		errs = append(errs, validateNoControlChars(arg, fmt.Sprintf("%s/args/%d", ptr, i))...)
	}
	if !exprDeclared {
		errs = append(errs, validateActionRefs(a, ptr, scope, files)...)
	}
	errs = append(errs, validateActionTiming(a, ptr)...)
	// args is @optional, but a declared list must hold at least one argument —
	// an empty array says "pass arguments" and then passes none.
	errs = append(errs, requireNonEmptyIfSet(a.ArgsSet, len(a.Args), ptr+"/args", "argument")...)
	return errs
}

// maxNotifyPeriodSeconds caps <CancelationMethodNotifyThenTerminate>'s
// notifyPeriodInSeconds (spec §5.3.2: maximum value 600).
const maxNotifyPeriodSeconds = 600

// validateActionTiming checks an action's timeout and cancelation block. Both
// numbers are <posinteger> in the spec, so an explicit zero or negative value
// is invalid — a zero timeout would cancel the action before it could run, and
// a zero notify period leaves no interval between the two signals.
func validateActionTiming(a Action, ptr string) ValidationErrors {
	var errs ValidationErrors
	if a.TimeoutSet && a.TimeoutSeconds < 1 {
		errs = append(errs, ValidationError{
			Pointer: ptr + "/timeout",
			Message: fmt.Sprintf("must be a positive number of seconds (got %d)", a.TimeoutSeconds),
		})
	}
	c := a.Cancelation
	if c == nil {
		return errs
	}
	switch c.Mode {
	case CancelModeTerminate, CancelModeNotifyThenTerminate, "":
		// "" means the key was absent; the spec default is TERMINATE.
	default:
		errs = append(errs, ValidationError{
			Pointer: ptr + "/cancelation/mode",
			Message: fmt.Sprintf("must be TERMINATE or NOTIFY_THEN_TERMINATE (got %q)", c.Mode),
		})
	}
	if c.NotifyPeriodSet {
		np := ptr + "/cancelation/notifyPeriodInSeconds"
		if c.NotifyPeriodSeconds < 1 {
			errs = append(errs, ValidationError{
				Pointer: np,
				Message: fmt.Sprintf("must be a positive number of seconds (got %d)", c.NotifyPeriodSeconds),
			})
		} else if c.NotifyPeriodSeconds > maxNotifyPeriodSeconds {
			errs = append(errs, ValidationError{
				Pointer: np,
				Message: fmt.Sprintf("must be at most %d seconds (got %d)", maxNotifyPeriodSeconds, c.NotifyPeriodSeconds),
			})
		}
	}
	return errs
}

// validateScriptRefs checks the format strings a script carries outside its
// actions: every embedded file's data, and — for an environment — every
// variable value. All three are resolved through the same scope split at run
// time (internal/worker/fmtres), so a reference that is out of scope here
// resolves to nothing on the worker and the action silently receives an empty
// value rather than failing.
//
// vars may be nil for a step script, which has no variables of its own.
// scriptBase points at the script object; varsBase at the object holding
// variables — on an Environment those are siblings, not nested.
func validateScriptRefs(files []EmbeddedFile, vars map[string]string, scope Scope, scriptBase, varsBase string, exprDeclared bool) ValidationErrors {
	if exprDeclared {
		// checkTemplateExpressions covers this position with the real
		// evaluator when EXPR is declared; the base-spec dotted-identifier
		// path below would misread every non-trivial expression as malformed.
		return nil
	}
	var errs ValidationErrors
	declared := embeddedFileNames(files)
	for i, f := range files {
		errs = append(errs, validateFormatString(f.Data,
			fmt.Sprintf("%s/embeddedFiles/%d/data", scriptBase, i), scope, declared)...)
	}
	// Sorted so the errors a template produces do not depend on map order.
	for _, k := range slices.Sorted(maps.Keys(vars)) {
		errs = append(errs, validateFormatString(vars[k], varsBase+"/variables/"+k, scope, declared)...)
	}
	return errs
}

// validateActionRefs checks the format strings in an action's command and args
// against the reference scope legal at its site. Embedded-file data and
// environment variable values are covered by [validateScriptRefs].
func validateActionRefs(a Action, ptr string, scope Scope, files map[string]struct{}) ValidationErrors {
	errs := validateFormatString(a.Command, ptr+"/command", scope, files)
	for i, arg := range a.Args {
		errs = append(errs, validateFormatString(arg, fmt.Sprintf("%s/args/%d", ptr, i), scope, files)...)
	}
	return errs
}

// embeddedFileNames indexes a script's embedded files by name, for resolving
// Task.File / Env.File references.
func embeddedFileNames(files []EmbeddedFile) map[string]struct{} {
	out := make(map[string]struct{}, len(files))
	for _, f := range files {
		out[f.Name] = struct{}{}
	}
	return out
}

// checkFileRef reports a Task.File/Env.File reference naming an embedded file
// the script never declares. Such a reference resolves to nothing at run time,
// so the action silently receives an empty path instead of failing.
func checkFileRef(ref, ptr string, files map[string]struct{}) ValidationErrors {
	for _, prefix := range []string{"Task.File.", "Env.File."} {
		name, ok := strings.CutPrefix(ref, prefix)
		if !ok {
			continue
		}
		if _, declared := files[name]; !declared {
			return ValidationErrors{{
				Pointer: ptr,
				Message: fmt.Sprintf("reference %q names no embedded file declared by this script", ref),
			}}
		}
	}
	return nil
}

// ─── environment validation ───────────────────────────────────────────────────

func validateEnvironments(envs []Environment, base string, scope Scope, exprDeclared bool) ValidationErrors {
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
		// Variables are checked even when the environment declares no script:
		// they are set for the session regardless.
		var envScriptFiles []EmbeddedFile
		if e.Script != nil {
			envScriptFiles = e.Script.EmbeddedFiles
		}
		errs = append(errs, validateScriptRefs(envScriptFiles, e.Variables, scope, ptr+"/script", ptr, exprDeclared)...)

		if e.Script != nil {
			envFiles := embeddedFileNames(e.Script.EmbeddedFiles)
			if e.Script.Actions.OnEnter == nil {
				errs = append(errs, ValidationError{
					Pointer: ptr + "/script/actions/onEnter",
					Message: "required",
				})
			} else {
				errs = append(errs, validateAction(*e.Script.Actions.OnEnter, ptr+"/script/actions/onEnter", scope, envFiles, exprDeclared)...)
			}
			if e.Script.Actions.OnExit != nil {
				errs = append(errs, validateAction(*e.Script.Actions.OnExit, ptr+"/script/actions/onExit", scope, envFiles, exprDeclared)...)
			}
			errs = append(errs, validateEmbeddedFiles(e.Script.EmbeddedFiles, e.Script.EmbeddedFilesSet, ptr+"/script/embeddedFiles")...)
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
func validateEmbeddedFiles(files []EmbeddedFile, set bool, ptr string) ValidationErrors {
	var errs ValidationErrors
	// embeddedFiles is @optional, but a declared list must hold at least one
	// file — an empty array declares attachments and supplies none.
	errs = append(errs, requireNonEmptyIfSet(set, len(files), ptr, "embedded file")...)
	seen := make(map[string]struct{}, len(files))
	for j, f := range files {
		errs = append(errs, validateEmbeddedFileEntry(f, fmt.Sprintf("%s/%d", ptr, j), seen)...)
	}
	return errs
}

// validateEmbeddedFileEntry checks one embedded file, recording its name in
// seen so a later duplicate is caught. Split from [validateEmbeddedFiles] to
// keep that function's cyclomatic complexity within bounds.
func validateEmbeddedFileEntry(f EmbeddedFile, base string, seen map[string]struct{}) ValidationErrors {
	var errs ValidationErrors
	typePtr := base + "/type"
	// name — an <Identifier>, and the key that Task.File/Env.File
	// references resolve against, so it must be unique within the script.
	switch {
	case f.Name == "":
		errs = append(errs, ValidationError{Pointer: base + "/name", Message: "required"})
	case !identifierRE.MatchString(f.Name):
		errs = append(errs, ValidationError{
			Pointer: base + "/name",
			Message: fmt.Sprintf("invalid identifier %q; must match [A-Za-z_][A-Za-z0-9_]*", f.Name),
		})
	default:
		errs = append(errs, validateIdentifierLen(f.Name, base+"/name")...)
		if _, dup := seen[f.Name]; dup {
			errs = append(errs, ValidationError{
				Pointer: base + "/name",
				Message: fmt.Sprintf("duplicate embedded file name %q", f.Name),
			})
		}
		seen[f.Name] = struct{}{}
	}

	// data — a <DataString>, minimum length 1. An embedded file with no
	// content writes an empty file the action then tries to run.
	if f.Data == "" {
		errs = append(errs, ValidationError{Pointer: base + "/data", Message: "required; must be at least 1 character"})
	}

	// filename — optional, but a declared one is a bare basename of at
	// most 64 characters. A path here would write outside the session dir.
	if f.FilenameSet && f.Filename == "" {
		errs = append(errs, ValidationError{
			Pointer: base + "/filename",
			Message: "must not be empty when provided",
		})
	}
	if f.Filename != "" {
		if n := utf8.RuneCountInString(f.Filename); n > maxEmbeddedFilenameLen {
			errs = append(errs, ValidationError{
				Pointer: base + "/filename",
				Message: fmt.Sprintf("must be at most %d characters (got %d)", maxEmbeddedFilenameLen, n),
			})
		}
		if strings.ContainsAny(f.Filename, `/\`) {
			errs = append(errs, ValidationError{
				Pointer: base + "/filename",
				Message: "must be a bare filename with no directory path",
			})
		}
	}
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
	return errs
}

// ─── step validation ──────────────────────────────────────────────────────────

// validateStepEnvCollisions reports a step environment that reuses a job
// environment's name. Both are entered for the same session, so the collision
// makes it ambiguous which one an Env.File reference or variable belongs to.
func validateStepEnvCollisions(s StepTemplate, idx int, jobEnvs []Environment) ValidationErrors {
	var errs ValidationErrors
	for k, se := range s.StepEnvironments {
		if se.Name == "" {
			continue
		}
		for _, je := range jobEnvs {
			if se.Name == je.Name {
				errs = append(errs, ValidationError{
					Pointer: fmt.Sprintf("/steps/%d/stepEnvironments/%d/name", idx, k),
					Message: fmt.Sprintf("collides with a job environment named %q", se.Name),
				})
			}
		}
	}
	return errs
}

func validateStep(s StepTemplate, idx int, stepNames map[string]struct{}, exprDeclared bool) ValidationErrors {
	var errs ValidationErrors
	base := fmt.Sprintf("/steps/%d", idx)

	// dependencies — @optional, but a declared list must hold at least one.
	errs = append(errs, requireNonEmptyIfSet(s.DependenciesSet, len(s.Dependencies),
		base+"/dependencies", "dependency")...)
	errs = append(errs, requireNonEmptyIfSet(s.StepEnvironmentsSet, len(s.StepEnvironments),
		base+"/stepEnvironments", "environment")...)
	seenDeps := make(map[string]struct{}, len(s.Dependencies))
	for j, dep := range s.Dependencies {
		ptr := fmt.Sprintf("%s/dependencies/%d/dependsOn", base, j)
		if _, dup := seenDeps[dep.DependsOn]; dup && dep.DependsOn != "" {
			errs = append(errs, ValidationError{
				Pointer: ptr,
				Message: fmt.Sprintf("duplicate dependency on step %q", dep.DependsOn),
			})
		}
		seenDeps[dep.DependsOn] = struct{}{}
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
		errs = append(errs, validateEmbeddedFiles(s.Script.EmbeddedFiles, s.Script.EmbeddedFilesSet, base+"/script/embeddedFiles")...)
		errs = append(errs, validateScriptRefs(s.Script.EmbeddedFiles, nil, ScopeStepScript, base+"/script", base, exprDeclared)...)
		errs = append(errs, validateAction(s.Script.Actions.OnRun, base+"/script/actions/onRun", ScopeStepScript, embeddedFileNames(s.Script.EmbeddedFiles), exprDeclared)...)
	}

	// step environments
	errs = append(errs, validateEnvironments(s.StepEnvironments, base+"/stepEnvironments", ScopeStepEnvironment, exprDeclared)...)

	// parameter space
	if s.ParameterSpace != nil {
		errs = append(errs, validateParameterSpace(*s.ParameterSpace, base+"/parameterSpace", exprDeclared)...)
	}

	// host requirements (structural; the size caps stay in validateLimits)
	if s.HostRequirements != nil {
		errs = append(errs, validateHostRequirements(*s.HostRequirements, base+"/hostRequirements", exprDeclared)...)
	}

	return errs
}

// ─── parameter space validation ───────────────────────────────────────────────

func validateParameterSpace(ps StepParameterSpace, base string, exprDeclared bool) ValidationErrors {
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
		errs = append(errs, validateTaskParam(tp, ptr, paramNames, exprDeclared)...)
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

func validateTaskParam(tp TaskParamDefinition, base string, seen map[string]struct{}, exprDeclared bool) ValidationErrors {
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

	errs = append(errs, validateTaskParamRangeAndChunks(tp, base, exprDeclared)...)

	return errs
}

// validateRangeListValues checks each literal value in a task parameter's
// range against the parameter's type. An INT range holding a float, or a PATH
// range holding an empty string, expands into a task whose parameter can never
// be used. Split from [validateTaskParamRangeAndChunks] to keep its cyclomatic
// complexity within bounds.
//
// It also checks that each entry is a well-scoped format string (ScopeJob --
// task parameters do not exist yet while their own range is being defined) --
// a new check as of sub-project E2's Task 9; a range entry had no
// format-string scope validation before this. Skipped when exprDeclared:
// checkTemplateExpressions covers this position with the real evaluator
// instead.
//
// RangeExpr (the whole-field alternative form) is checked too, but NOT here:
// [validateTaskParamRangeAndChunks], this function's caller, covers it on the
// base-spec path, and [checkParameterSpaceExpressions] (exprcheck.go) covers
// it on the EXPR path. Both were added by Task 9's own fix round. The EXPR
// side deliberately checks it with expr.TAny rather than a target derived from
// the parameter's declared type, because under EXPR a RangeExpr may be a
// list-valued expression (section 1.3.11) rather than a plain string -- the
// expr1.3.11--*-range-expression.yaml fixtures legitimately evaluate to
// list[float]/list[path]/list[string], and a fixed string target would reject
// them. See those two sites for the full reasoning.
func validateRangeListValues(tp TaskParamDefinition, base string, exprDeclared bool) ValidationErrors {
	var errs ValidationErrors
	for i, v := range tp.RangeList {
		vptr := fmt.Sprintf("%s/range/%d", base, i)
		if !exprDeclared {
			errs = append(errs, validateFormatString(v, vptr, ScopeJob, nil)...)
		}
		if strings.Contains(v, "{{") {
			continue
		}
		switch tp.Type {
		case TaskParamTypeInt, TaskParamTypeChunkInt:
			if _, err := strconv.ParseInt(v, 10, 64); err != nil {
				errs = append(errs, ValidationError{
					Pointer: vptr,
					Message: fmt.Sprintf("value %q is not a valid integer", v),
				})
			}
		case TaskParamTypeFloat:
			if _, err := strconv.ParseFloat(v, 64); err != nil {
				errs = append(errs, ValidationError{
					Pointer: vptr,
					Message: fmt.Sprintf("value %q is not a valid number", v),
				})
			}
		case TaskParamTypePath:
			// PATH only: an empty STRING range value is legal (conformance
			// fixture 3.4.2--empty-string-value.yaml), but an empty path names
			// no file.
			if v == "" {
				errs = append(errs, ValidationError{Pointer: vptr, Message: "path value must not be empty"})
			}
		}
	}
	return errs
}

// validateTaskParamRangeAndChunks validates the range field and, for CHUNK[INT]
// parameters, the chunks definition. It is extracted from [validateTaskParam]
// to keep that function's cyclomatic complexity within bounds.
func validateTaskParamRangeAndChunks(tp TaskParamDefinition, base string, exprDeclared bool) ValidationErrors {
	var errs ValidationErrors

	// Range must be present
	if tp.RangeExpr == nil && len(tp.RangeList) == 0 {
		errs = append(errs, ValidationError{Pointer: base + "/range", Message: "required"})
	}

	errs = append(errs, validateRangeListValues(tp, base, exprDeclared)...)

	// RangeExpr (the whole-field alternative to RangeList) is ALSO a
	// format-string position, and previously had NO scope check at all: a
	// base-spec template with range: "{{Session.WorkingDirectory}}" validated
	// with zero errors and resolved to nothing at run time -- the same hole
	// this task exists to close, at the same field RangeList already closed
	// it for. validateFormatString is prefix-only and type-blind (it has no
	// concept of a target type to get wrong), so this costs nothing and is
	// safe for the common case too: sqi's own preset templates all use
	// RangeExpr for a single job-parameter reference
	// ("{{Param.Frames}}"/"{{Param.FrameRange}}"), in scope at ScopeJob.
	// (checkParameterSpaceExpressions in exprcheck.go covers the EXPR-declared
	// counterpart with expr.TAny rather than a fixed target, for a reason
	// specific to that path -- see its doc comment.)
	if tp.RangeExpr != nil && !exprDeclared {
		errs = append(errs, validateFormatString(*tp.RangeExpr, base+"/range", ScopeJob, nil)...)
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

	if tp.Chunks != nil && tp.Chunks.TargetRuntimeSeconds != nil && *tp.Chunks.TargetRuntimeSeconds < 1 {
		errs = append(errs, ValidationError{
			Pointer: base + "/chunks/targetRuntimeSeconds",
			Message: fmt.Sprintf("must be a positive number of seconds (got %d)", *tp.Chunks.TargetRuntimeSeconds),
		})
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

	seenNames := make(map[string]struct{}, len(names))
	for _, name := range names {
		if _, ok := paramNames[name]; !ok {
			errs = append(errs, ValidationError{
				Pointer: ptr,
				Message: fmt.Sprintf("references undeclared parameter %q", name),
			})
		}
		// Each parameter contributes its range to the space exactly once;
		// referencing one twice ("Frame * Frame") has no defined meaning.
		if _, dup := seenNames[name]; dup {
			errs = append(errs, ValidationError{
				Pointer: ptr,
				Message: fmt.Sprintf("references parameter %q more than once", name),
			})
		}
		seenNames[name] = struct{}{}
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
