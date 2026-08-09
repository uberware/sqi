// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import "github.com/uberware/sqi/internal/openjd/expr"

// Scope identifies the symbol environment of a template position: which
// symbols exist there, and whether the position runs on a worker host.
//
// This is the SINGLE declaration of scope in the package. It has two consumers,
// which is the whole point:
//
//   - The EXPR path builds a typed symbol table from it, so a reference to a
//     symbol the scope does not expose fails as an unknown symbol from the
//     evaluator rather than from a second, parallel mechanism.
//   - The base-spec path keeps prefix-matching, against prefixes DERIVED from
//     this declaration (derivedPrefixes) rather than a hand-maintained list.
//
// Before E2 the base-spec prefixes were three literals in validate.go. Keeping
// them AND an EXPR scope model would put scope knowledge in two places, which
// is the shape of a drift this program has already been bitten by once.
type Scope int

const (
	// ScopeJob is a submission-time position with no session or task context:
	// the job's own name, host requirements, and a step's parameter space.
	ScopeJob Scope = iota
	// ScopeJobEnvironment is a job-level environment's script and variables.
	ScopeJobEnvironment
	// ScopeStepEnvironment is a step-level environment's script and variables.
	// It differs from ScopeJobEnvironment by exactly one symbol, Step.Name,
	// which is EXPR-only -- see derivedPrefixes.
	ScopeStepEnvironment
	// ScopeStepScript is a step script's actions and embedded files.
	ScopeStepScript
	// ScopeStepTemplate is a step template's own let: block (Template Schemas
	// section 3.6.2 row 1), evaluated at submission before any host exists.
	// It is not derivable from any other scope: ScopeJob is the closest
	// (also submission-time) but exposes neither Job.Name nor Step.Name,
	// while this scope needs both -- and unlike every host-context scope it
	// gets no Session.*, no Task.* and no Env.File.*. Appended at the end so
	// the four scopes above keep their existing values.
	ScopeStepTemplate
)

func (s Scope) String() string {
	switch s {
	case ScopeJob:
		return "job"
	case ScopeJobEnvironment:
		return "job environment"
	case ScopeStepEnvironment:
		return "step environment"
	case ScopeStepScript:
		return "step script"
	case ScopeStepTemplate:
		return "step template"
	}
	return "unknown scope"
}

// IsHostContext reports whether expressions in this scope are evaluated on a
// worker host, where session state exists.
//
// It gates the host-context-only functions. The specification does NOT commit
// to a single such function: its FunctionLibrary.with_host_context() enables
// "host-only functions like apply_path_mapping()" (wiki
// 2026-02-Expression-Language.md:514). apply_path_mapping is the only one in
// sqi's registry today because it is the only function that reads session
// state; the gate is a set (hostOnlyFunctions, exprcheck.go) rather than a
// name comparison so a second entrant costs one edit. (RFC 0005's
// "Host-Context Function Availability" states the rule at more length, but the
// RFC is the
// proposal behind the specification, not the specification -- see
// hostOnlyFunctions' own comment.)
//
// This is deliberately an explicit positive list of the host-context scopes,
// not a negation of the non-host ones. A negation (originally "s != ScopeJob")
// is an every-scope-but-this-one claim: it is correct only for as long as
// every scope added later happens to be a host context, which is not a
// property Go's type system -- or anything else -- enforces. A positive list
// fails CLOSED: a sixth scope that nobody adds a case for here is simply not a
// host context, and apply_path_mapping stays unavailable there until someone
// deliberately decides otherwise. ScopeStepTemplate is the case that exposed
// the negation as wrong -- it is not ScopeJob, so "s != ScopeJob" would have
// silently granted it host-only functions despite section 3.6.2 row 1 placing
// it at submission time, before any host exists.
func (s Scope) IsHostContext() bool {
	switch s {
	case ScopeJobEnvironment, ScopeStepEnvironment, ScopeStepScript:
		return true
	default:
		return false
	}
}

// fixedSymbol is a symbol whose name and type are known from the specification
// alone, independent of what a template declares.
type fixedSymbol struct {
	// Name is the whole symbol name, e.g. "Job.Name".
	Name string
	// Type is its expression type (section 1.2.2).
	Type expr.Type
	// EXPROnly excludes it from derivedPrefixes: the symbol exists only when
	// the EXPR extension is enabled, so a base-spec template referencing it
	// must still be rejected.
	EXPROnly bool
}

// symbolFamily is a symbol whose MEMBERS come from the template rather than the
// specification -- Param.<name> for each declared job parameter, and so on. The
// member types are resolved per template by exprcheck.go; only the prefix and
// its availability are scope facts.
type symbolFamily struct {
	// Prefix includes its trailing dot, e.g. "Param.".
	Prefix   string
	EXPROnly bool
}

// scopeFixed returns the fixed symbols a scope exposes, in declaration order.
//
// Every entry here is EXPR-only. Section 1.2.2 defines Job.Name, Step.Name and
// the Session.* triple as part of the EXPR extension's built-in symbols, and
// none of them appears in any pre-E2 prefix list -- which is exactly why
// 7.3.1--job-name-requires-expr and 7.3.1--step-name-requires-expr are invalid
// without the extension.
func scopeFixed(s Scope) []fixedSymbol {
	jobName := fixedSymbol{Name: "Job.Name", Type: expr.TString, EXPROnly: true}
	session := []fixedSymbol{
		jobName,
		{Name: "Session.WorkingDirectory", Type: expr.TPath, EXPROnly: true},
		{Name: "Session.PathMappingRulesFile", Type: expr.TPath, EXPROnly: true},
		{Name: "Session.HasPathMappingRules", Type: expr.TBool, EXPROnly: true},
	}
	stepName := fixedSymbol{Name: "Step.Name", Type: expr.TString, EXPROnly: true}

	switch s {
	case ScopeJob:
		return nil
	case ScopeJobEnvironment:
		return session
	case ScopeStepEnvironment, ScopeStepScript:
		return append(append([]fixedSymbol{}, session...), stepName)
	case ScopeStepTemplate:
		// Section 3.6.2 row 1: a step template's let may reference Job.Name
		// and Step.Name, and nothing session-scoped -- it is evaluated at
		// submission, before any host (and so any Session.* value) exists.
		return []fixedSymbol{jobName, stepName}
	}
	return nil
}

// scopeFamilies returns the template-populated symbol families a scope exposes,
// IN THE ORDER the pre-E2 prefix literals used. Order is load-bearing:
// derivedPrefixes feeds validateFormatString's "allowed: ..." message, and
// reordering it would change text users read.
func scopeFamilies(s Scope) []symbolFamily {
	base := []symbolFamily{{Prefix: "Param."}, {Prefix: "RawParam."}}
	switch s {
	case ScopeJob, ScopeStepTemplate:
		return base
	case ScopeJobEnvironment, ScopeStepEnvironment:
		return append(
			append([]symbolFamily{}, base...),
			symbolFamily{Prefix: "Env.File."},
		)
	case ScopeStepScript:
		return append(
			append([]symbolFamily{}, base...),
			symbolFamily{Prefix: "Task.Param."},
			symbolFamily{Prefix: "Task.RawParam."},
			symbolFamily{Prefix: "Task.File."},
		)
	}
	return nil
}

// derivedPrefixes is the base-spec allowed-prefix list for a scope: the family
// prefixes that are not EXPR-only, plus "Session." where the scope has session
// symbols.
//
// "Session." is a prefix in the base-spec lists but a set of three FIXED
// symbols in the EXPR model, so it is reconstructed here rather than living in
// scopeFamilies. That asymmetry is real and deliberate: the base spec never
// enumerated which Session symbols exist, and EXPR does.
//
// ScopeStepTemplate returns nil unconditionally, for a different reason than
// ScopeJob's nil scopeFixed: a <StepTemplate>.let block has no base-spec
// existence at all. Structural validation (validate.go) rejects it before the
// EXPR extension is enabled, so no base-spec template ever reaches a position
// this function would be consulted for -- there is no "allowed: ..." message
// to build here, because there is no legal base-spec reference to reject.
// Falling through to the general computation below would produce a
// plausible-looking but never-exercised list, and worse, a wrong one: the
// "s != ScopeJob" rule would append "Session." even though section 3.6.2 row
// 1 grants a step template no Session symbols at all (2.1 of the design
// spec). Returning nil here says plainly that this scope is out of scope for
// the base-spec path.
func derivedPrefixes(s Scope) []string {
	if s == ScopeStepTemplate {
		return nil
	}
	out := make([]string, 0, 6)
	for _, f := range scopeFamilies(s) {
		if !f.EXPROnly {
			out = append(out, f.Prefix)
		}
	}
	if s != ScopeJob {
		out = append(out, "Session.")
	}
	return out
}
