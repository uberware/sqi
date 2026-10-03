// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

// Tests for reconcileStuckSteps, the startup repair for steps left stuck by
// v0.3.0 (H4a F6, spec §6.2 and §8.6).
//
// A v0.3.0 database can hold a step whose tasks are all terminal but which was
// never finalized, because completion read the step's tasks through one
// store.MaxLimit-sized page and never decided on a step with more tasks than
// that. Nothing reports on those tasks again, so nothing would ever finalize
// the step, its job, the steps behind it, or the jobs blocked on it.
//
// The scenario tests start the scheduler through Run, not by calling the
// method, so they also pin WHERE the repair sits: it must have finished before
// the scheduler begins accepting leases. Every test runs over both backends
// (spec §8.1), so the fake cannot drift from the store it stands in for.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	nats "github.com/nats-io/nats.go"

	"github.com/uberware/sqi/internal/metrics"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/ws"
)

// stuckStepTasks is the task count of the stuck step the scenario seeds: one
// past the page size the v0.3.0 completion check read, which is what stranded
// it.
const stuckStepTasks = store.MaxLimit + 1

// errInjectedReconcile is the failure the fault-injecting store returns.
var errInjectedReconcile = errors.New("injected store failure")

// ── instrumented store ──────────────────────────────────────────────────────

// writeCountingStore counts the calls that make up the completion path
// (spec §6.2: FinalizeStep, then dependency propagation through ReleaseStep and
// CancelPendingStep, then FinalizeJob) and the ListStuckSteps query that finds
// the work. A healthy farm must see the query once and none of the four
// writes. It can also be told to fail, so the error paths are reachable.
type writeCountingStore struct {
	store.Store

	listStuck         atomic.Int64
	finalizeStep      atomic.Int64
	finalizeJob       atomic.Int64
	releaseStep       atomic.Int64
	cancelPendingStep atomic.Int64

	// listErr, when set, is returned by ListStuckSteps in place of the result.
	listErr error
	// failFinalizeStep, when set, makes FinalizeStep of that step ID fail.
	failFinalizeStep string
}

// completionWrites is the number of calls to the four completion-path writes.
func (s *writeCountingStore) completionWrites() int64 {
	return s.finalizeStep.Load() + s.finalizeJob.Load() + s.releaseStep.Load() + s.cancelPendingStep.Load()
}

func (s *writeCountingStore) ListStuckSteps(ctx context.Context) ([]store.Step, error) {
	s.listStuck.Add(1)
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.Store.ListStuckSteps(ctx)
}

func (s *writeCountingStore) FinalizeStep(ctx context.Context, id string, now time.Time) (store.StepStatus, bool, error) {
	s.finalizeStep.Add(1)
	if id == s.failFinalizeStep {
		return "", false, errInjectedReconcile
	}
	return s.Store.FinalizeStep(ctx, id, now)
}

func (s *writeCountingStore) FinalizeJob(ctx context.Context, id string, now time.Time) (store.JobStatus, bool, error) {
	s.finalizeJob.Add(1)
	return s.Store.FinalizeJob(ctx, id, now)
}

func (s *writeCountingStore) ReleaseStep(ctx context.Context, id string, now time.Time) (bool, []store.Task, error) {
	s.releaseStep.Add(1)
	return s.Store.ReleaseStep(ctx, id, now)
}

func (s *writeCountingStore) CancelPendingStep(ctx context.Context, id, reason string, now time.Time) (bool, []store.Task, error) {
	s.cancelPendingStep.Add(1)
	return s.Store.CancelPendingStep(ctx, id, reason, now)
}

// reconcileEvents records every WebSocket event the scheduler emits; the
// other Notify* methods are discarded. It is safe for the scheduler's
// goroutine to write while the test reads.
type reconcileEvents struct {
	ws.NoopNotifier

	mu    sync.Mutex
	jobs  []ws.JobEvent
	tasks []ws.TaskEvent
}

func (n *reconcileEvents) NotifyJob(e ws.JobEvent) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.jobs = append(n.jobs, e)
}

func (n *reconcileEvents) NotifyTask(e ws.TaskEvent) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.tasks = append(n.tasks, e)
}

// snapshot returns copies of the events recorded so far.
func (n *reconcileEvents) snapshot() (jobs []ws.JobEvent, tasks []ws.TaskEvent) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Clone(n.jobs), slices.Clone(n.tasks)
}

// leaseGateBus is a recordBus that notes the state of the store at the moment
// the scheduler subscribes to lease requests, which is when it begins leasing.
type leaseGateBus struct {
	recordBus

	st store.Store
	// leasing receives once the scheduler subscribes to leases.
	leasing chan struct{}
	// stuckAtLease is how many steps ListStuckSteps still returned at that
	// moment. It is written by Run's goroutine before it signals leasing, so a
	// reader that has received from leasing may read it without a lock.
	stuckAtLease int
	// stuckErr is the error from that query.
	stuckErr error
}

// newLeaseGateBus returns a gate that inspects st, which must be the raw store
// rather than a counting wrapper so the inspection is not itself counted.
func newLeaseGateBus(st store.Store) *leaseGateBus {
	return &leaseGateBus{st: st, leasing: make(chan struct{}, 1)}
}

func (b *leaseGateBus) SubscribeLease(_ func(string, string, []byte) []byte) (*nats.Subscription, error) {
	stuck, err := b.st.ListStuckSteps(context.Background())
	b.stuckAtLease, b.stuckErr = len(stuck), err
	select {
	case b.leasing <- struct{}{}:
	default:
	}
	return nil, nil
}

// newReconcileScheduler builds a scheduler for these tests. Its heartbeat sweep
// interval is an hour, so no sweep tick lands inside a test: anything that
// changes after the repair was done by the repair or by what the test calls.
func newReconcileScheduler(st store.Store, bus busClient, n ws.Notifier) *Scheduler {
	cfg := DefaultConfig()
	cfg.DefaultMaxAttempts = 1
	cfg.HeartbeatSweepInterval = time.Hour
	return New(cfg, st, bus, metrics.New(), slog.New(slog.DiscardHandler), n, nil)
}

// startUntilLeasing runs s.Run on a goroutine and returns once the scheduler
// has begun accepting leases, by which point reconcileStuckSteps must already
// have finished. The returned stop cancels Run and waits for it to return; it
// is also registered as a cleanup, and is safe to call more than once.
func startUntilLeasing(t *testing.T, s *Scheduler, bus *leaseGateBus) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	var runErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		runErr = s.Run(ctx)
	}()
	stop = func() {
		cancel()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Error("Run did not return after its context was canceled")
		}
	}
	t.Cleanup(stop)

	select {
	case <-bus.leasing:
	case <-done:
		t.Fatalf("Run returned before it began leasing: %v", runErr)
	case <-time.After(60 * time.Second):
		t.Fatal("the scheduler never began leasing")
	}
	return stop
}

// ── seeding ─────────────────────────────────────────────────────────────────

func seedReconcileFarm(t *testing.T, st store.Store) {
	t.Helper()
	if _, err := st.CreateFarm(t.Context(), store.Farm{ID: "farm-1", Name: "farm-1"}); err != nil {
		t.Fatalf("CreateFarm: %v", err)
	}
	if _, err := st.CreateQueue(t.Context(), store.Queue{ID: "queue-1", FarmID: "farm-1", Name: "queue-1"}); err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}
}

func newReconcileJob(name string, status store.JobStatus) store.Job {
	return store.Job{
		ID: uuid.NewString(), FarmID: "farm-1", QueueID: "queue-1", Name: name,
		Status: status, TemplateFormat: store.TemplateFormatJSON,
	}
}

func newReconcileStep(jobID, name string, order int, status store.StepStatus, dependsOn ...string) store.Step {
	return store.Step{
		ID: uuid.NewString(), JobID: jobID, Name: name, DependsOn: dependsOn,
		StepOrder: order, Status: status,
	}
}

func newReconcileTask(jobID, stepID string, n int, status store.TaskStatus) store.Task {
	return store.Task{
		ID: uuid.NewString(), JobID: jobID, StepID: stepID, Name: fmt.Sprintf("t%d", n), Status: status,
	}
}

// reconcileTasks builds one task per status, in order.
func reconcileTasks(jobID, stepID string, statuses ...store.TaskStatus) []store.Task {
	out := make([]store.Task, 0, len(statuses))
	for i, status := range statuses {
		out = append(out, newReconcileTask(jobID, stepID, i, status))
	}
	return out
}

// submitReconcile creates a whole job through the real one-transaction
// submission path, which keeps the statuses it is given. That is how a
// 1,001-task step is seeded without a thousand separate writes, and it is how
// the stuck rows got there: the job, step and task rows are exactly what a
// v0.3.0 submission followed by a full run leaves.
func submitReconcile(t *testing.T, st store.Store, sub store.JobSubmission) {
	t.Helper()
	if _, err := st.CreateJobSubmission(t.Context(), sub); err != nil {
		t.Fatalf("CreateJobSubmission %q: %v", sub.Job.Name, err)
	}
}

// stuckFarm names the rows seedStuckFarm creates.
//
//	big job     "Big" (stuckStepTasks succeeded tasks) <- "After" (pending)
//	bad job     "Bad" (one failed task) <- "Down" (pending) <- "Tail" (pending)
//	solo job    "Solo" (succeeded tasks), the only step
//	released    blocked on the solo job, so it must be released
//	canceled    blocked on the bad job, so it must be canceled
//
// "Big", "Bad" and "Solo" are the three stuck steps.
type stuckFarm struct {
	bigJob, bigStep, afterStep, afterTask                   string
	badJob, badStep, downStep, downTask, tailStep, tailTask string
	soloJob, soloStep                                       string
	releasedJob, releasedStep, releasedTask                 string
	canceledJob, canceledStep, canceledTask                 string
}

// seedStuckFarm seeds the v0.3.0 damage spec §8.6 lists, with bigTasks tasks in
// the big step. Every stuck step is in status running with only terminal tasks.
func seedStuckFarm(t *testing.T, st store.Store, bigTasks int) stuckFarm {
	t.Helper()
	seedReconcileFarm(t, st)
	var f stuckFarm

	// A stuck step with a dependent step behind it, in the same job.
	big := newReconcileJob("big", store.JobStatusRunning)
	bigStep := newReconcileStep(big.ID, "Big", 0, store.StepStatusRunning)
	afterStep := newReconcileStep(big.ID, "After", 1, store.StepStatusPending, "Big")
	bigTaskRows := make([]store.Task, 0, bigTasks+1)
	for i := range bigTasks {
		bigTaskRows = append(bigTaskRows, newReconcileTask(big.ID, bigStep.ID, i, store.TaskStatusSucceeded))
	}
	afterTask := newReconcileTask(big.ID, afterStep.ID, 0, store.TaskStatusPending)
	submitReconcile(t, st, store.JobSubmission{
		Job: big, Steps: []store.Step{bigStep, afterStep}, Tasks: append(bigTaskRows, afterTask),
	})
	f.bigJob, f.bigStep, f.afterStep, f.afterTask = big.ID, bigStep.ID, afterStep.ID, afterTask.ID

	// A stuck step that failed: its dependent, and that dependent's dependent,
	// can never run and must be canceled by the cascade.
	bad := newReconcileJob("bad", store.JobStatusRunning)
	badStep := newReconcileStep(bad.ID, "Bad", 0, store.StepStatusRunning)
	downStep := newReconcileStep(bad.ID, "Down", 1, store.StepStatusPending, "Bad")
	tailStep := newReconcileStep(bad.ID, "Tail", 2, store.StepStatusPending, "Down")
	downTask := newReconcileTask(bad.ID, downStep.ID, 0, store.TaskStatusPending)
	tailTask := newReconcileTask(bad.ID, tailStep.ID, 0, store.TaskStatusPending)
	badTasks := reconcileTasks(bad.ID, badStep.ID, store.TaskStatusSucceeded, store.TaskStatusFailed)
	submitReconcile(t, st, store.JobSubmission{
		Job: bad, Steps: []store.Step{badStep, downStep, tailStep}, Tasks: append(badTasks, downTask, tailTask),
	})
	f.badJob, f.badStep, f.downStep, f.downTask = bad.ID, badStep.ID, downStep.ID, downTask.ID
	f.tailStep, f.tailTask = tailStep.ID, tailTask.ID

	// A stuck step that is its job's only step: finalizing it finalizes the job.
	solo := newReconcileJob("solo", store.JobStatusRunning)
	soloStep := newReconcileStep(solo.ID, "Solo", 0, store.StepStatusRunning)
	submitReconcile(t, st, store.JobSubmission{
		Job: solo, Steps: []store.Step{soloStep},
		Tasks: reconcileTasks(solo.ID, soloStep.ID, store.TaskStatusSucceeded, store.TaskStatusSucceeded),
	})
	f.soloJob, f.soloStep = solo.ID, soloStep.ID

	// Cross-job dependents. One waits on the job that will complete, the other
	// on the job that will fail.
	released := newReconcileJob("released", store.JobStatusBlocked)
	releasedStep := newReconcileStep(released.ID, "S", 0, store.StepStatusPending)
	releasedTask := newReconcileTask(released.ID, releasedStep.ID, 0, store.TaskStatusPending)
	submitReconcile(t, st, store.JobSubmission{
		Job: released, DependsOn: []string{solo.ID},
		Steps: []store.Step{releasedStep}, Tasks: []store.Task{releasedTask},
	})
	f.releasedJob, f.releasedStep, f.releasedTask = released.ID, releasedStep.ID, releasedTask.ID

	canceled := newReconcileJob("canceled", store.JobStatusBlocked)
	canceledStep := newReconcileStep(canceled.ID, "S", 0, store.StepStatusPending)
	canceledTask := newReconcileTask(canceled.ID, canceledStep.ID, 0, store.TaskStatusPending)
	submitReconcile(t, st, store.JobSubmission{
		Job: canceled, DependsOn: []string{bad.ID},
		Steps: []store.Step{canceledStep}, Tasks: []store.Task{canceledTask},
	})
	f.canceledJob, f.canceledStep, f.canceledTask = canceled.ID, canceledStep.ID, canceledTask.ID
	return f
}

// healthyFarm lists the job IDs seedHealthyFarm created.
type healthyFarm struct {
	jobs []string
}

// seedHealthyFarm seeds a farm with nothing to repair, covering every shape the
// stuck-step query has to leave alone: work in flight, a step with no tasks at
// all, finished jobs, a failed job with a canceled dependent, and a blocked job.
func seedHealthyFarm(t *testing.T, st store.Store) healthyFarm {
	t.Helper()
	seedReconcileFarm(t, st)
	var f healthyFarm

	live := newReconcileJob("live", store.JobStatusRunning)
	render := newReconcileStep(live.ID, "Render", 0, store.StepStatusRunning)
	empty := newReconcileStep(live.ID, "Empty", 1, store.StepStatusReady)
	later := newReconcileStep(live.ID, "Later", 2, store.StepStatusPending, "Render")
	liveTasks := reconcileTasks(live.ID, render.ID, store.TaskStatusRunning, store.TaskStatusSucceeded, store.TaskStatusReady)
	submitReconcile(t, st, store.JobSubmission{
		Job: live, Steps: []store.Step{render, empty, later},
		Tasks: append(liveTasks, newReconcileTask(live.ID, later.ID, 0, store.TaskStatusPending)),
	})
	f.jobs = append(f.jobs, live.ID)

	done := newReconcileJob("done", store.JobStatusCompleted)
	doneStep := newReconcileStep(done.ID, "Done", 0, store.StepStatusCompleted)
	submitReconcile(t, st, store.JobSubmission{
		Job: done, Steps: []store.Step{doneStep},
		Tasks: reconcileTasks(done.ID, doneStep.ID, store.TaskStatusSucceeded, store.TaskStatusSucceeded),
	})
	f.jobs = append(f.jobs, done.ID)

	failed := newReconcileJob("failed", store.JobStatusFailed)
	failedStep := newReconcileStep(failed.ID, "Bad", 0, store.StepStatusFailed)
	skipped := newReconcileStep(failed.ID, "Skipped", 1, store.StepStatusCanceled, "Bad")
	failedTasks := reconcileTasks(failed.ID, failedStep.ID, store.TaskStatusSucceeded, store.TaskStatusFailed)
	submitReconcile(t, st, store.JobSubmission{
		Job: failed, Steps: []store.Step{failedStep, skipped},
		Tasks: append(failedTasks, newReconcileTask(failed.ID, skipped.ID, 0, store.TaskStatusCanceled)),
	})
	f.jobs = append(f.jobs, failed.ID)

	blocked := newReconcileJob("blocked", store.JobStatusBlocked)
	blockedStep := newReconcileStep(blocked.ID, "S", 0, store.StepStatusPending)
	submitReconcile(t, st, store.JobSubmission{
		Job: blocked, DependsOn: []string{live.ID}, Steps: []store.Step{blockedStep},
		Tasks: reconcileTasks(blocked.ID, blockedStep.ID, store.TaskStatusPending),
	})
	f.jobs = append(f.jobs, blocked.ID)
	return f
}

// snapshotFarm reads every job, step and task row of jobIDs, including their
// timestamps, so two snapshots are equal only if no row was written between
// them.
func snapshotFarm(t *testing.T, st store.Store, jobIDs []string) []any {
	t.Helper()
	var out []any
	for _, id := range jobIDs {
		out = append(out, mustJob(t, st, id))
		steps, err := st.ListSteps(t.Context(), id)
		if err != nil {
			t.Fatalf("ListSteps %s: %v", id, err)
		}
		for _, step := range steps {
			out = append(out, step)
			page, err := st.ListTasks(t.Context(), store.ListTasksOptions{
				StepID: step.ID, Pagination: store.Pagination{Limit: store.MaxLimit},
			})
			if err != nil {
				t.Fatalf("ListTasks %s: %v", step.ID, err)
			}
			// Rows created in one clock tick share a created_at, so the page order
			// is not stable; sort so equal data compares equal.
			slices.SortFunc(page.Items, func(a, b store.Task) int { return strings.Compare(a.ID, b.ID) })
			for _, task := range page.Items {
				out = append(out, task)
			}
		}
	}
	return out
}

// stuckStepIDs returns the IDs ListStuckSteps reports, sorted.
func stuckStepIDs(t *testing.T, st store.Store) []string {
	t.Helper()
	steps, err := st.ListStuckSteps(t.Context())
	if err != nil {
		t.Fatalf("ListStuckSteps: %v", err)
	}
	ids := make([]string, 0, len(steps))
	for _, s := range steps {
		ids = append(ids, s.ID)
	}
	sort.Strings(ids)
	return ids
}

// ── assertions ──────────────────────────────────────────────────────────────

func wantStepStatus(t *testing.T, st store.Store, id, label string, want store.StepStatus) {
	t.Helper()
	if got := mustStep(t, st, id); got.Status != want {
		t.Errorf("step %s = %q, want %q", label, got.Status, want)
	}
}

func wantJobStatus(t *testing.T, st store.Store, id, label string, want store.JobStatus) {
	t.Helper()
	if got := mustJob(t, st, id); got.Status != want {
		t.Errorf("job %s = %q, want %q", label, got.Status, want)
	}
}

func wantTaskStatus(t *testing.T, st store.Store, id, label string, want store.TaskStatus, wantReason string) {
	t.Helper()
	got := mustTaskOf(t, st, id)
	if got.Status != want {
		t.Errorf("task %s = %q, want %q", label, got.Status, want)
	}
	if got.FailureReason != wantReason {
		t.Errorf("task %s failure_reason = %q, want %q", label, got.FailureReason, wantReason)
	}
}

// wantRepaired asserts every row of the stuck farm after the repair and one
// sweepBlockedJobs tick.
func (f stuckFarm) wantRepaired(t *testing.T, st store.Store) {
	t.Helper()

	// The 1,001-task step completes, and the step behind it is released with
	// its task. Its job is not finished: the released step is still to run.
	wantStepStatus(t, st, f.bigStep, "Big", store.StepStatusCompleted)
	wantStepStatus(t, st, f.afterStep, "After", store.StepStatusReady)
	wantTaskStatus(t, st, f.afterTask, "After/0", store.TaskStatusReady, "")
	wantJobStatus(t, st, f.bigJob, "big", store.JobStatusRunning)

	// The failed step fails, its dependents are canceled transitively with the
	// durable reason, and the job fails.
	wantStepStatus(t, st, f.badStep, "Bad", store.StepStatusFailed)
	wantStepStatus(t, st, f.downStep, "Down", store.StepStatusCanceled)
	wantStepStatus(t, st, f.tailStep, "Tail", store.StepStatusCanceled)
	wantTaskStatus(t, st, f.downTask, "Down/0", store.TaskStatusCanceled, store.FailureReasonUpstreamFailed)
	wantTaskStatus(t, st, f.tailTask, "Tail/0", store.TaskStatusCanceled, store.FailureReasonUpstreamFailed)
	wantJobStatus(t, st, f.badJob, "bad", store.JobStatusFailed)

	// The one-step job completes.
	wantStepStatus(t, st, f.soloStep, "Solo", store.StepStatusCompleted)
	wantJobStatus(t, st, f.soloJob, "solo", store.JobStatusCompleted)

	// Cross-job dependents: released behind the completed job, canceled behind
	// the failed one.
	wantJobStatus(t, st, f.releasedJob, "released", store.JobStatusPending)
	wantStepStatus(t, st, f.releasedStep, "released/S", store.StepStatusReady)
	wantTaskStatus(t, st, f.releasedTask, "released/S/0", store.TaskStatusReady, "")
	wantJobStatus(t, st, f.canceledJob, "canceled", store.JobStatusCanceled)
	wantStepStatus(t, st, f.canceledStep, "canceled/S", store.StepStatusCanceled)
	wantTaskStatus(t, st, f.canceledTask, "canceled/S/0", store.TaskStatusCanceled, store.FailureReasonUpstreamFailed)
}

// wantEvents asserts the WebSocket events of the first start: exactly one job
// event for each job whose status the repair changed, and one canceled event
// for each cascade-canceled task, and nothing for the job that is still
// running.
func (f stuckFarm) wantEvents(t *testing.T, events *reconcileEvents) {
	t.Helper()
	jobs, tasks := events.snapshot()

	gotJobs := map[string][]string{}
	for _, e := range jobs {
		gotJobs[e.JobID] = append(gotJobs[e.JobID], e.Status)
	}
	wantJobs := map[string][]string{
		f.badJob:      {string(store.JobStatusFailed)},
		f.soloJob:     {string(store.JobStatusCompleted)},
		f.releasedJob: {string(store.JobStatusPending)},
		f.canceledJob: {string(store.JobStatusCanceled)},
	}
	if !reflect.DeepEqual(gotJobs, wantJobs) {
		t.Errorf("job events = %v, want %v (the still-running big job must emit none)", gotJobs, wantJobs)
	}

	gotTasks := map[string][]string{}
	for _, e := range tasks {
		gotTasks[e.TaskID] = append(gotTasks[e.TaskID], e.Status)
	}
	canceled := []string{string(store.TaskStatusCanceled)}
	wantTasks := map[string][]string{f.downTask: canceled, f.tailTask: canceled, f.canceledTask: canceled}
	if !reflect.DeepEqual(gotTasks, wantTasks) {
		t.Errorf("task events = %v, want %v", gotTasks, wantTasks)
	}
}

// ── tests ───────────────────────────────────────────────────────────────────

// TestReconcileStuckSteps_RepairsOnStart is spec §8.6's scenario: it seeds a
// stuck 1,001-task step with a dependent step behind it, a second stuck step
// whose failure cascade-cancels its dependent, a third that finishes its job,
// and a cross-job dependent on each of those jobs. It starts the scheduler,
// and asserts that the steps and their jobs are finalized, that the dependents
// are released or canceled, that the events fire, and that the repair had
// finished before the scheduler began leasing. A second start then finds
// nothing to do: no events and no writes.
func TestReconcileStuckSteps_RepairsOnStart(t *testing.T) {
	for name, st := range raceBackends(t) {
		t.Run(name, func(t *testing.T) {
			f := seedStuckFarm(t, st, stuckStepTasks)

			// The precondition that makes everything below mean something: the
			// three steps are stuck, the dependents are untouched, and the cross-job
			// dependents are blocked.
			want := []string{f.bigStep, f.badStep, f.soloStep}
			sort.Strings(want)
			if got := stuckStepIDs(t, st); !slices.Equal(got, want) {
				t.Fatalf("seeded farm: ListStuckSteps = %v, want exactly the three stuck steps %v", got, want)
			}
			wantStepStatus(t, st, f.afterStep, "After (before)", store.StepStatusPending)
			wantJobStatus(t, st, f.releasedJob, "released (before)", store.JobStatusBlocked)
			wantJobStatus(t, st, f.canceledJob, "canceled (before)", store.JobStatusBlocked)

			// First start.
			counted := &writeCountingStore{Store: st}
			events := &reconcileEvents{}
			bus := newLeaseGateBus(st)
			s := newReconcileScheduler(counted, bus, events)
			stop := startUntilLeasing(t, s, bus)

			if bus.stuckErr != nil {
				t.Fatalf("ListStuckSteps at lease time: %v", bus.stuckErr)
			}
			if bus.stuckAtLease != 0 {
				t.Errorf("%d steps were still stuck when the scheduler began leasing: the repair must finish first", bus.stuckAtLease)
			}
			if n := counted.listStuck.Load(); n != 1 {
				t.Errorf("ListStuckSteps calls = %d, want the one query", n)
			}
			// Each stuck step goes through the real completion path: one
			// FinalizeStep and one FinalizeJob per stuck step, plus the
			// propagation it triggers. This is also what shows the counters work.
			if n := counted.finalizeStep.Load(); n != 3 {
				t.Errorf("FinalizeStep calls = %d, want 3 (one per stuck step)", n)
			}
			if n := counted.finalizeJob.Load(); n != 3 {
				t.Errorf("FinalizeJob calls = %d, want 3 (one per stuck step)", n)
			}
			if counted.releaseStep.Load() == 0 || counted.cancelPendingStep.Load() == 0 {
				t.Errorf("propagation did not run: ReleaseStep calls = %d, CancelPendingStep calls = %d",
					counted.releaseStep.Load(), counted.cancelPendingStep.Load())
			}

			// Cross-job dependents follow on one sweepBlockedJobs tick, the
			// periodic backstop (the sweep interval is an hour, so it does not fire
			// by itself).
			if err := s.sweepBlockedJobs(t.Context()); err != nil {
				t.Fatalf("sweepBlockedJobs: %v", err)
			}

			f.wantRepaired(t, st)
			f.wantEvents(t, events)
			if got := stuckStepIDs(t, st); len(got) != 0 {
				t.Errorf("ListStuckSteps after the repair = %v, want none", got)
			}
			stop()

			// Second start on the repaired farm: the first one emitted events (above),
			// so silence here is a statement about the second.
			writesBefore, queriesBefore := counted.completionWrites(), counted.listStuck.Load()
			events2 := &reconcileEvents{}
			bus2 := newLeaseGateBus(st)
			stop2 := startUntilLeasing(t, newReconcileScheduler(counted, bus2, events2), bus2)
			stop2()

			if bus2.stuckErr != nil || bus2.stuckAtLease != 0 {
				t.Errorf("second start: stuck steps at lease time = %d, err %v, want none", bus2.stuckAtLease, bus2.stuckErr)
			}
			if n := counted.listStuck.Load() - queriesBefore; n != 1 {
				t.Errorf("second start ran ListStuckSteps %d times, want 1", n)
			}
			if n := counted.completionWrites() - writesBefore; n != 0 {
				t.Errorf("second start made %d completion writes, want 0", n)
			}
			if jobs, tasks := events2.snapshot(); len(jobs) != 0 || len(tasks) != 0 {
				t.Errorf("second start emitted events on a repaired farm: jobs %v, tasks %v", jobs, tasks)
			}
		})
	}
}

// TestReconcileStuckSteps_HealthyFarmSeesNoWrites starts the scheduler on a
// farm with nothing to repair. ListStuckSteps returning nothing is the
// precondition; that none of the completion-path writes is called, no row
// changes, and no event is emitted is the proof that a start costs a healthy
// farm one query.
func TestReconcileStuckSteps_HealthyFarmSeesNoWrites(t *testing.T) {
	for name, st := range raceBackends(t) {
		t.Run(name, func(t *testing.T) {
			f := seedHealthyFarm(t, st)
			if got := stuckStepIDs(t, st); len(got) != 0 {
				t.Fatalf("seeded healthy farm: ListStuckSteps = %v, want none", got)
			}
			before := snapshotFarm(t, st, f.jobs)

			counted := &writeCountingStore{Store: st}
			events := &reconcileEvents{}
			bus := newLeaseGateBus(st)
			stop := startUntilLeasing(t, newReconcileScheduler(counted, bus, events), bus)
			stop()

			if n := counted.listStuck.Load(); n != 1 {
				t.Errorf("ListStuckSteps calls = %d, want exactly the one query", n)
			}
			if n := counted.completionWrites(); n != 0 {
				t.Errorf("a healthy farm saw %d completion writes (FinalizeStep %d, FinalizeJob %d, ReleaseStep %d, CancelPendingStep %d), want 0",
					n, counted.finalizeStep.Load(), counted.finalizeJob.Load(), counted.releaseStep.Load(), counted.cancelPendingStep.Load())
			}
			if after := snapshotFarm(t, st, f.jobs); !reflect.DeepEqual(before, after) {
				t.Errorf("a row of a healthy farm changed across a start:\nbefore %+v\nafter  %+v", before, after)
			}
			if jobs, tasks := events.snapshot(); len(jobs) != 0 || len(tasks) != 0 {
				t.Errorf("a healthy farm emitted events: jobs %v, tasks %v", jobs, tasks)
			}
		})
	}
}

// TestReconcileStuckSteps_FailedStepIsSkippedAndRetried covers the error path.
// A step that cannot be finalized is logged and skipped, the rest of the pass
// still runs, and the step stays stuck so the next start picks it up again.
func TestReconcileStuckSteps_FailedStepIsSkippedAndRetried(t *testing.T) {
	for name, st := range raceBackends(t) {
		t.Run(name, func(t *testing.T) {
			f := seedStuckFarm(t, st, 3)

			failing := &writeCountingStore{Store: st, failFinalizeStep: f.badStep}
			newReconcileScheduler(failing, &recordBus{}, ws.NoopNotifier{}).reconcileStuckSteps(t.Context())

			wantStepStatus(t, st, f.bigStep, "Big", store.StepStatusCompleted)
			wantStepStatus(t, st, f.soloStep, "Solo", store.StepStatusCompleted)
			wantStepStatus(t, st, f.badStep, "Bad (its finalize failed)", store.StepStatusRunning)
			wantStepStatus(t, st, f.downStep, "Down (nothing cascaded)", store.StepStatusPending)
			if got := stuckStepIDs(t, st); !slices.Equal(got, []string{f.badStep}) {
				t.Fatalf("ListStuckSteps = %v, want only the step whose finalize failed", got)
			}

			// The next start, with the store healthy again, finishes the job.
			newReconcileScheduler(st, &recordBus{}, ws.NoopNotifier{}).reconcileStuckSteps(t.Context())
			wantStepStatus(t, st, f.badStep, "Bad (retried)", store.StepStatusFailed)
			wantStepStatus(t, st, f.downStep, "Down (retried)", store.StepStatusCanceled)
			wantJobStatus(t, st, f.badJob, "bad (retried)", store.JobStatusFailed)
		})
	}
}

// TestReconcileStuckSteps_ListFailureWritesNothing: when the one query fails the
// pass is abandoned without a write, and the steps stay stuck for the next
// start.
func TestReconcileStuckSteps_ListFailureWritesNothing(t *testing.T) {
	for name, st := range raceBackends(t) {
		t.Run(name, func(t *testing.T) {
			f := seedStuckFarm(t, st, 3)

			counted := &writeCountingStore{Store: st, listErr: errInjectedReconcile}
			newReconcileScheduler(counted, &recordBus{}, ws.NoopNotifier{}).reconcileStuckSteps(t.Context())

			if n := counted.completionWrites(); n != 0 {
				t.Errorf("%d completion writes after the query failed, want 0", n)
			}
			if got := stuckStepIDs(t, st); len(got) != 3 {
				t.Errorf("ListStuckSteps = %v, want the three steps still stuck", got)
			}
			wantStepStatus(t, st, f.bigStep, "Big", store.StepStatusRunning)
		})
	}
}
