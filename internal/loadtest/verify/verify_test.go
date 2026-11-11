// SPDX-License-Identifier: AGPL-3.0-or-later

package verify_test

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/loadtest/client"
	"github.com/uberware/sqi/internal/loadtest/driver"
	"github.com/uberware/sqi/internal/loadtest/scenario"
	"github.com/uberware/sqi/internal/loadtest/simworker"
	"github.com/uberware/sqi/internal/loadtest/verify"
)

// invariantNames are the six checks, in the order Check returns them.
var invariantNames = []string{"terminal", "outcomes", "single-attempt", "logs", "workers-converge", "caps"}

// lineBytes is the log line size of the "shot" class.
const lineBytes = 40

// fakeView is an in-memory server.
type fakeView struct {
	mu          sync.Mutex
	jobs        []client.Job
	tasks       map[string][]client.Task
	attempts    map[string][]client.Attempt
	logs        map[string][]client.LogLine
	workers     func(poll int) []client.Worker
	polls       int
	failAttempt map[string]bool
	failTasksAt int                 // a ListTasks at this offset fails; 0 means none
	calls       map[string][]string // method -> arguments, in call order
}

func newFakeView() *fakeView {
	return &fakeView{
		tasks:       map[string][]client.Task{},
		attempts:    map[string][]client.Attempt{},
		logs:        map[string][]client.LogLine{},
		failAttempt: map[string]bool{},
		calls:       map[string][]string{},
	}
}

func (f *fakeView) called(method, arg string) {
	f.calls[method] = append(f.calls[method], arg)
}

func (f *fakeView) callsTo(method string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls[method])
}

func page[T any](all []T, limit, offset int) client.Page[T] {
	if limit <= 0 {
		limit = 50
	}
	lo := min(offset, len(all))
	hi := min(lo+limit, len(all))
	return client.Page[T]{Items: slices.Clone(all[lo:hi]), Total: len(all), Limit: limit, Offset: offset}
}

func (f *fakeView) GetJob(_ context.Context, id string) (client.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.called("GetJob", id)
	for _, j := range f.jobs {
		if j.ID == id {
			return j, nil
		}
	}
	return client.Job{}, &client.APIError{Status: 404, Method: "GET", Path: "/jobs/" + id, Detail: "job not found"}
}

func (f *fakeView) ListJobs(_ context.Context, farmID string, limit, offset int) (client.Page[client.Job], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.called("ListJobs", farmID)
	return page(f.jobs, limit, offset), nil
}

func (f *fakeView) ListTasks(_ context.Context, jobID string, limit, offset int) (client.Page[client.Task], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.called("ListTasks", jobID)
	if offset > 0 && offset == f.failTasksAt {
		return client.Page[client.Task]{}, &client.APIError{Status: 500, Method: "GET", Path: "/jobs/" + jobID + "/tasks", Detail: "boom"}
	}
	return page(f.tasks[jobID], limit, offset), nil
}

func (f *fakeView) ListAttempts(_ context.Context, taskID string) ([]client.Attempt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.called("ListAttempts", taskID)
	if f.failAttempt[taskID] {
		return nil, &client.APIError{Status: 500, Method: "GET", Path: "/tasks/" + taskID + "/attempts", Detail: "boom"}
	}
	return slices.Clone(f.attempts[taskID]), nil
}

func (f *fakeView) TaskLogs(_ context.Context, taskID string, after int64, limit int) ([]client.LogLine, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.called("TaskLogs", taskID)
	all := slices.Clone(f.logs[taskID])
	slices.SortFunc(all, func(a, b client.LogLine) int { return int(a.NatsSeq - b.NatsSeq) })
	var out []client.LogLine
	for _, l := range all {
		if l.NatsSeq > after && len(out) < limit {
			out = append(out, l)
		}
	}
	cursor := after
	if len(out) > 0 {
		cursor = out[len(out)-1].NatsSeq
	}
	return out, cursor, nil
}

func (f *fakeView) ListWorkers(_ context.Context, farmID string, limit, offset int) (client.Page[client.Worker], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.called("ListWorkers", farmID)
	if offset == 0 {
		f.polls++
	}
	var ws []client.Worker
	if f.workers != nil {
		ws = f.workers(f.polls)
	}
	return page(ws, limit, offset), nil
}

func (f *fakeView) setJob(id, status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.jobs {
		if f.jobs[i].ID == id {
			f.jobs[i].Status = status
		}
	}
}

func (f *fakeView) setTask(jobID, taskID, status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.tasks[jobID] {
		if f.tasks[jobID][i].ID == taskID {
			f.tasks[jobID][i].Status = status
		}
	}
}

// workersOf returns a worker list with the given numbers of online and
// disabled workers.
func workersOf(online, disabled int) []client.Worker {
	var ws []client.Worker
	for i := range online + disabled {
		status := "online"
		if i >= online {
			status = "disabled"
		}
		ws = append(ws, client.Worker{ID: "w" + strconv.Itoa(i), FarmID: "farm", Status: status})
	}
	return ws
}

// att is one attempt of a synthetic task: what the simulator did with it and
// what the server recorded.
type att struct {
	id       string
	from, to int    // seconds after t0; to < 0 is still open
	end      string // the simulator's end: succeeded, failed, canceled, crashed, or "" for none yet
	server   string // the server's attempt status
	lines    int
}

// world is a synthetic run: a scenario, a ledger and a fake server that agree.
type world struct {
	t0      time.Time
	view    *fakeView
	ledger  *simworker.Ledger
	in      verify.Input
	natsSeq int64
}

func at(t0 time.Time, s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }

func newWorld() *world {
	w := &world{
		t0:     time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC),
		view:   newFakeView(),
		ledger: simworker.NewLedger(),
	}
	w.view.workers = func(int) []client.Worker { return workersOf(3, 0) }
	w.in = verify.Input{
		Ledger: w.ledger,
		Scenario: &scenario.Scenario{
			Farm: scenario.FarmSpec{
				Queues:     []scenario.QueueSpec{{Name: "render"}},
				UsagePools: []scenario.PoolSpec{{Name: "arnold", Limit: 6}},
			},
			Jobs: []scenario.JobClass{{
				Name: "shot", Queue: "render", UsagePool: "arnold",
				Task: scenario.TaskSpec{LogLineBytes: lineBytes},
			}},
		},
		FarmID:         "farm",
		LiveWorkers:    3,
		LogSample:      1,
		AttemptSample:  1,
		Seed:           1,
		ConvergeWithin: 50 * time.Millisecond,
		Settle:         time.Millisecond,
		LogSettle:      time.Millisecond,
	}
	// A rejected submission is never checked.
	w.in.Jobs = append(w.in.Jobs, driver.SubmittedJob{Class: "shot", Status: 422})
	return w
}

func (w *world) job(id, status string, canceledByRun bool) {
	w.view.jobs = append(w.view.jobs, client.Job{ID: id, Status: status})
	w.in.Jobs = append(w.in.Jobs, driver.SubmittedJob{ID: id, Name: "shot-" + id, Class: "shot", Status: 201})
	if canceledByRun {
		if w.in.CanceledAt == nil {
			w.in.CanceledAt = map[string]time.Time{}
		}
		// Far enough ahead never to shorten a canceled attempt's window.
		w.in.CanceledAt[id] = time.Now().Add(24 * time.Hour)
	}
}

var endKinds = map[string]string{
	"succeeded": simworker.KindTerminal,
	"failed":    simworker.KindTerminal,
	"canceled":  simworker.KindCanceled,
	"crashed":   simworker.KindCrashed,
}

func (w *world) task(jobID, taskID, status string, atts ...att) {
	w.view.tasks[jobID] = append(w.view.tasks[jobID], client.Task{ID: taskID, Status: status})
	for i, a := range atts {
		w.ledger.Record(simworker.Event{At: at(w.t0, a.from), WorkerID: "w0", TaskID: taskID, AttemptID: a.id, JobID: jobID, Kind: simworker.KindLeased})
		if a.end != "" {
			e := simworker.Event{At: at(w.t0, a.to), WorkerID: "w0", TaskID: taskID, AttemptID: a.id, JobID: jobID, Kind: endKinds[a.end], LogLines: a.lines}
			if e.Kind == simworker.KindTerminal {
				e.Outcome = a.end
			}
			w.ledger.Record(e)
		}
		started := at(w.t0, a.from)
		sa := client.Attempt{ID: strconv.Itoa(i + 1), AttemptNumber: i + 1, WorkerID: "w0", Status: a.server, StartedAt: &started}
		if a.to >= 0 && a.server != "running" {
			ended := at(w.t0, a.to)
			sa.EndedAt = &ended
		}
		w.view.attempts[taskID] = append(w.view.attempts[taskID], sa)
	}
	if len(atts) == 0 {
		return
	}
	last := atts[len(atts)-1]
	if last.end == "crashed" {
		return // a crashed worker's lines never reached the server
	}
	w.view.logs[taskID] = w.chunks(taskID, last.id, 1, last.lines)
}

// chunks returns the server's chunks for lines from..to of a task, 50 lines a
// chunk.
func (w *world) chunks(taskID, attemptID string, from, to int) []client.LogLine {
	var out []client.LogLine
	seq := int64(0)
	for start := from; start <= to; start += 50 {
		var lines []string
		for n := start; n <= min(start+49, to); n++ {
			lines = append(lines, simworker.LogLine(taskID, n, lineBytes))
		}
		seq++
		w.natsSeq++
		out = append(out, client.LogLine{
			ID: fmt.Sprintf("%s-%d", taskID, seq), AttemptID: attemptID, SeqNum: seq, NatsSeq: w.natsSeq,
			Stream: "stdout", Data: strings.Join(lines, "\n"),
		})
	}
	return out
}

// healthyWorld is a run every invariant passes. At t0+7..8 six attempts of
// the "shot" class (pool arnold, queue render) are in flight at once.
func healthyWorld() *world {
	w := newWorld()
	w.job("j1", "completed", false)
	w.task("j1", "t1", "succeeded", att{"a1", 0, 10, "succeeded", "succeeded", 120})
	// Retried: failed, then succeeded.
	w.task("j1", "t2", "succeeded",
		att{"a2a", 0, 5, "failed", "failed", 10},
		att{"a2b", 6, 12, "succeeded", "succeeded", 30})
	// Crashed, reclaimed by the server, then succeeded on another lease.
	w.task("j1", "t3", "succeeded",
		att{"a3a", 0, 4, "crashed", "failed", 5},
		att{"a3b", 7, 14, "succeeded", "succeeded", 20})
	w.task("j1", "t8", "succeeded", att{"a8", 3, 11, "succeeded", "succeeded", 0})

	// Canceled by an action.
	w.job("j2", "canceled", true)
	w.task("j2", "t4", "canceled") // never leased
	// The simulator finished it as the cancel landed; the server kept the cancel.
	w.task("j2", "t5", "canceled", att{"a5", 1, 9, "succeeded", "canceled", 8})
	w.task("j2", "t6", "canceled", att{"a6", 2, 8, "canceled", "canceled", 7})
	return w
}

func TestCheck(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(w *world)
		// fail maps each invariant expected to fail to a string its evidence
		// must contain. Every other invariant must pass.
		fail map[string]string
	}{
		{name: "healthy run with a retry, a reclaim and a canceled job"},

		// terminal
		{
			name:   "terminal: a job left running",
			mutate: func(w *world) { w.view.setJob("j1", "running") },
			fail:   map[string]string{"terminal": "j1"},
		},
		{
			name:   "terminal: a job paused at its failure limit",
			mutate: func(w *world) { w.view.setJob("j1", "paused") },
			fail:   map[string]string{"terminal": "paused"},
		},

		// outcomes
		{
			name:   "outcomes: the simulator succeeded a task the server failed",
			mutate: func(w *world) { w.view.setTask("j1", "t1", "failed") },
			fail:   map[string]string{"outcomes": "t1"},
		},
		{
			name: "outcomes: a completed job with a canceled task",
			mutate: func(w *world) {
				w.task("j1", "t7", "canceled")
			},
			fail: map[string]string{"outcomes": "completed"},
		},
		{
			name: "outcomes: the server succeeded a task the simulator never ran",
			mutate: func(w *world) {
				w.task("j1", "t9", "succeeded")
			},
			fail: map[string]string{"outcomes": "t9"},
		},
		{
			name: "outcomes: a canceled job the run did not cancel does not excuse a canceled task",
			mutate: func(w *world) {
				w.view.setJob("j1", "canceled")
				w.view.setTask("j1", "t1", "canceled")
			},
			fail: map[string]string{"outcomes": "t1"},
		},
		{
			name: "outcomes: every attempt failed and the server failed the task",
			mutate: func(w *world) {
				w.view.setJob("j1", "failed")
				w.task("j1", "t10", "failed",
					att{"a10a", 0, 1, "failed", "failed", 1},
					att{"a10b", 2, 3, "failed", "failed", 1})
			},
		},
		{
			name: "outcomes: a task whose last attempt crashed cannot be decided",
			mutate: func(w *world) {
				w.view.setJob("j1", "failed")
				w.task("j1", "t11", "failed", att{"a11", 0, 1, "crashed", "failed", 0})
			},
			fail: map[string]string{"outcomes": "t11"},
		},
		{
			name: "outcomes: a leased task whose last attempt crashed, canceled in a job the run did not cancel",
			mutate: func(w *world) {
				w.view.setJob("j1", "failed")
				w.task("j1", "t14", "canceled", att{"a14", 0, 1, "crashed", "failed", 0})
			},
			fail: map[string]string{"outcomes": "t14"},
		},
		{
			name: "outcomes: a crashed task of a canceled job is canceled",
			mutate: func(w *world) {
				w.task("j2", "t12", "canceled", att{"a12", 0, 1, "crashed", "failed", 0})
			},
		},

		// single-attempt
		{
			name: "single-attempt: two overlapping attempts, neither a retry",
			mutate: func(w *world) {
				w.view.attempts["t1"] = []client.Attempt{
					attempt(w.t0, 1, "succeeded", 0, 10),
					attempt(w.t0, 2, "succeeded", 5, 15),
				}
			},
			fail: map[string]string{"single-attempt": "t1"},
		},
		{
			name: "single-attempt: a retry that overlaps the failed attempt",
			mutate: func(w *world) {
				w.view.attempts["t2"] = []client.Attempt{
					attempt(w.t0, 1, "failed", 0, 8),
					attempt(w.t0, 2, "succeeded", 6, 12),
				}
			},
			fail: map[string]string{"single-attempt": "t2"},
		},
		{
			name: "single-attempt: a second attempt after a success",
			mutate: func(w *world) {
				w.view.attempts["t1"] = []client.Attempt{
					attempt(w.t0, 1, "succeeded", 0, 10),
					attempt(w.t0, 2, "succeeded", 11, 15),
				}
			},
			fail: map[string]string{"single-attempt": "succeeded"},
		},
		{
			name: "single-attempt: an attempt still open when the next started",
			mutate: func(w *world) {
				w.view.attempts["t3"] = []client.Attempt{
					attempt(w.t0, 1, "running", 0, -1),
					attempt(w.t0, 2, "succeeded", 7, 14),
				}
			},
			fail: map[string]string{"single-attempt": "t3"},
		},
		{
			name: "single-attempt: back to back, end equal to the next start",
			mutate: func(w *world) {
				w.view.attempts["t2"] = []client.Attempt{
					attempt(w.t0, 1, "failed", 0, 6),
					attempt(w.t0, 2, "succeeded", 6, 12),
				}
			},
		},
		{
			name: "single-attempt: a terminal task whose last attempt never ended",
			mutate: func(w *world) {
				w.view.attempts["t1"] = []client.Attempt{attempt(w.t0, 1, "running", 0, -1)}
			},
			fail: map[string]string{"single-attempt": "no ended_at"},
		},
		{
			name: "single-attempt: an overlap within the clock-skew tolerance",
			mutate: func(w *world) {
				prev := attempt(w.t0, 1, "failed", 0, 6)
				ended := prev.EndedAt.Add(500 * time.Millisecond) // the worker's clock runs ahead
				prev.EndedAt = &ended
				w.view.attempts["t2"] = []client.Attempt{prev, attempt(w.t0, 2, "succeeded", 6, 12)}
			},
		},
		{
			name:   "single-attempt: the server cannot list a task's attempts",
			mutate: func(w *world) { w.view.failAttempt["t1"] = true },
			fail:   map[string]string{"single-attempt": "t1"},
		},

		// logs
		{
			name: "logs: a line differs",
			mutate: func(w *world) {
				w.view.logs["t1"][1].Data = strings.Replace(w.view.logs["t1"][1].Data, "line 60 ", "line 06 ", 1)
			},
			fail: map[string]string{"logs": "t1"},
		},
		{
			name: "logs: overflow lost only the oldest whole chunk",
			mutate: func(w *world) {
				w.in.LogOverflowExpected = true
				w.view.logs["t1"] = w.view.logs["t1"][1:]
			},
		},
		{
			name: "logs: overflow may not lose the newest chunk, so not every chunk",
			mutate: func(w *world) {
				w.in.LogOverflowExpected = true
				delete(w.view.logs, "t1")
			},
			fail: map[string]string{"logs": "t1"},
		},
		{
			name: "logs: overflow with nothing sent and nothing kept",
			mutate: func(w *world) {
				w.in.LogOverflowExpected = true
				delete(w.view.logs, "t8") // t8 sent no lines
			},
		},
		{
			name: "logs: overflow lost a chunk in the middle",
			mutate: func(w *world) {
				w.in.LogOverflowExpected = true
				w.view.logs["t1"] = slices.Delete(w.view.logs["t1"], 1, 2)
			},
			fail: map[string]string{"logs": "t1"},
		},
		{
			name: "logs: overflow lost the newest chunk",
			mutate: func(w *world) {
				w.in.LogOverflowExpected = true
				w.view.logs["t1"] = w.view.logs["t1"][:2]
			},
			fail: map[string]string{"logs": "t1"},
		},
		{
			name:   "logs: the oldest chunk lost with no overflow expected",
			mutate: func(w *world) { w.view.logs["t1"] = w.view.logs["t1"][1:] },
			fail:   map[string]string{"logs": "t1"},
		},
		{
			name: "logs: chunks persisted out of order are read in sequence",
			mutate: func(w *world) {
				ch := w.view.logs["t1"]
				ch[0].NatsSeq, ch[2].NatsSeq = ch[2].NatsSeq, ch[0].NatsSeq
			},
		},
		{
			name: "logs: a chunk stored twice",
			mutate: func(w *world) {
				dup := w.view.logs["t1"][1]
				w.natsSeq++
				dup.NatsSeq = w.natsSeq
				w.view.logs["t1"] = append(w.view.logs["t1"], dup)
			},
			fail: map[string]string{"logs": "t1"},
		},
		{
			name: "logs: chunks from an attempt the simulator never ran",
			mutate: func(w *world) {
				for i := range w.view.logs["t1"] {
					w.view.logs["t1"][i].AttemptID = "ghost"
				}
			},
			fail: map[string]string{"logs": "ghost"},
		},

		// workers-converge
		{
			name: "workers-converge: 98 online for a fleet of 100",
			mutate: func(w *world) {
				w.in.LiveWorkers = 100
				w.view.workers = func(int) []client.Worker { return workersOf(98, 0) }
			},
			fail: map[string]string{"workers-converge": "98"},
		},
		{
			name: "workers-converge: converges after a few polls",
			mutate: func(w *world) {
				w.in.ConvergeWithin = 5 * time.Second
				w.view.workers = func(poll int) []client.Worker { return workersOf(min(poll, 3), 0) }
			},
		},
		{
			name: "workers-converge: disabled workers may account for the gap",
			mutate: func(w *world) {
				w.in.LiveWorkers = 100
				w.view.workers = func(int) []client.Worker { return workersOf(98, 2) }
			},
		},
		{
			name: "workers-converge: more online than live",
			mutate: func(w *world) {
				w.view.workers = func(int) []client.Worker { return workersOf(4, 0) }
			},
			fail: map[string]string{"workers-converge": "4"},
		},

		// caps
		{
			name:   "caps: peak 6 on pool arnold with limit 5",
			mutate: func(w *world) { w.in.Scenario.Farm.UsagePools[0].Limit = 5 },
			fail:   map[string]string{"caps": "arnold"},
		},
		{
			name:   "caps: peak 6 on queue render with cap 5",
			mutate: func(w *world) { w.in.Scenario.Farm.Queues[0].MaxConcurrentTasks = 5 },
			fail:   map[string]string{"caps": "render"},
		},
		{
			name:   "caps: a peak equal to the cap passes",
			mutate: func(w *world) { w.in.Scenario.Farm.Queues[0].MaxConcurrentTasks = 6 },
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := healthyWorld()
			if tc.mutate != nil {
				tc.mutate(w)
			}
			results := verify.Check(t.Context(), w.view, w.in)
			assertResults(t, results, tc.fail)
		})
	}
}

func attempt(t0 time.Time, n int, status string, from, to int) client.Attempt {
	started := at(t0, from)
	a := client.Attempt{ID: strconv.Itoa(n), AttemptNumber: n, Status: status, StartedAt: &started}
	if to >= 0 {
		ended := at(t0, to)
		a.EndedAt = &ended
	}
	return a
}

func assertResults(t *testing.T, results []verify.Result, fail map[string]string) {
	t.Helper()
	assertResultsNamed(t, invariantNames, results, fail)
}

// assertResultsNamed is assertResults for a run that reports the invariants
// want.
func assertResultsNamed(t *testing.T, want []string, results []verify.Result, fail map[string]string) {
	t.Helper()
	var names []string
	for _, r := range results {
		names = append(names, r.Name)
	}
	if !slices.Equal(names, want) {
		t.Fatalf("Check returned %v, want %v", names, want)
	}
	for _, r := range results {
		if len(r.Evidence) > 20 {
			t.Errorf("%s: %d evidence entries, want at most 20", r.Name, len(r.Evidence))
		}
		want, shouldFail := fail[r.Name]
		switch {
		case !shouldFail && !r.OK:
			t.Errorf("%s failed, want pass: %q", r.Name, r.Evidence)
		case shouldFail && r.OK:
			t.Errorf("%s passed, want a failure naming %q", r.Name, want)
		case shouldFail && !slices.ContainsFunc(r.Evidence, func(e string) bool { return strings.Contains(e, want) }):
			t.Errorf("%s evidence %q does not mention %q", r.Name, r.Evidence, want)
		}
	}
}

func TestCheck_DroppedWork(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(w *world)
		want   []string // outcomes evidence, each naming the job
	}{
		{name: "every task present"},
		{
			name: "a never-leased task row is missing",
			mutate: func(w *world) {
				w.view.tasks["j1"] = slices.DeleteFunc(w.view.tasks["j1"], func(t client.Task) bool { return t.ID == "t5" })
			},
			want: []string{"job j1 has 5 tasks (5 listed), want 6 (2 steps x 3 tasks)"},
		},
		{
			name: "a task the simulator ran is missing",
			mutate: func(w *world) {
				w.view.tasks["j1"] = slices.DeleteFunc(w.view.tasks["j1"], func(t client.Task) bool { return t.ID == "t0" })
			},
			want: []string{"job j1 has 5 tasks", "job j1: the simulator ran task t0, which the server does not list"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld()
			w.in.Scenario.Jobs[0].Steps, w.in.Scenario.Jobs[0].TasksPerStep = 2, 3
			w.job("j1", "failed", false)
			w.task("j1", "t0", "succeeded", att{"a0", 0, 1, "succeeded", "succeeded", 2})
			w.task("j1", "t1", "succeeded", att{"a1", 0, 1, "succeeded", "succeeded", 2})
			w.task("j1", "t2", "failed", att{"a2", 0, 1, "failed", "failed", 2})
			for _, id := range []string{"t3", "t4", "t5"} {
				w.task("j1", id, "canceled") // the dependent step, canceled after t2 failed
			}
			if tc.mutate != nil {
				tc.mutate(w)
			}
			results := verify.Check(t.Context(), w.view, w.in)
			if len(tc.want) == 0 {
				assertResults(t, results, nil)
				return
			}
			assertResults(t, results, map[string]string{"outcomes": "job j1"})
			for _, want := range tc.want {
				if !slices.ContainsFunc(results[1].Evidence, func(e string) bool { return strings.Contains(e, want) }) {
					t.Errorf("outcomes evidence %q does not contain %q", results[1].Evidence, want)
				}
			}
		})
	}
}

// TestCheck_CapsAfterACancel pins that caps ends a canceled attempt's window
// at the cancel action's start: the server frees the slot when it commits the
// cancel, before the simulator records the end.
func TestCheck_CapsAfterACancel(t *testing.T) {
	tests := []struct {
		name       string
		canceledAt int // seconds after t0; < 0 leaves CanceledAt unset
		fail       bool
	}{
		// jA's attempt is held 0-10s by the simulator; jB's lease lands at 6s.
		{"the action at 5s freed the slot before the next lease", 5, false},
		{"without the action's time the windows overlap", -1, true},
		{"an action after the next lease does not hide the overlap", 7, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld()
			w.in.Scenario.Farm.UsagePools[0].Limit = 1
			w.job("jA", "canceled", true)
			w.task("jA", "tA", "canceled", att{"aA", 0, 10, "canceled", "canceled", 0})
			w.job("jB", "completed", false)
			w.task("jB", "tB", "succeeded", att{"aB", 6, 12, "succeeded", "succeeded", 0})
			if tc.canceledAt >= 0 {
				w.in.CanceledAt = map[string]time.Time{"jA": at(w.t0, tc.canceledAt)}
			}
			want := map[string]string(nil)
			if tc.fail {
				want = map[string]string{"caps": "arnold"}
			}
			assertResults(t, verify.Check(t.Context(), w.view, w.in), want)
		})
	}
}

// slowFirstLogs answers the first read of t1's logs slowly and with the
// newest chunk missing, as a log consumer with a backlog would, then has every
// chunk.
type slowFirstLogs struct {
	*fakeView

	once sync.Once
	full []client.LogLine
}

func (s *slowFirstLogs) TaskLogs(ctx context.Context, taskID string, after int64, limit int) ([]client.LogLine, int64, error) {
	first := false
	if taskID == "t1" {
		s.once.Do(func() { first = true })
	}
	if !first {
		return s.fakeView.TaskLogs(ctx, taskID, after, limit)
	}
	time.Sleep(200 * time.Millisecond)
	items, cursor, err := s.fakeView.TaskLogs(ctx, taskID, after, limit)
	s.mu.Lock()
	s.logs["t1"] = s.full
	s.mu.Unlock()
	return items, cursor, err
}

// TestCheck_SettleStartsAfterTheFirstPass pins that a first pass slower than
// the settle time still re-reads what it found wrong.
func TestCheck_SettleStartsAfterTheFirstPass(t *testing.T) {
	w := healthyWorld()
	w.in.LogSettle = 50 * time.Millisecond
	full := w.view.logs["t1"]
	w.view.logs["t1"] = full[:2]
	v := &slowFirstLogs{fakeView: w.view, full: full}
	assertResults(t, verify.Check(t.Context(), v, w.in), nil)
}

func TestCancelTimes(t *testing.T) {
	t0 := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	recs := []driver.ActionRecord{
		{Kind: driver.ActionCancelJobs, Started: t0.Add(time.Minute), JobIDs: []string{"a", "b"}},
		{Kind: "pause_queue", Started: t0},
		{Kind: driver.ActionCancelJobs, Started: t0.Add(30 * time.Second), JobIDs: []string{"a"}},
	}
	got := verify.CancelTimes(recs)
	want := map[string]time.Time{"a": t0.Add(30 * time.Second), "b": t0.Add(time.Minute)}
	if len(got) != len(want) || !got["a"].Equal(want["a"]) || !got["b"].Equal(want["b"]) {
		t.Errorf("CancelTimes = %v, want %v", got, want)
	}
}

func TestCheck_EvidenceIsBounded(t *testing.T) {
	w := newWorld()
	for i := range 30 {
		w.job("job"+strconv.Itoa(i), "running", false)
	}
	results := verify.Check(t.Context(), w.view, w.in)
	r := results[0]
	if r.Name != "terminal" || r.OK {
		t.Fatalf("first result = %+v, want a failed terminal", r)
	}
	if len(r.Evidence) != 20 {
		t.Fatalf("%d evidence entries, want 20", len(r.Evidence))
	}
	if last := r.Evidence[19]; !strings.Contains(last, "11 more") {
		t.Errorf("last entry %q, want it to count the 11 entries left out", last)
	}
}

// manyTasks is a passing run of one job with n single-attempt tasks.
func manyTasks(n int) *world {
	w := newWorld()
	w.in.Scenario.Farm.UsagePools[0].Limit = 0
	w.job("j1", "completed", false)
	for i := range n {
		w.task("j1", "t"+strconv.Itoa(i), "succeeded", att{"a" + strconv.Itoa(i), 0, 1, "succeeded", "succeeded", 3})
	}
	return w
}

func TestCheck_Sampling(t *testing.T) {
	sampled := func(seed uint64, attempts, logs float64) (a, l []string) {
		w := manyTasks(200)
		w.in.Seed, w.in.AttemptSample, w.in.LogSample = seed, attempts, logs
		assertResults(t, verify.Check(t.Context(), w.view, w.in), nil)
		a, l = w.view.callsTo("ListAttempts"), w.view.callsTo("TaskLogs")
		slices.Sort(a)
		slices.Sort(l)
		return a, l
	}
	a, l := sampled(1, 0, 0)
	if len(a) != 0 || len(l) != 0 {
		t.Errorf("sample 0 read %d attempt lists and %d logs, want none", len(a), len(l))
	}
	a, l = sampled(1, 1, 1)
	if len(a) != 200 || len(l) != 200 {
		t.Errorf("sample 1 read %d attempt lists and %d logs, want 200 of each", len(a), len(l))
	}
	a1, l1 := sampled(7, 0.25, 0.25)
	a2, l2 := sampled(7, 0.25, 0.25)
	if !slices.Equal(a1, a2) || !slices.Equal(l1, l2) {
		t.Error("the same seed sampled different tasks")
	}
	if len(a1) < 25 || len(a1) > 75 || len(l1) < 25 || len(l1) > 75 {
		t.Errorf("sample 0.25 of 200 read %d attempt lists and %d logs, want about 50", len(a1), len(l1))
	}
	if a3, _ := sampled(8, 0.25, 0.25); slices.Equal(a1, a3) {
		t.Error("seeds 7 and 8 sampled the same tasks")
	}
}

func TestCheck_ListsTasksByThePage(t *testing.T) {
	w := manyTasks(2500)
	w.in.AttemptSample, w.in.LogSample = 0, 0
	assertResults(t, verify.Check(t.Context(), w.view, w.in), nil)
	// 2500 tasks at the server's maximum page of 1000 is three pages.
	if got := len(w.view.callsTo("ListTasks")); got != 3 {
		t.Errorf("ListTasks called %d times, want 3", got)
	}
}

func TestCheck_SettlesALateLedgerEvent(t *testing.T) {
	tests := []struct {
		name   string
		settle time.Duration
		late   bool // record the terminal event after Check starts
		pass   bool
	}{
		{"the terminal event arrives while settling", 5 * time.Second, true, true},
		{"the terminal event never arrives", 50 * time.Millisecond, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := healthyWorld()
			w.in.Settle = tc.settle
			// The open attempt would stay in flight to the end of the ledger.
			w.in.Scenario.Farm.UsagePools[0].Limit = 0
			// t13's attempt has no end event yet, as when Check runs between
			// the server applying a terminal status and the worker recording it.
			w.task("j1", "t13", "succeeded", att{"a13", 0, 1, "", "succeeded", 0})
			if tc.late {
				go func() {
					time.Sleep(100 * time.Millisecond)
					w.ledger.Record(simworker.Event{At: at(w.t0, 1), TaskID: "t13", AttemptID: "a13", JobID: "j1", Kind: simworker.KindTerminal, Outcome: "succeeded"})
				}()
			}
			results := verify.Check(t.Context(), w.view, w.in)
			if tc.pass {
				assertResults(t, results, nil)
			} else {
				assertResults(t, results, map[string]string{"outcomes": "t13"})
			}
		})
	}
}

func TestCheck_SettlesLateLogs(t *testing.T) {
	w := healthyWorld()
	w.in.LogSettle = 5 * time.Second
	full := w.view.logs["t1"]
	w.view.mu.Lock()
	w.view.logs["t1"] = full[:2]
	w.view.mu.Unlock()
	go func() {
		time.Sleep(100 * time.Millisecond)
		w.view.mu.Lock()
		w.view.logs["t1"] = full
		w.view.mu.Unlock()
	}()
	assertResults(t, verify.Check(t.Context(), w.view, w.in), nil)
}

func TestWaitDrained(t *testing.T) {
	jobsOf := func(statuses ...string) (*fakeView, []driver.SubmittedJob) {
		v := newFakeView()
		jobs := []driver.SubmittedJob{{Status: 503}} // rejected: ignored
		for i, s := range statuses {
			id := "job" + strconv.Itoa(i)
			v.jobs = append(v.jobs, client.Job{ID: id, Status: s})
			jobs = append(jobs, driver.SubmittedJob{ID: id, Status: 201})
		}
		return v, jobs
	}
	t.Run("all terminal", func(t *testing.T) {
		v, jobs := jobsOf("completed", "failed", "canceled")
		if err := verify.WaitDrained(t.Context(), v, "farm", jobs, time.Second); err != nil {
			t.Fatalf("WaitDrained: %v", err)
		}
	})
	t.Run("a job finishes while waiting", func(t *testing.T) {
		v, jobs := jobsOf("completed", "running")
		go func() {
			time.Sleep(100 * time.Millisecond)
			v.setJob("job1", "completed")
		}()
		if err := verify.WaitDrained(t.Context(), v, "farm", jobs, 5*time.Second); err != nil {
			t.Fatalf("WaitDrained: %v", err)
		}
	})
	t.Run("a stranded job", func(t *testing.T) {
		v, jobs := jobsOf("completed", "running", "pending")
		start := time.Now()
		err := verify.WaitDrained(t.Context(), v, "farm", jobs, 200*time.Millisecond)
		if err == nil {
			t.Fatal("WaitDrained returned nil, want the stranded jobs")
		}
		if took := time.Since(start); took < 200*time.Millisecond {
			t.Errorf("gave up after %s, before the 200ms timeout", took)
		}
		for _, want := range []string{"job1 (running)", "job2 (pending)"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not name %q", err, want)
			}
		}
		if strings.Contains(err.Error(), "job0") {
			t.Errorf("error %q names a terminal job", err)
		}
	})
	t.Run("many stranded jobs are listed by the page and capped at 20", func(t *testing.T) {
		statuses := make([]string, 25)
		for i := range statuses {
			statuses[i] = "running"
		}
		v, jobs := jobsOf(statuses...)
		err := verify.WaitDrained(t.Context(), v, "farm", jobs, 50*time.Millisecond)
		if err == nil {
			t.Fatal("WaitDrained returned nil")
		}
		if n := strings.Count(err.Error(), "(running)"); n != 20 {
			t.Errorf("error lists %d jobs, want 20: %v", n, err)
		}
		if !strings.Contains(err.Error(), "25") {
			t.Errorf("error %q does not give the stranded count", err)
		}
		if n := len(v.callsTo("GetJob")); n != 0 {
			t.Errorf("GetJob called %d times for 25 jobs, want farm pages only", n)
		}
		if farms := v.callsTo("ListJobs"); len(farms) == 0 || farms[0] != "farm" {
			t.Errorf("ListJobs calls %v, want farm-filtered pages", farms)
		}
	})
	t.Run("the context ends the wait", func(t *testing.T) {
		v, jobs := jobsOf("running")
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		err := verify.WaitDrained(ctx, v, "farm", jobs, time.Hour)
		if err == nil || !errors.Is(err, context.Canceled) {
			t.Errorf("WaitDrained = %v, want context.Canceled", err)
		}
	})
}

func TestCheck_TaskSeen(t *testing.T) {
	w := healthyWorld()
	var (
		mu   sync.Mutex
		seen = map[string]string{}
	)
	w.in.TaskSeen = func(jobID string, task client.Task) {
		mu.Lock()
		defer mu.Unlock()
		seen[task.ID] = jobID + "/" + task.Status
	}
	verify.Check(t.Context(), w.view, w.in)
	want := map[string]string{
		"t1": "j1/succeeded", "t2": "j1/succeeded", "t3": "j1/succeeded", "t8": "j1/succeeded",
		"t4": "j2/canceled", "t5": "j2/canceled", "t6": "j2/canceled",
	}
	if !maps.Equal(seen, want) {
		t.Errorf("TaskSeen saw %v, want every listed task once: %v", seen, want)
	}
}

// TestCheck_LedgerOverlap covers a worker that keeps running an attempt the
// server has already closed: the server reclaimed it (its attempts are
// sequential: failed, then a new one), but the ledger shows the old worker
// still running it when the new attempt was leased to another worker. Only
// the ledger can see the task ran twice at once.
func TestCheck_LedgerOverlap(t *testing.T) {
	tests := []struct {
		name     string
		staleEnd int // when the first attempt ended on its worker; -1 never
		endKind  string
		want     string // evidence single-attempt must carry; "" passes
	}{
		{"the reclaimed attempt kept running", 110, simworker.KindTerminal, "ran until"},
		{"the reclaimed attempt never ended", -1, "", "had not ended"},
		{"it crashed before the new lease", 105, simworker.KindCrashed, ""},
		{"it ended exactly at the new lease", 106, simworker.KindTerminal, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := healthyWorld()
			w.job("j3", "completed", false)
			w.view.tasks["j3"] = append(w.view.tasks["j3"], client.Task{ID: "t9", Status: "succeeded"})
			s1, e1, s2, e2 := at(w.t0, 100), at(w.t0, 104), at(w.t0, 106), at(w.t0, 112)
			w.view.attempts["t9"] = []client.Attempt{
				{ID: "1", AttemptNumber: 1, WorkerID: "w0", Status: "failed", StartedAt: &s1, EndedAt: &e1},
				{ID: "2", AttemptNumber: 2, WorkerID: "w1", Status: "succeeded", StartedAt: &s2, EndedAt: &e2},
			}
			w.ledger.Record(simworker.Event{At: s1, WorkerID: "w0", TaskID: "t9", AttemptID: "a9a", JobID: "j3", Kind: simworker.KindLeased})
			w.ledger.Record(simworker.Event{At: s2, WorkerID: "w1", TaskID: "t9", AttemptID: "a9b", JobID: "j3", Kind: simworker.KindLeased})
			w.ledger.Record(simworker.Event{At: e2, WorkerID: "w1", TaskID: "t9", AttemptID: "a9b", JobID: "j3", Kind: simworker.KindTerminal, Outcome: simworker.OutcomeSucceeded})
			if tt.staleEnd >= 0 {
				e := simworker.Event{At: at(w.t0, tt.staleEnd), WorkerID: "w0", TaskID: "t9", AttemptID: "a9a", JobID: "j3", Kind: tt.endKind}
				if e.Kind == simworker.KindTerminal {
					e.Outcome = simworker.OutcomeSucceeded
				}
				w.ledger.Record(e)
			}
			results := verify.Check(t.Context(), w.view, w.in)
			fail := map[string]string{}
			if tt.want != "" {
				fail[verify.NameSingleAttempt] = tt.want
			}
			assertResults(t, results, fail)
			if tt.want != "" {
				ev := strings.Join(results[2].Evidence, "\n")
				for _, name := range []string{"t9", "a9a", "a9b", "w0", "w1"} {
					if !strings.Contains(ev, name) {
						t.Errorf("single-attempt evidence does not name %s: %s", name, ev)
					}
				}
			}
		})
	}
}

// TestCheck_RealWorkerTasks pins how tasks real sqi-workers ran are judged.
// The ledger has nothing for them, so outcomes and logs must leave them out
// once Simulated says the worker is not the fleet's; without it they read as
// tasks the simulator never ran.
func TestCheck_RealWorkerTasks(t *testing.T) {
	addRealTasks := func(w *world) {
		w.view.tasks["j1"] = append(w.view.tasks["j1"], client.Task{ID: "tr", Status: "succeeded", AssignedWorkerID: "real-1"})
		w.view.attempts["tr"] = []client.Attempt{attempt(w.t0, 1, "succeeded", 0, 5)}
		w.view.attempts["tr"][0].WorkerID = "real-1" // its ID is the attempt number, as the client reports it
		w.view.logs["tr"] = w.chunks("tr", "ra1", 1, 10)
		// A canceled task's row does not name its worker; its attempt does.
		w.view.tasks["j2"] = append(w.view.tasks["j2"], client.Task{ID: "tc", Status: "canceled"})
		w.view.attempts["tc"] = []client.Attempt{attempt(w.t0, 1, "canceled", 0, 3)}
		w.view.attempts["tc"][0].WorkerID = "real-1"
		w.view.logs["tc"] = w.chunks("tc", "ra2", 1, 4)
	}
	tests := []struct {
		name      string
		simulated func(string) bool
		fail      map[string]string
	}{
		{name: "left out when Simulated names the fleet", simulated: func(id string) bool { return id == "w0" }},
		{name: "judged against the ledger without Simulated", fail: map[string]string{"outcomes": "tr", "logs": "attempt ra"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := healthyWorld()
			addRealTasks(w)
			w.in.Simulated = tt.simulated
			assertResults(t, verify.Check(t.Context(), w.view, w.in), tt.fail)
		})
	}
}

func TestCheck_ListsTasksInServerOrder(t *testing.T) {
	w := manyTasks(3500)
	w.in.AttemptSample, w.in.LogSample = 0, 0
	var (
		mu   sync.Mutex
		seen []string
	)
	w.in.TaskSeen = func(_ string, task client.Task) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, task.ID)
	}
	assertResults(t, verify.Check(t.Context(), w.view, w.in), nil)
	want := make([]string, 3500)
	for i := range want {
		want[i] = "t" + strconv.Itoa(i)
	}
	if !slices.Equal(seen, want) {
		t.Errorf("tasks were seen out of order or not once each: %d seen, want %d in the server's order", len(seen), len(want))
	}
}

func TestCheck_ReportsAFailedLaterTaskPage(t *testing.T) {
	w := manyTasks(2500)
	w.in.AttemptSample, w.in.LogSample = 0, 0
	w.view.failTasksAt = 2000
	assertResults(t, verify.Check(t.Context(), w.view, w.in), map[string]string{"outcomes": "job j1: listing tasks"})
}

// ── Chaos runs ───────────────────────────────────────────────────────────────

// chaosNames are the checks of a chaos run: the six, then submission-atomic.
var chaosNames = append(slices.Clone(invariantNames), verify.NameSubmissionAtomic)

func TestCheckNamesMatchVerify(t *testing.T) {
	if got := verify.Names(); !slices.Equal(got, scenario.CheckNames) {
		t.Errorf("verify.Names() = %v, want scenario.CheckNames %v", got, scenario.CheckNames)
	}
}

// logChunk is one chunk holding the given lines of a task's attempt.
func logChunk(w *world, taskID, attemptID string, seq int64, lines ...int) client.LogLine {
	parts := make([]string, len(lines))
	for i, n := range lines {
		parts[i] = simworker.LogLine(taskID, n, lineBytes)
	}
	w.natsSeq++
	return client.LogLine{
		ID: fmt.Sprintf("%s-%d-%d", taskID, seq, w.natsSeq), AttemptID: attemptID, SeqNum: seq, NatsSeq: w.natsSeq,
		Stream: "stdout", Data: strings.Join(parts, "\n"),
	}
}

func span(from, to int) []int {
	var out []int
	for n := from; n <= to; n++ {
		out = append(out, n)
	}
	return out
}

// faultLogWorld is a run of one task whose attempt (leased at t0, ended at
// t0+10 s on worker w0 of group "sim", 20 lines) the server kept lines 1-10
// and 15-20 of, with a fault window from t0+2 s to t0+4 s on groups.
func faultLogWorld(groups ...string) *world {
	w := newWorld()
	w.job("j1", "completed", false)
	w.task("j1", "t1", "succeeded", att{"a1", 0, 10, "succeeded", "succeeded", 20})
	w.view.logs["t1"] = []client.LogLine{
		logChunk(w, "t1", "a1", 1, span(1, 10)...),
		logChunk(w, "t1", "a1", 3, span(15, 20)...),
	}
	w.in.FaultWindows = []verify.FaultWindow{{Start: at(w.t0, 2), End: at(w.t0, 4), Groups: groups}}
	w.in.GroupOf = func(id string) string {
		if id == "w0" {
			return "sim"
		}
		return ""
	}
	return w
}

func resultNamed(t *testing.T, results []verify.Result, name string) verify.Result {
	t.Helper()
	for _, r := range results {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("no %s result in %+v", name, results)
	return verify.Result{}
}

func TestLogs_LossInsideFaultWindowIsCounted(t *testing.T) {
	for _, groups := range [][]string{{"sim"}, nil} {
		t.Run(fmt.Sprintf("groups %v", groups), func(t *testing.T) {
			w := faultLogWorld(groups...)
			results := verify.Check(t.Context(), w.view, w.in)
			assertResultsNamed(t, chaosNames, results, nil)
			logs := resultNamed(t, results, verify.NameLogs)
			if got := logs.Counters[verify.CounterLogsLostInFault]; got != 4 {
				t.Errorf("Counters[%q] = %d (%v), want 4", verify.CounterLogsLostInFault, got, logs.Counters)
			}
		})
	}
	if verify.CounterLogsLostInFault != "logs.lost_in_fault" {
		t.Errorf("CounterLogsLostInFault = %q", verify.CounterLogsLostInFault)
	}
}

func TestLogs_LossOutsideFaultWindowFails(t *testing.T) {
	t.Run("window on another group", func(t *testing.T) {
		w := faultLogWorld("other")
		assertResultsNamed(t, chaosNames, verify.Check(t.Context(), w.view, w.in), map[string]string{"logs": "line 11 is"})
	})
	t.Run("window after the attempt", func(t *testing.T) {
		w := faultLogWorld("sim")
		w.in.FaultWindows[0].Start, w.in.FaultWindows[0].End = at(w.t0, 11), at(w.t0, 12)
		assertResultsNamed(t, chaosNames, verify.Check(t.Context(), w.view, w.in), map[string]string{"logs": "line 11 is"})
	})
	t.Run("no GroupOf", func(t *testing.T) {
		w := faultLogWorld("sim")
		w.in.GroupOf = nil
		assertResultsNamed(t, chaosNames, verify.Check(t.Context(), w.view, w.in), map[string]string{"logs": "line 11 is"})
	})
}

func TestLogs_ReorderInsideFaultWindowFails(t *testing.T) {
	cases := []struct {
		name   string
		chunks func(w *world) []client.LogLine
	}{
		{"reordered", func(w *world) []client.LogLine {
			return []client.LogLine{logChunk(w, "t1", "a1", 1, 1, 3, 2)}
		}},
		{"duplicated", func(w *world) []client.LogLine {
			return []client.LogLine{logChunk(w, "t1", "a1", 1, 1, 2, 2, 3)}
		}},
		{"corrupted", func(w *world) []client.LogLine {
			c := logChunk(w, "t1", "a1", 1, 1, 2, 3)
			c.Data += "x"
			return []client.LogLine{c}
		}},
		{"a chunk stored twice", func(w *world) []client.LogLine {
			return []client.LogLine{logChunk(w, "t1", "a1", 1, 1), logChunk(w, "t1", "a1", 1, 1)}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := faultLogWorld("sim")
			w.ledger = simworker.NewLedger()
			w.in.Ledger = w.ledger
			w.view.tasks = map[string][]client.Task{}
			w.view.attempts = map[string][]client.Attempt{}
			w.task("j1", "t1", "succeeded", att{"a1", 0, 10, "succeeded", "succeeded", 3})
			w.view.logs["t1"] = tc.chunks(w)
			assertResultsNamed(t, chaosNames, verify.Check(t.Context(), w.view, w.in), map[string]string{"logs": "job j1 task t1"})
		})
	}
}

// orphanWorld is a healthy run plus orphan o1 of class "big" (2 steps x 5
// tasks) in status, with n tasks on the server.
func orphanWorld(status string, n int) *world {
	w := healthyWorld()
	w.in.Scenario.Jobs = append(w.in.Scenario.Jobs, scenario.JobClass{Name: "big", Queue: "render", Steps: 2, TasksPerStep: 5})
	w.view.jobs = append(w.view.jobs, client.Job{ID: "o1", Name: "big-7", Status: status})
	for i := range n {
		w.view.tasks["o1"] = append(w.view.tasks["o1"], client.Task{ID: fmt.Sprintf("o1t%d", i), Status: "succeeded"})
	}
	w.in.Orphans = []driver.SubmittedJob{{ID: "o1", Name: "big-7", Class: "big"}}
	return w
}

func TestTerminal_CoversOrphans(t *testing.T) {
	w := orphanWorld("running", 10)
	assertResultsNamed(t, chaosNames, verify.Check(t.Context(), w.view, w.in), map[string]string{"terminal": "o1"})
}

func TestSubmissionAtomic_CompleteOrphanPasses(t *testing.T) {
	w := orphanWorld("completed", 10)
	assertResultsNamed(t, chaosNames, verify.Check(t.Context(), w.view, w.in), nil)
}

func TestSubmissionAtomic_PartialOrphanFails(t *testing.T) {
	w := orphanWorld("completed", 7)
	results := verify.Check(t.Context(), w.view, w.in)
	assertResultsNamed(t, chaosNames, results, map[string]string{"submission-atomic": "7 of 10"})
	if ev := resultNamed(t, results, verify.NameSubmissionAtomic).Evidence; !strings.Contains(strings.Join(ev, "\n"), "o1") {
		t.Errorf("evidence %q does not name job o1", ev)
	}
}

func TestSubmissionAtomic_UnknownFarmJobFails(t *testing.T) {
	w := healthyWorld()
	w.view.jobs = append(w.view.jobs, client.Job{ID: "x9", Name: "shot-x9", Status: "completed"})
	w.in.Orphans = []driver.SubmittedJob{}
	assertResultsNamed(t, chaosNames, verify.Check(t.Context(), w.view, w.in), map[string]string{"submission-atomic": "x9"})
}

func TestCheck_LoadRunKeepsSixInvariants(t *testing.T) {
	w := healthyWorld()
	// A job the run cannot account for: only a chaos run looks for one.
	w.view.jobs = append(w.view.jobs, client.Job{ID: "x9", Status: "completed"})
	results := verify.Check(t.Context(), w.view, w.in)
	assertResults(t, results, nil)
	if c := resultNamed(t, results, verify.NameLogs).Counters; c != nil {
		t.Errorf("a load run's logs result carries counters %v, want none", c)
	}

	t.Run("fault windows alone add submission-atomic", func(t *testing.T) {
		w := healthyWorld()
		w.in.FaultWindows = []verify.FaultWindow{}
		assertResultsNamed(t, chaosNames, verify.Check(t.Context(), w.view, w.in), nil)
	})
}

func TestWaitDrained_CoversOrphans(t *testing.T) {
	v := newFakeView()
	v.jobs = []client.Job{{ID: "j1", Status: "completed"}, {ID: "o1", Status: "running"}}
	jobs := []driver.SubmittedJob{{ID: "j1", Status: 201}, {ID: "o1", Name: "shot-2", Status: 0}}
	err := verify.WaitDrained(t.Context(), v, "farm", jobs, 50*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "o1 (running)") {
		t.Fatalf("WaitDrained = %v, want it to wait for orphan o1", err)
	}
	v.setJob("o1", "canceled")
	if err := verify.WaitDrained(t.Context(), v, "farm", jobs, time.Second); err != nil {
		t.Fatalf("WaitDrained: %v", err)
	}
}
