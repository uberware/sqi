// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

// Task-assignment payload construction: builds the full task payload (resolved
// command, env, path map) returned to a worker when it leases a task.
//
// buildAssignPayload:
//
//  1. Parses job.RawTemplate to extract the matching StepTemplate by name.
//  2. Builds a protocol.AssignMsg with the step's OnRun action, embedded files,
//     and ordered environments (job environments first, then step environments).
//  3. Attaches the job-level parameters as JobParameters so the worker can
//     resolve {{Param.*}} format strings.
//  4. Populates PathMap with path-mapping rules (openjd mode) generated from
//     all registered storage locations for the worker's compute location.
//  5. Resolves loc:// URI references in command args, env vars, and task
// parameters to concrete local paths (resolved mode).
//
// Lease semantics:
// Workers request work via the core-NATS work.lease.<queue> request/reply
// protocol ([handleLeaseRequest]). The scheduler selects an eligible, fitting
// batch of ready tasks, leases each atomically, and returns the marshaled
// payloads built here in the reply. The server never pushes assignments to a
// worker address; a worker only ever runs what it explicitly leased.

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/uberware/sqi/internal/openjd"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// buildAssignPayload constructs a [protocol.AssignMsg] for the given task
// assignment and marshals it to JSON.
//
// It receives the store-level task, worker, job, step, and queue records.  It
// also receives the attemptID so the worker can correlate status reports and
// log chunks back to the correct attempt, and the storage location store used
// to build the path map and resolve loc:// URIs.
//
// An error is returned if the raw OpenJD template cannot be re-parsed (which
// should not happen in production since the template was validated at submission
// time), if the named step cannot be found in the template, or if a loc://
// reference cannot be resolved.
func buildAssignPayload(
	ctx context.Context,
	task store.Task,
	worker store.Worker,
	job store.Job,
	step store.Step,
	queue store.Queue,
	attemptID string,
	locStore store.StorageLocationStore,
) ([]byte, error) {
	// ── Parse the raw OpenJD template ─────────────────────────────────────
	format := openjd.FormatYAML
	if job.TemplateFormat == store.TemplateFormatJSON {
		format = openjd.FormatJSON
	}

	tmpl, err := openjd.Parse([]byte(job.RawTemplate), format)
	if err != nil {
		return nil, fmt.Errorf("build assign payload: re-parse template for job %s: %w", job.ID, err)
	}

	// ── Find the matching StepTemplate by step name ───────────────────────
	var stepTmpl *openjd.StepTemplate
	for i := range tmpl.Steps {
		if tmpl.Steps[i].Name == step.Name {
			stepTmpl = &tmpl.Steps[i]
			break
		}
	}
	if stepTmpl == nil {
		return nil, fmt.Errorf(
			"build assign payload: step %q not found in template for job %s",
			step.Name, job.ID,
		)
	}

	// ── Build the protocol message ─────────────────────────────────────────
	msg := protocol.AssignMsg{
		Version:       protocol.ProtocolVersion,
		Type:          protocol.TypeAssign,
		TaskID:        task.ID,
		JobID:         task.JobID,
		StepID:        task.StepID,
		AttemptID:     attemptID,
		TaskName:      task.Name,
		WorkerID:      worker.ID,
		AssignedAt:    time.Now().UTC(),
		Parameters:    task.Parameters,
		JobParameters: buildJobParameters(tmpl, job),
	}

	// ── Isolation (queue run-as-user) ──────────────────────────────────────
	// Nil RunAsUser means no isolation; msg.Isolation must stay nil in that
	// case (never an empty struct with a blank username — see
	// [protocol.IsolationSpec]).
	if queue.RunAsUser != nil && *queue.RunAsUser != "" {
		spec := &protocol.IsolationSpec{User: *queue.RunAsUser}
		if queue.RunAsGroup != nil {
			spec.Group = *queue.RunAsGroup
		}
		msg.Isolation = spec
	}

	// ── Expression evaluation (EXPR extension) ──────────────────────────────
	// internal/openjd's validateLetExtension rejects any let: block unless
	// the template declares EXPR, so a template that reaches here without
	// EXPR cannot have a populated Let anywhere in it; the hasEXPR gate in
	// populateEXPRFields is nonetheless what keeps ParameterTypes and
	// JobParameterTypes (which base-spec templates *do* populate, since
	// typed parameterDefinitions are base-spec, not EXPR-only) off the wire
	// for a base-spec assignment. A template without EXPR declared must
	// produce byte-for-byte the same assignment shape it produced before
	// this section existed — see [protocol.AssignMsg.EXPR].
	hasEXPR := populateEXPRFields(&msg, tmpl, stepTmpl)

	// ── OnRun action and step-level embedded files ─────────────────────────
	if stepTmpl.Script != nil {
		onRun := stepTmpl.Script.Actions.OnRun
		msg.OnRun = convertAction(&onRun)
		msg.EmbeddedFiles = convertEmbeddedFiles(stepTmpl.Script.EmbeddedFiles)
	}

	// ── Environments (job then step order, per OpenJD spec) ────────────────
	envs := make([]protocol.AssignEnvironment, 0,
		len(tmpl.JobEnvironments)+len(stepTmpl.StepEnvironments))
	for _, e := range tmpl.JobEnvironments {
		envs = append(envs, convertEnvironment(e, hasEXPR))
	}
	for _, e := range stepTmpl.StepEnvironments {
		envs = append(envs, convertEnvironment(e, hasEXPR))
	}
	msg.Environments = envs

	// ── Fetch all storage locations once ──────────────────────────────────
	allLocs, err := locStore.ListStorageLocations(ctx)
	if err != nil {
		return nil, fmt.Errorf("build assign payload: list storage locations: %w", err)
	}

	// ── PathMap (openjd mode) ──────────────────────────────────────────────
	// Build source→destination path-mapping rules for the worker's compute
	// location.  Workers write these to the OpenJD pathmapping-1.0
	// path_mapping.json file in the session working directory so that
	// applications with native OpenJD path-mapping support can translate paths
	// themselves.
	msg.PathMap = buildPathMap(allLocs, worker.ComputeLocation)

	// ── Path-translation deliveries ────────────────────────────────────────
	msg.PathDeliveries = convertPathDeliveries(tmpl.PathTranslation)

	// ── Resolved-mode path translation ───────────────────────────
	// Replace any loc:// URI references in the command, args, task parameters,
	// and environment variables with concrete local paths for this worker.
	// This ensures workers with no OpenJD path-mapping support see only
	// concrete paths.
	if err := resolveLocURIsInMsg(&msg, allLocs, worker.ComputeLocation); err != nil {
		return nil, fmt.Errorf("build assign payload: resolve loc:// URIs: %w", err)
	}

	// ── Staging manifest (uses already-resolved job parameters) ────────────
	if hasDelivery(msg.PathDeliveries, string(openjd.DeliveryStageLocally)) {
		msg.Staging = buildStagingManifest(tmpl, msg.JobParameters)
	}

	return json.Marshal(msg)
}

// ── Template → protocol conversion helpers ────────────────────────────────────

// buildJobParameters returns the job-parameter values to carry in the
// assignment message so the worker can resolve {{Param.*}} format strings.
//
// When job.Parameters is non-empty (jobs submitted after the parameters-
// persistence migration), those fully-bound values are used directly — they
// include both caller-supplied values and applied defaults, so no information
// is lost and submitted non-default values are preserved.
//
// When job.Parameters is empty (pre-migration jobs or jobs with no declared
// parameters), the function falls back to extracting defaults from the parsed
// template so that existing jobs continue to work correctly.
func buildJobParameters(tmpl *openjd.JobTemplate, job store.Job) map[string]string {
	// Prefer persisted bound values when available. Clone so downstream
	// resolution (e.g. loc:// rewriting in resolveLocURIsInMsg) never mutates
	// the caller's store.Job — or, with the in-memory fake, the shared map in
	// the store.
	if len(job.Parameters) > 0 {
		return maps.Clone(job.Parameters)
	}
	// Fallback: extract defaults from parameter definitions (pre-migration jobs).
	if len(tmpl.ParameterDefinitions) == 0 {
		return nil
	}
	params := make(map[string]string, len(tmpl.ParameterDefinitions))
	for _, p := range tmpl.ParameterDefinitions {
		if p.Default != nil {
			params[p.Name] = *p.Default
		}
	}
	if len(params) == 0 {
		return nil
	}
	return params
}

// convertAction converts an [openjd.Action] to its protocol counterpart.
// A nil pointer is returned as nil.
func convertAction(a *openjd.Action) *protocol.Action {
	if a == nil {
		return nil
	}
	pa := &protocol.Action{
		Command:        a.Command,
		Args:           a.Args,
		TimeoutSeconds: a.TimeoutSeconds,
	}
	if a.Cancelation != nil {
		pa.Cancelation = &protocol.CancelationMethod{
			Mode:                string(a.Cancelation.Mode),
			NotifyPeriodSeconds: a.Cancelation.NotifyPeriodSeconds,
		}
	}
	return pa
}

// convertEmbeddedFiles converts a slice of [openjd.EmbeddedFile] to the
// protocol representation.
func convertEmbeddedFiles(files []openjd.EmbeddedFile) []protocol.EmbeddedFile {
	if len(files) == 0 {
		return nil
	}
	out := make([]protocol.EmbeddedFile, len(files))
	for i, f := range files {
		out[i] = protocol.EmbeddedFile{
			Name:      f.Name,
			Filename:  f.Filename,
			Data:      f.Data,
			Runnable:  f.Runnable,
			EndOfLine: f.EndOfLine,
		}
	}
	return out
}

// convertEnvironment converts an [openjd.Environment] to the protocol
// representation, merging its script embedded files and actions. hasEXPR
// gates copying the environment's let: block onto the wire — see
// [protocol.AssignEnvironment.Let].
func convertEnvironment(e openjd.Environment, hasEXPR bool) protocol.AssignEnvironment {
	ae := protocol.AssignEnvironment{
		Name:      e.Name,
		Variables: e.Variables,
	}
	if e.Script != nil {
		ae.OnEnter = convertAction(e.Script.Actions.OnEnter)
		ae.OnExit = convertAction(e.Script.Actions.OnExit)
		ae.EmbeddedFiles = convertEmbeddedFiles(e.Script.EmbeddedFiles)
		if hasEXPR {
			ae.Let = e.Script.Let
		}
	}
	return ae
}

// populateEXPRFields sets msg's EXPR flag and, only when the template
// declares the EXPR extension, its step-level let blocks and declared
// parameter-type maps. Returns hasEXPR so the caller can gate the
// EXPR-only environment let blocks it assembles afterward without
// re-deriving the same flag.
func populateEXPRFields(msg *protocol.AssignMsg, tmpl *openjd.JobTemplate, stepTmpl *openjd.StepTemplate) bool {
	hasEXPR := declaresEXPR(tmpl)
	msg.EXPR = hasEXPR
	if !hasEXPR {
		return false
	}
	msg.StepTemplateLet = stepTmpl.Let
	if stepTmpl.Script != nil {
		msg.StepScriptLet = stepTmpl.Script.Let
	}
	msg.ParameterTypes = buildParameterTypes(stepTmpl)
	msg.JobParameterTypes = buildJobParameterTypes(tmpl)
	return true
}

// declaresEXPR reports whether tmpl declares the EXPR extension
// (extensions: [EXPR]). Mirrors internal/openjd's own gate for the identical
// question — exprcheck.go's checkTemplateExpressions tests
// tmpl.hasExtension("EXPR"), which is unexported and therefore not callable
// from this package; JobTemplate.Extensions is exported, so the check is
// duplicated here rather than exported solely for this one caller.
func declaresEXPR(tmpl *openjd.JobTemplate) bool {
	return slices.Contains(tmpl.Extensions, "EXPR")
}

// buildJobParameterTypes returns the declared OpenJD type of each job
// parameter from tmpl's parameterDefinitions, keyed by parameter name. See
// [protocol.AssignMsg.JobParameterTypes].
func buildJobParameterTypes(tmpl *openjd.JobTemplate) map[string]string {
	if len(tmpl.ParameterDefinitions) == 0 {
		return nil
	}
	types := make(map[string]string, len(tmpl.ParameterDefinitions))
	for _, p := range tmpl.ParameterDefinitions {
		types[p.Name] = string(p.Type)
	}
	return types
}

// buildParameterTypes returns the declared OpenJD type of each task
// parameter from the step's taskParameterDefinitions, keyed by parameter
// name. See [protocol.AssignMsg.ParameterTypes].
func buildParameterTypes(stepTmpl *openjd.StepTemplate) map[string]string {
	if stepTmpl.ParameterSpace == nil || len(stepTmpl.ParameterSpace.TaskParameterDefinitions) == 0 {
		return nil
	}
	defs := stepTmpl.ParameterSpace.TaskParameterDefinitions
	types := make(map[string]string, len(defs))
	for _, p := range defs {
		types[p.Name] = string(p.Type)
	}
	return types
}

// buildPathMap builds the ordered list of OpenJD path-mapping rules from all
// registered storage locations for the given compute location.
//
// For each location that has both a "default" root and a compute-location-
// specific root (and the two differ), a rule is emitted whose SourcePath is the
// default root, DestinationPath is the compute-location-specific root, and
// SourcePathFormat is the OpenJD enum derived from the source root via
// [detectPathFormat] (WINDOWS for drive-letter/backslash paths, else POSIX).
// This allows workers on non-default compute locations to translate canonical
// paths transparently.
//
// Locations that only have a "default" root are omitted — their canonical
// paths are already correct and no translation is needed.
func buildPathMap(locs []store.StorageLocation, computeLocation string) []protocol.PathMapRule {
	var rules []protocol.PathMapRule
	for _, loc := range locs {
		defaultRoot, hasDefault := loc.Roots["default"]
		if !hasDefault || defaultRoot == "" {
			continue
		}

		// Find the compute-location-specific root.
		destRoot := defaultRoot
		if computeLocation != "" {
			if r, ok := loc.Roots[computeLocation]; ok && r != "" {
				destRoot = r
			}
		}

		// Only emit a rule when translation is actually needed.
		if destRoot == defaultRoot {
			continue
		}

		rules = append(rules, protocol.PathMapRule{
			SourcePathFormat: detectPathFormat(defaultRoot),
			SourcePath:       defaultRoot,
			DestinationPath:  destRoot,
		})
	}
	return rules
}

// detectPathFormat returns the OpenJD pathmapping source-path-format enum for
// p using a simple heuristic: a path that contains a backslash or begins with a
// drive-letter specifier (e.g. "C:\\foo" or "C:/foo") is treated as "WINDOWS";
// everything else (POSIX absolute paths, S3/loc URIs, etc.) is "POSIX".
//
// This is a best-effort classification for OpenJD-aware applications reading
// path_mapping.json; sqi's own substitution (pathmap.Apply) is format-agnostic
// and keys solely on the literal SourcePath string.
func detectPathFormat(p string) string {
	if strings.Contains(p, `\`) {
		return "WINDOWS"
	}
	// Drive-letter prefix: a single ASCII letter followed by a colon, e.g. "C:".
	if len(p) >= 2 && p[1] == ':' {
		c := p[0]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') {
			return "WINDOWS"
		}
	}
	return "POSIX"
}

// resolveLocURIsInMsg rewrites all loc:// URI references inside msg in-place.
// It covers the OnRun action command/args, all environment variables and
// embedded-file data in every AssignEnvironment, and all task parameter values.
func resolveLocURIsInMsg(
	msg *protocol.AssignMsg,
	locs []store.StorageLocation,
	computeLocation string,
) error {
	// Build a quick lookup map by location name.
	locMap := make(map[string]store.StorageLocation, len(locs))
	for _, loc := range locs {
		locMap[loc.Name] = loc
	}

	resolve := func(name, relPath string) (string, error) {
		loc, ok := locMap[name]
		if !ok {
			return "", fmt.Errorf("storage location %q not found", name)
		}
		root, err := openjd.ResolveRoot(name, loc.Roots, computeLocation)
		if err != nil {
			return "", err
		}
		return openjd.JoinPath(root, relPath), nil
	}

	// Resolve job parameters. A PATH job parameter whose value (or applied
	// default) is a loc:// URI must be concretized here, server-side; otherwise
	// the worker would splice the raw URI into a {{Param.*}} expansion. The map
	// is a clone (see buildJobParameters), so this in-place rewrite is safe.
	for k, v := range msg.JobParameters {
		resolved, err := openjd.ResolveLocURIs(v, resolve)
		if err != nil {
			return fmt.Errorf("job parameter %q: %w", k, err)
		}
		msg.JobParameters[k] = resolved
	}

	// Resolve task parameters.
	for k, v := range msg.Parameters {
		resolved, err := openjd.ResolveLocURIs(v, resolve)
		if err != nil {
			return fmt.Errorf("parameter %q: %w", k, err)
		}
		msg.Parameters[k] = resolved
	}

	// Resolve OnRun action.
	if msg.OnRun != nil {
		if err := resolveAction(msg.OnRun, resolve); err != nil {
			return fmt.Errorf("onRun: %w", err)
		}
	}

	// Resolve embedded files (step-level).
	for i := range msg.EmbeddedFiles {
		resolved, err := openjd.ResolveLocURIs(msg.EmbeddedFiles[i].Data, resolve)
		if err != nil {
			return fmt.Errorf("embedded file %q: %w", msg.EmbeddedFiles[i].Name, err)
		}
		msg.EmbeddedFiles[i].Data = resolved
	}

	// Resolve environments.
	for i := range msg.Environments {
		if err := resolveEnvironment(&msg.Environments[i], resolve); err != nil {
			return fmt.Errorf("environment %q: %w", msg.Environments[i].Name, err)
		}
	}

	return nil
}

// convertPathDeliveries maps the parsed SQI_PATH_TRANSLATION block to the
// protocol delivery set, or the implicit default when the extension is absent.
func convertPathDeliveries(pt *openjd.PathTranslation) []protocol.PathDelivery {
	src := openjd.DefaultPathDeliveries()
	if pt != nil {
		src = pt.Deliveries
	}
	out := make([]protocol.PathDelivery, len(src))
	for i, d := range src {
		out[i] = protocol.PathDelivery{Kind: string(d.Kind), Pattern: d.Pattern, Variable: d.Variable}
	}
	return out
}

func hasDelivery(ds []protocol.PathDelivery, kind string) bool {
	for _, d := range ds {
		if d.Kind == kind {
			return true
		}
	}
	return false
}

// buildStagingManifest produces a StageEntry per job-level PATH parameter that
// declares a dataFlow direction (IN/OUT/INOUT). NONE/undirected params and
// non-PATH params are skipped. The path value is taken from the already
// loc://-resolved job parameters.
func buildStagingManifest(tmpl *openjd.JobTemplate, jobParams map[string]string) []protocol.StageEntry {
	var out []protocol.StageEntry
	for _, p := range tmpl.ParameterDefinitions {
		if p.Type != openjd.JobParamTypePath {
			continue
		}
		dir := string(p.DataFlow)
		if dir != "IN" && dir != "OUT" && dir != "INOUT" {
			continue
		}
		val, ok := jobParams[p.Name]
		if !ok || val == "" {
			continue
		}
		out = append(out, protocol.StageEntry{
			Path:       val,
			Direction:  dir,
			ObjectType: string(p.ObjectType),
		})
	}
	return out
}

// resolveAction rewrites loc:// URIs in an action's command and args.
func resolveAction(a *protocol.Action, resolve func(string, string) (string, error)) error {
	if a == nil {
		return nil
	}
	cmd, err := openjd.ResolveLocURIs(a.Command, resolve)
	if err != nil {
		return fmt.Errorf("command: %w", err)
	}
	a.Command = cmd

	for i, arg := range a.Args {
		resolved, err := openjd.ResolveLocURIs(arg, resolve)
		if err != nil {
			return fmt.Errorf("arg[%d]: %w", i, err)
		}
		a.Args[i] = resolved
	}
	return nil
}

// resolveEnvironment rewrites loc:// URIs in an environment's variables,
// embedded files, and lifecycle actions.
func resolveEnvironment(
	e *protocol.AssignEnvironment,
	resolve func(string, string) (string, error),
) error {
	for k, v := range e.Variables {
		resolved, err := openjd.ResolveLocURIs(v, resolve)
		if err != nil {
			return fmt.Errorf("variable %q: %w", k, err)
		}
		e.Variables[k] = resolved
	}
	for i := range e.EmbeddedFiles {
		resolved, err := openjd.ResolveLocURIs(e.EmbeddedFiles[i].Data, resolve)
		if err != nil {
			return fmt.Errorf("embedded file %q: %w", e.EmbeddedFiles[i].Name, err)
		}
		e.EmbeddedFiles[i].Data = resolved
	}
	if err := resolveAction(e.OnEnter, resolve); err != nil {
		return fmt.Errorf("onEnter: %w", err)
	}
	if err := resolveAction(e.OnExit, resolve); err != nil {
		return fmt.Errorf("onExit: %w", err)
	}
	return nil
}
