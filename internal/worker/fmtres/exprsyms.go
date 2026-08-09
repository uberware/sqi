// SPDX-License-Identifier: AGPL-3.0-or-later

package fmtres

// This file builds the EXPR phase-3 symbol table: the worker-side analog of
// internal/openjd's symbolsFor (exprcheck.go), which the worker cannot call
// directly (it takes a *openjd.JobTemplate, and the worker has no template —
// only the assignment). See EXPR sub-project E4a's design spec, section 3,
// whose table is the contract every family below implements, and section
// 3.1 for why parameter TYPES travel on the assignment (protocol.AssignMsg's
// JobParameterTypes/ParameterTypes fields) rather than being inferred from
// value text.
//
// Two builders, mirroring the split TaskScope/EnvScope already establish for
// the base-spec (non-EXPR) resolution path in fmtres.go:
//
//   - TaskSymbols: the task-action scope (a step's onRun command, args, and
//     embedded files).
//   - EnvSymbols: the environment-action scope (one environment's
//     onEnter/onExit actions, variable values, and embedded files).
//
// Both share the job-parameter and Session.* symbols; TaskSymbols alone adds
// Task.Param.*/Task.RawParam.*/Task.File.* (environments are session-scoped
// and entered once, not per task); EnvSymbols alone adds Env.File.* (scoped
// to the ONE environment the caller identifies, matching
// internal/openjd's symbolsFor -- a template may declare many environments,
// each with its own files).
//
// Every symbol this file binds is CONCRETE (expr.Unresolved never appears in
// the returned tables directly, though a value that failed to parse its
// declared type -- see expr.ValueFromText -- falls back to it), which is the
// one thing that distinguishes phase 3 from phases 1 and 2: "the same walk
// with a different table," now with every placeholder replaced by a value.
//
// FIX ROUND 1 (Task 4 review): Param.<name>/Task.Param.<name> for a PATH-
// declared parameter are now bound with the assignment's PathMap rules
// ALREADY APPLIED, via [mapPathParamValue] (expres.go) -- section 1.2.2:
// "Param.<name> ... path mapping rules already applied", RawParam carrying
// "the original unmapped value". Earlier revisions of this file bound BOTH
// from identical, unmapped raw text, which is silently wrong on every
// platform (path mapping never applied to a PATH parameter reached through
// Param./Task.Param. at all, apply_path_mapping() being the only way to
// invoke it) and additionally wrong in a Windows-specific way had mapping
// been bolted on at the TEMPLATE level instead of here: matching a rule
// against an already-host-flavor-rendered string (a PathNative Value's
// stringified form) rather than the untouched submitted text would make a
// POSIX-sourced rule silently fail to match on a Windows worker. Binding the
// mapped value from raw, submitted text -- before any flavor-specific
// rendering happens -- avoids that: SourceFormat matching happens on the
// form the rule itself declares, not on a re-rendered value.

import (
	"fmt"

	"github.com/uberware/sqi/internal/openjd/expr"
	"github.com/uberware/sqi/internal/openjd/fmtstring"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// pathFlavor is the expr.PathFormat every concrete Path value this file
// constructs uses -- job/task PATH parameters, Task.File.*/Env.File.*,
// Session.WorkingDirectory, Session.PathMappingRulesFile.
//
// PathNative, not the PathPOSIX internal/openjd's own phase-2
// concreteJobParamValue hardcodes: phase 3 runs ON THE WORKER HOST, so this
// is the one evaluation context in sqi today that IS a host context in the
// sense expr.PathFormat's own doc comment describes ("Nothing in sqi
// selects [PathNative] yet; sub-project E does, for host contexts") --
// E4a's whole premise is that the worker is where a template first executes
// against a real machine. Using PathNative here means a worker running on
// Windows gets Windows path semantics for its own session paths, matching
// the filesystem it is actually writing to, rather than the POSIX default
// that exists specifically so a server-side template expands identically
// regardless of the submission machine's OS -- a concern that does not
// apply to a Value built from the worker's own, already-concrete, host
// paths.
const pathFlavor = expr.PathNative

// TaskSymbols builds the phase-3 EXPR symbol table for a task action (a
// step's onRun command, args, and embedded files). It exposes:
//
//   - Param.<name> / RawParam.<name>, typed from msg.JobParameterTypes and
//     concretized from msg.JobParameters.
//   - Task.Param.<name> / Task.RawParam.<name>, typed from
//     msg.ParameterTypes and concretized from msg.Parameters.
//   - Task.File.<name>, one path per entry in msg.EmbeddedFiles, at its
//     materialized on-disk location under workDir (via AddFileVars -- the
//     SAME computation the pre-EXPR path already uses, so the two paths
//     cannot diverge).
//   - Session.WorkingDirectory, Session.PathMappingRulesFile (only when
//     hasPathMap), Session.HasPathMappingRules.
//   - Job.Name, Step.Name, from msg.JobName/msg.StepName.
//
// It does NOT expose Env.File.* -- see EnvSymbols for that family, and
// TestTaskSymbols_ExcludesEnvFile for the negative this asymmetry is tested
// against.
//
// workDir is the absolute session working directory; pathMapFile is the
// absolute path to the written OpenJD path-mapping JSON file (meaningful
// only when hasPathMap is true). These are host facts the caller supplies,
// exactly as they already do for [TaskScope].
//
// An error is returned only when an embedded file's Name/Filename is
// invalid -- see [EmbeddedFileName].
func TaskSymbols(msg *protocol.AssignMsg, workDir, pathMapFile string, hasPathMap bool) (expr.MapSymbols, error) {
	syms := expr.MapSymbols{}
	if err := bindJobParamSymbols(msg, syms); err != nil {
		return nil, err
	}
	if err := bindTaskParamSymbols(msg, syms); err != nil {
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
//   - Env.File.<name>, one path per entry in env.EmbeddedFiles -- THIS
//     environment's own files, not any other environment's and not the
//     step's Task.File.* files (a template may declare many environments,
//     each with its own files; mirrors internal/openjd's symbolsFor, which
//     takes the specific *Environment for the same reason).
//   - Session.WorkingDirectory, Session.PathMappingRulesFile (only when
//     hasPathMap), Session.HasPathMappingRules.
//   - Job.Name, from msg.JobName.
//   - Step.Name, from msg.StepName, but ONLY when env.StepEnvironment is
//     true -- see below.
//
// It does NOT expose Task.Param.*/Task.RawParam.*/Task.File.*:
// environments are session-scoped and entered once, not per task --
// matching [EnvScope]'s existing, pre-EXPR behavior.
//
// Step.Name is CONDITIONAL, unlike every other symbol here, because
// section 3.6.2 (and internal/openjd's own ScopeJobEnvironment/
// ScopeStepEnvironment split, scope.go) grants it to a STEP environment's
// script but not a JOB environment's: ScopeStepEnvironment is exactly
// ScopeJobEnvironment plus Step.Name. protocol.AssignEnvironment.
// StepEnvironment is the bit that distinguishes them on the wire (assign.go
// sets it from which of tmpl.JobEnvironments/stepTmpl.StepEnvironments the
// entry came from), so this function trusts it directly rather than
// guessing.
func EnvSymbols(
	msg *protocol.AssignMsg, env *protocol.AssignEnvironment, workDir, pathMapFile string, hasPathMap bool,
) (expr.MapSymbols, error) {
	syms := expr.MapSymbols{}
	if err := bindJobParamSymbols(msg, syms); err != nil {
		return nil, err
	}
	bindSessionSymbols(syms, workDir, pathMapFile, hasPathMap)
	syms["Job.Name"] = expr.String(msg.JobName)
	if env != nil {
		if env.StepEnvironment {
			syms["Step.Name"] = expr.String(msg.StepName)
		}
		if err := bindFileSymbols(syms, "Env.File", env.EmbeddedFiles, workDir); err != nil {
			return nil, err
		}
	}
	return syms, nil
}

// bindJobParamSymbols binds Param.<name> and RawParam.<name> for every entry
// in msg.JobParameters, typed from msg.JobParameterTypes[name] via
// expr.JobParamTypes -- the SAME mapping internal/openjd's phase-2
// symbolsFor uses (exprcheck.go's jobParamTypes now delegates to it), so a
// declared type cannot type differently between the two phases. A name
// missing from JobParameterTypes floors to expr.TAny via the same
// unrecognized-spelling rule expr.JobParamTypes applies to any unparseable
// spelling -- defensive only; JobParameterTypes and JobParameters share a
// key set in production (buildJobParameterTypes/buildJobParameters,
// internal/scheduler/assign.go, both walk the same parameterDefinitions/
// bound-values source).
//
// Param.<name> goes through [paramValueForBinding], which applies
// msg.PathMap for a PATH-declared parameter (section 1.2.2: "path mapping
// rules already applied"); RawParam.<name> is always built straight from
// the untouched raw text ("the original unmapped value"), never mapped --
// even where rawType is itself CodePath, which cannot happen for a JOB
// parameter (expr.JobParamTypes degrades RawParam to string for every PATH-
// family declared type) but IS reachable for a TASK parameter, see
// bindTaskParamSymbols below.
//
// An error is returned only when mapping a PATH parameter's value fails --
// see [mapPathParamValue]; ordinary INT/FLOAT/STRING parsing failures still
// fall back to expr.Unresolved exactly as before, unchanged.
func bindJobParamSymbols(msg *protocol.AssignMsg, syms expr.MapSymbols) error {
	for name, raw := range msg.JobParameters {
		paramType, rawType := expr.JobParamTypes(msg.JobParameterTypes[name])
		v, err := paramValueForBinding(paramType, raw, msg.PathMap)
		if err != nil {
			return fmt.Errorf("job parameter %q: %w", name, err)
		}
		syms["Param."+name] = v
		syms["RawParam."+name] = expr.ValueFromText(rawType, raw, pathFlavor)
	}
	return nil
}

// bindTaskParamSymbols binds Task.Param.<name> and Task.RawParam.<name> for
// every entry in msg.Parameters, typed from msg.ParameterTypes[name] via
// expr.TaskParamType.
//
// Unlike job parameters, sqi's assignment carries only ONE value per
// task-parameter name (no separate raw-vs-path-mapped wire variant), so
// before this fix round Task.Param and Task.RawParam were bound from
// IDENTICAL raw text. That was always wrong for a PATH-declared task
// parameter -- section 1.2.2 gives Task.RawParam the SAME type as
// Task.Param (unlike the job-parameter pair, which degrades to string) but
// still distinguishes them by whether mapping was applied -- and is fixed
// the same way as bindJobParamSymbols: Task.Param.<name> goes through
// [paramValueForBinding] (mapped when t is TPath), Task.RawParam.<name>
// stays built straight from raw (unmapped), even when both share the exact
// same declared TYPE.
func bindTaskParamSymbols(msg *protocol.AssignMsg, syms expr.MapSymbols) error {
	for name, raw := range msg.Parameters {
		t := expr.TaskParamType(msg.ParameterTypes[name])
		v, err := paramValueForBinding(t, raw, msg.PathMap)
		if err != nil {
			return fmt.Errorf("task parameter %q: %w", name, err)
		}
		syms["Task.Param."+name] = v
		syms["Task.RawParam."+name] = expr.ValueFromText(t, raw, pathFlavor)
	}
	return nil
}

// paramValueForBinding builds the CONCRETE value a Param.<name> or
// Task.Param.<name> symbol binds to: expr.ValueFromText for every declared
// type except a bare PATH, which instead runs the assignment's real
// path-mapping rules over raw via [mapPathParamValue] -- section 1.2.2's
// "path mapping rules already applied" requirement for Param/Task.Param,
// which RawParam/Task.RawParam (bound separately, from the SAME raw text,
// by each caller above) deliberately does not get.
//
// t.Equal(expr.TPath) rather than t.Code == expr.CodePath is deliberate:
// LIST[PATH] job parameters stay expr.Unresolved regardless (sqi's template
// model cannot declare a concrete list value yet -- expr.ValueFromText's own
// list-of-anything case; see exprsyms_test.go's
// TestTaskSymbols_JobParamTypes), so there is no concrete LIST[PATH] value
// for path mapping to ever run against today. Equal, not Code alone, so a
// bare path[T]-shaped union or unresolved wrapper (neither reachable here in
// practice) does not accidentally match a scalar-only branch.
func paramValueForBinding(t expr.Type, raw string, pathMap []protocol.PathMapRule) (expr.Value, error) {
	if t.Equal(expr.TPath) {
		return mapPathParamValue(raw, pathMap)
	}
	return expr.ValueFromText(t, raw, pathFlavor), nil
}

// bindSessionSymbols binds the three fixed Session.* symbols shared by
// TaskSymbols and EnvSymbols. Mirrors [addPathMappingKeys]'s existing
// (pre-EXPR) rule for the path-mapping pair: Session.PathMappingRulesFile is
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
// entry in files, reusing [AddFileVars]'s own filename computation via a
// scratch fmtstring.MapScope rather than recomputing it -- the string path
// AddFileVars produces and the path Value bound here must be the exact same
// on-disk location, and calling the one existing computation is what makes
// that true by construction instead of by two implementations happening to
// agree.
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
