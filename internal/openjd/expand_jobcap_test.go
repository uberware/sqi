// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

// Tests for the job-wide task cap ([maxTasksPerJob]) -- EXPR sub-project H1,
// task 4. The cap is base-spec, always-on, and summed across a job's steps;
// none of these templates declare any extension.
//
// package openjd (not openjd_test) on purpose: the boundary test drives the
// unexported expansion entry point with a pre-loaded [jobTaskBudget], which is
// the only way to sit exactly ON the cap without materializing a million task
// rows.

import (
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
)

// jobWideCapTemplate is a two-step job whose task counts sum to
// maxTasksPerJob + 1: step "Tiny" has no parameter space (1 task) and step
// "Huge" is a 1,000 x 1,000 Cartesian product (exactly maxTasksPerStep).
//
// Every EXISTING limit is respected: 1,000 values per parameter is under
// maxTaskParamValues (1024), 1,000,000 tasks in one step is exactly
// maxTasksPerStep, and 2 steps is far under maxSteps. Only the job-wide sum is
// over. That is the whole point -- this template is legal without the cap.
//
// Step order matters for the test's cost: "Tiny" materializes its single row,
// then "Huge" is rejected ARITHMETICALLY, before any of its million rows is
// built. If this test ever becomes slow, the cap has stopped being checked
// before materialization.
const jobWideCapTemplate = `
specificationVersion: jobtemplate-2023-09
name: JobWideCapJob
steps:
  - name: Tiny
    script:
      actions:
        onRun:
          command: echo
  - name: Huge
    parameterSpace:
      taskParameterDefinitions:
        - name: A
          type: INT
          range: "1-1000"
        - name: B
          type: INT
          range: "2001-3000"
    script:
      actions:
        onRun:
          command: echo
`

// seedJobCapPrereqs inserts a farm and a queue into st and returns their IDs.
func seedJobCapPrereqs(t *testing.T, st *fake.Store) (farmID, queueID string) {
	t.Helper()
	ctx := t.Context()

	farm, err := st.CreateFarm(ctx, store.Farm{ID: "farm-jobcap", Name: "jobcap-farm"})
	if err != nil {
		t.Fatalf("CreateFarm: %v", err)
	}
	queue, err := st.CreateQueue(ctx, store.Queue{ID: "queue-jobcap", FarmID: farm.ID, Name: "jobcap-queue"})
	if err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}
	return farm.ID, queue.ID
}

// TestExpand_JobWideTaskCap pins the bound that maxTasksPerStep does not
// provide: maxSteps (100) multiplies straight through the per-step cap, so a
// template can reach 10^8 task rows from a single POST with every existing
// limit respected.
//
// It drives the whole submission pipeline rather than the guard in isolation,
// because a cap that is implemented but never threaded into Submit would bound
// nothing while passing any unit test of the guard itself.
func TestExpand_JobWideTaskCap(t *testing.T) {
	st := fake.New()
	farmID, queueID := seedJobCapPrereqs(t, st)
	sub := NewSubmitter(st)

	_, err := sub.Submit(t.Context(), jobWideCapTemplate, store.TemplateFormatYAML, SubmitOptions{
		FarmID:  farmID,
		QueueID: queueID,
		Owner:   "alice",
	})
	if err == nil {
		t.Fatal("a job summing to maxTasksPerJob+1 tasks was accepted; want it rejected")
	}
	if !strings.Contains(err.Error(), "across all steps") {
		t.Fatalf("error = %v; want it to name the JOB-WIDE limit, not the per-step one", err)
	}
	if strings.Contains(err.Error(), "parameter space expands to too many tasks") {
		t.Fatalf("error = %v; this is the per-step message, and the per-step cap is not what "+
			"this template breaches (each step is within it)", err)
	}
}

// TestExpand_JobWideTaskCapAllowsTheLimit pins the boundary from the other
// side: a job whose steps sum to exactly the cap is still accepted, so the
// check is not off by one.
//
// It pre-loads the budget rather than submitting a template that really
// produces a million tasks: the guard under test is arithmetic, and
// materializing 10^6 TaskParams rows to reach the same assertion would make
// this the slowest test in the package for no added coverage. The wiring from
// Submit down to this call site is what TestExpand_JobWideTaskCap covers.
func TestExpand_JobWideTaskCapAllowsTheLimit(t *testing.T) {
	three := "1-3"
	ps := &StepParameterSpace{
		TaskParameterDefinitions: []TaskParamDefinition{
			{Name: "A", Type: TaskParamTypeInt, RangeExpr: &three},
		},
	}

	// The preceding steps have used everything but three tasks; this step's
	// three land exactly ON the cap.
	budget := &jobTaskBudget{total: maxTasksPerJob - 3}
	rows, err := expandParameterSpace(ps, budget)
	if err != nil {
		t.Fatalf("a job summing to exactly maxTasksPerJob was rejected: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("expansion produced %d rows, want 3", len(rows))
	}
	if budget.total != maxTasksPerJob {
		t.Fatalf("budget total = %d, want exactly maxTasksPerJob (%d)", budget.total, maxTasksPerJob)
	}

	// One more task -- from any later step -- is one too many.
	one := "1-1"
	if _, err := expandParameterSpace(&StepParameterSpace{
		TaskParameterDefinitions: []TaskParamDefinition{
			{Name: "B", Type: TaskParamTypeInt, RangeExpr: &one},
		},
	}, budget); err == nil {
		t.Fatal("a job summing to maxTasksPerJob+1 was accepted; the cap is off by one")
	}
}

// threeStepJobTemplate is a job of three cheap steps covering all three shapes
// [expandParameterSpace] can return rows through: no parameterSpace at all
// (1 task), and two ordinary ranges (3 and 5 tasks). Nine tasks in total.
const threeStepJobTemplate = `
specificationVersion: jobtemplate-2023-09
name: ThreeStepJob
steps:
  - name: Bare
    script:
      actions:
        onRun:
          command: echo
  - name: Three
    parameterSpace:
      taskParameterDefinitions:
        - name: A
          type: INT
          range: "1-3"
    script:
      actions:
        onRun:
          command: echo
  - name: Five
    parameterSpace:
      taskParameterDefinitions:
        - name: B
          type: INT
          range: "1-5"
    script:
      actions:
        onRun:
          command: echo
`

// TestJobTaskBudget_ChargeEqualsRowsProduced pins the invariant the two cap
// tests above do not: that what the budget is CHARGED equals what expansion
// actually produces.
//
// Both of those sit at cap+1 or pre-load the budget directly, so neither would
// fail if a refactor charged one step twice (halving the real allowance) or
// added a fourth row-producing return to expandParameterSpace without a charge
// (leaving rows the cap cannot see). This walks all three of its current
// row-producing paths -- nil parameter space, empty combination expression, and
// the materialized product -- against ONE shared budget, exactly as a job's
// steps do.
func TestJobTaskBudget_ChargeEqualsRowsProduced(t *testing.T) {
	three, five := "1-3", "1-5"
	spaces := []*StepParameterSpace{
		nil, // the nil-ps return: one task
		{TaskParameterDefinitions: []TaskParamDefinition{
			{Name: "A", Type: TaskParamTypeInt, RangeExpr: &three},
		}},
		{TaskParameterDefinitions: []TaskParamDefinition{
			{Name: "B", Type: TaskParamTypeInt, RangeExpr: &five},
		}},
		{}, // no definitions: the empty-expression return, one task
	}

	budget := &jobTaskBudget{}
	produced := 0
	for i, ps := range spaces {
		rows, err := expandParameterSpace(ps, budget)
		if err != nil {
			t.Fatalf("step %d: expandParameterSpace: %v", i, err)
		}
		produced += len(rows)
	}

	if budget.total != produced {
		t.Fatalf("the job budget was charged %d tasks but expansion produced %d rows.\n"+
			"Charging MORE than is produced shrinks the real allowance below maxTasksPerJob "+
			"(%d) and rejects jobs that fit; charging LESS leaves rows the cap cannot see, "+
			"which is the exposure the cap exists to close.",
			budget.total, produced, maxTasksPerJob)
	}
}

// TestSubmit_ChargesEveryStepExactlyOnce is the same invariant one level up,
// where the budget is created: Submit walks the steps itself, so a step
// expanded twice would be charged twice.
//
// The budget is not observable after Submit returns -- it is a local, and
// deliberately so, since it must be per-submission. What IS observable is the
// rows it charged for, and a step expanded twice produces its tasks twice. Read
// with the test above, which pins charge == rows inside one expansion, that
// closes the loop.
func TestSubmit_ChargesEveryStepExactlyOnce(t *testing.T) {
	st := fake.New()
	farmID, queueID := seedJobCapPrereqs(t, st)

	result, err := NewSubmitter(st).Submit(t.Context(), threeStepJobTemplate, store.TemplateFormatYAML,
		SubmitOptions{FarmID: farmID, QueueID: queueID, Owner: "alice"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	const want = 1 + 3 + 5
	if len(result.Tasks) != want {
		t.Fatalf("Submit created %d tasks, want %d (1 + 3 + 5): a step expanded more than once "+
			"is charged more than once, so the job-wide cap would bound half the tasks it claims to",
			len(result.Tasks), want)
	}
}

// TestExpand_JobWideTaskCapAppliesWithoutEnforceLimits pins that the cap is
// always-on, matching maxTasksPerStep's stance: a caller that relaxes the
// quantitative POLICY limits does not thereby acquire the right to insert a
// hundred million rows.
func TestExpand_JobWideTaskCapAppliesWithoutEnforceLimits(t *testing.T) {
	st := fake.New()
	farmID, queueID := seedJobCapPrereqs(t, st)
	sub := NewSubmitterWithOptions(st, SubmitterOptions{EnforceLimits: false})

	_, err := sub.Submit(t.Context(), jobWideCapTemplate, store.TemplateFormatYAML, SubmitOptions{
		FarmID:  farmID,
		QueueID: queueID,
		Owner:   "alice",
	})
	if err == nil {
		t.Fatal("EnforceLimits: false accepted a job over maxTasksPerJob; the cap must be always-on")
	}
	if !strings.Contains(err.Error(), "across all steps") {
		t.Fatalf("error = %v; want the job-wide limit message", err)
	}
}
