// SPDX-License-Identifier: AGPL-3.0-only

package openjd

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/store"
)

// ── Submitter ─────────────────────────────────────────────────────────────────

// Submitter handles the full OpenJD submission pipeline: parse, validate,
// expand the parameter space, and persist the normalized job/step/task rows
// alongside the verbatim raw template.
//
// Create one with [NewSubmitter] and reuse it across requests.
type Submitter struct {
	st store.Store
}

// NewSubmitter returns a [Submitter] backed by st.
func NewSubmitter(st store.Store) *Submitter {
	return &Submitter{st: st}
}

// ── SubmitOptions ─────────────────────────────────────────────────────────────

// SubmitOptions carries the caller-supplied metadata attached to a job at
// submission time.  All fields are optional except FarmID and QueueID.
type SubmitOptions struct {
	// FarmID is the farm this job belongs to. Required.
	FarmID string
	// QueueID is the queue this job is placed in. Required.
	QueueID string
	// Owner is the human responsible for the job (e.g. artist login name).
	Owner string
	// Submitter is the authenticated identity that placed the job (e.g. a
	// service account or API token owner). May differ from Owner.
	Submitter string
	// Priority overrides the default priority (50). Values ≤ 0 are treated as
	// the default.
	Priority int
	// Project is an optional label for grouping jobs in the UI and API.
	Project string
}

// ── SubmitResult ──────────────────────────────────────────────────────────────

// SubmitResult is returned by [Submitter.Submit] and holds every persisted row
// created during submission.
type SubmitResult struct {
	// Job is the persisted job row, including the verbatim raw template.
	Job store.Job
	// Steps holds the persisted step rows in template order.
	Steps []store.Step
	// Tasks holds all persisted task rows across all steps.
	Tasks []store.Task
}

// ── Submit ────────────────────────────────────────────────────────────────────

// Submit parses rawTemplate, validates it, expands each step's parameter space
// into concrete tasks, and persists:
//
//   - one [store.Job] row containing rawTemplate verbatim
//   - one [store.Step] row per step in the template
//   - one [store.Task] row per element of the parameter-space expansion
//
// Steps whose DependsOn list is empty start in [store.StepStatusReady]; steps
// with dependencies start in [store.StepStatusPending].  Tasks inherit their
// step's initial status.
//
// Submit does not run in a database transaction. If it fails partway through,
// orphaned rows may remain; the REST layer or a cleanup sweep should handle
// such cases by checking job.Status == pending with no tasks.
func (s *Submitter) Submit(
	ctx context.Context,
	rawTemplate string,
	format store.TemplateFormat,
	opts SubmitOptions,
) (*SubmitResult, error) {
	// ── 1. Parse ──────────────────────────────────────────────────────────
	parseFormat := FormatYAML
	if format == store.TemplateFormatJSON {
		parseFormat = FormatJSON
	}

	tmpl, err := Parse([]byte(rawTemplate), parseFormat)
	if err != nil {
		return nil, fmt.Errorf("openjd: submit: parse: %w", err)
	}

	// ── 2. Validate ───────────────────────────────────────────────────────
	if errs := Validate(tmpl); len(errs) > 0 {
		return nil, fmt.Errorf("openjd: submit: validation: %w", errs)
	}

	// ── 3. Resolve priority default ───────────────────────────────────────
	priority := opts.Priority
	if priority <= 0 {
		priority = 50
	}

	// ── 4. Create Job row (verbatim template stored as-is) ────────────────
	now := time.Now().UTC()
	job := store.Job{
		ID:             uuid.NewString(),
		FarmID:         opts.FarmID,
		QueueID:        opts.QueueID,
		Name:           tmpl.Name,
		Owner:          opts.Owner,
		Submitter:      opts.Submitter,
		Priority:       priority,
		Status:         store.JobStatusPending,
		Project:        opts.Project,
		RawTemplate:    rawTemplate,
		TemplateFormat: format,
		CreatedAt:      now,
		UpdatedAt:      now,
	}

	job, err = s.st.CreateJob(ctx, job)
	if err != nil {
		return nil, fmt.Errorf("openjd: submit: create job: %w", err)
	}

	result := &SubmitResult{Job: job}

	// ── 5. Create Step and Task rows ──────────────────────────────────────
	for i, stepTmpl := range tmpl.Steps {
		// Collect dependency names from the template.
		dependsOn := make([]string, 0, len(stepTmpl.Dependencies))
		for _, dep := range stepTmpl.Dependencies {
			dependsOn = append(dependsOn, dep.DependsOn)
		}

		// Initial step status: ready immediately when there are no deps.
		stepStatus := store.StepStatusReady
		if len(dependsOn) > 0 {
			stepStatus = store.StepStatusPending
		}

		hostReqs, computeLoc := toStoreHostRequirements(stepTmpl.HostRequirements)

		step := store.Step{
			ID:               uuid.NewString(),
			JobID:            job.ID,
			Name:             stepTmpl.Name,
			DependsOn:        dependsOn,
			StepOrder:        i,
			Status:           stepStatus,
			HostRequirements: hostReqs,
			ComputeLocation:  computeLoc,
			CreatedAt:        now,
			UpdatedAt:        now,
		}

		step, err = s.st.CreateStep(ctx, step)
		if err != nil {
			return nil, fmt.Errorf("openjd: submit: create step %q: %w", stepTmpl.Name, err)
		}

		result.Steps = append(result.Steps, step)

		// Task status mirrors the step's initial status.
		taskStatus := store.TaskStatusReady
		if stepStatus == store.StepStatusPending {
			taskStatus = store.TaskStatusPending
		}

		// ── 6. Expand parameter space ──────────────────────────────────────
		taskParamList, err := ExpandParameterSpace(stepTmpl.ParameterSpace)
		if err != nil {
			return nil, fmt.Errorf("openjd: submit: expand step %q: %w", stepTmpl.Name, err)
		}

		// ── 7. Create one Task row per parameter combination ───────────────
		for j, params := range taskParamList {
			task := store.Task{
				ID:         uuid.NewString(),
				JobID:      job.ID,
				StepID:     step.ID,
				Name:       buildTaskName(stepTmpl.Name, j, params),
				Parameters: params,
				Status:     taskStatus,
				CreatedAt:  now,
				UpdatedAt:  now,
			}

			task, err = s.st.CreateTask(ctx, task)
			if err != nil {
				return nil, fmt.Errorf(
					"openjd: submit: create task %d of step %q: %w",
					j, stepTmpl.Name, err,
				)
			}

			result.Tasks = append(result.Tasks, task)
		}
	}

	return result, nil
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// ── Host-requirements conversion ──────────────────────────────────────────────

// licensePoolPrefix is the amount-requirement name prefix that signals a
// license pool capacity requirement in the OpenJD hostRequirements block.
// The pool name follows the prefix, e.g. "amount.worker.licensepool.arnold".
const licensePoolPrefix = "amount.worker.licensepool."

// computeLocationAttr is the attribute name used to declare a compute-location
// affinity in the OpenJD hostRequirements block.
const computeLocationAttr = "attr.worker.computelocation"

// toStoreHostRequirements converts an OpenJD [HostRequirements] value into the
// scheduler-friendly [store.StepHostRequirements] representation. It also
// extracts the compute-location affinity (if present) as a plain string for
// SQL-level pre-filtering.
//
// Returns (nil, "") when hr is nil, indicating no requirements.
func toStoreHostRequirements(hr *HostRequirements) (reqs *store.StepHostRequirements, computeLoc string) {
	if hr == nil {
		return nil, ""
	}

	shr := &store.StepHostRequirements{}

	for _, a := range hr.Amounts {
		if poolName, ok := strings.CutPrefix(a.Name, licensePoolPrefix); ok && poolName != "" {
			shr.LicensePools = append(shr.LicensePools, poolName)
		}
		shr.Amounts = append(shr.Amounts, store.StepAmountRequirement{
			Name: a.Name,
			Min:  a.Min,
			Max:  a.Max,
		})
	}

	for _, a := range hr.Attributes {
		// Extract compute-location from the well-known attribute, if present,
		// and mirror it as a plain string for SQL pre-filtering.
		if a.Name == computeLocationAttr {
			switch {
			case len(a.AnyOf) == 1:
				computeLoc = a.AnyOf[0]
			case len(a.AllOf) == 1:
				computeLoc = a.AllOf[0]
			}
		}
		shr.Attributes = append(shr.Attributes, store.StepAttributeRequirement{
			Name:  a.Name,
			AnyOf: a.AnyOf,
			AllOf: a.AllOf,
		})
	}

	return shr, computeLoc
}

// buildTaskName constructs a human-readable name for one task instance.
//
// For parameter-free steps the step name alone is returned (there is exactly
// one task, so the step name uniquely identifies it).
//
// For parametric steps, parameter key=value pairs are sorted and joined in
// square brackets so names are deterministic regardless of map iteration order:
//
//	render[Frame=1]
//	composite[Frame=1,Layer=bg]
func buildTaskName(stepName string, _ int, params TaskParams) string {
	if len(params) == 0 {
		return stepName
	}

	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var sb strings.Builder
	sb.WriteString(stepName)
	sb.WriteByte('[')
	for i, k := range keys {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(k)
		sb.WriteByte('=')
		sb.WriteString(params[k])
	}
	sb.WriteByte(']')
	return sb.String()
}
