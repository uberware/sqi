// SPDX-License-Identifier: AGPL-3.0-or-later

package verify

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/uberware/sqi/internal/loadtest/internal/sleep"

	"github.com/uberware/sqi/internal/loadtest/client"
	"github.com/uberware/sqi/internal/loadtest/driver"
	"github.com/uberware/sqi/internal/loadtest/simworker"
)

// ── The ledger, per task ─────────────────────────────────────────────────────

// ledgerAttempt is one attempt as the simulator saw it.
type ledgerAttempt struct {
	id, taskID, jobID string
	// leased is when the lease reached the worker, workerID which worker
	// that was; zero and "" when the ledger has no leased event for it.
	leased   time.Time
	workerID string
	// assigned is the server's time for the assignment, zero when unknown.
	assigned time.Time
	// end is the attempt's first terminal, canceled or crashed event; nil
	// while the simulator has not ended it.
	end *simworker.Event
}

// ledgerIndex is the ledger by task and by attempt. It is built once per
// [Check] and brought up to date with [ledgerIndex.refresh] before each read
// that must see events recorded since: the ledger only grows.
type ledgerIndex struct {
	l    *simworker.Ledger
	seen int // events folded in so far
	// byTask lists each task's attempts in the order the server assigned
	// them. The server leases a task's next attempt only after closing the
	// previous one, so the last is the attempt the server holds as the task's
	// latest. The order the leases reached the workers can differ: a reply
	// held up in flight (after a server stall, say) can land after the next
	// attempt's, so an attempt's place comes from its assignment time when
	// the message carried one, and from when the ledger first saw it
	// otherwise.
	byTask map[string][]*ledgerAttempt
	byID   map[string]*ledgerAttempt
	// byJob lists each job's leased tasks, in the order first leased.
	byJob map[string][]string
}

func indexLedger(l *simworker.Ledger) *ledgerIndex {
	x := &ledgerIndex{l: l, byTask: map[string][]*ledgerAttempt{}, byID: map[string]*ledgerAttempt{}, byJob: map[string][]string{}}
	x.refresh()
	return x
}

// refresh folds in the events recorded since the last refresh. It must not
// run concurrently with a read of x.
func (x *ledgerIndex) refresh() {
	if x.l == nil {
		return
	}
	events := x.l.EventsSince(x.seen)
	x.seen += len(events)
	for _, e := range events {
		x.add(e)
	}
}

func (x *ledgerIndex) add(e simworker.Event) {
	a := x.byID[e.AttemptID]
	if a == nil {
		a = &ledgerAttempt{id: e.AttemptID, taskID: e.TaskID, jobID: e.JobID, assigned: e.AssignedAt}
		x.byID[e.AttemptID] = a
		if len(x.byTask[e.TaskID]) == 0 {
			x.byJob[e.JobID] = append(x.byJob[e.JobID], e.TaskID)
		}
		x.byTask[e.TaskID] = insertByAssignment(x.byTask[e.TaskID], a)
	}
	switch e.Kind {
	case simworker.KindLeased:
		if a.leased.IsZero() {
			a.leased, a.workerID = e.At, e.WorkerID
		}
	case simworker.KindTerminal, simworker.KindCanceled, simworker.KindCrashed:
		if a.end == nil {
			a.end = &e
		}
	}
}

// LeasedAttempt is an attempt a simulated worker leased, as the invariant
// checks read it from the ledger.
type LeasedAttempt struct {
	ID, TaskID, WorkerID string
	// Leased is when the lease reached the worker.
	Leased time.Time
	// End is the attempt's terminal, canceled or crashed event; nil while the
	// worker has not ended it. A worker records one per attempt.
	End *simworker.Event
}

// Outcome is the outcome of the attempt's terminal event, "" when it ended
// otherwise or has not ended.
func (a LeasedAttempt) Outcome() string {
	if a.End == nil || a.End.Kind != simworker.KindTerminal {
		return ""
	}
	return a.End.Outcome
}

// LeasedAttempts returns every attempt l has a leased event for, in lease
// order.
func LeasedAttempts(l *simworker.Ledger) []LeasedAttempt {
	x := indexLedger(l)
	out := make([]LeasedAttempt, 0, len(x.byID))
	for _, a := range x.byID {
		if !a.leased.IsZero() {
			out = append(out, LeasedAttempt{ID: a.id, TaskID: a.taskID, WorkerID: a.workerID, Leased: a.leased, End: a.end})
		}
	}
	slices.SortFunc(out, func(a, b LeasedAttempt) int {
		if c := a.Leased.Compare(b.Leased); c != 0 {
			return c
		}
		return cmp.Compare(a.ID, b.ID)
	})
	return out
}

// insertByAssignment adds a to atts, a task's attempts in assignment order: it
// goes after the last attempt assigned no later than it. An attempt with no
// assignment time, or after one with none, goes at the end.
func insertByAssignment(atts []*ledgerAttempt, a *ledgerAttempt) []*ledgerAttempt {
	i := len(atts)
	if !a.assigned.IsZero() {
		for i > 0 && !atts[i-1].assigned.IsZero() && atts[i-1].assigned.After(a.assigned) {
			i--
		}
	}
	return slices.Insert(atts, i, a)
}

// decision is what the simulator decided for a task: the outcome of its
// last attempt.
type decision struct {
	status  string // the task status it implies, when decided
	decided bool
	leased  int            // how many attempts the simulator leased
	last    *ledgerAttempt // the last of them, nil when never leased
}

// decide returns the task's decision. Only a succeeded or failed terminal
// event (a timeout is a failed one) or a canceled event decides; a task never
// leased, or whose last attempt crashed, ended in a worker shutdown or has
// not ended, is undecided.
func (x *ledgerIndex) decide(taskID string) decision {
	atts := x.byTask[taskID]
	if len(atts) == 0 {
		return decision{}
	}
	a := atts[len(atts)-1]
	d := decision{leased: len(atts), last: a}
	switch {
	case a.end == nil, a.end.Kind == simworker.KindCrashed:
	case a.end.Kind == simworker.KindCanceled:
		d.status, d.decided = statusCancel, true
	case a.end.Outcome == simworker.OutcomeSucceeded, a.end.Outcome == simworker.OutcomeFailed:
		d.status, d.decided = a.end.Outcome, true
	}
	return d
}

// why describes the decision for evidence. It is formatted only when a check
// reports one, not for every task it decides.
func (d decision) why() string {
	a := d.last
	if a == nil {
		return "never leased"
	}
	prefix := fmt.Sprintf("last of %d attempts, %s, ", d.leased, a.id)
	switch {
	case a.end == nil:
		return prefix + "not ended"
	case a.end.Kind == simworker.KindCanceled:
		return prefix + "canceled"
	case a.end.Kind == simworker.KindCrashed:
		return prefix + "crashed"
	case d.decided:
		return prefix + a.end.Outcome
	default:
		return prefix + "ended " + a.end.Outcome
	}
}

// ── terminal ─────────────────────────────────────────────────────────────────

// terminal: every accepted job and every orphan is completed, failed or
// canceled.
func (c *checker) terminal(statuses map[string]string, errs map[string]error) Result {
	var ev Evidence
	for _, id := range append(slices.Clone(c.order), c.orphanIDs...) {
		what := "job " + id
		if o, ok := c.orphans[id]; ok {
			what = fmt.Sprintf("job %s (orphan of submission %s)", id, o.Name)
		}
		if err := errs[id]; err != nil {
			ev.Add(fmt.Sprintf("%s: reading its status: %v", what, err))
			continue
		}
		if s := statuses[id]; !terminalJob(s) {
			ev.Add(fmt.Sprintf("%s is %s, want completed, failed or canceled", what, s))
		}
	}
	return ev.Result(NameTerminal)
}

// ── The task sweep ───────────────────────────────────────────────────────────

// taskRef is a task as the server listed it.
type taskRef struct{ jobID, taskID, status string }

// suspect is a task whose server status disagrees with the ledger as first
// read.
type suspect struct {
	taskRef

	server string
}

// sweep is what reading every task list found.
type sweep struct {
	evidence []string  // listing errors, dropped work and completed jobs with unfinished tasks, by job
	suspects []suspect // outcome disagreements to settle
	attempts []taskRef // sampled for single-attempt
	logs     []taskRef // sampled for logs
}

// sweepTasks reads every accepted job's tasks a page at a time, applies the
// outcome rules and draws the attempt and log samples.
func (c *checker) sweepTasks(ctx context.Context, statuses map[string]string) sweep {
	x := c.ledger
	parts := parallelMap(len(c.order), func(i int) sweep {
		id := c.order[i]
		return c.sweepJob(ctx, id, statuses[id], x)
	})
	var all sweep
	for _, p := range parts {
		all.evidence = append(all.evidence, p.evidence...)
		all.suspects = append(all.suspects, p.suspects...)
		all.attempts = append(all.attempts, p.attempts...)
		all.logs = append(all.logs, p.logs...)
	}
	return all
}

func (c *checker) sweepJob(ctx context.Context, jobID, jobStatus string, x *ledgerIndex) sweep {
	var s sweep
	tasks, total, err := c.listTasks(ctx, jobID)
	if err != nil {
		s.evidence = append(s.evidence, fmt.Sprintf("job %s: listing tasks: %v", jobID, err))
		return s
	}
	s.evidence = append(s.evidence, c.droppedTasks(jobID, tasks, total, x)...)
	for _, t := range tasks {
		if c.in.TaskSeen != nil {
			c.in.TaskSeen(jobID, t)
		}
		ref := taskRef{jobID: jobID, taskID: t.ID, status: t.Status}
		if jobStatus == jobCompleted && t.Status != statusOK {
			s.evidence = append(s.evidence, fmt.Sprintf("job %s is completed but task %s is %s, want succeeded", jobID, t.ID, t.Status))
		}
		if sampled(c.in.Seed, saltAttempts, t.ID, c.in.AttemptSample) {
			s.attempts = append(s.attempts, ref)
		}
		// A real worker's task has no ledger to compare with; the server-side
		// rules above and the attempt sample still apply to it.
		if c.realWorker(t.AssignedWorkerID) {
			continue
		}
		if c.outcomeMsg(ref, t.Status, x) != "" {
			s.suspects = append(s.suspects, suspect{taskRef: ref, server: t.Status})
		}
		if sampled(c.in.Seed, saltLogs, t.ID, c.in.LogSample) {
			s.logs = append(s.logs, ref)
		}
	}
	return s
}

// droppedTasks reports work missing from a job: a task count other than the
// class's steps x tasks_per_step (each step expands to exactly tasks_per_step
// tasks), and any task the simulator was leased that the server does not
// list.
func (c *checker) droppedTasks(jobID string, tasks []client.Task, total int, x *ledgerIndex) []string {
	var out []string
	if jc, ok := c.classOf(jobID); ok && jc.Steps > 0 && jc.TasksPerStep > 0 {
		want := jc.Steps * jc.TasksPerStep
		if total != want || len(tasks) != want {
			out = append(out, fmt.Sprintf("job %s has %d tasks (%d listed), want %d (%d steps x %d tasks)",
				jobID, total, len(tasks), want, jc.Steps, jc.TasksPerStep))
		}
	}
	listed := make(map[string]bool, len(tasks))
	for _, t := range tasks {
		listed[t.ID] = true
	}
	for _, id := range x.byJob[jobID] {
		if !listed[id] {
			out = append(out, fmt.Sprintf("job %s: the simulator ran task %s, which the server does not list", jobID, id))
		}
	}
	return out
}

// listTasks returns all of a job's tasks, in the server's order, and the total
// the server reports. The first page gives the total; the remaining pages are
// read in parallel, since a job of a million tasks is a thousand pages and the
// server takes longer over each as its offset grows.
func (c *checker) listTasks(ctx context.Context, jobID string) (tasks []client.Task, total int, err error) {
	first, err := c.v.ListTasks(ctx, jobID, maxPage, 0)
	if err != nil {
		return nil, 0, err
	}
	tasks, total = first.Items, first.Total
	if len(first.Items) == 0 {
		return tasks, total, nil
	}
	var offsets []int
	for off := len(first.Items); off < total; off += maxPage {
		offsets = append(offsets, off)
	}
	type tail struct {
		items []client.Task
		err   error
	}
	rest := parallelMap(len(offsets), func(i int) tail {
		p, err := c.v.ListTasks(ctx, jobID, maxPage, offsets[i])
		return tail{p.Items, err}
	})
	for _, r := range rest {
		if r.err != nil {
			return nil, 0, r.err
		}
		tasks = append(tasks, r.items...)
	}
	return tasks, total, nil
}

// ── outcomes ─────────────────────────────────────────────────────────────────

// outcomes: every task's server status is the one the simulator decided, and
// a completed job has only succeeded tasks.
func (c *checker) outcomes(ctx context.Context, s sweep) Result {
	var ev Evidence
	ev.AddAll(s.evidence)
	for _, v := range settleLoop(ctx, c.settle, s.suspects, func(sus []suspect) []verdict {
		x := c.ledger
		x.refresh()
		vs := make([]verdict, len(sus))
		for i, t := range sus {
			vs[i] = verdict{msg: c.outcomeMsg(t.taskRef, t.server, x)}
		}
		return vs
	}) {
		ev.Add(v.msg)
	}
	return ev.Result(NameOutcomes)
}

// outcomeMsg returns why the task's server status is wrong, or "".
//
// A decided task must have the decided status. A task the simulator never
// leased can only have been canceled (its job was canceled or blocked, or a
// step it depends on failed). A task of a job the run canceled may be
// canceled whatever was decided: the cancel can land just as the simulator
// finishes the task, and the server keeps it. Nothing else excuses a cancel:
// the server requeues a leased task whose attempt crashed or never ended, a
// cascade cancel touches only pending tasks, and a server job cancel only
// blocked jobs.
func (c *checker) outcomeMsg(ref taskRef, server string, x *ledgerIndex) string {
	d := x.decide(ref.taskID)
	if server == statusCancel && (d.leased == 0 || c.canceled[ref.jobID]) {
		return ""
	}
	if d.decided && server == d.status {
		return ""
	}
	want := statusCancel
	switch {
	case d.decided:
		want = d.status
	case d.leased > 0:
		want = "a status the simulator decided (it decided none)"
	}
	return fmt.Sprintf("job %s task %s: server %s, want %s (simulator: %s)", ref.jobID, ref.taskID, server, want, d.why())
}

// ── single-attempt ───────────────────────────────────────────────────────────

// singleAttempt: on each sampled task, no two of the server's attempts are
// held at once, every attempt after the first follows a failed or canceled
// one (a retry or a reclaim), and a task in a terminal status holds no open
// attempt. On every task, the simulator never ran two attempts at once (see
// [ledgerOverlaps]).
func (c *checker) singleAttempt(ctx context.Context, refs []taskRef) Result {
	var unread atomic.Bool
	msgs := parallelMap(len(refs), func(i int) string {
		ref := refs[i]
		atts, err := c.v.ListAttempts(ctx, ref.taskID)
		if err != nil {
			unread.Store(true)
			return fmt.Sprintf("job %s task %s: listing attempts: %v", ref.jobID, ref.taskID, err)
		}
		return attemptsMsg(ref, atts)
	})
	var ev Evidence
	ev.AddAll(c.ledgerOverlaps())
	ev.AddAll(msgs)
	// A task whose attempts could not be listed was not judged: the result
	// says so, and no known violation may excuse it.
	ev.unmeasured = unread.Load()
	return ev.Result(NameSingleAttempt)
}

// ledgerOverlaps returns, for every task of an accepted job, each attempt the
// simulator was leased while an earlier attempt of the same task had not yet
// ended (by a terminal, canceled or crashed event) on the generator's own
// clock. The server's attempt list cannot show it: a worker that keeps
// running an attempt the server already closed (reclaimed after its
// heartbeats were lost, or canceled with the cancel lost) leaves the server
// with sequential attempts and the stale worker's report rejected, although
// the task ran twice at once.
func (c *checker) ledgerOverlaps() []string {
	x := c.ledger
	x.refresh()
	var out []string
	for _, jobID := range c.order {
		for _, taskID := range x.byJob[jobID] {
			if len(x.byTask[taskID]) < 2 {
				continue
			}
			atts := slices.Clone(x.byTask[taskID])
			atts = slices.DeleteFunc(atts, func(a *ledgerAttempt) bool { return a.leased.IsZero() })
			slices.SortStableFunc(atts, func(a, b *ledgerAttempt) int { return a.leased.Compare(b.leased) })
			for i, later := range atts {
				for _, earlier := range atts[:i] {
					if msg := overlapMsg(jobID, taskID, earlier, later); msg != "" {
						out = append(out, msg)
						break
					}
				}
			}
		}
	}
	return out
}

// overlapMsg describes earlier still running when later was leased, or "".
func overlapMsg(jobID, taskID string, earlier, later *ledgerAttempt) string {
	if earlier.end != nil && !earlier.end.At.After(later.leased) {
		return ""
	}
	until := "had not ended"
	if earlier.end != nil {
		until = fmt.Sprintf("ran until %s (%s)", stamp(earlier.end.At), earlier.end.Kind)
	}
	return fmt.Sprintf("job %s task %s ran twice at once: attempt %s was leased to worker %s at %s while attempt %s on worker %s, leased at %s, %s",
		jobID, taskID, later.id, later.workerID, stamp(later.leased), earlier.id, earlier.workerID, stamp(earlier.leased), until)
}

// skewTolerance is how far an attempt may start before the previous one's
// ended_at without counting as an overlap. The two stamps come from different
// clocks: started_at is the server's, at lease, while a worker-reported end
// carries the worker's own time.
const skewTolerance = time.Second

// attemptsMsg returns the first violation in a task's attempts, or "". An
// attempt is held over [started_at, ended_at), to the end of time while
// ended_at is unset.
func attemptsMsg(ref taskRef, atts []client.Attempt) string {
	atts = slices.Clone(atts)
	slices.SortFunc(atts, func(a, b client.Attempt) int { return cmp.Compare(a.AttemptNumber, b.AttemptNumber) })
	var (
		heldUntil time.Time // the latest end so far
		heldBy    int       // its attempt number
		openBy    int       // an attempt with no end, or 0
	)
	for i, a := range atts {
		where := fmt.Sprintf("job %s task %s attempt %d", ref.jobID, ref.taskID, a.AttemptNumber)
		if a.StartedAt == nil {
			return where + ": no started_at"
		}
		if i > 0 {
			prev := atts[i-1]
			switch {
			case openBy != 0:
				return fmt.Sprintf("%s started %s while attempt %d was still held (no ended_at)", where, stamp(*a.StartedAt), openBy)
			case a.StartedAt.Before(heldUntil.Add(-skewTolerance)):
				return fmt.Sprintf("%s started %s, before attempt %d ended at %s", where, stamp(*a.StartedAt), heldBy, stamp(heldUntil))
			case prev.Status != statusFailed && prev.Status != statusCancel:
				return fmt.Sprintf("%s follows attempt %d with status %s, want failed or canceled (a retry or a reclaim)", where, prev.AttemptNumber, prev.Status)
			}
		}
		switch {
		case a.EndedAt == nil:
			openBy = a.AttemptNumber
		case a.EndedAt.After(heldUntil):
			heldUntil, heldBy = *a.EndedAt, a.AttemptNumber
		}
	}
	if openBy != 0 && terminalTask[ref.status] {
		return fmt.Sprintf("job %s task %s is %s but attempt %d has no ended_at", ref.jobID, ref.taskID, ref.status, openBy)
	}
	return ""
}

// terminalTask holds the task statuses that close every attempt.
var terminalTask = map[string]bool{statusOK: true, statusFailed: true, statusCancel: true}

func stamp(t time.Time) string { return t.UTC().Format("15:04:05.000000") }

// ── logs ─────────────────────────────────────────────────────────────────────

// logs: on each sampled task, the lines of the server's latest attempt are the
// lines the simulator sent for that attempt, in order. With overflow expected,
// losing the attempt's oldest whole chunks is allowed and nothing else is: at
// least the newest chunk must be there. A chunk lost on a connection drop
// counts as a violation: the worker publishes log chunks with a core NATS
// publish, which has no acknowledgement and no retry.
//
// Inside a fault window it does not: when the attempt's ledger interval, from
// its lease to its end, overlaps a window that hit the attempt's worker (its
// group is in the window's Groups, or Groups is empty), the server's lines
// need only be an in-order subsequence of the lines sent: none duplicated,
// reordered or corrupted, any missing. A line still missing may only be late,
// in the log consumer's backlog (a restarted server replays the log stream),
// so such a task is re-read like a disagreeing one, for the logs settle time;
// what its last read still misses is counted in [CounterLogsLostInFault],
// which the result carries.
func (c *checker) logs(ctx context.Context, refs []taskRef) Result {
	var ev Evidence
	vs := settleLoop(ctx, c.logSettle, refs, func(rs []taskRef) []verdict {
		x := c.ledger
		x.refresh()
		return parallelMap(len(rs), func(i int) verdict { return c.logVerdict(ctx, rs[i], x) })
	})
	// A task still missing lines inside a fault window once the settle time
	// is up passes, with its loss counted. A settled task lost none.
	var lost int64
	for _, v := range vs {
		if v.msg != "" {
			ev.Add(v.msg)
		}
		lost += int64(v.lost)
	}
	r := ev.Result(NameLogs)
	r.Counters = map[string]int64{CounterLogsLostInFault: lost}
	return r
}

// inFaultWindow reports whether a fault window overlapping the attempt's
// interval, from its lease to its end, hit the attempt's worker.
func (c *checker) inFaultWindow(a *ledgerAttempt) bool {
	if len(c.in.FaultWindows) == 0 || a.end == nil {
		return false
	}
	lo, hi := a.leased, a.end.At
	if lo.IsZero() {
		lo = hi
	}
	worker := cmp.Or(a.workerID, a.end.WorkerID)
	group := ""
	if c.in.GroupOf != nil && worker != "" {
		group = c.in.GroupOf(worker)
	}
	for _, w := range c.in.FaultWindows {
		if w.Start.After(hi) || w.End.Before(lo) {
			continue
		}
		if len(w.Groups) == 0 || (group != "" && slices.Contains(w.Groups, group)) {
			return true
		}
	}
	return false
}

func (c *checker) logVerdict(ctx context.Context, ref taskRef, x *ledgerIndex) verdict {
	chunks, err := c.readLogs(ctx, ref.taskID)
	if err != nil {
		return verdict{msg: fmt.Sprintf("job %s task %s: reading logs: %v", ref.jobID, ref.taskID, err)}
	}
	if len(chunks) > 0 && x.byID[chunks[0].AttemptID] == nil && c.latestOnRealWorker(ctx, ref.taskID) {
		return verdict{}
	}
	where := fmt.Sprintf("job %s task %s", ref.jobID, ref.taskID)
	a, msg := logAttempt(where, ref.taskID, chunks, x)
	if msg != "" {
		return verdict{msg: msg}
	}
	// No ledger attempt and no chunks: nothing was sent. A crashed attempt's
	// lines die with its connection, and an attempt not yet ended has no
	// final count, so neither can be compared.
	if a == nil || a.end == nil || a.end.Kind == simworker.KindCrashed {
		return verdict{}
	}
	jc, ok := c.classOf(ref.jobID)
	if !ok {
		return verdict{}
	}
	where += " attempt " + a.id
	slices.SortStableFunc(chunks, func(p, q client.LogLine) int { return cmp.Compare(p.SeqNum, q.SeqNum) })
	for i := 1; i < len(chunks); i++ {
		if chunks[i].SeqNum == chunks[i-1].SeqNum {
			return verdict{msg: fmt.Sprintf("%s: chunk seq_num %d stored twice", where, chunks[i].SeqNum)}
		}
	}
	lines := chunkLines(chunks)
	want := func(n int) string { return simworker.LogLine(ref.taskID, n, jc.Task.LogLineBytes) }
	if c.inFaultWindow(a) {
		return windowDiff(where, lines, a.end.LogLines, want)
	}
	if c.in.LogOverflowExpected {
		return verdict{msg: overflowDiff(where, chunks, lines, a.end.LogLines, want)}
	}
	return verdict{msg: exactDiff(where, lines, 1, a.end.LogLines, want)}
}

// latestOnRealWorker reports whether the task's latest attempt, whose chunks
// the logs endpoint serves, ran on a worker outside the simulated fleet, so
// the ledger cannot speak for its logs. A canceled task's server row does not
// name its worker, so this reads the attempt list (ordered by attempt number;
// it carries no attempt row ID to match a chunk's against), once, for a task
// whose chunks name an attempt the ledger does not know.
func (c *checker) latestOnRealWorker(ctx context.Context, taskID string) bool {
	if c.in.Simulated == nil {
		return false
	}
	atts, err := c.v.ListAttempts(ctx, taskID)
	if err != nil || len(atts) == 0 {
		return false
	}
	return c.realWorker(atts[len(atts)-1].WorkerID)
}

// logAttempt finds the ledger attempt the server's chunks belong to: the
// chunks' own attempt, or the task's last leased attempt when there are none.
func logAttempt(where, taskID string, chunks []client.LogLine, x *ledgerIndex) (a *ledgerAttempt, violation string) {
	if len(chunks) == 0 {
		atts := x.byTask[taskID]
		if len(atts) == 0 {
			return nil, ""
		}
		return atts[len(atts)-1], ""
	}
	id := chunks[0].AttemptID
	for _, ch := range chunks[1:] {
		if ch.AttemptID != id {
			return nil, fmt.Sprintf("%s: the latest attempt's chunks name attempts %s and %s", where, id, ch.AttemptID)
		}
	}
	a = x.byID[id]
	if a == nil || a.taskID != taskID {
		return nil, fmt.Sprintf("%s: chunks of attempt %s, which the simulator never ran for this task", where, id)
	}
	return a, ""
}

// readLogs returns every chunk of the task's latest attempt.
func (c *checker) readLogs(ctx context.Context, taskID string) ([]client.LogLine, error) {
	var all []client.LogLine
	var after int64
	for {
		items, next, err := c.v.TaskLogs(ctx, taskID, after, maxPage)
		if err != nil {
			return nil, err
		}
		all = append(all, items...)
		if len(items) < maxPage || next <= after {
			return all, nil
		}
		after = next
	}
}

// chunkLines splits chunks, in order, into lines.
func chunkLines(chunks []client.LogLine) []string {
	var lines []string
	for _, ch := range chunks {
		lines = append(lines, strings.Split(ch.Data, "\n")...)
	}
	return lines
}

// exactDiff compares got with lines first..last of want.
func exactDiff(where string, got []string, first, last int, want func(int) string) string {
	n := last - first + 1
	for i := range max(len(got), n) {
		switch {
		case i >= len(got):
			return fmt.Sprintf("%s: server has %d lines, simulator sent %d: line %d missing", where, len(got), n, first+i)
		case i >= n:
			return fmt.Sprintf("%s: server has %d lines, simulator sent %d: extra line %q", where, len(got), n, clip(got[i]))
		case got[i] != want(first+i):
			return fmt.Sprintf("%s: line %d is %q, want %q", where, first+i, clip(got[i]), clip(want(first+i)))
		}
	}
	return ""
}

// windowDiff compares a task's lines inside a fault window (see
// subsequenceDiff), counts the ones missing, and keeps the task pending while
// any are, so it is read again.
func windowDiff(where string, got []string, n int, want func(int) string) verdict {
	lost, msg := subsequenceDiff(where+" (inside a fault window)", got, n, want)
	return verdict{msg: msg, pending: lost > 0, lost: lost}
}

// subsequenceDiff checks that got is lines 1..n of want in order with any
// missing, and returns how many are, or why it is not: a line duplicated,
// out of order, or matching no line sent.
func subsequenceDiff(where string, got []string, n int, want func(int) string) (lost int, violation string) {
	next := 1
	for i, line := range got {
		for next <= n && want(next) != line {
			next++
		}
		if next > n {
			return 0, fmt.Sprintf("%s: server line %d of %d, %q, is not one of the %d lines sent after the lines before it: a duplicated, reordered or corrupted line",
				where, i+1, len(got), clip(line), n)
		}
		next++
	}
	return n - len(got), ""
}

// overflowDiff allows the oldest whole chunks to be missing: the chunks the
// server kept are not none (unless nothing was sent), run on from one another,
// and their lines are the last ones the simulator sent, so the newest chunk is
// among them.
func overflowDiff(where string, chunks []client.LogLine, got []string, n int, want func(int) string) string {
	if len(chunks) == 0 && n > 0 {
		return fmt.Sprintf("%s: the server kept no chunk of %d lines sent; overflow may only lose the oldest chunks", where, n)
	}
	for i := 1; i < len(chunks); i++ {
		if chunks[i].SeqNum != chunks[i-1].SeqNum+1 {
			return fmt.Sprintf("%s: chunks %d to %d are missing, after chunk %d the server kept", where, chunks[i-1].SeqNum+1, chunks[i].SeqNum-1, chunks[i-1].SeqNum)
		}
	}
	if len(got) > n {
		return fmt.Sprintf("%s: server has %d lines, simulator sent %d", where, len(got), n)
	}
	return exactDiff(where+" (oldest chunks lost to overflow)", got, n-len(got)+1, n, want)
}

// ── workers-converge ─────────────────────────────────────────────────────────

// workersConverge: within ConvergeWithin, the farm's online worker count is
// LiveWorkers. The API reports a disabled worker as disabled whether or not it
// is connected, so with disabled workers the online count may fall short of
// LiveWorkers by at most their number.
func (c *checker) workersConverge(ctx context.Context) Result {
	deadline := time.Now().Add(c.in.ConvergeWithin)
	poll := pollInterval(c.in.ConvergeWithin)
	for {
		online, disabled, err := c.countWorkers(ctx)
		if err == nil && online <= c.in.LiveWorkers && c.in.LiveWorkers <= online+disabled {
			return Result{Name: NameWorkersConverge, OK: true}
		}
		if !time.Now().Before(deadline) || ctx.Err() != nil {
			var ev Evidence
			if err != nil {
				ev.AddUnmeasured(fmt.Sprintf("farm %s: listing workers: %v", c.in.FarmID, err))
			} else {
				ev.Add(fmt.Sprintf("farm %s: %d workers online and %d disabled, want %d online (the live fleet) after %s",
					c.in.FarmID, online, disabled, c.in.LiveWorkers, c.in.ConvergeWithin))
			}
			return ev.Result(NameWorkersConverge)
		}
		sleep.For(ctx, min(poll, time.Until(deadline)))
	}
}

func (c *checker) countWorkers(ctx context.Context) (online, disabled int, err error) {
	err = client.EachPage(maxPage, func(limit, offset int) (client.Page[client.Worker], error) {
		return c.v.ListWorkers(ctx, c.in.FarmID, limit, offset)
	}, func(p client.Page[client.Worker]) {
		for _, w := range p.Items {
			switch w.Status {
			case workerOnline:
				online++
			case workerOff:
				disabled++
			}
		}
	})
	if err != nil {
		return 0, 0, err
	}
	return online, disabled, nil
}

// ── caps ─────────────────────────────────────────────────────────────────────

// caps: the most attempts the simulator held at once on a queue, or on a
// usage pool (each task claims one unit), is within the queue's
// max_concurrent_tasks (0 is unlimited) or the pool's limit. Attempts real
// sqi-workers ran are not in the ledger and are not counted, so with real
// workers in the farm caps can miss a violation but never invent one.
//
// An attempt is held from its leased event to its first end event (or to the
// end of the ledger when it has none), on the worker's clock. That window sits
// inside the server's: the server commits the lease before the worker records
// leased, and the simulator's dispatcher stamps an attempt's end event with a
// time taken before it publishes the terminal status, so before the server can
// free the slot. A peak above a server-enforced cap is therefore a violation.
//
// Canceled attempts are the exception. The server frees a canceled task's slot
// and claims when it commits the cancel, while the simulator ends the attempt
// only once the cancel message has reached it and its logs are flushed, so
// another lease can land in between. An attempt the simulator ended as
// canceled, of a job a cancel action targeted, is therefore held no later than
// that action's start (CanceledAt).
func (c *checker) caps() Result {
	var ev Evidence
	if c.in.Ledger == nil || c.in.Scenario == nil {
		return ev.Result(NameCaps)
	}
	queues, pools := c.peaks()
	for _, q := range c.in.Scenario.Farm.Queues {
		if q.MaxConcurrentTasks > 0 && queues[q.Name] > q.MaxConcurrentTasks {
			ev.Add(fmt.Sprintf("queue %s: %d tasks in flight at once, max_concurrent_tasks %d", q.Name, queues[q.Name], q.MaxConcurrentTasks))
		}
	}
	for _, p := range c.in.Scenario.Farm.UsagePools {
		if p.Limit > 0 && pools[p.Name] > p.Limit {
			name := p.Name
			if server := c.in.PoolNames[p.Name]; server != "" && server != p.Name {
				name += " (" + server + " on the server)"
			}
			ev.Add(fmt.Sprintf("usage pool %s: %d tasks in flight at once, limit %d", name, pools[p.Name], p.Limit))
		}
	}
	return ev.Result(NameCaps)
}

// edge is an attempt starting (+1) or ending (-1) on a queue and usage pool.
type edge struct {
	at          time.Time
	queue, pool string
	delta       int
}

// peaks returns the most attempts in flight at once per queue and per usage
// pool. An attempt that ends at the instant another starts does not overlap it.
func (c *checker) peaks() (queues, pools map[string]int) {
	x := c.ledger
	x.refresh()
	var edges []edge
	for _, a := range x.byID {
		jc, ok := c.classOf(a.jobID)
		if !ok || a.leased.IsZero() {
			continue
		}
		edges = append(edges, edge{at: a.leased, queue: jc.Queue, pool: jc.UsagePool, delta: 1})
		if a.end != nil {
			edges = append(edges, edge{at: c.heldUntil(*a.end, a.leased), queue: jc.Queue, pool: jc.UsagePool, delta: -1})
		}
	}
	// Ends sort before starts at the same instant.
	slices.SortFunc(edges, func(a, b edge) int {
		return cmp.Or(a.at.Compare(b.at), cmp.Compare(a.delta, b.delta))
	})
	queues, pools = map[string]int{}, map[string]int{}
	curQ, curP := map[string]int{}, map[string]int{}
	for _, e := range edges {
		curQ[e.queue] += e.delta
		queues[e.queue] = max(queues[e.queue], curQ[e.queue])
		if e.pool != "" {
			curP[e.pool] += e.delta
			pools[e.pool] = max(pools[e.pool], curP[e.pool])
		}
	}
	return queues, pools
}

// heldUntil is when the attempt ended by e, leased at start, stopped holding
// its slot: e's time, or for an attempt the simulator ended as canceled the
// start of the cancel action that targeted its job when that is earlier (but
// never before start).
func (c *checker) heldUntil(e simworker.Event, start time.Time) time.Time {
	if e.Kind != simworker.KindCanceled {
		return e.At
	}
	at, ok := c.in.CanceledAt[e.JobID]
	if !ok || !at.Before(e.At) {
		return e.At
	}
	if at.Before(start) {
		return start
	}
	return at
}

// ── submission-atomic ────────────────────────────────────────────────────────

// submissionAtomic: every job of the farm is an accepted job or an orphan, and
// every orphan has all of its class's tasks (steps x tasks_per_step). A
// submission is one transaction on the server, so one the server answered
// with no 2xx (a timeout, a dropped connection, a 5xx) must have created its
// whole job or nothing. A farm job matching neither, such as a second job
// created for one submission, is one the run cannot account for.
func (c *checker) submissionAtomic(ctx context.Context) Result {
	var ev Evidence
	err := listFarmJobs(ctx, c.v, c.in.FarmID, func(j client.Job) {
		if _, ok := c.jobs[j.ID]; ok {
			return
		}
		if _, ok := c.orphans[j.ID]; ok {
			return
		}
		ev.Add(fmt.Sprintf("farm %s holds job %s (%q), which matches no accepted submission and no orphan of a rejected one", c.in.FarmID, j.ID, j.Name))
	})
	if err != nil {
		ev.Add(err.Error())
	}
	ev.AddAll(parallelMap(len(c.orphanIDs), func(i int) string { return c.orphanMsg(ctx, c.orphans[c.orphanIDs[i]]) }))
	return ev.Result(NameSubmissionAtomic)
}

// orphanMsg returns why an orphan is not a whole job, or "".
func (c *checker) orphanMsg(ctx context.Context, o driver.SubmittedJob) string {
	jc, ok := c.classes[o.Class]
	if !ok || jc.TasksPerStep <= 0 {
		return ""
	}
	steps := max(jc.Steps, 1)
	want := steps * jc.TasksPerStep
	tasks, total, err := c.listTasks(ctx, o.ID)
	switch {
	case err != nil:
		return fmt.Sprintf("orphan job %s (submission %s): listing tasks: %v", o.ID, o.Name, err)
	case total != want || len(tasks) != want:
		return fmt.Sprintf("orphan job %s (submission %s, HTTP status %d) has %d of %d tasks (%d listed; %d steps x %d tasks): a submission with no 2xx answer created part of its job",
			o.ID, o.Name, o.Status, total, want, len(tasks), steps, jc.TasksPerStep)
	}
	return ""
}
