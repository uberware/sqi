// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import "github.com/uberware/sqi/internal/openjd/expr"

// Scope identifies the symbol environment of a template position: which
// symbols exist there, and whether the position runs on a worker host.
//
// This is the package's single declaration of scope, with two consumers:
//
//   - The EXPR path builds a typed symbol table from it, so a reference to a
//     symbol the scope does not expose fails as an unknown symbol from the
//     evaluator.
//   - The base-spec path prefix-matches against prefixes derived from this
//     declaration (derivedPrefixes) rather than a hand-maintained list.
//
// One declaration keeps the two from drifting apart.
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
	// while this scope needs both, and unlike every host-context scope it
	// gets no Session.*, no Task.* and no Env.File.*.
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
// It gates the host-context-only functions. The specification's
// FunctionLibrary.with_host_context() enables "host-only functions like
// apply_path_mapping()" (wiki 2026-02-Expression-Language.md:527), so the
// gate is a set (hostOnlyFunctions, exprcheck.go) rather than a name
// comparison; apply_path_mapping is its only member because it is the only
// function that reads session state.
//
// This is a positive list of the host-context scopes, not a negation of the
// non-host ones, so a new scope with no case here is not a host context and
// apply_path_mapping stays unavailable there. ScopeStepTemplate shows why: a
// rule such as "s != ScopeJob" would grant it host-only functions although
// section 3.6.2 row 1 places it at submission time, before any host exists.
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

// symbolFamily is a symbol whose members come from the template rather than the
// specification -- Param.<name> for each declared job parameter, and so on. The
// member types are resolved per template by exprcheck.go; only the prefix and
// its availability are scope facts.
type symbolFamily struct {
	// Prefix includes its trailing dot, e.g. "Param.".
	Prefix   string
	EXPROnly bool
}

// numScopes is one past the highest [Scope] constant: the size of the
// precomputed tables below, and therefore the range of scopes they answer for.
// A scope outside it falls through to the compute function, which is what keeps
// the tables an optimization rather than a behavior change.
const numScopes = int(ScopeStepTemplate) + 1

// The three scope-derived answers, precomputed once per Scope at package init.
//
// Each is a pure function of a Scope, and derivedPrefixes is called once per
// format-string position on every submission, EXPR or not
// (validateFormatString), so the slices are built once rather than per call.
//
// The shared slices are read-only. Callers of scopeFixed, scopeFamilies and
// derivedPrefixes range over, join or prefix-match against them; a caller
// that appended would write into every other caller's copy.
var (
	scopeFixedTable    [numScopes][]fixedSymbol
	scopeFamiliesTable [numScopes][]symbolFamily
	derivedPrefixTable [numScopes][]string
)

func init() {
	for i := range numScopes {
		s := Scope(i)
		scopeFixedTable[i] = computeScopeFixed(s)
		scopeFamiliesTable[i] = computeScopeFamilies(s)
		derivedPrefixTable[i] = computeDerivedPrefixes(s)
	}
}

// scopeFixed returns the fixed symbols a scope exposes, in declaration order.
//
// Every entry here is EXPR-only. Section 1.2.2 defines Job.Name, Step.Name and
// the Session.* triple as part of the EXPR extension's built-in symbols, and
// none of them is a base-spec prefix -- which is why
// 7.3.1--job-name-requires-expr and 7.3.1--step-name-requires-expr are invalid
// without the extension.
//
// The result is the shared slice built at init; see the table declaration above
// for why no caller may modify it.
func scopeFixed(s Scope) []fixedSymbol {
	if s >= 0 && int(s) < len(scopeFixedTable) {
		return scopeFixedTable[s]
	}
	return computeScopeFixed(s)
}

// computeScopeFixed is [scopeFixed]'s declaration of the mapping, called once
// per scope at init rather than once per lookup.
func computeScopeFixed(s Scope) []fixedSymbol {
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
// in a fixed order. Order matters: derivedPrefixes feeds validateFormatString's
// "allowed: ..." message, and reordering it would change text users read.
//
// The result is the shared slice built at init; see the table declaration above
// for why no caller may modify it.
func scopeFamilies(s Scope) []symbolFamily {
	if s >= 0 && int(s) < len(scopeFamiliesTable) {
		return scopeFamiliesTable[s]
	}
	return computeScopeFamilies(s)
}

// computeScopeFamilies is [scopeFamilies]' declaration of the mapping, called
// once per scope at init rather than once per lookup.
func computeScopeFamilies(s Scope) []symbolFamily {
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
// "Session." is a prefix in the base-spec lists but a set of three fixed
// symbols in the EXPR model, so it is reconstructed here rather than living in
// scopeFamilies: the base spec does not enumerate which Session symbols
// exist, and EXPR does.
//
// ScopeStepTemplate returns nil: a <StepTemplate>.let block has no base-spec
// existence. validateLetExtension (validate.go) rejects one on a template
// without EXPR, so no base-spec reference is ever checked at this scope. The
// general computation below would wrongly append "Session." (by the
// "s != ScopeJob" rule), although section 3.6.2 row 1 grants a step template
// no Session symbols.
//
// The result is the shared slice built at init; see the table declaration above
// for why no caller may modify it. This is the most frequently called of the
// three: one lookup per format-string position on every submission.
func derivedPrefixes(s Scope) []string {
	if s >= 0 && int(s) < len(derivedPrefixTable) {
		return derivedPrefixTable[s]
	}
	return computeDerivedPrefixes(s)
}

// computeDerivedPrefixes is [derivedPrefixes]' declaration of the mapping,
// called once per scope at init rather than once per lookup. It consults
// computeScopeFamilies rather than [scopeFamilies] so it has no dependency on
// which table is populated first.
func computeDerivedPrefixes(s Scope) []string {
	if s == ScopeStepTemplate {
		return nil
	}
	out := make([]string, 0, 6)
	for _, f := range computeScopeFamilies(s) {
		if !f.EXPROnly {
			out = append(out, f.Prefix)
		}
	}
	if s != ScopeJob {
		out = append(out, "Session.")
	}
	return out
}
