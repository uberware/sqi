// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

import (
	"context"
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
	return seedExprLeaseFixtureJob(t, st, store.Job{RawTemplate: rawTemplate}, workerCaps)
}

// seedExprLeaseFixtureJob is [seedExprLeaseFixture] for a caller that needs to
// control more of the job row than its raw template -- specifically whether the
// declared-extension list was recorded at all, which is the one thing a job
// written before migration 00027 cannot have. Only ID, FarmID, QueueID, Name
// and the lifecycle columns are supplied here; everything else comes from seed.
func seedExprLeaseFixtureJob(
	t *testing.T,
	st *fake.Store,
	seed store.Job,
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
	seed.ID, seed.FarmID, seed.QueueID, seed.Name = uuid.NewString(), "f1", "q1", "j"
	seed.Status, seed.TemplateFormat = store.JobStatusRunning, store.TemplateFormatJSON
	seed.CreatedAt, seed.UpdatedAt = now, now
	job, err := st.CreateJob(ctx, seed)
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
// the handler directly, so the whole decode-and-convert hop is exercised end
// to end.
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
		subject: bus.WorkerRegisterSubject("w-1"),
		data: workerMsgJSON(t, protocol.RegisterMsg{
			Version: protocol.ProtocolVersion, Type: protocol.TypeRegister,
			WorkerID: "w-1", FarmID: "farm-1", Hostname: "node-1", OS: "linux",
			ExprLimits: protocol.ExprLimits(want),
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
			subject: bus.WorkerRegisterSubject("w-1"),
			data: workerMsgJSON(t, protocol.RegisterMsg{
				Version: protocol.ProtocolVersion, Type: protocol.TypeRegister,
				WorkerID: "w-1", FarmID: "farm-1", Hostname: "node-1", OS: "linux",
				ExprLimits: protocol.ExprLimits{
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

// TestHandleWorkerRegister_EveryWireFieldReachesTheStore drives a
// fully-populated [protocol.RegisterMsg] through the real handler as real JSON
// bytes and asserts every field arrives on the persisted [store.Worker].
//
// WHY IT EXISTS. It replaces two tests that guarded a hand-maintained
// duplicate of protocol.RegisterMsg that used to live in scheduler.go: the two
// structs were related by nothing but matching json tags, so a rename on
// either side decoded to the zero value on every registration, silently and
// forever. That is not hypothetical -- a reviewer renamed the outer
// expr_limits key on the protocol side and watched the entire unit suite, the
// integration suite and make ci stay green while every worker in the farm
// reported as "not advertised", workerExprCapsOrLegacy substituted the legacy
// defaults, and a genuinely tight worker would have been handed EXPR work it
// cannot run.
//
// The duplicate is gone: the handler decodes the shared protocol type and
// converts the two nested structs with Go conversions, which the compiler
// checks on field name, type AND declaration order. What is NOT compiler-
// checked is the field-by-field copy into store.Worker, which is why this test
// drives the real handler rather than a struct-to-struct decode, and why the
// field-count guard at the end still earns its place: a field added to
// protocol.RegisterMsg that the server should persist is otherwise just
// unread.
//
// One residual the version gate now covers rather than this test: renaming a
// json tag on the shared type still breaks OLD workers, which send the old key
// and decode to zero. That is a cross-version disagreement, and a tag rename
// is precisely the breaking change [protocol.ProtocolVersion] must be bumped
// for -- at which point discardOnVersionMismatch refuses the message instead
// of half-reading it.
func TestHandleWorkerRegister_EveryWireFieldReachesTheStore(t *testing.T) {
	st := fake.New()
	s := newMetricsScheduler(st, &recordBus{}, "")

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
		Arch:               "arm64",
		WorkerVersion:      "v0.9.9",
		CPUCount:           16,
		RAMMb:              32768,
		GPUInfo:            protocol.GPUInfo{Vendor: "NVIDIA", Model: "RTX4090", VRAMMb: 24576, Count: 2},
		MaxConcurrentTasks: 4,
		Tags:               map[string]string{"env": "prod"},
		// Five distinct values: five same-typed int64 fields crossing a package
		// boundary is exactly the shape where a transposition survives review.
		ExprLimits: protocol.ExprLimits{
			OperationLimit:          11_111,
			MemoryLimit:             2_222_222,
			AssignmentPositions:     3_333,
			AssignmentRetainedBytes: 4_444_444,
			LetRetainedBytes:        5_555_555,
		},
	}

	msg := &fakeJSMsg{subject: bus.WorkerRegisterSubject(sent.WorkerID), data: workerMsgJSON(t, sent)}
	s.handleWorkerMessage(msg)

	w, err := st.GetWorker(t.Context(), sent.WorkerID)
	if err != nil {
		t.Fatalf("GetWorker: %v", err)
	}

	// One row per field the server persists. A field the handler forgets to
	// copy shows up here as a zero value, which is exactly how it shows up in
	// production.
	fields := []struct {
		name      string
		got, want any
	}{
		{"worker_id", w.ID, sent.WorkerID},
		{"farm_id", w.FarmID, sent.FarmID},
		{"queue_id", w.QueueID, sent.QueueID},
		{"name", w.Name, sent.Name},
		{"hostname", w.Hostname, sent.Hostname},
		{"ip_address", w.IPAddress, sent.IPAddress},
		{"compute_location", w.ComputeLocation, sent.ComputeLocation},
		{"os", w.OS, sent.OS},
		{"os_version", w.OSVersion, sent.OSVersion},
		{"arch", w.Arch, sent.Arch},
		{"worker_version", w.Version, sent.WorkerVersion},
		{"cpu_count", w.CPUCount, sent.CPUCount},
		{"ram_mb", w.RAMMb, sent.RAMMb},
		{"gpu_info.vendor", w.GPUInfo.Vendor, sent.GPUInfo.Vendor},
		{"gpu_info.model", w.GPUInfo.Model, sent.GPUInfo.Model},
		{"gpu_info.vram_mb", w.GPUInfo.VRAMMb, sent.GPUInfo.VRAMMb},
		{"gpu_info.count", w.GPUInfo.Count, sent.GPUInfo.Count},
		{"tags", w.Tags["env"], sent.Tags["env"]},
		{"expr_limits.operation_limit", w.ExprLimits.OperationLimit, sent.ExprLimits.OperationLimit},
		{"expr_limits.memory_limit", w.ExprLimits.MemoryLimit, sent.ExprLimits.MemoryLimit},
		{"expr_limits.assignment_positions", w.ExprLimits.AssignmentPositions, sent.ExprLimits.AssignmentPositions},
		{
			"expr_limits.assignment_retained_bytes",
			w.ExprLimits.AssignmentRetainedBytes, sent.ExprLimits.AssignmentRetainedBytes,
		},
		{
			"expr_limits.let_retained_bytes",
			w.ExprLimits.LetRetainedBytes, sent.ExprLimits.LetRetainedBytes,
		},
	}
	for _, f := range fields {
		t.Run(f.name, func(t *testing.T) {
			if f.got != f.want {
				t.Fatalf("%s = %v on the persisted worker, want %v -- the registration hop "+
					"from protocol.RegisterMsg to store.Worker is a hand-written field copy; "+
					"a field it misses is not a compile error, it is a permanent zero.",
					f.name, f.got, f.want)
			}
		})
	}

	// protocol.RegisterMsg carries three fields the server deliberately does
	// not persist: Version and Type (the envelope -- read by the version gate
	// and the subject respectively, not stored), and MaxConcurrentTasks, which
	// Phase 1 does not persist because the worker enforces its own concurrency
	// locally. The counts below are what make a NEW protocol field visible
	// here rather than silently unread, and they are deliberately separate:
	// the first catches a field added to the wire and never persisted, the
	// second catches one added to both and left out of the table above --
	// which satisfies the first and would otherwise leave the new field
	// unchecked, the exact residual that let the outer expr_limits key go
	// uncovered in the first place.
	t.Run("field counts", func(t *testing.T) {
		const unpersisted = 3 // version, type, max_concurrent_tasks
		sentFields := reflect.TypeFor[protocol.RegisterMsg]().NumField()
		if want := len(persistedRegisterFields) + unpersisted; sentFields != want {
			t.Fatalf("protocol.RegisterMsg has %d fields; this test accounts for %d persisted "+
				"plus %d deliberately unpersisted. A field added to the wire must be "+
				"persisted and given a row above, or added to this comment with a reason "+
				"the server does not need it.", sentFields, len(persistedRegisterFields), unpersisted)
		}

		// One row per persisted field, except that gpu_info and expr_limits
		// each contribute several rows instead of one (their inner keys are
		// what actually carry the values).
		const nestedExtraRows = 3 + 4 // gpu_info: 4 rows for 1 field; expr_limits: 5 for 1
		if want := len(persistedRegisterFields) + nestedExtraRows; len(fields) != want {
			t.Fatalf("the table covers %d fields but %d are persisted (%d rows expected with "+
				"gpu_info and expr_limits expanded). Every persisted field needs a row: a "+
				"field with no row is a field nothing checks, which is how the outer "+
				"expr_limits key went unguarded until a reviewer renamed it and watched CI "+
				"stay green.", len(fields), len(persistedRegisterFields), want)
		}
	})
}

// persistedRegisterFields names the protocol.RegisterMsg fields the register
// handler copies onto a store.Worker. It exists so the count guard above reads
// as a list rather than a magic number -- adding a field to the wire and
// persisting it means adding its name here, which is the moment to notice it
// also needs a row in the table.
var persistedRegisterFields = []string{
	"WorkerID", "FarmID", "QueueID", "Name", "Hostname", "IPAddress",
	"ComputeLocation", "OS", "OSVersion", "Arch", "WorkerVersion", "CPUCount", "RAMMb",
	"GPUInfo", "Tags", "ExprLimits",
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

func mustJob(t *testing.T, st *fake.Store, id string) store.Job {
	t.Helper()
	job, err := st.GetJob(t.Context(), id)
	if err != nil {
		t.Fatalf("GetJob %s: %v", id, err)
	}
	return job
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
// final review asked for. The byte scan's false positive is LIVE — its false
// negative was unreachable while the extension was StatusInProgress, because it
// needs an EXPR template and none could be submitted, but sub-project H2 made
// EXPR submittable and only deliberate obfuscation bounds it now (see
// jobMayUseEXPR's own comment) — and its consequence is not cosmetic: on a
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

// ── The declared-extension column: exact where the byte scan guessed ─────────
//
// jobMayUseEXPR's own comment named the fix it was waiting for: persist the
// declared extension list on the job row at submission, where internal/openjd
// has already parsed and validated it, and read a column on the lease path.
// These tests are that fix, at the level that matters -- whether work is
// withheld -- and they fence in the state the column cannot represent: a row
// written before it existed, which must still go through the byte scan.

// taskChunkingWithEXPRCacheJSON is the byte scan's LIVE false positive, in the
// only shape that still triggers it after E4d's narrowing: a template that
// declares SOME OTHER extension (so the bytes "extensions" are present) and
// separately mentions the four bytes EXPR in an environment variable name.
// It declares nothing of the sort, and every phase-3 evaluation it will ever
// ask a worker for is none.
const taskChunkingWithEXPRCacheJSON = `{
  "specificationVersion": "jobtemplate-2023-09",
  "extensions": ["TASK_CHUNKING"],
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

// minWorkerExprCaps is a worker short in every dimension: the configuration
// that makes exprCapShortfall non-empty, and therefore the only configuration
// under which the EXPR/not-EXPR decision has any consequence at all.
func minWorkerExprCaps() store.WorkerExprLimits {
	return store.WorkerExprLimits{
		OperationLimit:          fmtres.MinExprOperationLimit,
		MemoryLimit:             fmtres.MinExprMemoryLimit,
		AssignmentPositions:     fmtres.MinExprAssignmentPositions,
		AssignmentRetainedBytes: fmtres.MinExprAssignmentRetainedBytes,
		LetRetainedBytes:        fmtres.MinExprLetRetainedBytes,
	}
}

// seedSubmittedExprLeaseFixture is seedExprLeaseFixture through the REAL
// submission path: openjd.Submitter parses and validates rawTemplate and
// persists the job, so the declared-extension column holds whatever
// internal/openjd actually decoded rather than whatever a test literal claims.
// That is the whole point -- a fixture that sets the field by hand cannot show
// that submission records it.
func seedSubmittedExprLeaseFixture(
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
	res, err := openjd.NewSubmitter(st).Submit(ctx, rawTemplate, store.TemplateFormatJSON, openjd.SubmitOptions{
		FarmID: "f1", QueueID: "q1", Owner: "alice",
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if len(res.Tasks) == 0 {
		t.Fatal("Submit produced no tasks")
	}
	return w, res.Tasks[0].ID
}

// TestExprCaps_SubmittedBaseSpecJobMentioningEXPRIsDispatched is the false
// positive the byte scan still has, closed. The template declares
// TASK_CHUNKING and names an environment variable HOUDINI_EXPR_CACHE, so
// jobMayUseEXPR answers true (the residual its own comment documents and
// deliberately kept). With the declared list persisted at submission, the
// answer comes from what internal/openjd decoded -- ["TASK_CHUNKING"] -- and
// the job is dispatched to a worker that is short on EXPR limits it does not
// use.
func TestExprCaps_SubmittedBaseSpecJobMentioningEXPRIsDispatched(t *testing.T) {
	st := fake.New()
	w, taskID := seedSubmittedExprLeaseFixture(t, st, taskChunkingWithEXPRCacheJSON, minWorkerExprCaps())

	// The premise: the byte scan really does get this one wrong, so the test
	// below is testing the fix and not an easier case.
	job := mustJob(t, st, mustTask(t, st, taskID).JobID)
	if !jobMayUseEXPR(job) {
		t.Fatal("premise broken: the byte scan no longer matches this template, so this " +
			"test would pass without reading the declared-extension column")
	}
	if declared, recorded := job.DeclaresExtension(openjd.ExtensionEXPR); !recorded || declared {
		t.Fatalf("submission recorded declared=%v recorded=%v; want recorded with EXPR absent",
			declared, recorded)
	}

	s := schedulerWithExprLimits(st, openjd.DefaultExprLimits())
	if leased := leaseOnce(t, s, w); leased != 1 {
		t.Fatalf("leased %d assignments for a job whose recorded extensions are %v; want 1",
			leased, job.DeclaredExtensions)
	}
	s.reconcileTaskSchedulability(t.Context(), mustTask(t, st, taskID), []store.Worker{w})
	if got := mustTask(t, st, taskID).UnschedulableReason; got != "" {
		t.Fatalf("UnschedulableReason = %q; a job declaring only TASK_CHUNKING must not be "+
			"flagged with EXPR limits it does not use", got)
	}
}

// TestExprCaps_SubmittedEXPRJobIsStillWithheld is the other direction through
// the same real path: replacing a heuristic that says "maybe" with a column
// that says "no" must not stop saying "yes" when the template really does
// declare the extension. This is design spec §2's incident.
func TestExprCaps_SubmittedEXPRJobIsStillWithheld(t *testing.T) {
	st := fake.New()
	w, taskID := seedSubmittedExprLeaseFixture(t, st, exprTemplateJSON, minWorkerExprCaps())

	job := mustJob(t, st, mustTask(t, st, taskID).JobID)
	if declared, recorded := job.DeclaresExtension(openjd.ExtensionEXPR); !recorded || !declared {
		t.Fatalf("submission recorded declared=%v recorded=%v; want recorded with EXPR present",
			declared, recorded)
	}

	s := schedulerWithExprLimits(st, openjd.DefaultExprLimits())
	if leased := leaseOnce(t, s, w); leased != 0 {
		t.Fatalf("leased %d assignments of an EXPR job to a worker short in every dimension; want 0", leased)
	}
	s.reconcileTaskSchedulability(t.Context(), mustTask(t, st, taskID), []store.Worker{w})
	if got := mustTask(t, st, taskID).UnschedulableReason; !strings.Contains(got, "expr.assignment_positions") {
		t.Fatalf("UnschedulableReason = %q, want the EXPR shortfall naming both config keys", got)
	}
}

// TestExprCaps_LegacyRowFallsBackToTheByteScan is what protects deployments
// that upgrade: every job row written before migration 00027 reads as "not
// recorded", and for those the byte scan is still the only evidence there is.
// Defaulting the column to '[]' instead of ” would make every one of them
// look like a job that declares nothing -- an EXPR job already in the queue
// would silently lose the gate, which is a REGRESSION against the heuristic
// this change replaces, and an invisible one.
func TestExprCaps_LegacyRowFallsBackToTheByteScan(t *testing.T) {
	st := fake.New()
	// seedExprLeaseFixture writes the job row directly, recording nothing --
	// exactly the shape a pre-migration row has after the column is added.
	w, taskID := seedExprLeaseFixture(t, st, exprTemplateJSON, minWorkerExprCaps())

	job := mustJob(t, st, mustTask(t, st, taskID).JobID)
	if _, recorded := job.DeclaresExtension(openjd.ExtensionEXPR); recorded {
		t.Fatal("premise broken: the fixture recorded a declared-extension list, so this " +
			"test is not exercising the legacy path")
	}

	s := schedulerWithExprLimits(st, openjd.DefaultExprLimits())
	if leased := leaseOnce(t, s, w); leased != 0 {
		t.Fatalf("leased %d assignments of a PRE-MIGRATION EXPR job to a worker short in "+
			"every dimension; want 0 -- a legacy row must still be gated by the byte scan", leased)
	}
	s.reconcileTaskSchedulability(t.Context(), mustTask(t, st, taskID), []store.Worker{w})
	if got := mustTask(t, st, taskID).UnschedulableReason; !strings.Contains(got, "expr.assignment_positions") {
		t.Fatalf("UnschedulableReason = %q, want the EXPR shortfall for a legacy row", got)
	}
}

// TestExprCaps_RecordedEmptyIsNotUnrecorded is the distinction the column's ”
// default exists to make. A job that was recorded and declares NOTHING must
// skip the byte scan entirely -- so a raw template whose bytes would match is
// dispatched anyway. If the two states were conflated, this job would be
// withheld on evidence the row already contradicts.
func TestExprCaps_RecordedEmptyIsNotUnrecorded(t *testing.T) {
	st := fake.New()
	w, taskID := seedExprLeaseFixtureJob(t, st, store.Job{
		// Bytes that the scan matches, and a recorded list that says otherwise.
		RawTemplate:        exprTemplateJSON,
		DeclaredExtensions: []string{},
		ExtensionsRecorded: true,
	}, minWorkerExprCaps())

	s := schedulerWithExprLimits(st, openjd.DefaultExprLimits())
	if leased := leaseOnce(t, s, w); leased != 1 {
		t.Fatalf("leased %d assignments for a job RECORDED as declaring no extensions; want 1 "+
			"-- recorded-empty must not fall back to the byte scan", leased)
	}
	s.reconcileTaskSchedulability(t.Context(), mustTask(t, st, taskID), []store.Worker{w})
	if got := mustTask(t, st, taskID).UnschedulableReason; got != "" {
		t.Fatalf("UnschedulableReason = %q for a job recorded as declaring no extensions", got)
	}
}

// TestJobUsesEXPR_ThreeStates tabulates the predicate the gate now asks,
// including the case no byte scan can ever get right: a declaration spelled
// with an escape (JSON's "\u0045XPR" decodes to EXPR, parses, and is stored
// verbatim in RawTemplate). Recording the DECODED list at submission is what makes that
// case exact rather than a documented escape hatch.
func TestJobUsesEXPR_ThreeStates(t *testing.T) {
	tests := []struct {
		name string
		job  store.Job
		want bool
	}{
		{
			name: "not recorded, bytes match: the legacy byte scan decides",
			job:  store.Job{RawTemplate: exprTemplateJSON},
			want: true,
		},
		{
			name: "not recorded, bytes do not match: the legacy byte scan decides",
			job:  store.Job{RawTemplate: houdiniEXPRCacheJSON},
			want: false,
		},
		{
			name: "recorded with EXPR: the column decides",
			job: store.Job{
				RawTemplate:        houdiniEXPRCacheJSON, // bytes say no
				DeclaredExtensions: []string{"EXPR"},
				ExtensionsRecorded: true,
			},
			want: true,
		},
		{
			name: "recorded without EXPR: the column decides",
			job: store.Job{
				RawTemplate:        exprTemplateJSON, // bytes say yes
				DeclaredExtensions: []string{"TASK_CHUNKING"},
				ExtensionsRecorded: true,
			},
			want: false,
		},
		{
			name: "recorded empty: the column decides, and the scan is not consulted",
			job: store.Job{
				RawTemplate:        exprTemplateJSON, // bytes say yes
				DeclaredExtensions: []string{},
				ExtensionsRecorded: true,
			},
			want: false,
		},
		{
			name: "an escaped declaration the byte scan cannot see",
			job: store.Job{
				RawTemplate:        `{"extensions":["\u0045XPR"],"name":"j"}`,
				DeclaredExtensions: []string{"EXPR"},
				ExtensionsRecorded: true,
			},
			want: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := jobUsesEXPR(tc.job); got != tc.want {
				t.Fatalf("jobUsesEXPR = %v, want %v", got, tc.want)
			}
		})
	}
}
