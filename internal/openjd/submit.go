// SPDX-License-Identifier: AGPL-3.0-or-later

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
// expand the parameter space, persist the normalized job/step/task rows
// alongside the verbatim raw template, and enforce storage-location coverage.
//
// Create one with [NewSubmitter] and reuse it across requests.
type Submitter struct {
	st    store.Store
	locSt store.StorageLocationStore
}

// NewSubmitter returns a [Submitter] backed by st.  The store must implement
// [store.StorageLocationStore] (which [store.Store] always does) so that
// named-location references in submitted templates can be validated at
// submission time.
func NewSubmitter(st store.Store) *Submitter {
	return &Submitter{st: st, locSt: st}
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
		return nil, &SubmitValidationError{Cause: fmt.Errorf("openjd: submit: parse: %w", err)}
	}

	// ── 2. Validate ───────────────────────────────────────────────────────
	if errs := Validate(tmpl); len(errs) > 0 {
		return nil, &SubmitValidationError{Cause: fmt.Errorf("openjd: submit: validation: %w", errs)}
	}

	// ── 2b. Validate named storage location coverage ────────────
	if err := s.validateStorageLocations(ctx, tmpl); err != nil {
		return nil, &SubmitValidationError{Cause: fmt.Errorf("openjd: submit: storage location validation: %w", err)}
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
			return nil, &SubmitValidationError{
				Cause: fmt.Errorf("openjd: submit: expand step %q: %w", stepTmpl.Name, err),
			}
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

// ── Storage location validation ────────────────────────────────────

// locRootCache maps location name → its roots map (or an error if not found).
type locRootCache map[string]locRootEntry

type locRootEntry struct {
	roots map[string]string
	err   error
}

// validateStorageLocations checks that every named storage location referenced
// via a "loc://" URI in the template:
//
//  1. Exists in the registry.
//  2. Has a root for every compute location a referencing step could run on,
//     or at minimum a "default" root when no affinity is declared.
//
// All problems are accumulated and returned as a single error.
func (s *Submitter) validateStorageLocations(ctx context.Context, tmpl *JobTemplate) error {
	allNames := ExtractTemplateLocRefs(tmpl)
	if len(allNames) == 0 {
		return nil
	}

	cache := s.buildLocRootCache(ctx, allNames)

	var errs []string
	errs = append(errs, s.validateStepLocRefs(tmpl.Steps, cache)...)
	errs = append(errs, s.validateJobLocRefs(tmpl, cache)...)
	errs = append(errs, unreportedCacheErrors(cache, errs)...)

	if len(errs) == 0 {
		return nil
	}
	sort.Strings(errs)
	return fmt.Errorf("%s", strings.Join(errs, "; "))
}

// buildLocRootCache fetches each referenced location once and returns a cache.
func (s *Submitter) buildLocRootCache(ctx context.Context, names []string) locRootCache {
	cache := make(locRootCache, len(names))
	for _, name := range names {
		loc, err := s.locSt.GetStorageLocationByName(ctx, name)
		if err != nil {
			cache[name] = locRootEntry{err: fmt.Errorf("location %q not found in registry", name)}
		} else {
			cache[name] = locRootEntry{roots: loc.Roots}
		}
	}
	return cache
}

// validateStepLocRefs checks root coverage for each step that references
// named storage locations.
func (*Submitter) validateStepLocRefs(steps []StepTemplate, cache locRootCache) []string {
	var errs []string
	for i, st := range steps {
		stepNames := ExtractStepLocRefs(st)
		if len(stepNames) == 0 {
			continue
		}
		_, computeLoc := toStoreHostRequirements(st.HostRequirements)
		for _, name := range stepNames {
			errs = append(errs, checkLocRootCoverage(i, name, computeLoc, cache)...)
		}
	}
	return errs
}

// validateJobLocRefs checks root coverage for locations referenced at job scope
// (job-parameter defaults and job environments). Because a job environment may
// run on a worker in any compute location, a job-scope reference requires a
// "default" root — the universal resolution fallback. This reuses the
// no-affinity coverage rule (missingRootMsg with an empty compute location).
func (*Submitter) validateJobLocRefs(tmpl *JobTemplate, cache locRootCache) []string {
	var errs []string
	for _, name := range ExtractJobLevelLocRefs(tmpl) {
		entry := cache[name]
		if entry.err != nil {
			// Existence errors are reported by unreportedCacheErrors.
			continue
		}
		if msg := missingRootMsg(name, "", entry.roots); msg != "" {
			errs = append(errs, "/job: "+msg)
		}
	}
	return errs
}

// checkLocRootCoverage returns any coverage error for a single location
// reference within a step.
func checkLocRootCoverage(stepIdx int, name, computeLoc string, cache locRootCache) []string {
	entry := cache[name]
	if entry.err != nil {
		return []string{fmt.Sprintf("/steps/%d: %v", stepIdx, entry.err)}
	}
	if msg := missingRootMsg(name, computeLoc, entry.roots); msg != "" {
		return []string{fmt.Sprintf("/steps/%d: %s", stepIdx, msg)}
	}
	return nil
}

// missingRootMsg returns a non-empty string when roots lacks coverage for
// computeLoc, or an empty string when coverage is adequate.
func missingRootMsg(name, computeLoc string, roots map[string]string) string {
	_, hasDefault := roots["default"]
	if computeLoc != "" {
		if _, hasSpecific := roots[computeLoc]; hasSpecific {
			return "" // compute-location-specific root present — OK
		}
		if hasDefault {
			return "" // falls back to default — OK
		}
		return fmt.Sprintf(
			"storage location %q has no root for compute location %q and no default root",
			name, computeLoc,
		)
	}
	// No affinity declared — a default root is required.
	if hasDefault {
		return ""
	}
	return fmt.Sprintf(
		"storage location %q has no default root (required when no compute location affinity is declared)",
		name,
	)
}

// unreportedCacheErrors returns errors for locations that failed lookup but
// were not already mentioned in the accumulated error list (e.g. locations
// referenced only in job-level parameters or environments, not in any step).
func unreportedCacheErrors(cache locRootCache, existing []string) []string {
	var errs []string
	for name, entry := range cache {
		if entry.err == nil {
			continue
		}
		quoted := fmt.Sprintf("%q", name)
		reported := false
		for _, msg := range existing {
			if strings.Contains(msg, quoted) {
				reported = true
				break
			}
		}
		if !reported {
			errs = append(errs, entry.err.Error())
		}
	}
	return errs
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// ── Host-requirements conversion ──────────────────────────────────────────────

// usagePoolPrefix is the amount-requirement name prefix that signals a
// usage pool capacity requirement in the OpenJD hostRequirements block.
// The pool name follows the prefix, e.g. "amount.worker.usagepool.arnold".
const usagePoolPrefix = "amount.worker.usagepool."

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
		if poolName, ok := strings.CutPrefix(a.Name, usagePoolPrefix); ok && poolName != "" {
			shr.UsagePools = append(shr.UsagePools, poolName)
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
