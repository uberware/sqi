// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

import (
	"context"
	"encoding/json"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/metrics"
	"github.com/uberware/sqi/internal/openjd"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
	"github.com/uberware/sqi/internal/worker/fmtres"
	"github.com/uberware/sqi/internal/worker/protocol"
	"github.com/uberware/sqi/internal/ws"
)

// This file is EXPR sub-project E4d Task 3: the cross-binary invariant that
// E4d Tasks 1 and 2 made breakable by configuration.
//
// THE FAILURE IT PREVENTS, measured on this branch (design spec §2): when the
// server allowed 10,000 expression positions and the worker allowed 5,000, a
// job with one 5,000-variable environment was ACCEPTED, CREATED AND PERSISTED,
// and then EVERY TASK IN IT FAILED at runtime naming a budget the submitter
// never saw. E4c closed it by relating the two CONSTANTS
// (internal/openjd's TestTemplateBudget_WorkerCapIsNotTighter). Tasks 1 and 2
// turned both constants into operator configuration, so that test now compares
// two DEFAULTS and a farm's YAML can violate the relation with it green.
//
// The mechanism: the worker advertises its own caps in its registration
// message, the server persists them on the worker record, and the scheduler
// REFUSES TO DISPATCH an EXPR job to a worker whose advertised caps are below
// the server's configured limits. The refusal is the bound; the
// UnschedulableReason the sweep writes is what an operator sees.
//
// The tests below are the "test that fails when the relation is violated
// through CONFIGURATION, not only through constants" that design spec §2
// requires. TestExprCaps_ViolationThroughConfigurationIsCaught is the one that
// names the original incident; the rest fence it in (defaults unaffected,
// base-spec work unaffected, both call sites covered).

// exprTemplateJSON is a minimal template that DECLARES the EXPR extension, so
// the dispatch gate treats jobs built from it as capable of phase-3 (worker
// side) expression evaluation.
const exprTemplateJSON = `{
  "specificationVersion": "jobtemplate-2023-09",
  "extensions": ["EXPR"],
  "name": "j",
  "steps": [
    {
      "name": "render",
      "script": {
        "actions": {
          "onRun": { "command": "render" }
        }
      }
    }
  ]
}`

// seedExprLeaseFixture seeds a farm/queue/job/step/ready-task plus one online
// worker advertising workerCaps, using rawTemplate as the job's template.
// It returns the worker and the task ID.
func seedExprLeaseFixture(
	t *testing.T,
	st *fake.Store,
	rawTemplate string,
	workerCaps store.WorkerExprLimits,
) (store.Worker, string) {
	t.Helper()
	ctx := t.Context()
	now := time.Now().UTC()
	if _, err := st.CreateFarm(ctx, store.Farm{ID: "f1", Name: "F1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateQueue(ctx, store.Queue{ID: "q1", FarmID: "f1", Name: "Q1"}); err != nil {
		t.Fatal(err)
	}
	w, err := st.RegisterWorker(ctx, store.Worker{
		ID: "w1", FarmID: "f1", Hostname: "h1", Status: store.WorkerStatusOnline,
		CPUCount: 4, LastHeartbeatAt: &now, Tags: map[string]string{},
		ExprLimits: workerCaps,
	})
	if err != nil {
		t.Fatal(err)
	}
	job, err := st.CreateJob(ctx, store.Job{
		ID: uuid.NewString(), FarmID: "f1", QueueID: "q1", Name: "j",
		Status: store.JobStatusRunning, TemplateFormat: store.TemplateFormatJSON,
		RawTemplate: rawTemplate,
		CreatedAt:   now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	step, err := st.CreateStep(ctx, store.Step{
		ID: uuid.NewString(), JobID: job.ID, Name: "render",
		Status: store.StepStatusReady, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	one := 1
	tk, err := st.CreateTask(ctx, store.Task{
		ID: uuid.NewString(), JobID: job.ID, StepID: step.ID,
		Name: "t", Status: store.TaskStatusReady, Parameters: map[string]string{},
		RequiredCores: &one, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return w, tk.ID
}

// schedulerWithExprLimits returns a scheduler whose server-side expression
// limits are lim — i.e. the values openjd.expr_* would have produced.
func schedulerWithExprLimits(st store.Store, lim openjd.ExprLimits) *Scheduler {
	s := newMetricsScheduler(st, &recordBus{}, "f1")
	s.cfg.ExprLimits = lim.Normalized()
	return s
}

// leaseOnce runs one lease request for w and returns the number of assignments
// the scheduler handed out.
func leaseOnce(t *testing.T, s *Scheduler, w store.Worker) int {
	t.Helper()
	batch, err := s.selectLeaseBatch(t.Context(), w)
	if err != nil {
		t.Fatalf("selectLeaseBatch: %v", err)
	}
	return len(batch)
}

// TestExprCaps_ViolationThroughConfigurationIsCaught is design spec §2's
// non-negotiable requirement: the relation must be guarded by something a
// CONFIGURATION can trip, not only a constant.
//
// The row that matters is "server raised above the worker's default": the
// operator sets openjd.expr_template_positions to 50,000 and leaves a worker
// at the shipped 10,000. Before this task that farm accepted a template
// costing 20,000 positions and then failed every task of it on that worker.
// Now the scheduler refuses to lease the job to that worker at all.
//
// Every dimension is exercised separately so that removing any one comparison
// fails exactly one sub-test -- five dimensions sharing one row are one
// dimension.
func TestExprCaps_ViolationThroughConfigurationIsCaught(t *testing.T) {
	dflt := openjd.DefaultExprLimits()
	workerDefaults := store.WorkerExprLimits{
		OperationLimit:          fmtres.DefaultExprLimits().OperationLimit,
		MemoryLimit:             fmtres.DefaultExprLimits().MemoryLimit,
		AssignmentPositions:     fmtres.DefaultExprLimits().AssignmentPositions,
		AssignmentRetainedBytes: fmtres.DefaultExprLimits().AssignmentRetainedBytes,
		LetRetainedBytes:        fmtres.DefaultExprLimits().LetRetainedBytes,
	}

	tests := []struct {
		name string
		// serverLimits is the openjd.expr_* configuration.
		serverLimits openjd.ExprLimits
		// workerCaps is what the worker's expr.* configuration advertises.
		workerCaps store.WorkerExprLimits
		wantBlock  bool
		// wantIn, when blocking, must appear in the operator-facing reason.
		wantIn []string
	}{
		{
			name:         "both sides at their defaults dispatches",
			serverLimits: dflt,
			workerCaps:   workerDefaults,
			wantBlock:    false,
		},
		{
			name:         "worker advertising nothing dispatches at server defaults",
			serverLimits: dflt,
			workerCaps:   store.WorkerExprLimits{},
			wantBlock:    false,
		},
		{
			name:         "server positions raised above the worker's blocks",
			serverLimits: withPositions(dflt, 50_000),
			workerCaps:   workerDefaults,
			wantBlock:    true,
			wantIn:       []string{"expression positions", "10000", "50000"},
		},
		{
			name:         "worker positions tightened below the server's blocks",
			serverLimits: dflt,
			workerCaps:   withWorkerPositions(workerDefaults, fmtres.MinExprAssignmentPositions),
			wantBlock:    true,
			wantIn:       []string{"expression positions", "2000", "10000"},
		},
		{
			name:         "server positions raised to exactly the worker's cap dispatches",
			serverLimits: withPositions(dflt, workerDefaults.AssignmentPositions),
			workerCaps:   workerDefaults,
			wantBlock:    false,
		},
		{
			name:         "server positions one above the worker's cap blocks",
			serverLimits: withPositions(dflt, workerDefaults.AssignmentPositions+1),
			workerCaps:   workerDefaults,
			wantBlock:    true,
			wantIn:       []string{"10001"},
		},
		{
			name:         "server operation limit raised above the worker's blocks",
			serverLimits: withOperations(dflt, openjd.MaxExprSubmissionOperations),
			workerCaps:   withWorkerOperations(workerDefaults, fmtres.MinExprOperationLimit),
			wantBlock:    true,
			wantIn:       []string{"operations", "10000", "100000"},
		},
		{
			name:         "server memory limit raised above the worker's blocks",
			serverLimits: withMemory(dflt, openjd.MaxExprSubmissionMemoryBytes),
			workerCaps:   withWorkerMemory(workerDefaults, fmtres.MinExprMemoryLimit),
			wantBlock:    true,
			wantIn:       []string{"live bytes", "1000000", "10000000"},
		},
		{
			name:         "worker retained-bytes cap tightened below the server's blocks",
			serverLimits: dflt,
			workerCaps:   withWorkerRetained(workerDefaults, fmtres.MinExprAssignmentRetainedBytes),
			wantBlock:    true,
			wantIn:       []string{"retain", "2000000", "10000000"},
		},
		// The dimension E4d Task 3 left out, and the configuration the wave's
		// final review reached it through. Server: expr_memory_limit at its
		// legal maximum, everything else default. Worker: let_retained_bytes at
		// its legal floor, everything else default. All four ORIGINAL
		// comparisons pass (memory 20,000,000 >= 10,000,000; operations
		// 1,000,000 >= 10,000; positions 10,000 >= 10,000; assignment retention
		// 20,000,000 >= 10,000,000) -- so before the fifth comparison this farm
		// dispatched, and a step with `let: [big = "x" * 5000000]` was accepted
		// at 5,000,064 live bytes (inside the raised 10 MB per-evaluation
		// budget, charged against the 10 MB template-wide budget) and then
		// failed EVERY task of the job on that host at 5,000,064 > 1,000,000.
		{
			name:         "worker let-retention tightened below the server's template budget blocks",
			serverLimits: withMemory(dflt, openjd.MaxExprSubmissionMemoryBytes),
			workerCaps:   withWorkerLetRetained(workerDefaults, fmtres.MinExprLetRetainedBytes),
			wantBlock:    true,
			wantIn:       []string{"one symbol table", "1000000", "10000000", "expr.let_retained_bytes"},
		},
		// The same shortfall with NOTHING raised on the server: the floor of
		// expr.let_retained_bytes is a tenth of the server's DEFAULT
		// template-wide retention, so this needs no server misconfiguration at
		// all -- only a worker tightened to a value its own config layer
		// accepts.
		{
			name:         "worker let-retention at its floor blocks even at server defaults",
			serverLimits: dflt,
			workerCaps:   withWorkerLetRetained(workerDefaults, fmtres.MinExprLetRetainedBytes),
			wantBlock:    true,
			wantIn:       []string{"one symbol table", "1000000", "10000000"},
		},
		{
			name:         "worker let-retention exactly at the server's template budget dispatches",
			serverLimits: dflt,
			workerCaps: withWorkerLetRetained(
				workerDefaults, openjd.DefaultExprLimits().TemplateRetainedBytes,
			),
			wantBlock: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := fake.New()
			w, taskID := seedExprLeaseFixture(t, st, exprTemplateJSON, tc.workerCaps)
			s := schedulerWithExprLimits(st, tc.serverLimits)

			leased := leaseOnce(t, s, w)
			if tc.wantBlock && leased != 0 {
				t.Fatalf("leased %d assignments to a worker whose EXPR caps are below the "+
					"server's configured limits; want 0 -- this is the accepted-then-failed-"+
					"per-task incident design spec §2 records", leased)
			}
			if !tc.wantBlock && leased != 1 {
				t.Fatalf("leased %d assignments, want 1: a configuration that SATISFIES the "+
					"relation must not be blocked", leased)
			}
			if !tc.wantBlock {
				return
			}

			// The bound must also be visible to the operator, on the task, not
			// only in a log line nobody reads.
			s.reconcileTaskSchedulability(t.Context(), mustTask(t, st, taskID), []store.Worker{w})
			got := mustTask(t, st, taskID).UnschedulableReason
			if got == "" {
				t.Fatal("task carries no UnschedulableReason: a refusal to dispatch that the " +
					"operator cannot see is a silent stall")
			}
			for _, want := range tc.wantIn {
				if !strings.Contains(got, want) {
					t.Errorf("UnschedulableReason %q does not contain %q", got, want)
				}
			}
		})
	}
}

// TestExprCaps_BaseSpecWorkIsUnaffected pins that the gate costs a
// deliberately-tightened worker only EXPR work. Removing the whole host from
// the farm over a limit that only applies to EXPR templates would be a far
// worse outage than the one being prevented, and §1 chose per-worker
// configuration precisely so a small host CAN be sized down.
func TestExprCaps_BaseSpecWorkIsUnaffected(t *testing.T) {
	st := fake.New()
	w, _ := seedExprLeaseFixture(t, st, minimalRenderJSON, store.WorkerExprLimits{
		OperationLimit:          fmtres.MinExprOperationLimit,
		MemoryLimit:             fmtres.MinExprMemoryLimit,
		AssignmentPositions:     fmtres.MinExprAssignmentPositions,
		AssignmentRetainedBytes: fmtres.MinExprAssignmentRetainedBytes,
	})
	s := schedulerWithExprLimits(st, withPositions(openjd.DefaultExprLimits(), openjd.MaxExprTemplatePositions))

	if leased := leaseOnce(t, s, w); leased != 1 {
		t.Fatalf("a base-spec job was not leased (%d assignments) to a worker whose EXPR caps "+
			"are below the server's: the gate must narrow to EXPR templates", leased)
	}
}

// TestExprCaps_HeterogeneousFarmKeepsTheCapableWorker pins the property that
// made a per-task refusal the right shape: with one tight worker and one
// capable worker, the EXPR job runs on the capable one and no task is flagged.
func TestExprCaps_HeterogeneousFarmKeepsTheCapableWorker(t *testing.T) {
	st := fake.New()
	tight, taskID := seedExprLeaseFixture(t, st, exprTemplateJSON, store.WorkerExprLimits{
		OperationLimit:          fmtres.MinExprOperationLimit,
		MemoryLimit:             fmtres.MinExprMemoryLimit,
		AssignmentPositions:     fmtres.MinExprAssignmentPositions,
		AssignmentRetainedBytes: fmtres.MinExprAssignmentRetainedBytes,
	})
	now := time.Now().UTC()
	big, err := st.RegisterWorker(t.Context(), store.Worker{
		ID: "w2", FarmID: "f1", Hostname: "h2", Status: store.WorkerStatusOnline,
		CPUCount: 4, LastHeartbeatAt: &now, Tags: map[string]string{},
		ExprLimits: store.WorkerExprLimits{
			OperationLimit:          fmtres.MaxExprOperationLimit,
			MemoryLimit:             fmtres.MaxExprMemoryLimit,
			AssignmentPositions:     fmtres.MaxExprAssignmentPositions,
			AssignmentRetainedBytes: fmtres.MaxExprAssignmentRetainedBytes,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := schedulerWithExprLimits(st, withPositions(openjd.DefaultExprLimits(), openjd.MaxExprTemplatePositions))

	if leased := leaseOnce(t, s, tight); leased != 0 {
		t.Fatalf("the tight worker was leased %d EXPR assignments, want 0", leased)
	}
	if leased := leaseOnce(t, s, big); leased != 1 {
		t.Fatalf("the capable worker was leased %d EXPR assignments, want 1", leased)
	}

	// With a capable worker online the task is schedulable, so the sweep must
	// not flag it.
	s.reconcileTaskSchedulability(t.Context(), mustTask(t, st, taskID), []store.Worker{tight, big})
	if got := mustTask(t, st, taskID).UnschedulableReason; got != "" {
		t.Errorf("UnschedulableReason = %q, want empty: one capable worker makes the task schedulable", got)
	}
}

// TestExprCapShortfall_Table exercises the comparison itself at its boundary,
// independently of the store and the scheduler. Equality must PASS: the
// relation is worker >= server, so a worker configured to exactly the server's
// value can run everything the server accepted.
func TestExprCapShortfall_Table(t *testing.T) {
	srv := openjd.ExprLimits{
		SubmissionOperations:  5_000,
		SubmissionMemoryBytes: 500_000,
		TemplatePositions:     5_000,
		TemplateRetainedBytes: 5_000_000,
	}
	exact := store.WorkerExprLimits{
		OperationLimit:          5_000,
		MemoryLimit:             500_000,
		AssignmentPositions:     5_000,
		AssignmentRetainedBytes: 5_000_000,
		LetRetainedBytes:        5_000_000,
	}

	tests := []struct {
		name  string
		caps  store.WorkerExprLimits
		short bool
	}{
		{"exactly equal in every dimension is not a shortfall", exact, false},
		{"one operation below", withWorkerOperations(exact, 4_999), true},
		{"one live byte below", withWorkerMemory(exact, 499_999), true},
		{"one position below", withWorkerPositions(exact, 4_999), true},
		{"one retained byte below", withWorkerRetained(exact, 4_999_999), true},
		{"one let-retained byte below", withWorkerLetRetained(exact, 4_999_999), true},
		{"above in every dimension", store.WorkerExprLimits{
			OperationLimit:          6_000,
			MemoryLimit:             600_000,
			AssignmentPositions:     6_000,
			AssignmentRetainedBytes: 6_000_000,
			LetRetainedBytes:        6_000_000,
		}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := exprCapShortfall(tc.caps, srv)
			if tc.short && got == "" {
				t.Fatal("want a shortfall, got none")
			}
			if !tc.short && got != "" {
				t.Fatalf("want no shortfall, got %q", got)
			}
		})
	}
}

// TestExprCaps_UnadvertisedCapsAreThePreE4dDefaults pins the decoding
// convention for a worker that sends no caps at all, and pins it against
// fmtres rather than against a literal.
//
// A worker built from Task 3 onward always sends validated non-zero values (its
// config layer rejects 0), so silence means an older binary. For one built
// before Task 2 the assumption is exact -- the limits were fixed constants, and
// Task 2's TestExprLimits_DefaultsMatchPreE4dConstants pins that today's
// defaults still equal them. For one built BETWEEN Tasks 2 and 3 it is a guess,
// because that binary's limits were already configurable; see
// legacyWorkerExprCaps for why that residual is accepted (it cannot exist
// outside this unreleased branch, and both alternatives are worse).
func TestExprCaps_UnadvertisedCapsAreThePreE4dDefaults(t *testing.T) {
	d := fmtres.DefaultExprLimits()
	got := workerExprCapsOrLegacy(store.WorkerExprLimits{})
	want := store.WorkerExprLimits{
		OperationLimit:          d.OperationLimit,
		MemoryLimit:             d.MemoryLimit,
		AssignmentPositions:     d.AssignmentPositions,
		AssignmentRetainedBytes: d.AssignmentRetainedBytes,
		LetRetainedBytes:        d.LetRetainedBytes,
	}
	if got != want {
		t.Fatalf("unadvertised worker caps resolve to %+v, want the worker's own defaults %+v", got, want)
	}
}

// TestExprCaps_RelationIsSatisfiableAtEveryLegalServerSetting is the
// configuration-range half of the invariant, and it is what stops this task's
// gate from becoming a trap: for every dimension, the worker's configurable
// CEILING must be at least the server's, or an operator could choose a legal
// server value that NO legal worker configuration can match -- at which point
// the gate would refuse every worker in the farm and EXPR work would never run.
//
// E4d Task 2 established this for positions; it holds for the other three by
// their existing ranges, and this pins all five so a later range change cannot
// break it silently.
func TestExprCaps_RelationIsSatisfiableAtEveryLegalServerSetting(t *testing.T) {
	tests := []struct {
		dimension          string
		workerMax, srvMax  int64
		workerDef, srvDef  int64
		serverKey, workKey string
	}{
		{
			dimension: "positions",
			workerMax: fmtres.MaxExprAssignmentPositions, srvMax: openjd.MaxExprTemplatePositions,
			workerDef: fmtres.DefaultExprLimits().AssignmentPositions,
			srvDef:    openjd.DefaultExprLimits().TemplatePositions,
			serverKey: "openjd.expr_template_positions", workKey: "expr.assignment_positions",
		},
		{
			dimension: "operations",
			workerMax: fmtres.MaxExprOperationLimit, srvMax: openjd.MaxExprSubmissionOperations,
			workerDef: fmtres.DefaultExprLimits().OperationLimit,
			srvDef:    openjd.DefaultExprLimits().SubmissionOperations,
			serverKey: "openjd.expr_operation_limit", workKey: "expr.operation_limit",
		},
		{
			dimension: "memory", workerMax: fmtres.MaxExprMemoryLimit, srvMax: openjd.MaxExprSubmissionMemoryBytes,
			workerDef: fmtres.DefaultExprLimits().MemoryLimit,
			srvDef:    openjd.DefaultExprLimits().SubmissionMemoryBytes,
			serverKey: "openjd.expr_memory_limit", workKey: "expr.memory_limit",
		},
		{
			dimension: "retained bytes",
			workerMax: fmtres.MaxExprAssignmentRetainedBytes, srvMax: openjd.MaxExprTemplateRetainedBytes,
			workerDef: fmtres.DefaultExprLimits().AssignmentRetainedBytes,
			srvDef:    openjd.DefaultExprLimits().TemplateRetainedBytes,
			serverKey: "openjd.expr_template_retained_bytes", workKey: "expr.assignment_retained_bytes",
		},
		{
			// The fifth dimension is compared against the SAME server key as
			// the fourth, so satisfiability is not inherited -- expr's
			// per-table ceiling is its own number and could be lowered
			// independently.
			dimension: "let retained bytes",
			workerMax: fmtres.MaxExprLetRetainedBytes, srvMax: openjd.MaxExprTemplateRetainedBytes,
			workerDef: fmtres.DefaultExprLimits().LetRetainedBytes,
			srvDef:    openjd.DefaultExprLimits().TemplateRetainedBytes,
			serverKey: "openjd.expr_template_retained_bytes", workKey: "expr.let_retained_bytes",
		},
	}
	for _, tc := range tests {
		t.Run(tc.dimension, func(t *testing.T) {
			if tc.workerMax < tc.srvMax {
				t.Errorf("%s ceiling: %s may be set as high as %d but %s may not exceed %d, "+
					"so a legal server setting exists that no worker can match and the dispatch "+
					"gate would starve every EXPR job in the farm",
					tc.dimension, tc.serverKey, tc.srvMax, tc.workKey, tc.workerMax)
			}
			if tc.workerDef < tc.srvDef {
				t.Errorf("%s default: a fresh install would violate the relation on its own "+
					"(%s=%d, %s=%d)", tc.dimension, tc.workKey, tc.workerDef, tc.serverKey, tc.srvDef)
			}
		})
	}
}

// TestHandleWorkerRegister_PersistsAdvertisedExprCaps pins the server half of
// the registration hop: a worker's advertised EXPR caps must reach the worker
// record, because that record is what BOTH gate call sites read. Dropped, they
// read back as zero -- "not advertised" -- and a genuinely tight worker is
// handed EXPR work it cannot run, which is the incident this whole file exists
// for.
//
// It also drives the message through handleWorkerMessage rather than calling
// the handler directly, so the JSON tag on the scheduler's own RegisterMsg is
// exercised end to end.
func TestHandleWorkerRegister_PersistsAdvertisedExprCaps(t *testing.T) {
	st := fake.New()
	s := newMetricsScheduler(st, &recordBus{}, "")

	want := store.WorkerExprLimits{
		OperationLimit:          11_111,
		MemoryLimit:             2_222_222,
		AssignmentPositions:     3_333,
		AssignmentRetainedBytes: 4_444_444,
	}
	msg := &fakeJSMsg{
		subject: bus.SubjectWorkerRegister,
		data: workerMsgJSON(t, RegisterMsg{
			WorkerID: "w-1", FarmID: "farm-1", Hostname: "node-1", OS: "linux",
			ExprLimits: want,
		}),
	}
	s.handleWorkerMessage(msg)

	w, err := st.GetWorker(t.Context(), "w-1")
	if err != nil {
		t.Fatalf("GetWorker: %v", err)
	}
	if w.ExprLimits != want {
		t.Fatalf("persisted ExprLimits = %+v, want %+v", w.ExprLimits, want)
	}
}

// TestHandleWorkerRegister_ExprCapWarningIsDeDuplicated pins the registration
// diagnostic's noise control (post-review MINOR 7). SetupReconnectHook
// re-registers on every NATS reconnect, and on a farm that never submits an
// EXPR template the warning is true but vacuous, so it must not accumulate one
// line per reconnect. It must still re-report when the shortfall CHANGES --
// either side reconfigured is news.
func TestHandleWorkerRegister_ExprCapWarningIsDeDuplicated(t *testing.T) {
	st := fake.New()
	logs := &countingHandler{}
	s := New(schedulerConfigWithPositions(50_000), st, &recordBus{},
		metrics.New(), slog.New(logs), ws.NoopNotifier{}, nil)
	s.ctx = t.Context()

	register := func(positions int64) {
		s.handleWorkerMessage(&fakeJSMsg{
			subject: bus.SubjectWorkerRegister,
			data: workerMsgJSON(t, RegisterMsg{
				WorkerID: "w-1", FarmID: "farm-1", Hostname: "node-1", OS: "linux",
				ExprLimits: store.WorkerExprLimits{
					OperationLimit:          fmtres.DefaultExprLimits().OperationLimit,
					MemoryLimit:             fmtres.DefaultExprLimits().MemoryLimit,
					AssignmentPositions:     positions,
					AssignmentRetainedBytes: fmtres.DefaultExprLimits().AssignmentRetainedBytes,
				},
			}),
		})
	}

	register(10_000) // short: 10,000 < the server's configured 50,000
	if got := logs.warns; got != 1 {
		t.Fatalf("first registration logged %d warnings, want 1", got)
	}
	register(10_000) // a reconnect, nothing changed
	if got := logs.warns; got != 1 {
		t.Fatalf("an unchanged re-registration logged again (total %d, want 1): a warning per "+
			"NATS reconnect is noise, not a signal", got)
	}
	register(20_000) // still short, but by a different amount: news
	if got := logs.warns; got != 2 {
		t.Fatalf("a CHANGED shortfall logged %d warnings in total, want 2", got)
	}
	register(50_000) // fixed
	if got := logs.warns; got != 2 {
		t.Fatalf("a worker that now satisfies the relation logged a warning (total %d, want 2)", got)
	}
	// Regressed to the SAME shortfall it last had. This is the row that pins
	// the de-duplication being CLEARED on recovery: if the remembered text
	// survives the recovery, this recurrence is silently suppressed and the
	// operator is never told the worker went short again.
	register(20_000)
	if got := logs.warns; got != 3 {
		t.Fatalf("a recurrence of a previously-seen shortfall, after the worker had recovered, "+
			"logged %d warnings in total, want 3", got)
	}
}

// schedulerConfigWithPositions returns a scheduler config whose server-side
// template-position limit is n.
func schedulerConfigWithPositions(n int64) Config {
	cfg := DefaultConfig()
	cfg.FarmID = ""
	cfg.ExprLimits = withPositions(openjd.DefaultExprLimits(), n)
	return cfg
}

// countingHandler counts WARN-level records. Only the count matters here; the
// message text is asserted by the tests that read UnschedulableReason.
type countingHandler struct{ warns int }

func (*countingHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= slog.LevelWarn }

func (h *countingHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Level >= slog.LevelWarn {
		h.warns++
	}
	return nil
}

func (h *countingHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }

func (h *countingHandler) WithGroup(_ string) slog.Handler { return h }

// TestNew_NormalizesExprLimits pins that a partially-populated (or entirely
// zero) Config.ExprLimits is filled in with the defaults ONCE, at construction.
// An unset field left at 0 would make every worker look adequate in that
// dimension, which is the gate silently failing open.
func TestNew_NormalizesExprLimits(t *testing.T) {
	tests := []struct {
		name string
		in   openjd.ExprLimits
		want openjd.ExprLimits
	}{
		{"zero value is the defaults", openjd.ExprLimits{}, openjd.DefaultExprLimits()},
		{
			name: "one field set keeps the other three at their defaults",
			in:   openjd.ExprLimits{TemplatePositions: 42},
			want: withPositions(openjd.DefaultExprLimits(), 42),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.ExprLimits = tc.in
			s := New(cfg, fake.New(), &recordBus{}, metrics.New(), slog.New(slog.DiscardHandler), ws.NoopNotifier{}, nil)
			if got := s.cfg.ExprLimits; got != tc.want {
				t.Fatalf("scheduler ExprLimits = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestWorkerExprLimits_WireKeysMatchTheProtocol pins that the JSON the worker
// sends decodes into the store type the server persists. The two structs live
// in different packages on purpose (no production file in the worker imports
// internal/store); nothing but this test and its outer-key companion,
// TestRegisterMsg_WireFieldsSurviveTheDuplication, relates their field names.
func TestWorkerExprLimits_WireKeysMatchTheProtocol(t *testing.T) {
	// Five distinct values: five same-typed int64 fields crossing a package
	// boundary is exactly the shape where a transposition compiles and runs.
	sent := protocol.ExprLimits{
		OperationLimit:          11,
		MemoryLimit:             22,
		AssignmentPositions:     33,
		AssignmentRetainedBytes: 44,
		LetRetainedBytes:        55,
	}
	raw, err := json.Marshal(sent)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got store.WorkerExprLimits
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := store.WorkerExprLimits{
		OperationLimit:          sent.OperationLimit,
		MemoryLimit:             sent.MemoryLimit,
		AssignmentPositions:     sent.AssignmentPositions,
		AssignmentRetainedBytes: sent.AssignmentRetainedBytes,
		LetRetainedBytes:        sent.LetRetainedBytes,
	}
	if got != want {
		t.Fatalf("protocol JSON %s decoded to %+v, want %+v -- a renamed json tag on either "+
			"side silently drops the cap to zero, which reads as 'unadvertised'", raw, got, want)
	}

	// The comparison above is only as complete as the literal that feeds it: a
	// SIXTH dimension added to both structs and left out of `sent` would leave
	// both sides zero and this test green. Count the fields so that cannot
	// happen -- the same reason the outer-key test counts RegisterMsg's.
	const dimensions = 5
	if n := reflect.TypeOf(sent).NumField(); n != dimensions {
		t.Errorf("protocol.ExprLimits has %d fields, this test populates %d: a dimension that "+
			"is not in the literal above is not covered by it", n, dimensions)
	}
	if n := reflect.TypeOf(got).NumField(); n != dimensions {
		t.Errorf("store.WorkerExprLimits has %d fields, this test compares %d", n, dimensions)
	}
}

// TestRegisterMsg_WireFieldsSurviveTheDuplication marshals a fully-populated
// [protocol.RegisterMsg] -- the struct the worker actually publishes -- and
// decodes it into this package's hand-maintained duplicate, asserting EVERY
// field arrives.
//
// WHY IT EXISTS: TestWorkerExprLimits_WireKeysMatchTheProtocol covers the four
// INNER keys of the caps object and nothing covered the OUTER expr_limits key.
// A reviewer renamed it to worker_expr_limits on the protocol side and every
// unit test, the integration suite and make ci stayed green -- while every
// worker in the farm reported as "not advertised", workerExprCapsOrLegacy
// substituted the legacy defaults, and a genuinely tight worker would have been
// handed EXPR work it cannot run. That is the design spec §2 incident, silently,
// with CI green.
//
// It is deliberately NOT limited to ExprLimits. The duplication is structural:
// every field of these two structs is related by nothing but a matching string
// literal, and any of them can be renamed on one side without a compile error.
// Adding a field to protocol.RegisterMsg that the server needs and forgetting
// it here produces the same silence, so the last sub-test fails if the two
// structs stop having the same number of fields.
func TestRegisterMsg_WireFieldsSurviveTheDuplication(t *testing.T) {
	sent := protocol.RegisterMsg{
		Version:            protocol.ProtocolVersion,
		Type:               protocol.TypeRegister,
		WorkerID:           "w-1",
		FarmID:             "farm-1",
		QueueID:            "queue-1",
		Name:               "render-node-alpha",
		Hostname:           "node-1",
		IPAddress:          "10.0.0.1",
		ComputeLocation:    "onprem_linux",
		OS:                 "linux",
		OSVersion:          "6.1",
		WorkerVersion:      "v0.9.9",
		CPUCount:           16,
		RAMMb:              32768,
		GPUInfo:            protocol.GPUInfo{Vendor: "NVIDIA", Model: "RTX4090", VRAMMb: 24576, Count: 2},
		MaxConcurrentTasks: 4,
		Tags:               map[string]string{"env": "prod"},
		ExprLimits: protocol.ExprLimits{
			OperationLimit:          11_111,
			MemoryLimit:             2_222_222,
			AssignmentPositions:     3_333,
			AssignmentRetainedBytes: 4_444_444,
		},
	}
	raw, err := json.Marshal(sent)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got RegisterMsg
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// One row per field the server reads. A mismatched json tag shows up here
	// as a zero value, which is exactly how it shows up in production.
	fields := []struct {
		name      string
		got, want any
	}{
		{"worker_id", got.WorkerID, sent.WorkerID},
		{"farm_id", got.FarmID, sent.FarmID},
		{"queue_id", got.QueueID, sent.QueueID},
		{"name", got.Name, sent.Name},
		{"hostname", got.Hostname, sent.Hostname},
		{"ip_address", got.IPAddress, sent.IPAddress},
		{"compute_location", got.ComputeLocation, sent.ComputeLocation},
		{"os", got.OS, sent.OS},
		{"os_version", got.OSVersion, sent.OSVersion},
		{"worker_version", got.WorkerVersion, sent.WorkerVersion},
		{"cpu_count", got.CPUCount, sent.CPUCount},
		{"ram_mb", got.RAMMb, sent.RAMMb},
		{"gpu_info.vendor", got.GPUInfo.Vendor, sent.GPUInfo.Vendor},
		{"gpu_info.model", got.GPUInfo.Model, sent.GPUInfo.Model},
		{"gpu_info.vram_mb", got.GPUInfo.VRAMMb, sent.GPUInfo.VRAMMb},
		{"gpu_info.count", got.GPUInfo.Count, sent.GPUInfo.Count},
		{"tags", got.Tags["env"], sent.Tags["env"]},
		{"expr_limits.operation_limit", got.ExprLimits.OperationLimit, sent.ExprLimits.OperationLimit},
		{"expr_limits.memory_limit", got.ExprLimits.MemoryLimit, sent.ExprLimits.MemoryLimit},
		{"expr_limits.assignment_positions", got.ExprLimits.AssignmentPositions, sent.ExprLimits.AssignmentPositions},
		{
			"expr_limits.assignment_retained_bytes",
			got.ExprLimits.AssignmentRetainedBytes, sent.ExprLimits.AssignmentRetainedBytes,
		},
		{
			"expr_limits.let_retained_bytes",
			got.ExprLimits.LetRetainedBytes, sent.ExprLimits.LetRetainedBytes,
		},
	}
	for _, f := range fields {
		t.Run(f.name, func(t *testing.T) {
			if f.got != f.want {
				t.Fatalf("%s = %v after the protocol -> scheduler round trip, want %v.\n"+
					"The two RegisterMsg structs are related by nothing but matching json "+
					"tags; a rename on either side decodes to the zero value on every "+
					"registration, silently and forever.", f.name, f.got, f.want)
			}
		})
	}

	// protocol.RegisterMsg carries three fields this struct deliberately does
	// not (Version and Type, which the subject already identifies, and
	// MaxConcurrentTasks, which Phase 1 does not persist). The two count checks
	// below are what make a NEW field visible here rather than silently unread,
	// and they are deliberately separate: the first catches a field added to
	// ONE side, the second catches one added to BOTH -- which satisfies the
	// first and would otherwise leave the new tag unguarded, the exact residual
	// that let the outer expr_limits key go uncovered in the first place.
	t.Run("field counts", func(t *testing.T) {
		sentFields := reflect.TypeFor[protocol.RegisterMsg]().NumField()
		gotFields := reflect.TypeFor[RegisterMsg]().NumField()
		if sentFields != gotFields+3 {
			t.Fatalf("protocol.RegisterMsg has %d fields and scheduler.RegisterMsg %d; the "+
				"known gap is 3 (version, type, max_concurrent_tasks). A field added to "+
				"one side must be added here, to the table above, or to this comment "+
				"with a reason the server does not need it.", sentFields, gotFields)
		}

		// One row per field of scheduler.RegisterMsg, except that gpu_info and
		// expr_limits each contribute several rows instead of one (their inner
		// keys are what actually carry the values).
		const nestedExtraRows = 3 + 4 // gpu_info: 4 rows for 1 field; expr_limits: 5
		if want := gotFields + nestedExtraRows; len(fields) != want {
			t.Fatalf("the table covers %d fields but scheduler.RegisterMsg has %d (%d rows "+
				"expected with gpu_info and expr_limits expanded). Every field must have a "+
				"row: a json tag with no row is a tag nothing checks, which is how the outer "+
				"expr_limits key went unguarded until a reviewer renamed it and watched CI "+
				"stay green.", len(fields), gotFields, want)
		}
	})
}

// ── small helpers, one per dimension, so a row reads as its own mutation ──

func withPositions(l openjd.ExprLimits, n int64) openjd.ExprLimits {
	l.TemplatePositions = n
	return l
}

func withOperations(l openjd.ExprLimits, n int64) openjd.ExprLimits {
	l.SubmissionOperations = n
	return l
}

func withMemory(l openjd.ExprLimits, n int64) openjd.ExprLimits {
	l.SubmissionMemoryBytes = n
	return l
}

func withWorkerPositions(c store.WorkerExprLimits, n int64) store.WorkerExprLimits {
	c.AssignmentPositions = n
	return c
}

func withWorkerOperations(c store.WorkerExprLimits, n int64) store.WorkerExprLimits {
	c.OperationLimit = n
	return c
}

func withWorkerMemory(c store.WorkerExprLimits, n int64) store.WorkerExprLimits {
	c.MemoryLimit = n
	return c
}

func withWorkerRetained(c store.WorkerExprLimits, n int64) store.WorkerExprLimits {
	c.AssignmentRetainedBytes = n
	return c
}

func withWorkerLetRetained(c store.WorkerExprLimits, n int64) store.WorkerExprLimits {
	c.LetRetainedBytes = n
	return c
}

func mustTask(t *testing.T, st *fake.Store, id string) store.Task {
	t.Helper()
	task, err := st.GetTask(t.Context(), id)
	if err != nil {
		t.Fatalf("GetTask %s: %v", id, err)
	}
	return task
}

// ── EXPR sub-project E4d, whole-branch review: the heuristic and the order ──

// houdiniEXPRCacheJSON is the documentation's own false-positive example: a
// BASE-SPEC template — no extensions key at all — whose environment declares a
// variable named HOUDINI_EXPR_CACHE. It contains the four bytes EXPR and
// nothing else about the extension.
const houdiniEXPRCacheJSON = `{
  "specificationVersion": "jobtemplate-2023-09",
  "name": "j",
  "jobEnvironments": [
    {
      "name": "houdini",
      "variables": { "HOUDINI_EXPR_CACHE": "/tmp/cache" }
    }
  ],
  "steps": [
    {
      "name": "render",
      "script": { "actions": { "onRun": { "command": "render" } } }
    }
  ]
}`

// TestJobMayUseEXPR_RequiresAnExtensionDeclaration is the narrowing the wave's
// final review asked for. The byte scan's false positive is LIVE — unlike its
// false negative, which needs an EXPR template and cannot be submitted while
// the extension is StatusInProgress — and its consequence is not cosmetic: on a
// farm whose workers are short (reachable through documented configuration,
// since "raise the workers first" is guidance and not enforcement) the job's
// tasks sit `ready` forever with no capable worker, flagged with a reason
// naming limits the template does not use.
//
// The check stays a byte scan, and stays conservative in the safe direction: a
// false positive only withholds work, a false negative re-opens design spec §2.
func TestJobMayUseEXPR_RequiresAnExtensionDeclaration(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want bool
	}{
		{"declares the extension", exprTemplateJSON, true},
		{"base-spec template with no mention at all", minimalRenderJSON, false},
		{
			name: "base-spec template mentioning EXPR in a variable name",
			raw:  houdiniEXPRCacheJSON,
			want: false,
		},
		{
			name: "base-spec template mentioning EXPR in a comment",
			raw:  "# Job.Name requires the EXPR extension.\nname: j\n",
			want: false,
		},
		{
			name: "declares some other extension and does not mention EXPR",
			raw:  `{"extensions":["TASK_CHUNKING"],"name":"j"}`,
			want: false,
		},
		{
			// The residual, kept deliberately: this errs toward withholding.
			name: "declares another extension AND mentions EXPR elsewhere",
			raw:  `{"extensions":["TASK_CHUNKING"],"name":"j","description":"no EXPR here"}`,
			want: true,
		},
		{"yaml block sequence form", "extensions:\n  - EXPR\nname: j\n", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := jobMayUseEXPR(store.Job{RawTemplate: tc.raw}); got != tc.want {
				t.Fatalf("jobMayUseEXPR = %v, want %v for:\n%s", got, tc.want, tc.raw)
			}
		})
	}
}

// TestExprCaps_BaseSpecJobMentioningEXPRIsStillDispatched is the same
// narrowing at the level that matters: not what the predicate returns, but
// whether a base-spec job is withheld from a short worker. Before the
// narrowing this leased 0 assignments, and with no capable worker in the queue
// the task waited `ready` indefinitely.
func TestExprCaps_BaseSpecJobMentioningEXPRIsStillDispatched(t *testing.T) {
	st := fake.New()
	w, taskID := seedExprLeaseFixture(t, st, houdiniEXPRCacheJSON, store.WorkerExprLimits{
		OperationLimit:          fmtres.MinExprOperationLimit,
		MemoryLimit:             fmtres.MinExprMemoryLimit,
		AssignmentPositions:     fmtres.MinExprAssignmentPositions,
		AssignmentRetainedBytes: fmtres.MinExprAssignmentRetainedBytes,
		LetRetainedBytes:        fmtres.MinExprLetRetainedBytes,
	})
	// A server tightened nowhere and raised nowhere: the worker is short in
	// every dimension purely by its own configuration.
	s := schedulerWithExprLimits(st, openjd.DefaultExprLimits())

	if leased := leaseOnce(t, s, w); leased != 1 {
		t.Fatalf("leased %d assignments for a BASE-SPEC job whose only relation to EXPR is an "+
			"environment variable named HOUDINI_EXPR_CACHE; want 1", leased)
	}
	s.reconcileTaskSchedulability(t.Context(), mustTask(t, st, taskID), []store.Worker{w})
	if got := mustTask(t, st, taskID).UnschedulableReason; got != "" {
		t.Fatalf("UnschedulableReason = %q; a base-spec job must not be flagged with EXPR "+
			"limits it does not use", got)
	}
}

// TestEvaluateSchedulability_GenuineIneligibilityOutranksEXPR pins the order of
// the two tests in evaluateSchedulability. The worker here is BOTH short on
// EXPR caps and ineligible for an ordinary reason (queue affinity). Reporting
// the EXPR shortfall would tell the operator to reconfigure two expression
// limits when the actual problem is that no worker serves the job's queue --
// and on a farm where every worker is short, the EXPR text overwrote the real
// reason for every one of them.
func TestEvaluateSchedulability_GenuineIneligibilityOutranksEXPR(t *testing.T) {
	st := fake.New()
	w, taskID := seedExprLeaseFixture(t, st, exprTemplateJSON, store.WorkerExprLimits{
		OperationLimit:          fmtres.MinExprOperationLimit,
		MemoryLimit:             fmtres.MinExprMemoryLimit,
		AssignmentPositions:     fmtres.MinExprAssignmentPositions,
		AssignmentRetainedBytes: fmtres.MinExprAssignmentRetainedBytes,
		LetRetainedBytes:        fmtres.MinExprLetRetainedBytes,
	})
	w.QueueID = "some-other-queue" // ordinary ineligibility, nothing to do with EXPR
	s := schedulerWithExprLimits(st, openjd.DefaultExprLimits())

	s.reconcileTaskSchedulability(t.Context(), mustTask(t, st, taskID), []store.Worker{w})
	got := mustTask(t, st, taskID).UnschedulableReason
	if !strings.Contains(got, "queue affinity") {
		t.Fatalf("UnschedulableReason = %q, want the ordinary ineligibility reason "+
			"(%q): a worker that could never have run this job must not be reported as "+
			"blocked by EXPR limits", got, "queue affinity")
	}
	if strings.Contains(got, "expr.") {
		t.Fatalf("UnschedulableReason = %q names EXPR config keys for a worker that is "+
			"ineligible for an unrelated reason", got)
	}
}

// TestEvaluateSchedulability_EXPRStillReportedForAnOtherwiseEligibleWorker is
// the other half of that order: reordering must not silence the EXPR reason for
// the worker the operator actually needs to fix.
func TestEvaluateSchedulability_EXPRStillReportedForAnOtherwiseEligibleWorker(t *testing.T) {
	st := fake.New()
	w, taskID := seedExprLeaseFixture(t, st, exprTemplateJSON, store.WorkerExprLimits{
		OperationLimit:          fmtres.MinExprOperationLimit,
		MemoryLimit:             fmtres.MinExprMemoryLimit,
		AssignmentPositions:     fmtres.MinExprAssignmentPositions,
		AssignmentRetainedBytes: fmtres.MinExprAssignmentRetainedBytes,
		LetRetainedBytes:        fmtres.MinExprLetRetainedBytes,
	})
	s := schedulerWithExprLimits(st, openjd.DefaultExprLimits())

	s.reconcileTaskSchedulability(t.Context(), mustTask(t, st, taskID), []store.Worker{w})
	got := mustTask(t, st, taskID).UnschedulableReason
	if !strings.Contains(got, "expr.assignment_positions") {
		t.Fatalf("UnschedulableReason = %q, want the EXPR shortfall: an eligible-but-short "+
			"worker is exactly what this reason exists to explain", got)
	}
}
