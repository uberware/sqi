// SPDX-License-Identifier: AGPL-3.0-or-later

package verify

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/uberware/sqi/internal/loadtest/client"
	"github.com/uberware/sqi/internal/loadtest/scenario"
	"github.com/uberware/sqi/internal/loadtest/simworker"
)

// ── The ledger, per task ─────────────────────────────────────────────────────

// ledgerAttempt is one attempt as the simulator saw it.
type ledgerAttempt struct {
	id, taskID, jobID string
	// end is the attempt's first terminal, canceled or crashed event; nil
	// while the simulator has not ended it.
	end *simworker.Event
}

// ledgerIndex is the ledger by task and by attempt.
type ledgerIndex struct {
	// byTask lists each task's attempts in the order they were leased. The
	// server leases a task's next attempt only after closing the previous
	// one, so the last is the attempt the server holds as the task's latest.
	byTask map[string][]*ledgerAttempt
	byID   map[string]*ledgerAttempt
	// byJob lists each job's leased tasks, in the order first leased.
	byJob map[string][]string
}

func indexLedger(l *simworker.Ledger) *ledgerIndex {
	x := &ledgerIndex{byTask: map[string][]*ledgerAttempt{}, byID: map[string]*ledgerAttempt{}, byJob: map[string][]string{}}
	if l == nil {
		return x
	}
	for _, e := range l.Events() {
		a := x.byID[e.AttemptID]
		if a == nil {
			a = &ledgerAttempt{id: e.AttemptID, taskID: e.TaskID, jobID: e.JobID}
			x.byID[e.AttemptID] = a
			if len(x.byTask[e.TaskID]) == 0 {
				x.byJob[e.JobID] = append(x.byJob[e.JobID], e.TaskID)
			}
			x.byTask[e.TaskID] = append(x.byTask[e.TaskID], a)
		}
		switch e.Kind {
		case simworker.KindTerminal, simworker.KindCanceled, simworker.KindCrashed:
			if a.end == nil {
				ev := e
				a.end = &ev
			}
		}
	}
	return x
}

// decision is what the simulator decided for a task: the outcome of its
// last attempt.
type decision struct {
	status  string // the task status it implies, when decided
	decided bool
	leased  int    // how many attempts the simulator leased
	why     string // for evidence
}

// decide returns the task's decision. Only a succeeded or failed terminal
// event (a timeout is a failed one) or a canceled event decides; a task never
// leased, or whose last attempt crashed, ended in a worker shutdown or has
// not ended, is undecided.
func (x *ledgerIndex) decide(taskID string) decision {
	atts := x.byTask[taskID]
	if len(atts) == 0 {
		return decision{why: "never leased"}
	}
	a := atts[len(atts)-1]
	d := decision{leased: len(atts)}
	prefix := fmt.Sprintf("last of %d attempts, %s, ", len(atts), a.id)
	switch {
	case a.end == nil:
		d.why = prefix + "not ended"
	case a.end.Kind == simworker.KindCanceled:
		d.status, d.decided, d.why = statusCancel, true, prefix+"canceled"
	case a.end.Kind == simworker.KindCrashed:
		d.why = prefix + "crashed"
	case a.end.Outcome == simworker.OutcomeSucceeded, a.end.Outcome == simworker.OutcomeFailed:
		d.status, d.decided, d.why = a.end.Outcome, true, prefix+a.end.Outcome
	default:
		d.why = prefix + "ended " + a.end.Outcome
	}
	return d
}

// ── terminal ─────────────────────────────────────────────────────────────────

// terminal: every accepted job is completed, failed or canceled.
func (c *checker) terminal(statuses map[string]string, errs map[string]error) Result {
	var ev evidence
	for _, id := range c.order {
		if err := errs[id]; err != nil {
			ev.add(fmt.Sprintf("job %s: reading its status: %v", id, err))
			continue
		}
		if s := statuses[id]; !terminalJob[s] {
			ev.add(fmt.Sprintf("job %s is %s, want completed, failed or canceled", id, s))
		}
	}
	return ev.result(NameTerminal)
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
	x := indexLedger(c.in.Ledger)
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
		ref := taskRef{jobID: jobID, taskID: t.ID, status: t.Status}
		if jobStatus == jobCompleted && t.Status != statusOK {
			s.evidence = append(s.evidence, fmt.Sprintf("job %s is completed but task %s is %s, want succeeded", jobID, t.ID, t.Status))
		}
		if c.outcomeMsg(ref, t.Status, x) != "" {
			s.suspects = append(s.suspects, suspect{taskRef: ref, server: t.Status})
		}
		if sampled(c.in.Seed, saltAttempts, t.ID, c.in.AttemptSample) {
			s.attempts = append(s.attempts, ref)
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

// listTasks returns all of a job's tasks and the total the server reports.
func (c *checker) listTasks(ctx context.Context, jobID string) (tasks []client.Task, total int, err error) {
	for {
		var p client.Page[client.Task]
		err = withRetry(ctx, func() error {
			var err error
			p, err = c.v.ListTasks(ctx, jobID, maxPage, len(tasks))
			return err
		})
		if err != nil {
			return nil, 0, err
		}
		tasks = append(tasks, p.Items...)
		total = p.Total
		if len(p.Items) == 0 || len(tasks) >= p.Total {
			return tasks, total, nil
		}
	}
}

// ── outcomes ─────────────────────────────────────────────────────────────────

// outcomes: every task's server status is the one the simulator decided, and
// a completed job has only succeeded tasks.
func (c *checker) outcomes(ctx context.Context, s sweep) Result {
	var ev evidence
	ev.addAll(s.evidence)
	ev.addAll(settleLoop(ctx, c.settle, s.suspects, func(sus []suspect) []string {
		x := indexLedger(c.in.Ledger)
		msgs := make([]string, len(sus))
		for i, t := range sus {
			msgs[i] = c.outcomeMsg(t.taskRef, t.server, x)
		}
		return msgs
	}))
	return ev.result(NameOutcomes)
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
	return fmt.Sprintf("job %s task %s: server %s, want %s (simulator: %s)", ref.jobID, ref.taskID, server, want, d.why)
}

// ── single-attempt ───────────────────────────────────────────────────────────

// singleAttempt: on each sampled task, no two of the server's attempts are
// held at once, every attempt after the first follows a failed or canceled
// one (a retry or a reclaim), and a task in a terminal status holds no open
// attempt.
func (c *checker) singleAttempt(ctx context.Context, refs []taskRef) Result {
	msgs := parallelMap(len(refs), func(i int) string {
		ref := refs[i]
		var atts []client.Attempt
		err := withRetry(ctx, func() error {
			var err error
			atts, err = c.v.ListAttempts(ctx, ref.taskID)
			return err
		})
		if err != nil {
			return fmt.Sprintf("job %s task %s: listing attempts: %v", ref.jobID, ref.taskID, err)
		}
		return attemptsMsg(ref, atts)
	})
	var ev evidence
	ev.addAll(msgs)
	return ev.result(NameSingleAttempt)
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
// (log chunks go over core NATS, at most once) is a finding, not excused.
func (c *checker) logs(ctx context.Context, refs []taskRef) Result {
	var ev evidence
	ev.addAll(settleLoop(ctx, c.logSettle, refs, func(rs []taskRef) []string {
		x := indexLedger(c.in.Ledger)
		return parallelMap(len(rs), func(i int) string { return c.logMsg(ctx, rs[i], x) })
	}))
	return ev.result(NameLogs)
}

func (c *checker) logMsg(ctx context.Context, ref taskRef, x *ledgerIndex) string {
	chunks, err := c.readLogs(ctx, ref.taskID)
	if err != nil {
		return fmt.Sprintf("job %s task %s: reading logs: %v", ref.jobID, ref.taskID, err)
	}
	where := fmt.Sprintf("job %s task %s", ref.jobID, ref.taskID)
	a, msg := logAttempt(where, ref.taskID, chunks, x)
	if msg != "" {
		return msg
	}
	// No ledger attempt and no chunks: nothing was sent. A crashed attempt's
	// lines die with its connection, and an attempt not yet ended has no
	// final count, so neither can be compared.
	if a == nil || a.end == nil || a.end.Kind == simworker.KindCrashed {
		return ""
	}
	jc, ok := c.classOf(ref.jobID)
	if !ok {
		return ""
	}
	where += " attempt " + a.id
	slices.SortStableFunc(chunks, func(p, q client.LogLine) int { return cmp.Compare(p.SeqNum, q.SeqNum) })
	for i := 1; i < len(chunks); i++ {
		if chunks[i].SeqNum == chunks[i-1].SeqNum {
			return fmt.Sprintf("%s: chunk seq_num %d stored twice", where, chunks[i].SeqNum)
		}
	}
	lines := chunkLines(chunks)
	want := func(n int) string { return simworker.LogLine(ref.taskID, n, jc.Task.LogLineBytes) }
	if c.in.LogOverflowExpected {
		return overflowDiff(where, chunks, lines, a.end.LogLines, want)
	}
	return exactDiff(where, lines, 1, a.end.LogLines, want)
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
		var items []client.LogLine
		var next int64
		err := withRetry(ctx, func() error {
			var err error
			items, next, err = c.v.TaskLogs(ctx, taskID, after, maxPage)
			return err
		})
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
			var ev evidence
			if err != nil {
				ev.add(fmt.Sprintf("farm %s: listing workers: %v", c.in.FarmID, err))
			} else {
				ev.add(fmt.Sprintf("farm %s: %d workers online and %d disabled, want %d online (the live fleet) after %s",
					c.in.FarmID, online, disabled, c.in.LiveWorkers, c.in.ConvergeWithin))
			}
			return ev.result(NameWorkersConverge)
		}
		sleepCtx(ctx, min(poll, time.Until(deadline)))
	}
}

func (c *checker) countWorkers(ctx context.Context) (online, disabled int, err error) {
	for offset := 0; ; {
		var p client.Page[client.Worker]
		err := withRetry(ctx, func() error {
			var err error
			p, err = c.v.ListWorkers(ctx, c.in.FarmID, maxPage, offset)
			return err
		})
		if err != nil {
			return 0, 0, err
		}
		for _, w := range p.Items {
			switch w.Status {
			case workerOnline:
				online++
			case workerOff:
				disabled++
			}
		}
		offset += len(p.Items)
		if len(p.Items) == 0 || offset >= p.Total {
			return online, disabled, nil
		}
	}
}

// ── caps ─────────────────────────────────────────────────────────────────────

// caps: the most attempts the simulator held at once on a queue, or on a
// usage pool (each task claims one unit), is within the queue's
// max_concurrent_tasks (0 is unlimited) or the pool's limit.
//
// An attempt is held from its leased event to its end event, as in
// [simworker.Ledger.ConcurrentPeak], with one difference. The server frees a
// canceled task's slot and claims when it commits the cancel, while the
// simulator ends the attempt only once the cancel message has reached it and
// its logs are flushed, so another lease can land in between. An attempt the
// simulator ended as canceled, of a job a cancel action targeted, is therefore
// held no later than that action's start (CanceledAt). Everything else keeps
// the simulator's window, which sits inside the server's.
func (c *checker) caps() Result {
	var ev evidence
	if c.in.Ledger == nil || c.in.Scenario == nil {
		return ev.result(NameCaps)
	}
	queues, pools := c.peaks()
	for _, q := range c.in.Scenario.Farm.Queues {
		if q.MaxConcurrentTasks > 0 && queues[q.Name] > q.MaxConcurrentTasks {
			ev.add(fmt.Sprintf("queue %s: %d tasks in flight at once, max_concurrent_tasks %d", q.Name, queues[q.Name], q.MaxConcurrentTasks))
		}
	}
	for _, p := range c.in.Scenario.Farm.UsagePools {
		if p.Limit > 0 && pools[p.Name] > p.Limit {
			name := p.Name
			if server := c.in.PoolNames[p.Name]; server != "" && server != p.Name {
				name += " (" + server + " on the server)"
			}
			ev.add(fmt.Sprintf("usage pool %s: %d tasks in flight at once, limit %d", name, pools[p.Name], p.Limit))
		}
	}
	return ev.result(NameCaps)
}

// edge is an attempt starting (+1) or ending (-1) on a job class.
type edge struct {
	at    time.Time
	jc    scenario.JobClass
	delta int
}

// peaks returns the most attempts in flight at once per queue and per usage
// pool. An attempt that ends at the instant another starts does not overlap it.
func (c *checker) peaks() (queues, pools map[string]int) {
	type start struct {
		at time.Time
		jc scenario.JobClass
	}
	starts := map[string]start{}
	ended := map[string]bool{}
	var edges []edge
	for _, e := range c.in.Ledger.Events() {
		switch e.Kind {
		case simworker.KindLeased:
			jc, ok := c.classOf(e.JobID)
			if _, dup := starts[e.AttemptID]; ok && !dup {
				starts[e.AttemptID] = start{at: e.At, jc: jc}
				edges = append(edges, edge{at: e.At, jc: jc, delta: 1})
			}
		case simworker.KindTerminal, simworker.KindCanceled, simworker.KindCrashed:
			s, ok := starts[e.AttemptID]
			if ok && !ended[e.AttemptID] {
				ended[e.AttemptID] = true
				edges = append(edges, edge{at: c.heldUntil(e, s.at), jc: s.jc, delta: -1})
			}
		}
	}
	// Ends sort before starts at the same instant.
	slices.SortStableFunc(edges, func(a, b edge) int {
		return cmp.Or(a.at.Compare(b.at), cmp.Compare(a.delta, b.delta))
	})
	queues, pools = map[string]int{}, map[string]int{}
	curQ, curP := map[string]int{}, map[string]int{}
	for _, e := range edges {
		curQ[e.jc.Queue] += e.delta
		queues[e.jc.Queue] = max(queues[e.jc.Queue], curQ[e.jc.Queue])
		if e.jc.UsagePool != "" {
			curP[e.jc.UsagePool] += e.delta
			pools[e.jc.UsagePool] = max(pools[e.jc.UsagePool], curP[e.jc.UsagePool])
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
