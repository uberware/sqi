// SPDX-License-Identifier: AGPL-3.0-or-later

package fmtres

// This file builds the EXPR phase-3 symbol table: the worker-side analog of
// internal/openjd's symbolsFor (exprcheck.go), which the worker cannot call
// directly (it takes a *openjd.JobTemplate, and the worker has only the
// assignment). Parameter types travel on the assignment
// (protocol.AssignMsg's JobParameterTypes/ParameterTypes fields) rather than
// being inferred from value text.
//
// Two builders, mirroring the TaskScope/EnvScope split of the base-spec
// (non-EXPR) resolution path in fmtres.go:
//
//   - TaskSymbols: the task-action scope (a step's onRun command, args, and
//     embedded files).
//   - EnvSymbols: the environment-action scope (one environment's
//     onEnter/onExit actions, variable values, and embedded files).
//
// Both share the job-parameter and Session.* symbols; TaskSymbols alone adds
// Task.Param.*/Task.RawParam.*/Task.File.* (environments are session-scoped
// and entered once, not per task); EnvSymbols alone adds Env.File.* (scoped
// to the one environment the caller identifies, as in internal/openjd's
// symbolsFor: a template may declare many environments, each with its own
// files).
//
// Every symbol this file binds is concrete: expr.Unresolved never appears in
// the returned tables directly, though a value that failed to parse as its
// declared type (see expr.ValueFromText) falls back to it. That is what
// distinguishes phase 3 from phases 1 and 2: the same walk with a different
// table, every placeholder replaced by a value.
//
// Three divergences from phase 2 are intended (also listed in
// docs/openjd-extensions/expr.md, "Phase-2/phase-3 differences"):
//
//  1. Session.PathMappingRulesFile is bound only when the session has rules,
//     matching the non-EXPR addPathMappingKeys rule (bindSessionSymbols
//     below).
//  2. Paths are PathNative here and PathPOSIX at phase 2 (pathFlavor below,
//     which quotes Expression-Language section 1.2.1).
//  3. Path mapping is applied to Param.<PATH>/Task.Param.<PATH> only here.
//     Section 1.2.2 requires it; phase 2 has no session, hence no rules
//     (paramValueForBinding below).
//
// Param.<name>/Task.Param.<name> for a PATH-declared parameter are bound with
// the assignment's PathMap rules already applied, via [mapPathParamValue]
// (expres.go); section 1.2.2: "Param.<name> ... path mapping rules already
// applied", RawParam carrying "the original unmapped value". The mapping is
// applied to the raw submitted text: matching a rule against a string already
// rendered in the host flavor (a PathNative Value's stringified form) would
// make a POSIX-sourced rule fail to match on a Windows worker. Mapping the raw
// text means SourceFormat matching happens on the form the rule declares.

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/uberware/sqi/internal/openjd/expr"
	"github.com/uberware/sqi/internal/openjd/fmtstring"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// pathFlavor is the expr.PathFormat of every concrete Path value this file
// constructs: job/task PATH parameters, Task.File.*/Env.File.*,
// Session.WorkingDirectory, Session.PathMappingRulesFile.
//
// It is PathNative, where internal/openjd's phase-2 concreteJobParamValue
// uses PathPOSIX. Expression-Language section 1.2.1: "The path type can have
// either POSIX or Windows path semantics depending on the evaluation
// context... In TEMPLATE scope contexts, POSIX semantics are used for
// consistency. In host contexts (SESSION and TASK scopes), the semantics
// match the host's operating system." Phase 3 is a host context (it runs on
// the worker host, against a real session directory), so the flavor is the
// host's; phase 2 runs at template scope and uses POSIX.
//
// A Windows worker therefore gets Windows path semantics for its session
// paths, matching the filesystem it writes to. The POSIX default exists so a
// server-side template expands identically whatever OS submitted it, which
// does not apply to a Value built from the worker's own host paths.
const pathFlavor = expr.PathNative

// TaskSymbols builds the phase-3 EXPR symbol table for a task action (a
// step's onRun command, args, and embedded files). It exposes:
//
//   - Param.<name> / RawParam.<name>, typed from msg.JobParameterTypes and
//     concretized from msg.JobParameters.
//   - Task.Param.<name> / Task.RawParam.<name>, typed from
//     msg.ParameterTypes and concretized from msg.Parameters.
//   - Task.File.<name>, one path per entry in msg.EmbeddedFiles, at its
//     materialized on-disk location under workDir (via AddFileVars, the
//     computation the non-EXPR path uses, so the two cannot diverge).
//   - Session.WorkingDirectory, Session.PathMappingRulesFile (only when
//     hasPathMap), Session.HasPathMappingRules.
//   - Job.Name, Step.Name, from msg.JobName/msg.StepName.
//
// It does not expose Env.File.* (see EnvSymbols for that family, and
// TestTaskSymbols_ExcludesEnvFile).
//
// workDir is the absolute session working directory; pathMapFile is the
// absolute path to the written OpenJD path-mapping JSON file (meaningful
// only when hasPathMap is true). The caller supplies these host facts, as for
// [TaskScope].
//
// An error is returned only when an embedded file's Name/Filename is invalid
// (see [EmbeddedFileName]) or when a PATH parameter's value cannot be mapped.
//
// budget is used here only to carry the limits: symbol building charges no
// positions or retained bytes (the callers that resolve fields and evaluate
// let: blocks do), but binding a PATH parameter runs an apply_path_mapping
// evaluation through [mapPathParamValue], which must be metered by the same
// operator-configured per-evaluation limits as every other evaluation in this
// package. It is required; nil (passed by this package's unit tests) meters
// against the defaults. See [budgetOrDefault], which also documents why a
// production caller must not pass nil.
func TaskSymbols(
	msg *protocol.AssignMsg, workDir, pathMapFile string, hasPathMap bool, budget *AssignmentBudget,
) (expr.MapSymbols, error) {
	opts := ExprEvalOptions(budgetOrDefault(budget).Limits(), msg.PathMap)
	syms := expr.MapSymbols{}
	if err := bindJobParamSymbols(msg, syms, opts); err != nil {
		return nil, err
	}
	if err := bindTaskParamSymbols(msg, syms, opts); err != nil {
		return nil, err
	}
	bindSessionSymbols(syms, workDir, pathMapFile, hasPathMap)
	syms["Job.Name"] = expr.String(msg.JobName)
	syms["Step.Name"] = expr.String(msg.StepName)
	if err := bindFileSymbols(syms, "Task.File", msg.EmbeddedFiles, workDir); err != nil {
		return nil, err
	}
	return syms, nil
}

// EnvSymbols builds the phase-3 EXPR symbol table for one environment's
// actions (onEnter/onExit) and variable values. It exposes:
//
//   - Param.<name> / RawParam.<name>, exactly as TaskSymbols.
//   - Env.File.<name>, one path per entry in env.EmbeddedFiles: this
//     environment's own files, not another environment's and not the
//     step's Task.File.* files (a template may declare many environments,
//     each with its own files; internal/openjd's symbolsFor takes the
//     specific *Environment for the same reason).
//   - Session.WorkingDirectory, Session.PathMappingRulesFile (only when
//     hasPathMap), Session.HasPathMappingRules.
//   - Job.Name, from msg.JobName.
//   - Step.Name, from msg.StepName, only when env.StepEnvironment is true
//     (see below).
//   - The enclosing step template's let-bound names (msg.StepTemplateLet),
//     also only when env.StepEnvironment is true (see below).
//
// It does not expose Task.Param.*/Task.RawParam.*/Task.File.*: environments
// are session-scoped and entered once, not per task, as in [EnvScope].
//
// Step.Name is conditional because section 3.6.2 (and internal/openjd's
// ScopeJobEnvironment/ScopeStepEnvironment split, scope.go) grants it to a
// step environment's script but not a job environment's:
// ScopeStepEnvironment is ScopeJobEnvironment plus Step.Name.
// protocol.AssignEnvironment.StepEnvironment distinguishes them on the wire
// (assign.go sets it from whether the entry came from tmpl.JobEnvironments or
// stepTmpl.StepEnvironments), and this function trusts it.
//
// msg.StepTemplateLet is evaluated and folded in here, for a step environment
// only. Template Schemas section 3.6.2 row 1 makes a <StepTemplate>.let
// binding's names available in stepEnvironments, hostRequirements,
// parameterSpace and script, and section 3.6 states the same rule from the
// other side ("a let binding in a <StepTemplate>'s stepEnvironments cannot
// shadow a binding from that step's let block"). Phase 2 implements it too:
// checkStepExpressions passes stepLet as checkEnvironmentExpressions'
// outerLet, which folds it into baseSyms before anything else, so it is
// visible to variables, embedded files, onEnter and onExit alike. Without it
// here, a template phase 2 accepted would fail every task in the step with
// unknown symbol "<name>" at enterOne, and fail teardown the same way.
//
// It is folded in inside the table builder rather than exposed as a second
// "apply" step the caller could forget. That matches phase 2:
// checkEnvironmentExpressions treats outerLet as part of the table an
// environment starts from (baseSyms := clone(outerLet), then symbolsFor's
// additions), not as a let: block the environment evaluates. The
// environment's own let: block (section 3.6.2 row 4) is a separate, later
// step, [ApplyEnvLet], because section 3.6.2 places the environment's
// Variables between the two, and phase 2's ordering there is observable.
//
// Job environments get none of this: they have no enclosing step, so no
// step-template let block is in scope, and the OpenJD conformance fixture
// 7.3.1--step-name-in-job-environment-let.invalid.yaml requires that.
// env.StepEnvironment is the same bit Step.Name keys off, for the same
// reason.
//
// An error is returned when an embedded file's Name/Filename is invalid
// (see [EmbeddedFileName]), when a PATH parameter's value cannot be mapped,
// or when any step-template let: binding fails to parse or evaluate.
// See [TaskSymbols] for budget, which carries the limits here for the same
// reason (a job PATH parameter is bound here too).
func EnvSymbols(
	msg *protocol.AssignMsg, env *protocol.AssignEnvironment, workDir, pathMapFile string, hasPathMap bool,
	budget *AssignmentBudget,
) (expr.MapSymbols, error) {
	opts := ExprEvalOptions(budgetOrDefault(budget).Limits(), msg.PathMap)
	syms := expr.MapSymbols{}
	if err := bindJobParamSymbols(msg, syms, opts); err != nil {
		return nil, err
	}
	bindSessionSymbols(syms, workDir, pathMapFile, hasPathMap)
	syms["Job.Name"] = expr.String(msg.JobName)
	if env == nil {
		return syms, nil
	}
	if env.StepEnvironment {
		syms["Step.Name"] = expr.String(msg.StepName)
	}
	if err := bindFileSymbols(syms, "Env.File", env.EmbeddedFiles, workDir); err != nil {
		return nil, err
	}
	if env.StepEnvironment {
		if err := applyStepTemplateLet(msg.StepTemplateLet, syms, opts, budget); err != nil {
			return nil, err
		}
	}
	return syms, nil
}

// bindJobParamSymbols binds Param.<name> and RawParam.<name> for every entry
// in msg.JobParameters, typed from msg.JobParameterTypes[name] via
// expr.JobParamTypes, the mapping internal/openjd's phase-2 symbolsFor uses
// (exprcheck.go's jobParamTypes delegates to it), so a declared type cannot
// type differently between the two phases. A name missing from
// JobParameterTypes falls to expr.TAny via expr.JobParamTypes' rule for an
// unrecognized spelling. That case is defensive: JobParameterTypes and
// JobParameters share a key set (buildJobParameterTypes/buildJobParameters in
// internal/scheduler/assign.go both walk the template's parameter
// definitions).
//
// Param.<name> goes through [paramValueForBinding], which applies
// msg.PathMap for a PATH-declared parameter (section 1.2.2: "path mapping
// rules already applied"); RawParam.<name> is always built from the raw text
// ("the original unmapped value"), never mapped, even where rawType is
// CodePath. That cannot happen for a job parameter (expr.JobParamTypes
// degrades RawParam to string for every PATH-family declared type) but can
// for a task parameter; see bindTaskParamSymbols below.
//
// An error is returned only when mapping a PATH parameter's value fails
// (see [mapPathParamValue]); INT/FLOAT/STRING parsing failures fall back to
// expr.Unresolved.
func bindJobParamSymbols(msg *protocol.AssignMsg, syms expr.MapSymbols, opts []expr.Option) error {
	return bindParamSymbols(
		syms, "job", "Param.", "RawParam.",
		msg.JobParameters, msg.JobParameterTypes, expr.JobParamTypes, opts,
	)
}

// bindTaskParamSymbols binds Task.Param.<name> and Task.RawParam.<name> for
// every entry in msg.Parameters, typed from msg.ParameterTypes[name] via
// expr.TaskParamType.
//
// The assignment carries one value per task-parameter name (no separate raw
// and path-mapped wire variants). For a PATH-declared task parameter, section
// 1.2.2 gives Task.RawParam the same type as Task.Param (the job-parameter
// pair degrades RawParam to string) but still distinguishes them by whether
// mapping was applied, so this works like bindJobParamSymbols:
// Task.Param.<name> goes through [paramValueForBinding] (mapped when t is
// TPath), and Task.RawParam.<name> is built from the raw text, unmapped, even
// though both share the declared type.
//
// That type rule is the only difference from [bindJobParamSymbols], and the
// types func passed to [bindParamSymbols] states it: returning
// expr.TaskParamType's one type twice is §1.2.2's "a task parameter's
// RawParam keeps the declared type; a job parameter's degrades to string".
func bindTaskParamSymbols(msg *protocol.AssignMsg, syms expr.MapSymbols, opts []expr.Option) error {
	return bindParamSymbols(
		syms, "task", "Task.Param.", "Task.RawParam.",
		msg.Parameters, msg.ParameterTypes,
		func(declared string) (paramType, rawType expr.Type) {
			t := expr.TaskParamType(declared)
			return t, t
		},
		opts,
	)
}

// bindParamSymbols implements §1.2.2's parameter binding for both
// [bindJobParamSymbols] and [bindTaskParamSymbols]: the sorted walk, the
// [paramValueForBinding] call for the mapped Param twin, the
// expr.ValueFromText call for the unmapped Raw twin, and the error wrapping.
//
// values maps a parameter name to its raw submitted text; declaredTypes maps
// the same names to their declared type spellings; types turns one spelling
// into the (Param, RawParam) type pair, where the two families differ. kind
// ("job"/"task") names the family in an error; paramPrefix/rawPrefix are the
// symbol prefixes.
func bindParamSymbols(
	syms expr.MapSymbols,
	kind, paramPrefix, rawPrefix string,
	values, declaredTypes map[string]string,
	types func(declared string) (paramType, rawType expr.Type),
	opts []expr.Option,
) error {
	// Sorted, because the loop returns on the first failure: iterating the
	// map directly would make an assignment with two bad PATH parameters
	// report a different one from run to run.
	for _, name := range slices.Sorted(maps.Keys(values)) {
		raw := values[name]
		paramType, rawType := types(declaredTypes[name])
		v, err := paramValueForBinding(paramType, raw, opts)
		if err != nil {
			return fmt.Errorf("%s parameter %q: %w", kind, name, err)
		}
		syms[paramPrefix+name] = v
		syms[rawPrefix+name] = expr.ValueFromText(rawType, raw, pathFlavor)
	}
	return nil
}

// paramValueForBinding builds the concrete value a Param.<name> or
// Task.Param.<name> symbol binds to: expr.ValueFromText for every declared
// type except PATH and LIST[PATH], which run the assignment's path-mapping
// rules over raw via [mapPathParamValue]. That is section 1.2.2's "path
// mapping rules already applied" requirement for Param/Task.Param;
// RawParam/Task.RawParam, bound separately from the same raw text by each
// caller above, stay unmapped.
//
// It uses t.Equal(expr.TPath) rather than t.Code == expr.CodePath so a
// path-shaped union or unresolved wrapper does not match the scalar branch.
//
// A LIST[PATH] parameter is mapped element by element through
// [mapListPathParamValue], as RFC 0007 requires.
//
// opts is the pre-built [ExprEvalOptions] slice for this table; see
// [mapPathParamValue] for why it is passed down rather than rebuilt per value.
func paramValueForBinding(t expr.Type, raw string, opts []expr.Option) (expr.Value, error) {
	if t.Equal(expr.TPath) {
		return mapPathParamValue(raw, opts)
	}
	if t.Equal(expr.ListOf(expr.TPath)) {
		return mapListPathParamValue(raw, opts)
	}
	return expr.ValueFromText(t, raw, pathFlavor), nil
}

// mapListPathParamValue applies path mapping to every element of a LIST[PATH]
// value, per RFC 0007: Param.<name> for a list of paths "Returns a list[path]
// type value with path mapping applied".
//
// Each element goes through mapPathParamValue, so each is matched against the
// rules on the submitted text, before any flavor-specific rendering, for the
// reason the comment at the top of this file gives for the scalar case. Each
// element's mapping is metered by the same per-evaluation limits rather than
// one budget covering the whole list: opts is shared across the elements, but
// an expr.Option is configuration applied to a fresh evaluation context per
// Eval, so sharing the slice shares the numbers, not a ledger. Sharing it
// avoids rebuilding the options and re-copying the rules slice
// (ConvertPathMapRules) per element.
//
// A value that does not decode returns Unresolved rather than an error, the
// same fallback expr.ValueFromText uses for a scalar that fails to parse: this
// function binds symbols, and upstream validation rejects a bad value.
func mapListPathParamValue(raw string, opts []expr.Option) (expr.Value, error) {
	elems, err := openjdDecodeList(raw)
	if err != nil {
		//nolint:nilerr // an undecodable value falls back to Unresolved, not
		// an error: this function binds symbols, and upstream validation
		// rejects a bad value (see the doc comment above).
		return expr.Unresolved(expr.ListOf(expr.TPath)), nil
	}
	out := make([]expr.Value, 0, len(elems))
	for _, e := range elems {
		s, ok := e.(string)
		if !ok {
			return expr.Unresolved(expr.ListOf(expr.TPath)), nil
		}
		v, err := mapPathParamValue(s, opts)
		if err != nil {
			return expr.Value{}, err
		}
		out = append(out, v)
	}
	return expr.List(expr.TPath, out), nil
}

// openjdDecodeList parses a canonical list parameter value.
//
// It duplicates internal/openjd.DecodeListParamValue because no production
// file under internal/worker may import internal/openjd, which pulls in
// internal/store. The encoding is plain JSON, so the duplicate is one
// json.Unmarshal.
func openjdDecodeList(raw string) ([]any, error) {
	var out []any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, err
	}
	return out, nil
}

// bindSessionSymbols binds the three fixed Session.* symbols shared by
// TaskSymbols and EnvSymbols. It follows [addPathMappingKeys]'s non-EXPR
// rule for the path-mapping pair: Session.PathMappingRulesFile is
// bound only when hasPathMap is true, so a reference to it with no rules
// present fails as an unknown symbol rather than resolving to an empty
// path.
func bindSessionSymbols(syms expr.MapSymbols, workDir, pathMapFile string, hasPathMap bool) {
	syms["Session.WorkingDirectory"] = expr.Path(workDir, pathFlavor)
	syms["Session.HasPathMappingRules"] = expr.Bool(hasPathMap)
	if hasPathMap {
		syms["Session.PathMappingRulesFile"] = expr.Path(pathMapFile, pathFlavor)
	}
}

// bindFileSymbols binds "<prefix>.<name>" as a concrete path Value for every
// entry in files, reusing [AddFileVars]'s filename computation via a scratch
// fmtstring.MapScope: the string path AddFileVars produces and the path Value
// bound here must name the same on-disk location.
func bindFileSymbols(syms expr.MapSymbols, prefix string, files []protocol.EmbeddedFile, workDir string) error {
	scratch := fmtstring.MapScope{}
	if err := AddFileVars(scratch, prefix, files, workDir); err != nil {
		return err
	}
	for k, v := range scratch {
		syms[k] = expr.Path(v, pathFlavor)
	}
	return nil
}

// ─── let bindings ───────────────────────────────────────────────────────────
//
// A let: block cannot be pre-resolved server-side: a StepScript or
// EnvironmentScript binding may reference Task.Param.*, Task.File.*,
// Env.File.* or Session.*, none of which exist until a session directory has
// been created on a specific worker host. So the raw binding strings travel
// on the wire (AssignMsg.StepTemplateLet/StepScriptLet,
// AssignEnvironment.Let), and this section re-evaluates them against the
// phase-3 tables TaskSymbols/EnvSymbols build, as phase 2 does for job/task
// parameters.
//
// The mechanism follows internal/openjd/exprcheck.go's checkLetBindings, the
// phase-1/phase-2 counterpart: bindings are evaluated in declaration order;
// each successful result is inserted into syms so a later binding (in the
// same block, or, for the step-level pair, the following block) can reference
// it; a failed binding is not inserted (so a later reference to it reports an
// unknown symbol rather than a second copy of the same fault); and evaluation
// continues past a failure. Section 3.6's shadowing rule ("a let binding may
// not shadow a previous binding in the same let block or any enclosing
// scope") is enforced as checkLetBindings enforces it: a name already present
// as a key, checked by lookup, so it also catches a collision with a spec
// symbol. A submitted template cannot produce that collision, because section
// 3.6.1's lowercase-first <UserIdentifier> grammar keeps the two namespaces
// apart, and splitLetBinding does not re-check that grammar on wire data.
//
// The lookup is against the table the results are merged into, which for the
// step-template block is not the narrowed table the block is evaluated
// against. Checked against the narrowed view, a binding named for a spec
// symbol outside the projection ("Session.WorkingDirectory = \"/pwned\"",
// "Task.Param.N = 999") would pass the check and then overwrite the real
// symbol on merge, so an onRun would render "/pwned" and "999" with no error.
// evalLetBindings therefore takes the merge target separately. No privilege
// boundary depends on it (parseLetBinding rejects a dotted name at submit),
// but a let binding must never silently replace a spec symbol.
//
// Three differences from checkLetBindings, because phase 3 runs a task and has
// no template author to hand a JSON-pointer-keyed diagnostic back to:
//
//  1. checkHostOnlyFunctions has no counterpart here.
//
//     Phase 3 is not always a host context in the sense of
//     Scope.IsHostContext. IsHostContext (scope.go) is true for
//     ScopeJobEnvironment, ScopeStepEnvironment and ScopeStepScript;
//     ScopeStepTemplate, StepTemplateLet's phase-1/2 scope, returns false. A
//     StepTemplateLet binding is evaluated at submission server-side because
//     it must not see host state, and stepTemplateLetScope (below) enforces
//     that at phase 3 too.
//
//     The missing gate is safe for a narrower reason: hostOnlyFunctions
//     (exprcheck.go) has one member, apply_path_mapping, which has no side
//     effects (it returns a mapped path), and anyone able to forge
//     StepTemplateLet already controls Command/Args on the same unsigned
//     AssignMsg, so gating that function here would not limit a forged
//     assignment. If a side-effecting function joins hostOnlyFunctions, this
//     reasoning does not hold and nothing here would fail: add a real
//     checkHostOnlyFunctions counterpart then.
//  2. Errors are returned as one joined error (errors.Join), not a
//     ValidationErrors slice keyed by JSON pointer. A worker task fails with
//     a single error message.
//  3. [splitLetBinding] does not re-validate section 3.6.1's <UserIdentifier>
//     character-class grammar as internal/openjd's parseLetBinding
//     (letbinding.go) does; the server enforces it at submission. It also
//     trims differently: splitLetBinding uses strings.TrimSpace where
//     parseLetBinding uses strings.Trim(s, " \t"), because section 3.6.1's
//     <WS> is tab and space only. The two agree on every binding the server
//     accepts (a validated name or expression never has a newline or other
//     space character at its edges); the wider trim matters only for a
//     binding the server rejects.
//
// The 50-binding cap (maxLetBindings, mirroring internal/openjd/validate.go's
// constant of the same name) is enforced here as checkLetBindings enforces
// it: the evaluator that retains the bindings' values must enforce the cap,
// not only report it.
//
// On the server, checkLetBindings truncates (it evaluates the first 50 and
// drops the rest, as this function does), which bounds cost, and
// validateLetElementCounts rejects the template (ValidateWithOptions does not
// short-circuit on the report, so both are required; see validate.go's
// maxLetBindings). The worker neither reports nor rejects; it relies on
// validateLetElementCounts having failed the submission, and truncates here as
// defense in depth. The bound that protects the worker against an accepted
// but expensive block is defaultLetRetainedBytes, below.

// maxLetBindings caps the number of bindings evalLetBindings evaluates from a
// single let: block. It mirrors internal/openjd/validate.go's constant of the
// same name and value, which the worker binary cannot import (see the section
// comment above).
const maxLetBindings = 50

// defaultLetRetainedBytes is the default cap on the total section 1.3.9 size
// of everything one phase-3 symbol table holds live, measured across every
// let: block evaluated into it. The enforced value is
// [ExprLimits.LetRetainedBytes], carried on the assignment's budget and
// configurable by the operator.
//
// [ExprLimits.OperationLimit]/[ExprLimits.MemoryLimit] are per-Eval budgets,
// and a let: block is the one construct in this package that retains a
// result rather than rendering and dropping it: without this bound every
// binding gets a fresh 20 MB budget it may keep. maxLetBindings bounds the
// count, so the structural ceiling would be maxLetBindings x
// [ExprLimits.MemoryLimit] = 1 GB per block, two blocks per task table plus
// one per environment table, all live concurrently across a worker's task
// slots. For example, 50 bindings of `a<i> = "x" * (Task.Param.N * 100000)`
// with N = 99 retain 495 MB, and phase 2 cannot reject that template: at
// submission Task.Param.N is unresolved[int], so `"x" * unresolved` costs
// nothing under the server's per-evaluation limits. Without this bound, any
// user with jobs.write could exhaust sqi-worker's memory and end every task on
// the host.
//
// This is the per-table bound; the assignment-wide cumulative budget is
// [AssignmentBudget]. It bounds retained bytes rather than operations because
// the construction above spends almost no operations.
//
// It is enforced by measuring the table itself (tableRetainedBytes) rather
// than threading an accumulator between the callers, which makes it a
// per-table limit: the step-template block's results are merged into the
// table the step-script block then measures, and EnvSymbols' merged
// step-template names are in the table ApplyEnvLet measures. The table
// measured is always the one the bindings land in, never the narrower one a
// block may resolve against (see evalLetBindings' mergeTarget).
//
// The default of 10 MB is generous for a legitimate block (bindings are
// derived from parameters and path text, and phase 2 type-checks the whole
// block inside a 1 MB per-Eval budget), and limits the worst case to 10 MB
// retained plus at most one in-flight evaluation's [ExprLimits.MemoryLimit]:
// 30 MB per table at the defaults.
//
// Two limitations of the default, both adjustable through configuration:
//
//  1. 10,000,000 equals limits.go's maxStringBytes, so the largest single
//     string the evaluator can produce cannot be bound, even into an
//     otherwise empty table (the check is retained+size > limit, and a
//     10,000,000-byte string measures slightly more once its value header is
//     counted). The two numbers are chosen independently.
//  2. Because the accounting measures the whole table (tableRetainedBytes),
//     a job whose parameters are large (a big LIST[PATH], a multi-megabyte
//     STRING) spends budget its let: block never asked for, and fails at
//     execution with no phase-2 counterpart to warn the submitter. Metering
//     only let-bound names would let a table hold its parameters plus a full
//     budget of bindings, so the table is measured whole and the knob is the
//     adjustment.
const defaultLetRetainedBytes int64 = 10_000_000

// splitLetBinding splits one raw "<name> = <expression>" binding into its
// bound name and unparsed expression source. The split happens at the first
// "=" byte, matching internal/openjd's parseLetBinding (letbinding.go): a
// legal <UserIdentifier> never contains "=", so the first occurrence ends the
// name however many "=" bytes the expression contains ("x = a == b" yields
// the expression "a == b").
//
// It does not re-validate section 3.6.1's <UserIdentifier> character-class
// grammar (lowercase-first start, alnum/underscore continuation, 1-512
// length) as parseLetBinding does: the server enforces that grammar
// (checkLetBindings runs at both ValidateWithOptions and submit) before the
// assignment reaches the worker.
func splitLetBinding(raw string) (name, exprSrc string, err error) {
	before, after, found := strings.Cut(raw, "=")
	if !found {
		return "", "", fmt.Errorf("let binding %q: must be of the form \"name = expression\"", raw)
	}
	name = strings.TrimSpace(before)
	exprSrc = strings.TrimSpace(after)
	if name == "" {
		return "", "", fmt.Errorf("let binding %q: missing name before \"=\"", raw)
	}
	if exprSrc == "" {
		return "", "", fmt.Errorf("let binding %q: missing expression after \"=\"", raw)
	}
	return name, exprSrc, nil
}

// evalLetBindings evaluates one let: block's raw bindings against syms,
// mutating it in place; see the section comment above for the mechanism
// shared with checkLetBindings. label identifies the block in a returned
// error ("step template let", "step script let", "environment let"), together
// with each binding's position within the (possibly truncated) block, so a
// block with more than one failure names each one distinctly.
//
// mergeTarget is the table this block's results land in, when that is wider
// than the syms the block resolves against. Only [applyStepTemplateLet]
// passes one: it resolves against a ScopeStepTemplate projection and merges
// into the full phase-3 table. Every other caller passes nil, because syms is
// the target.
//
// Two things must key off the target rather than the projection:
//
//   - the shadow check, or a binding named for a spec symbol the projection
//     drops passes the check and then overwrites the real symbol on merge;
//   - the retained-bytes accounting, or the budget is measured against a
//     table smaller than the one the bytes end up in, and "no phase-3 symbol
//     table retains more than defaultLetRetainedBytes bytes" is false. A task
//     table holding a 3 MB STRING task parameter starts at 6 MB while the
//     projection measures 0 MB, so measuring the projection would admit a
//     9 MB binding and leave the table at 15 MB against a 10 MB limit.
//     Task.Param./Task.RawParam./Task.File./Session./Env.File. are all
//     outside the projection.
//
// Every error is collected rather than stopping the block, and errors.Join'd
// into one returned error, nil when every binding in the (possibly
// truncated) block evaluated. The exceptions are the retained-bytes bounds,
// which stop the block; see defaultLetRetainedBytes.
//
// budget is charged the size (expr.SizeOf(v)) computed for the per-table
// defaultLetRetainedBytes check, right after that check passes, so a binding
// within its table's limit that would push the whole assignment's retained
// bytes (task table plus every environment's) over
// [ExprLimits.AssignmentRetainedBytes] also stops the block. See
// [AssignmentBudget]. It is required; nil (passed by this package's unit
// tests) charges a fresh, default-limited throwaway budget. See
// [budgetOrDefault].
func evalLetBindings(
	label string, lets []string, syms, mergeTarget expr.MapSymbols, opts []expr.Option, budget *AssignmentBudget,
) error {
	if len(lets) > maxLetBindings {
		lets = lets[:maxLetBindings]
	}

	// The projection's keys are a subset of the target's, holding the same
	// Values (stepTemplateLetScope filters the target), so measuring the
	// target alone counts every live value once.
	budgeted := syms
	if mergeTarget != nil {
		budgeted = mergeTarget
	}

	taken := func(name string) bool {
		if _, ok := syms[name]; ok {
			return true
		}
		_, ok := budgeted[name]
		return ok
	}

	retained := tableRetainedBytes(budgeted)
	b := budgetOrDefault(budget)
	var errs []error
	for i, raw := range lets {
		if err := b.Err(); err != nil {
			// The assignment-wide budget (charged by an earlier binding in
			// this block, an earlier block on this table, or another table
			// this assignment built) is spent. Stop: every remaining binding
			// would repeat the same fault.
			errs = append(errs, fmt.Errorf("%s[%d]: %w", label, i, err))
			break
		}
		name, src, err := splitLetBinding(raw)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s[%d]: %w", label, i, err))
			continue
		}

		if taken(name) {
			errs = append(errs, fmt.Errorf(
				"%s[%d]: let binding %q shadows a name already in scope", label, i, name,
			))
			continue
		}

		e, err := expr.Parse(src)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s[%d] (%s): %w", label, i, name, err))
			continue
		}
		v, err := e.Eval(syms, expr.TAny, opts...)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s[%d] (%s): %w", label, i, name, err))
			continue
		}

		// Stop the block rather than continue: the failures above are faults
		// in one binding that the rest of the block can be evaluated past,
		// but this one means the table is full. Continuing would spend a
		// fresh MemoryLimit allowance per remaining binding, the exhaustion
		// this guard prevents. The offending value is dropped, so the table
		// never exceeds the limit.
		size := expr.SizeOf(v)
		if retained+size > b.limits.LetRetainedBytes {
			errs = append(errs, fmt.Errorf(
				"%s[%d] (%s): let bindings would retain %d bytes, over this symbol table's %d-byte limit",
				label, i, name, retained+size, b.limits.LetRetainedBytes,
			))
			break
		}
		if err := b.ChargeRetainedBytes(size, fmt.Sprintf("%s[%d] (%s)", label, i, name)); err != nil {
			errs = append(errs, err)
			break
		}
		retained += size
		syms[name] = v
	}
	return errors.Join(errs...)
}

// tableRetainedBytes reports the total section 1.3.9 size of every value syms
// holds live, the quantity defaultLetRetainedBytes bounds.
//
// It measures the whole table, not only let-bound names: the invariant is
// "one phase-3 symbol table never retains more than defaultLetRetainedBytes
// bytes", and a table's spec symbols (a big STRING or PATH parameter's value,
// an embedded file's path) are retained bytes too. Callers must pass the
// table the bindings land in (see evalLetBindings' mergeTarget). Cost is
// O(len(syms)) once per let: block, typically a few dozen entries.
//
// As a result, a job whose parameters are large consumes budget its let:
// block never asked for, and can fail at execution with no phase-2
// counterpart; the limit is operator configuration
// ([ExprLimits.LetRetainedBytes]) for that reason.
func tableRetainedBytes(syms expr.MapSymbols) int64 {
	var total int64
	for _, v := range syms {
		total += expr.SizeOf(v)
	}
	return total
}

// stepTemplateLetScope filters full, a phase-3 TaskSymbols table, down to the
// symbols internal/openjd's ScopeStepTemplate exposes: Job.Name, Step.Name,
// Param.<name> and RawParam.<name> (scope.go's scopeFixed/scopeFamilies for
// ScopeStepTemplate). It drops Task.*, Session.* and *.File.* because a step
// template's let: block (Template Schemas 3.6.2 row 1) is checked by
// checkStepExpressions at ScopeStepTemplate, not ScopeStepScript, at
// submission, before any session or task exists. A StepTemplateLet binding
// referencing Session.WorkingDirectory or Task.Param.* is an unknown-symbol
// error at phase 2, and phase 3 must reject it the same way even though the
// full TaskSymbols table has the value.
//
// See ApplyTaskLet's doc comment for how the names this block binds are
// carried into the script's block, following checkStepExpressions'
// preLetKeys/stepLet mechanism (exprcheck.go).
//
// Filtering the built table, rather than re-deriving
// Param./RawParam./Job.Name/Step.Name from msg, means a StepTemplateLet
// binding sees the same Values TaskSymbols computed, restricted to the
// narrower key set.
//
// The returned table is what the block resolves against. The block's shadow
// check must consult the merge target instead, or a binding named for a spec
// symbol this projection drops passes the check and then overwrites the real
// symbol. [applyStepTemplateLet] passes both.
func stepTemplateLetScope(full expr.MapSymbols) expr.MapSymbols {
	out := expr.MapSymbols{}
	for k, v := range full {
		switch {
		case k == "Job.Name", k == "Step.Name":
			out[k] = v
		case strings.HasPrefix(k, "Param."), strings.HasPrefix(k, "RawParam."):
			out[k] = v
		}
	}
	return out
}

// ApplyTaskLet evaluates a task's phase-3 let: bindings over syms, the step
// template's block (msg.StepTemplateLet) first and then the step script's
// (msg.StepScriptLet), mutating syms in place. This is Template Schemas
// section 3.6.2 row 1/2's ordering. The two blocks travel as separate wire
// fields because the script's block may not shadow a name the template's
// block bound, which is enforceable only while they stay distinguishable
// (see protocol.go's doc comment on StepTemplateLet/StepScriptLet).
//
// As in checkStepExpressions (exprcheck.go), the two blocks do not share one
// table: the template's block is evaluated against [stepTemplateLetScope]'s
// narrower view of syms, then only the names it bound are merged into the
// full syms before the script's block, which sees the full table, runs. A
// name the template's block bound is visible to the script's block (and may
// not be rebound by it), but a Session.*/Task.*/*.File.* symbol the
// template's block tried to reference is not, as ScopeStepTemplate denies it
// server-side.
//
// Call this once per table. It inserts every bound name from both blocks
// into syms, so a second call over the same syms makes every binding fail
// the shadow check. A caller resolving more than one action, embedded file or
// variable for the same task calls ApplyTaskLet once to build the table and
// reuses that syms for every resolution.
//
// Callers pass the result of TaskSymbols as syms. ApplyTaskLet is separate
// from TaskSymbols for two reasons: some callers want the base
// job/task/session table alone (TaskSymbols' tests, and
// TestPhase2Phase3Agreement's key-set comparison against SymbolsFor, which
// never touches let); and let evaluation needs pathMap for its metering
// options, which TaskSymbols does not otherwise require.
//
// pathMap is forwarded to ExprEvalOptions, as for every other phase-3
// evaluator in this package; see ResolveActionExpr's doc comment for why it
// is a plain parameter rather than a variadic opts tail.
//
// It returns a non-nil error if any binding in either block failed
// (errors.Join of every failure across both blocks); syms still holds every
// binding that succeeded, since a failed binding does not stop the walk (see
// the section comment above).
//
// budget is forwarded to both blocks' evalLetBindings calls, so the task's
// let: retention is charged against the same [AssignmentBudget] every
// environment table of this assignment shares; see session.Session's
// accessor for how the budget reaches every phase-3 call site.
func ApplyTaskLet(
	msg *protocol.AssignMsg, syms expr.MapSymbols, pathMap []protocol.PathMapRule, budget *AssignmentBudget,
) error {
	b := budgetOrDefault(budget)
	opts := ExprEvalOptions(b.Limits(), pathMap)
	tmplErr := applyStepTemplateLet(msg.StepTemplateLet, syms, opts, b)
	scriptErr := evalLetBindings("step script let", msg.StepScriptLet, syms, nil, opts, b)
	return errors.Join(tmplErr, scriptErr)
}

// applyStepTemplateLet evaluates a step template's let: block (lets) against
// [stepTemplateLetScope]'s narrowed view of syms and merges the names it
// newly bound back into syms, mutating it in place. It is the shared half of
// [ApplyTaskLet] and [EnvSymbols]: Template Schemas section 3.6.2 row 1 makes
// the same block's names visible to a step's script and to its
// stepEnvironments, and one function keeps the two positions consistent (see
// EnvSymbols' doc comment).
//
// The merge is a set difference, not a copy of the narrowed table: the
// narrowed table also holds Job.Name/Step.Name/Param.*/RawParam.*, which syms
// already has. This follows checkStepExpressions' preLetKeys/stepLet
// computation (exprcheck.go).
func applyStepTemplateLet(
	lets []string, syms expr.MapSymbols, opts []expr.Option, budget *AssignmentBudget,
) error {
	tmplScope := stepTemplateLetScope(syms)
	preLetKeys := make(map[string]struct{}, len(tmplScope))
	for k := range tmplScope {
		preLetKeys[k] = struct{}{}
	}
	err := evalLetBindings("step template let", lets, tmplScope, syms, opts, budget)
	for k, v := range tmplScope {
		if _, existed := preLetKeys[k]; !existed {
			syms[k] = v
		}
	}
	return err
}

// ApplyEnvLet is ApplyTaskLet's environment counterpart: it evaluates env's
// own script let: block (AssignEnvironment.Let, Template Schemas section
// 3.6.2 row 4) over syms, mutating it in place. A nil env is a no-op
// returning nil, matching EnvSymbols' nil handling.
//
// It does not evaluate the enclosing step template's block (section 3.6.2
// row 1): [EnvSymbols] already folded those names into syms, so they are
// visible here, and per section 3.6 a binding in this block that tries to
// rebind one of them is rejected by the shadow check.
//
// Call this after resolving env.Variables. Section 3.6.2 row 4 grants an
// environment script's let: names to the script's children (actions and
// embeddedFiles), and Variables is a sibling of Script on the parent
// Environment, not a descendant, so a variable value may not see them.
// Phase 2 encodes that by checking Variables against baseSyms
// before cloning it into scriptSyms and binding the block
// (checkEnvironmentExpressions, exprcheck.go); phase 3 encodes it by
// ordering the two calls, since it mutates one table rather than cloning.
//
// Callers pass the result of EnvSymbols as syms. See [ApplyTaskLet] for
// pathMap and budget.
func ApplyEnvLet(
	env *protocol.AssignEnvironment, syms expr.MapSymbols, pathMap []protocol.PathMapRule, budget *AssignmentBudget,
) error {
	if env == nil {
		return nil
	}
	b := budgetOrDefault(budget)
	return evalLetBindings("environment let", env.Let, syms, nil, ExprEvalOptions(b.Limits(), pathMap), b)
}
