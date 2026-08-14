// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
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
// Create one with [NewSubmitter] or [NewSubmitterWithOptions] and reuse it
// across requests.
type Submitter struct {
	st            store.Store
	locSt         store.StorageLocationStore
	enforceLimits bool
}

// SubmitterOptions carries optional configuration for a [Submitter].
type SubmitterOptions struct {
	// EnforceLimits controls whether quantitative OpenJD limit checks are run
	// during validation.  Mirrors [ValidateOptions.EnforceLimits].
	// Defaults to true when using [NewSubmitter].
	EnforceLimits bool
}

// NewSubmitter returns a [Submitter] backed by st with default options
// (EnforceLimits: true).  The store must implement [store.StorageLocationStore]
// (which [store.Store] always does) so that named-location references in
// submitted templates can be validated at submission time.
func NewSubmitter(st store.Store) *Submitter {
	return NewSubmitterWithOptions(st, SubmitterOptions{EnforceLimits: true})
}

// NewSubmitterWithOptions returns a [Submitter] backed by st with the supplied
// options.  Use this when the caller needs to control [SubmitterOptions.EnforceLimits]
// based on operator configuration.
func NewSubmitterWithOptions(st store.Store, opts SubmitterOptions) *Submitter {
	return &Submitter{st: st, locSt: st, enforceLimits: opts.EnforceLimits}
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
	// Name overrides the job name. When empty, the template's own name
	// (tmpl.Name) is used. Lets callers (e.g. product submissions) give each
	// job a distinct, human-meaningful name without editing the template.
	Name string
	// Parameters holds the caller-supplied values for job-level parameters
	// declared in the template's parameterDefinitions.  Keys are parameter
	// names; values are raw strings.  Missing entries are filled from each
	// parameter's Default; parameters with neither a supplied value nor a
	// default produce a [SubmitValidationError].
	Parameters map[string]string
	// MaxAttempts, RetryDelaySeconds, and FailureLimit are optional per-job
	// retry overrides. Nil means inherit (queue -> farm -> server default).
	MaxAttempts       *int
	RetryDelaySeconds *int
	FailureLimit      *int
	// DependsOn lists the IDs of upstream jobs this job must wait for (whole-job
	// cross-job dependencies). Each must already exist and be in the same farm;
	// none may already be failed or canceled. When any is not yet completed, the
	// job is created in store.JobStatusBlocked with all tasks held pending.
	DependsOn []string
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
	// BoundParameters is the fully-resolved name→value map produced by
	// [BindJobParameters].  Defaults from the template are merged in; every
	// declared parameter is guaranteed to have an entry here.
	// Later tasks ({{Param.*}} resolution, worker carry) consume this map.
	BoundParameters map[string]string
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
// Everything one submission creates — the job row, its cross-job dependency
// edges, every step and every task — is written by a single
// [store.JobStore.CreateJobSubmission] call, which is atomic on both store
// backends. A failure at any point therefore leaves nothing behind: there is no
// orphaned pending job for a sweep to reap, and no job missing the steps that
// failed to write (which checkJobCompletion, deriving job status from the steps
// that exist, would have reported completed).
//
// Expansion runs to completion before that write, so a template that cannot
// expand never reaches the store at all.
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
	if errs := ValidateWithOptions(tmpl, ValidateOptions{EnforceLimits: s.enforceLimits}); len(errs) > 0 {
		return nil, &SubmitValidationError{Cause: fmt.Errorf("openjd: submit: validation: %w", errs)}
	}

	// ── 2b. Validate named storage location coverage ────────────
	if err := s.validateStorageLocations(ctx, tmpl); err != nil {
		return nil, &SubmitValidationError{Cause: fmt.Errorf("openjd: submit: storage location validation: %w", err)}
	}

	// ── 2c. Bind job parameters ────────────────────────────────────────────
	boundParams, bindErrs := BindJobParameters(tmpl.ParameterDefinitions, opts.Parameters)
	if len(bindErrs) > 0 {
		return nil, &SubmitValidationError{Cause: fmt.Errorf("openjd: submit: parameter binding: %w", bindErrs)}
	}

	// ── 2d. Validate cross-job dependencies ────────────────────────────────
	blocked, err := s.resolveDependencies(ctx, opts.DependsOn, opts.FarmID)
	if err != nil {
		return nil, err
	}

	// ── 3. Resolve priority default ───────────────────────────────────────
	priority := opts.Priority
	if priority <= 0 {
		priority = 50
	}

	// ── 4. Build the Job row (verbatim template stored as-is) ─────────────
	now := time.Now().UTC()
	jobName := tmpl.Name
	if opts.Name != "" {
		jobName = opts.Name
	}
	// A job with an unsatisfied cross-job dependency is created blocked, in the
	// same write as the dependency edges that justify it — see step 6.
	jobStatus := store.JobStatusPending
	if blocked {
		jobStatus = store.JobStatusBlocked
	}
	job := store.Job{
		ID:             uuid.NewString(),
		FarmID:         opts.FarmID,
		QueueID:        opts.QueueID,
		Name:           jobName,
		Owner:          opts.Owner,
		Submitter:      opts.Submitter,
		Priority:       priority,
		Status:         jobStatus,
		Project:        opts.Project,
		RawTemplate:    rawTemplate,
		TemplateFormat: format,
		// Persist the fully-bound parameter values so the scheduler can carry
		// them to the worker without re-deriving from the raw template.
		Parameters:        boundParams,
		MaxAttempts:       opts.MaxAttempts,
		RetryDelaySeconds: opts.RetryDelaySeconds,
		FailureLimit:      opts.FailureLimit,
		CreatedAt:         now,
		UpdatedAt:         now,
	}

	// ── 5. Expand every step and task into memory ─────────────────────────
	// Nothing is written yet. Expansion runs to completion first so that a
	// template which cannot expand never reaches the store at all, and so that
	// everything this submission creates can be handed to a single write.
	// Each step is handled by a helper to keep Submit's cyclomatic complexity
	// within bounds.
	deriveBounds := tmpl.hasExtension("SQI_CHUNK_BOUNDS")
	steps := make([]store.Step, 0, len(tmpl.Steps))
	var tasks []store.Task
	for i, stepTmpl := range tmpl.Steps {
		step, stepTasks, err := s.buildStepWithTasks(job, stepTmpl, i, boundParams, deriveBounds, blocked, now)
		if err != nil {
			return nil, err
		}
		steps = append(steps, step)
		tasks = append(tasks, stepTasks...)
	}

	// ── 6. Persist the whole submission in one atomic write ───────────────
	//
	// The blocked status travels with the dependency edges, deliberately.
	//
	// It used to be written last, by a separate UpdateJobStatus after
	// everything else was durable, because Submit was not transactional and the
	// heartbeat sweep (scheduler.sweepBlockedJobs) scans for status=blocked jobs
	// and releases any whose edges are all satisfied. Creating the job
	// already-blocked let a sweep tick land in the window after the job row
	// existed but before its edges were written, see a blocked job with ZERO
	// edges, read that as "nothing left to wait on", and release it to pending.
	// Submit would then write the edges and pending tasks anyway, leaving a job
	// that is neither blocked nor scheduled — the sweep never revisits a
	// non-blocked job, so it hung forever.
	//
	// That window cannot exist now: the job row and its edges commit together,
	// so no sweep can observe one without the other, and the status write that
	// used to close the window is gone. It had a failure mode of its own —
	// succeeding here and then failing left the job stranded in pending with
	// pending tasks, which reconcileBlockedJob skips (it early-returns unless
	// the status is blocked) and the scheduler never leases.
	//
	// Splitting this back into separate writes recreates one hang or the other.
	out, err := s.st.CreateJobSubmission(ctx, store.JobSubmission{
		Job:       job,
		DependsOn: opts.DependsOn,
		Steps:     steps,
		Tasks:     tasks,
	})
	if err != nil {
		return nil, fmt.Errorf("openjd: submit: create job: %w", err)
	}

	return &SubmitResult{
		Job:             out.Job,
		Steps:           out.Steps,
		Tasks:           out.Tasks,
		BoundParameters: boundParams,
	}, nil
}

// buildStepWithTasks builds one [store.Step] value and all of its [store.Task]
// values for a single step template. It performs NO store writes: everything
// one submission creates is written by a single
// [store.JobStore.CreateJobSubmission] call in [Submitter.Submit], so a failure
// anywhere — including in this function's expansion — leaves nothing behind.
//
// It is extracted from [Submitter.Submit] to reduce that function's cyclomatic
// complexity.
func (s *Submitter) buildStepWithTasks(
	job store.Job,
	stepTmpl StepTemplate,
	stepIdx int,
	boundParams map[string]string,
	deriveBounds bool,
	holdPending bool,
	now time.Time,
) (step store.Step, tasks []store.Task, err error) {
	// Collect dependency names from the template.
	dependsOn := make([]string, 0, len(stepTmpl.Dependencies))
	for _, dep := range stepTmpl.Dependencies {
		dependsOn = append(dependsOn, dep.DependsOn)
	}

	// Initial step status: ready immediately when there are no deps AND the job
	// is not blocked on a cross-job dependency.
	stepStatus := store.StepStatusReady
	if holdPending || len(dependsOn) > 0 {
		stepStatus = store.StepStatusPending
	}

	hostReqs, computeLoc := toStoreHostRequirements(stepTmpl.HostRequirements)

	step = store.Step{
		ID:               uuid.NewString(),
		JobID:            job.ID,
		Name:             stepTmpl.Name,
		DependsOn:        dependsOn,
		StepOrder:        stepIdx,
		Status:           stepStatus,
		HostRequirements: hostReqs,
		ComputeLocation:  computeLoc,
		CreatedAt:        now,
		UpdatedAt:        now,
	}

	// Task status mirrors the step's initial status.
	taskStatus := store.TaskStatusReady
	if stepStatus == store.StepStatusPending {
		taskStatus = store.TaskStatusPending
	}

	// ── Expand parameter space ──────────────────────────────────────────────
	taskParamList, err := s.expandStepTaskParams(stepTmpl, stepIdx, boundParams, deriveBounds)
	if err != nil {
		return store.Step{}, nil, err
	}

	// ── Build one Task row per parameter combination ────────────────────────
	var reqCores *int
	if hostReqs != nil {
		reqCores = requiredCoresFromAmounts(hostReqs.Amounts)
	}

	tasks = make([]store.Task, 0, len(taskParamList))
	for j, params := range taskParamList {
		tasks = append(tasks, store.Task{
			ID:            uuid.NewString(),
			JobID:         job.ID,
			StepID:        step.ID,
			Name:          buildTaskName(stepTmpl.Name, j, params),
			Parameters:    params,
			Status:        taskStatus,
			RequiredCores: reqCores,
			CreatedAt:     now,
			UpdatedAt:     now,
		})
	}

	return step, tasks, nil
}

// expandStepTaskParams resolves {{Param.*}} / {{RawParam.*}} references in the
// step's parameter space, re-validates the resolved space's quantitative
// limits, expands it into one parameter set per task, and derives chunk
// bounds when requested. It is extracted from [Submitter.buildStepWithTasks]
// to keep that function's cyclomatic complexity within bounds.
func (s *Submitter) expandStepTaskParams(
	stepTmpl StepTemplate,
	stepIdx int,
	boundParams map[string]string,
	deriveBounds bool,
) ([]TaskParams, error) {
	// Resolve {{Param.*}} / {{RawParam.*}} in the parameter space first.
	resolvedPS, resolveErrs := ResolveParameterSpaceParams(stepTmpl.ParameterSpace, boundParams)
	if len(resolveErrs) > 0 {
		stepPrefix := fmt.Sprintf("/steps/%d", stepIdx)
		for k := range resolveErrs {
			resolveErrs[k].Pointer = stepPrefix + resolveErrs[k].Pointer
		}
		return nil, &SubmitValidationError{
			Cause: fmt.Errorf("openjd: submit: resolve step %q parameter space: %w", stepTmpl.Name, resolveErrs),
		}
	}

	// Re-run the gated per-parameter value-count and overlap limits on the
	// RESOLVED space. Validation (step 2) runs on the unresolved template and
	// skips ranges containing {{...}}; without this re-check a parameterized
	// range like "{{Param.Start}}-{{Param.End}}" would bypass maxTaskParamValues
	// and overlap detection entirely. Gated by enforceLimits to match validation.
	if s.enforceLimits && resolvedPS != nil {
		stepPrefix := fmt.Sprintf("/steps/%d", stepIdx)
		if errs := validateParameterSpaceLimits(*resolvedPS, stepPrefix+"/parameterSpace"); len(errs) > 0 {
			return nil, &SubmitValidationError{
				Cause: fmt.Errorf("openjd: submit: step %q resolved parameter space: %w", stepTmpl.Name, errs),
			}
		}
	}

	taskParamList, err := ExpandParameterSpace(resolvedPS)
	if err != nil {
		return nil, &SubmitValidationError{
			Cause: fmt.Errorf("openjd: submit: expand step %q: %w", stepTmpl.Name, err),
		}
	}

	if deriveBounds {
		DeriveChunkBounds(taskParamList, resolvedPS)
	}

	return taskParamList, nil
}

// resolveDependencies validates the requested cross-job dependencies against the
// store and reports whether the new job must start blocked. It returns a
// *SubmitValidationError (client fault) when a dependency is missing, in a
// different farm, or already terminated unsuccessfully.
func (s *Submitter) resolveDependencies(ctx context.Context, dependsOn []string, farmID string) (blocked bool, err error) {
	seen := make(map[string]struct{}, len(dependsOn))
	for _, id := range dependsOn {
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}

		up, gerr := s.st.GetJob(ctx, id)
		if errors.Is(gerr, store.ErrNotFound) {
			return false, &SubmitValidationError{Cause: fmt.Errorf("openjd: submit: depends_on job %q not found", id)}
		}
		if gerr != nil {
			return false, fmt.Errorf("openjd: submit: look up depends_on job %q: %w", id, gerr)
		}
		if up.FarmID != farmID {
			return false, &SubmitValidationError{Cause: fmt.Errorf("openjd: submit: depends_on job %q is in a different farm", id)}
		}
		switch up.Status {
		case store.JobStatusFailed, store.JobStatusCanceled:
			return false, &SubmitValidationError{Cause: fmt.Errorf("openjd: submit: depends_on job %q already terminated unsuccessfully (%s)", id, up.Status)}
		case store.JobStatusCompleted:
			// already satisfied — does not block
		default:
			blocked = true
		}
	}
	return blocked, nil
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
		// Capability names are case-insensitive (OpenJD jobtemplate-2023-09), so
		// detect the usagepool namespace prefix case-insensitively. The pool name
		// after the prefix keeps its original case — it identifies an
		// operator-registered usage pool.
		if hasPrefixFold(a.Name, usagePoolPrefix) {
			if poolName := a.Name[len(usagePoolPrefix):]; poolName != "" {
				shr.UsagePools = append(shr.UsagePools, poolName)
			}
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
		if strings.EqualFold(a.Name, computeLocationAttr) {
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

// hasPrefixFold reports whether s begins with prefix, compared case-insensitively.
func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

// requiredCoresFromAmounts extracts the task CPU reservation from a step's host
// requirements: the floor of amount.worker.vcpu min, parsed as an int.
//
// A present amount.worker.vcpu with no explicit min defaults to a 1-core
// reservation: per OpenJD jobtemplate-2023-09, an omitted min defaults to the
// capability's reserved minimum, which is 1 for amount.worker.vcpu (see
// reservedAmountMinimums in validate.go). Returns nil only when the step
// declares no amount.worker.vcpu at all (undeclared = full machine at lease
// time) or when an explicit min cannot be parsed as a positive number.
func requiredCoresFromAmounts(amts []store.StepAmountRequirement) *int {
	for _, a := range amts {
		if !strings.EqualFold(a.Name, "amount.worker.vcpu") {
			continue
		}
		if a.Min == nil {
			n := 1 // spec default: reserved minimum for amount.worker.vcpu
			return &n
		}
		f, err := strconv.ParseFloat(*a.Min, 64)
		if err != nil || f <= 0 {
			return nil
		}
		n := max(int(f), 1) // floor; a 2.5-core reservation reserves whole cores conservatively
		return &n
	}
	return nil
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
