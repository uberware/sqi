// SPDX-License-Identifier: AGPL-3.0-only

package scheduler

// Task 58: task-assignment publishing that respects per-worker pull semantics
// and includes the full task payload (resolved command, env, path map).
//
// buildAssignPayload (previously a minimal stub in scheduler.go) now:
//
//  1. Parses job.RawTemplate to extract the matching StepTemplate by name.
//  2. Builds a protocol.AssignMsg with the step's OnRun action, embedded files,
//     and ordered environments (job environments first, then step environments).
//  3. Attaches the job-level parameters as JobParameters so the worker can
//     resolve {{Param.*}} format strings.
//  4. Populates PathMap with path-mapping rules derived from the server's named
//     storage location configuration for the worker's compute location.
//     In Phase 1 (before tasks 60–62 implement storage location CRUD) the
//     PathMap is always empty; the field is included so workers can begin
//     reading it without a protocol change later.
//
// Pull semantics:
// Workers pull assignments from their per-queue durable JetStream pull consumer
// (EnsureWorkConsumer). The per-worker pull model means the server never pushes
// a message directly to a worker address; instead, a worker that becomes ready
// calls Consumer.Fetch and receives the next available assignment for its queue.
// This is enforced by using WorkQueuePolicy on the SQI_WORK JetStream stream.

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/uberware/sqi/internal/openjd"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// buildAssignPayload constructs a [protocol.AssignMsg] for the given task
// assignment and marshals it to JSON.
//
// It receives the store-level task, worker, job, and step records.  It also
// receives the attemptID so the worker can correlate status reports and log
// chunks back to the correct attempt.
//
// An error is returned if the raw OpenJD template cannot be re-parsed (which
// should not happen in production since the template was validated at submission
// time) or if the named step cannot be found in the template.
func buildAssignPayload(
	task store.Task,
	worker store.Worker,
	job store.Job,
	step store.Step,
	attemptID string,
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
		JobParameters: buildJobParameters(tmpl),
	}

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
		envs = append(envs, convertEnvironment(e))
	}
	for _, e := range stepTmpl.StepEnvironments {
		envs = append(envs, convertEnvironment(e))
	}
	msg.Environments = envs

	// ── PathMap ────────────────────────────────────────────────────────────
	// Phase 1: named storage location CRUD (tasks 60–62) is not yet
	// implemented, so the path map is always empty.  Workers that support
	// the OpenJD path-mapping file will see no entries and treat all paths
	// as already resolved — which is correct for deployments without named
	// storage locations.
	msg.PathMap = buildPathMap(worker.ComputeLocation)

	return json.Marshal(msg)
}

// ── Template → protocol conversion helpers ────────────────────────────────────

// buildJobParameters extracts the job-level parameter values from the parsed
// template.  In OpenJD the template stores parameter definitions (name, type,
// default); the actual values are provided at submission time via
// job.RawTemplate's parameterValues block (if present) or inferred from
// defaults.  For Phase 1, the default value from each definition is included
// when non-nil; submitter-provided overrides are stored in job.RawTemplate
// and will already be reflected in the expanded task parameters via the
// openjd.ExpandParameterSpace path.
func buildJobParameters(tmpl *openjd.JobTemplate) map[string]string {
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
// representation, merging its script embedded files and actions.
func convertEnvironment(e openjd.Environment) protocol.AssignEnvironment {
	ae := protocol.AssignEnvironment{
		Name:      e.Name,
		Variables: e.Variables,
	}
	if e.Script != nil {
		ae.OnEnter = convertAction(e.Script.Actions.OnEnter)
		ae.OnExit = convertAction(e.Script.Actions.OnExit)
		ae.EmbeddedFiles = convertEmbeddedFiles(e.Script.EmbeddedFiles)
	}
	return ae
}

// buildPathMap returns the ordered list of path-mapping rules for the given
// compute location.  In Phase 1 this always returns nil; tasks 60–62 will
// replace this stub with a real lookup against the storage_locations table.
func buildPathMap(_ string) []protocol.PathMapRule {
	// TODO(tasks 60–62): query storage_locations for roots matching
	// computeLocation and convert them to PathMapRule entries.
	return nil
}
