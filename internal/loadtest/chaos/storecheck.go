// SPDX-License-Identifier: AGPL-3.0-or-later

package chaos

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/uberware/sqi/internal/loadtest/marker"
	"github.com/uberware/sqi/internal/loadtest/scenario"
	"github.com/uberware/sqi/internal/loadtest/simworker"
	"github.com/uberware/sqi/internal/loadtest/verify"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/backend"
	"github.com/uberware/sqi/internal/store/sqlite"
)

// The store checks' names, as verify.Names and scenario.CheckNames list them.
const (
	NameStoreIntegrity     = "store-integrity"
	NameExecutionsRecorded = "executions-recorded"
)

// StoreInput is what the store checks read and compare.
type StoreInput struct {
	// Options open the stopped server's store. StoreChecks never migrates it
	// and never takes the single-server lock, whatever they say.
	Options backend.Options
	// FarmID is the run's farm; the status checks read only its jobs.
	FarmID string
	// Ledger is what the simulated workers did. Nil is an empty ledger.
	Ledger *simworker.Ledger
	// Markers are the real tasks' executions; only the main ones count. Nil
	// when the run had no real task.
	Markers []marker.Exec
}

// StoreChecks opens the stopped server's store and returns
// [store-integrity, executions-recorded].
//
// store-integrity: on SQLite, PRAGMA integrity_check is "ok" and PRAGMA
// foreign_key_check finds nothing; on both backends, no active usage claim
// breaks invariant I3, and over the farm's jobs a terminal step holds only
// terminal tasks, a terminal task has no running attempt, a completed job has
// only completed steps, and a failed or canceled job no step still open.
//
// executions-recorded: every attempt a simulated worker leased is in the
// store, closed, and not succeeded when the worker reported it failed (one
// the worker reported succeeded may have been reclaimed, so it may be failed
// or canceled); and no real task ran more times than the store has attempts
// for it.
//
// A store the checks cannot open or read fails both, with the error as
// evidence.
func StoreChecks(ctx context.Context, in StoreInput) []verify.Result {
	opts := in.Options
	opts.AutoMigrate, opts.ServerLock = false, false
	integrity, executions := &evidence{}, &evidence{}
	fail := func(s string) []verify.Result {
		integrity.add(s)
		executions.add(s)
		return []verify.Result{integrity.result(NameStoreIntegrity), executions.result(NameExecutionsRecorded)}
	}
	if sqliteDriver(opts.Driver) {
		if _, err := os.Stat(opts.SQLitePath); err != nil {
			// sqlite.Open would create a database where there is none.
			return fail(fmt.Sprintf("no SQLite database at %s: %v", opts.SQLitePath, err))
		}
		integrity.addAll(sqliteIntegrity(ctx, opts.SQLitePath))
	}
	st, err := backend.Open(ctx, opts)
	if err != nil {
		return fail("open the store: " + err.Error())
	}
	defer st.Close()
	integrity.addAll(claimViolations(ctx, st))
	farm, err := readFarm(ctx, st, in.FarmID)
	if err != nil {
		return fail(err.Error())
	}
	integrity.addAll(farm.statusViolations(ctx))
	executions.addAll(leasedAttempts(ctx, st, in.Ledger))
	executions.addAll(farm.realExecutions(ctx, in.Markers))
	return []verify.Result{integrity.result(NameStoreIntegrity), executions.result(NameExecutionsRecorded)}
}

func sqliteDriver(driver string) bool { return driver == "" || driver == backend.DriverSQLite }

// sqliteIntegrity runs SQLite's own checks on the database at path, opened
// read-only, through the "sqlite" driver package sqlite registers. The name
// is sqlite.FileURI's read-only form: sqlite.DSN has none, and FileURI is how
// DSN escapes a path the driver would misread.
func sqliteIntegrity(ctx context.Context, path string) []string {
	dsn, err := sqlite.FileURI(path, true)
	if err != nil {
		return []string{fmt.Sprintf("integrity_check: open %s: %v", path, err)}
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return []string{fmt.Sprintf("integrity_check: open %s: %v", path, err)}
	}
	defer db.Close()
	var out []string
	rows, err := pragmaRows(ctx, db, "PRAGMA integrity_check")
	switch {
	case err != nil:
		out = append(out, "integrity_check: "+err.Error())
	case !slices.Equal(rows, []string{"ok"}):
		for _, r := range rows {
			out = append(out, "integrity_check: "+r)
		}
	}
	rows, err = pragmaRows(ctx, db, "PRAGMA foreign_key_check")
	if err != nil {
		return append(out, "foreign_key_check: "+err.Error())
	}
	for _, r := range rows {
		out = append(out, "foreign_key_check: "+r)
	}
	return out
}

// pragmaRows runs a pragma and returns each row's columns joined with " ".
func pragmaRows(ctx context.Context, db *sql.DB, pragma string) ([]string, error) {
	rows, err := db.QueryContext(ctx, pragma)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out []string
	for rows.Next() {
		vals := make([]sql.NullString, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return out, err
		}
		parts := make([]string, len(vals))
		for i, v := range vals {
			parts[i] = v.String
		}
		out = append(out, strings.Join(parts, " "))
	}
	return out, rows.Err()
}

// claimChecker is the I3 diagnostic both concrete stores carry beside
// store.Store.
type claimChecker interface {
	ClaimInvariantViolations(ctx context.Context) ([]string, error)
}

func claimViolations(ctx context.Context, st store.Store) []string {
	c, ok := st.(claimChecker)
	if !ok {
		return []string{fmt.Sprintf("store %T has no usage-claim invariant check", st)}
	}
	ids, err := c.ClaimInvariantViolations(ctx)
	if err != nil {
		return []string{"usage-claim invariant check: " + err.Error()}
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, fmt.Sprintf("usage claim %s is active, but its attempt is not running or its task is terminal", id))
	}
	return out
}

// farmView is every job of the farm with its steps and tasks.
type farmView struct {
	st    store.Store
	jobs  []store.Job
	steps map[string][]store.Step // by job ID
	tasks map[string][]store.Task // by job ID
	// attempts caches ListTaskAttempts by task ID.
	attempts map[string][]store.TaskAttempt
}

func readFarm(ctx context.Context, st store.Store, farmID string) (*farmView, error) {
	f := &farmView{st: st, steps: map[string][]store.Step{}, tasks: map[string][]store.Task{}, attempts: map[string][]store.TaskAttempt{}}
	jobs, err := allPages(func(p store.Pagination) (store.Page[store.Job], error) {
		return st.ListJobs(ctx, store.ListJobsOptions{FarmID: farmID, Pagination: p})
	})
	if err != nil {
		return nil, fmt.Errorf("list the jobs of farm %s: %w", farmID, err)
	}
	f.jobs = jobs
	for _, j := range jobs {
		if f.steps[j.ID], err = st.ListSteps(ctx, j.ID); err != nil {
			return nil, fmt.Errorf("list the steps of job %s: %w", j.ID, err)
		}
		f.tasks[j.ID], err = allPages(func(p store.Pagination) (store.Page[store.Task], error) {
			return st.ListTasks(ctx, store.ListTasksOptions{JobID: j.ID, Pagination: p})
		})
		if err != nil {
			return nil, fmt.Errorf("list the tasks of job %s: %w", j.ID, err)
		}
	}
	return f, nil
}

// allPages reads every page of a list.
func allPages[T any](list func(store.Pagination) (store.Page[T], error)) ([]T, error) {
	var out []T
	for {
		page, err := list(store.Pagination{Limit: store.MaxLimit, Offset: len(out)})
		if err != nil {
			return nil, err
		}
		out = append(out, page.Items...)
		if len(page.Items) == 0 || len(out) >= page.Total {
			return out, nil
		}
	}
}

// taskAttempts returns the task's attempts, read once.
func (f *farmView) taskAttempts(ctx context.Context, taskID string) ([]store.TaskAttempt, error) {
	if a, ok := f.attempts[taskID]; ok {
		return a, nil
	}
	a, err := f.st.ListTaskAttempts(ctx, taskID)
	if err != nil {
		return nil, err
	}
	f.attempts[taskID] = a
	return a, nil
}

func terminalTask(s store.TaskStatus) bool {
	return s == store.TaskStatusSucceeded || s == store.TaskStatusFailed || s == store.TaskStatusCanceled
}

// statusViolations checks that each job's, step's, task's and attempt's
// statuses agree.
func (f *farmView) statusViolations(ctx context.Context) []string {
	var out []string
	for _, j := range f.jobs {
		out = append(out, jobStepViolations(j, f.steps[j.ID])...)
		terminalSteps := map[string]store.StepStatus{}
		for _, s := range f.steps[j.ID] {
			if s.Status.IsTerminal() {
				terminalSteps[s.ID] = s.Status
			}
		}
		for _, t := range f.tasks[j.ID] {
			if ss, ok := terminalSteps[t.StepID]; ok && !terminalTask(t.Status) {
				out = append(out, fmt.Sprintf("job %s step %s is %s but its task %s is %s", j.ID, t.StepID, ss, t.ID, t.Status))
			}
			if terminalTask(t.Status) {
				out = append(out, f.openAttempts(ctx, j.ID, t)...)
			}
		}
	}
	return out
}

// jobStepViolations checks a terminal job's steps.
func jobStepViolations(j store.Job, steps []store.Step) []string {
	var out []string
	for _, s := range steps {
		switch j.Status {
		case store.JobStatusCompleted:
			if s.Status != store.StepStatusCompleted {
				out = append(out, fmt.Sprintf("job %s is completed but its step %s is %s", j.ID, s.ID, s.Status))
			}
		case store.JobStatusFailed, store.JobStatusCanceled:
			if !s.Status.IsTerminal() {
				out = append(out, fmt.Sprintf("job %s is %s but its step %s is %s", j.ID, j.Status, s.ID, s.Status))
			}
		}
	}
	return out
}

// openAttempts names each running attempt of terminal task t.
func (f *farmView) openAttempts(ctx context.Context, jobID string, t store.Task) []string {
	attempts, err := f.taskAttempts(ctx, t.ID)
	if err != nil {
		return []string{fmt.Sprintf("list the attempts of task %s: %v", t.ID, err)}
	}
	var out []string
	for _, a := range attempts {
		if a.Status == store.AttemptStatusRunning {
			out = append(out, fmt.Sprintf("task %s of job %s is %s but its attempt %s (worker %s) is still running",
				t.ID, jobID, t.Status, a.ID, a.WorkerID))
		}
	}
	return out
}

// ledgerAttempt is what the ledger says of one attempt.
type ledgerAttempt struct {
	id, taskID, workerID string
	// outcome is the last terminal event's, "" when there is none.
	outcome string
}

// ledgerAttempts returns every attempt the ledger has a leased event for, in
// lease order, with its terminal outcome.
func ledgerAttempts(l *simworker.Ledger) []ledgerAttempt {
	if l == nil {
		return nil
	}
	var out []ledgerAttempt
	index := map[string]int{}
	for _, e := range l.Events() {
		switch e.Kind {
		case simworker.KindLeased:
			if _, ok := index[e.AttemptID]; !ok {
				index[e.AttemptID] = len(out)
				out = append(out, ledgerAttempt{id: e.AttemptID, taskID: e.TaskID, workerID: e.WorkerID})
			}
		case simworker.KindTerminal:
			if i, ok := index[e.AttemptID]; ok {
				out[i].outcome = e.Outcome
			}
		}
	}
	return out
}

// leasedAttempts checks every attempt the simulated workers leased against
// the store.
func leasedAttempts(ctx context.Context, st store.Store, l *simworker.Ledger) []string {
	var out []string
	for _, la := range ledgerAttempts(l) {
		a, err := st.GetTaskAttempt(ctx, la.id)
		switch {
		case errors.Is(err, store.ErrNotFound):
			out = append(out, fmt.Sprintf("attempt %s of task %s, leased by worker %s, is not in the store", la.id, la.taskID, la.workerID))
			continue
		case err != nil:
			out = append(out, fmt.Sprintf("read attempt %s of task %s: %v", la.id, la.taskID, err))
			continue
		}
		if s := attemptViolation(la, a.Status); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// attemptViolation compares the store's status of an attempt with the
// worker's outcome.
func attemptViolation(la ledgerAttempt, status store.AttemptStatus) string {
	switch {
	case status == store.AttemptStatusRunning:
		return fmt.Sprintf("attempt %s of task %s is still running in the stopped server's store", la.id, la.taskID)
	case la.outcome == string(store.TaskStatusSucceeded) && !terminalTask(store.TaskStatus(status)):
		return fmt.Sprintf("attempt %s of task %s: the worker reported succeeded, the store holds %q", la.id, la.taskID, status)
	case la.outcome == string(store.TaskStatusFailed) && status == store.AttemptStatusSucceeded:
		return fmt.Sprintf("attempt %s of task %s: the worker reported failed, the store recorded succeeded", la.id, la.taskID)
	}
	return ""
}

// realExecutions checks that no real task ran more times than the store has
// attempts for it. A task's key is its job's name, its step's name and its
// Frame (and Block) parameters, as the driver builds it.
func (f *farmView) realExecutions(ctx context.Context, markers []marker.Exec) []string {
	runs := map[string]int{}
	for _, e := range markers {
		if e.Role == marker.RoleMain {
			runs[e.Key]++
		}
	}
	if len(runs) == 0 {
		return nil
	}
	byKey := f.tasksByKey()
	var out []string
	for _, key := range slices.Sorted(maps.Keys(runs)) {
		tasks, ok := byKey[key]
		if !ok {
			out = append(out, fmt.Sprintf("real task key %q ran %d times but names no task of the farm", key, runs[key]))
			continue
		}
		n := 0
		for _, id := range tasks {
			attempts, err := f.taskAttempts(ctx, id)
			if err != nil {
				out = append(out, fmt.Sprintf("list the attempts of task %s: %v", id, err))
			}
			n += len(attempts)
		}
		if runs[key] > n {
			out = append(out, fmt.Sprintf("real task key %q (task %v) ran %d times, but the store has %d attempts for it",
				key, tasks, runs[key], n))
		}
	}
	return out
}

// tasksByKey maps each task key to its tasks: one, unless two jobs share a
// name.
func (f *farmView) tasksByKey() map[string][]string {
	out := map[string][]string{}
	for _, j := range f.jobs {
		names := map[string]string{}
		for _, s := range f.steps[j.ID] {
			names[s.ID] = s.Name
		}
		for _, t := range f.tasks[j.ID] {
			key := scenario.TaskKey(j.Name, names[t.StepID], t.Parameters["Frame"], t.Parameters["Block"])
			out[key] = append(out[key], t.ID)
		}
	}
	return out
}

// evidence collects violations, keeping the first verify.MaxEvidence, as the
// verify package's checks do.
type evidence struct {
	lines []string
	n     int
}

func (e *evidence) add(s string) {
	e.n++
	if len(e.lines) < verify.MaxEvidence {
		e.lines = append(e.lines, s)
	}
}

func (e *evidence) addAll(ss []string) {
	for _, s := range ss {
		e.add(s)
	}
}

func (e *evidence) result(name string) verify.Result {
	lines := e.lines
	if e.n > verify.MaxEvidence {
		lines = append(lines[:verify.MaxEvidence-1:verify.MaxEvidence-1], fmt.Sprintf("... and %d more", e.n-(verify.MaxEvidence-1)))
	}
	return verify.Result{Name: name, OK: e.n == 0, Evidence: lines}
}
