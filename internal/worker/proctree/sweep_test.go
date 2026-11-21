// SPDX-License-Identifier: AGPL-3.0-or-later

package proctree

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"
)

// fakeProc is one process in a fake process table.
type fakeProc struct {
	token  string
	zombie bool
	// pgid is the process's group.
	pgid int
	// envUnreadable means the process's environment cannot be read (another
	// account's process, without CAP_SYS_PTRACE).
	envUnreadable bool
	// readErr, when set, is what inspect returns for the process.
	readErr error
	// survives makes kill a no-op for this process.
	survives bool
}

// fakeTable is a process table the sweep's ops read and kill.
type fakeTable struct {
	mu     sync.Mutex
	procs  map[int]*fakeProc
	killed []int
	// onKill, when set, runs after a kill, under no lock.
	onKill func(pid int)
	listed int
}

func (f *fakeTable) ops() sweepOps {
	return sweepOps{
		list: func() ([]int, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.listed++
			pids := make([]int, 0, len(f.procs))
			for pid := range f.procs {
				pids = append(pids, pid)
			}
			slices.Sort(pids)
			return pids, nil
		},
		inspect: func(pid int) (procInfo, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			p, ok := f.procs[pid]
			if !ok {
				return procInfo{}, os.ErrNotExist
			}
			if p.envUnreadable {
				return procInfo{pgid: p.pgid, zombie: p.zombie, envUnreadable: true}, p.readErr
			}
			return procInfo{token: p.token, pgid: p.pgid, zombie: p.zombie}, p.readErr
		},
		kill: func(pid int) error {
			f.mu.Lock()
			p, ok := f.procs[pid]
			if ok && !p.survives {
				delete(f.procs, pid)
				f.killed = append(f.killed, pid)
			}
			f.mu.Unlock()
			if f.onKill != nil {
				f.onKill(pid)
			}
			return nil
		},
		deadline: 2 * time.Second,
		slack:    time.Second,
		poll:     10 * time.Millisecond,
		self:     -1,
		// The real hint is per operating system; any text stands in.
		unreadableHint: "test hint",
	}
}

func (f *fakeTable) killedPIDs() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Sorted(slices.Values(f.killed))
}

// logCapture records every log record a sweep emits.
type logCapture struct {
	mu   sync.Mutex
	recs []slog.Record
}

func (*logCapture) Enabled(context.Context, slog.Level) bool { return true }

func (c *logCapture) Handle(_ context.Context, r slog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.recs = append(c.recs, r.Clone())
	return nil
}

func (c *logCapture) WithAttrs([]slog.Attr) slog.Handler { return c }

func (c *logCapture) WithGroup(string) slog.Handler { return c }

// at returns the records logged at level, each as its message and attributes.
func (c *logCapture) at(level slog.Level) []map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []map[string]string
	for _, r := range c.recs {
		if r.Level != level {
			continue
		}
		m := map[string]string{"msg": r.Message}
		r.Attrs(func(a slog.Attr) bool {
			m[a.Key] = a.Value.String()
			return true
		})
		out = append(out, m)
	}
	return out
}

// testLedger returns a ledger in a fresh directory with boot ID "boot-1".
func testLedger(t *testing.T) *Ledger {
	t.Helper()
	return &Ledger{dir: t.TempDir(), bootID: "boot-1"}
}

// putRecord writes a record into l's directory as the worker would.
func putRecord(t *testing.T, l *Ledger, rec record) {
	t.Helper()
	if rec.BootID == "" {
		rec.BootID = l.bootID
	}
	if rec.StartedAt.IsZero() {
		rec.StartedAt = time.Now().Add(-time.Minute).UTC()
	}
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(l.dir, rec.Token+recordExt), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// ledgerFiles lists what is left in l's directory.
func ledgerFiles(t *testing.T, l *Ledger) []string {
	t.Helper()
	ents, err := os.ReadDir(l.dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(ents))
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return names
}

func runSweep(t *testing.T, l *Ledger, ops sweepOps) *logCapture {
	t.Helper()
	logs := &logCapture{}
	if err := l.sweepWith(t.Context(), slog.New(logs), ops); err != nil {
		t.Fatalf("sweepWith: %v", err)
	}
	return logs
}

func TestSweep_NoRecordsDoesNotList(t *testing.T) {
	l := testLedger(t)
	ops := (&fakeTable{}).ops()
	ops.list = func() ([]int, error) {
		t.Fatal("list called with no records")
		return nil, nil
	}
	runSweep(t, l, ops)
}

func TestSweep_KillsOnlyMarkedProcesses(t *testing.T) {
	l := testLedger(t)
	putRecord(t, l, record{Token: "A", PGID: 10, Label: Label{Kind: "task", AttemptID: "att-a", TaskID: "task-a"}})
	putRecord(t, l, record{Token: "B", PGID: 30, Label: Label{Kind: "environment-action", Command: "sh"}})
	tab := &fakeTable{procs: map[int]*fakeProc{
		10: {token: "A"}, 11: {token: "A"}, 12: {token: "other"}, 13: {},
	}}
	logs := runSweep(t, l, tab.ops())

	if got := tab.killedPIDs(); !slices.Equal(got, []int{10, 11}) {
		t.Errorf("killed %v, want [10 11]", got)
	}
	warns := logs.at(slog.LevelWarn)
	if len(warns) != 1 {
		t.Fatalf("WARN records = %v, want exactly one (for A)", warns)
	}
	w := warns[0]
	if w["msg"] != "killed a process tree the previous worker left running" || w["attempt_id"] != "att-a" ||
		w["task_id"] != "task-a" || w["kind"] != "task" || w["pgid"] != "10" || w["processes"] != "2" || w["running_for"] == "" {
		t.Errorf("WARN = %v", w)
	}
	if _, ok := w["command"]; ok {
		t.Errorf("WARN carries an empty command field: %v", w)
	}
	if left := ledgerFiles(t, l); len(left) != 0 {
		t.Errorf("ledger not emptied: %v", left)
	}
}

func TestSweep_RescansForLateForks(t *testing.T) {
	l := testLedger(t)
	putRecord(t, l, record{Token: "A", PGID: 10, Label: Label{Kind: "task"}})
	tab := &fakeTable{procs: map[int]*fakeProc{10: {token: "A"}}}
	tab.onKill = func(pid int) {
		if pid == 10 {
			tab.mu.Lock()
			tab.procs[20] = &fakeProc{token: "A"}
			tab.mu.Unlock()
		}
	}
	runSweep(t, l, tab.ops())
	if got := tab.killedPIDs(); !slices.Equal(got, []int{10, 20}) {
		t.Errorf("killed %v, want [10 20] (20 forked during the first pass)", got)
	}
}

func TestSweep_SkipsUnreadableProcesses(t *testing.T) {
	l := testLedger(t)
	putRecord(t, l, record{Token: "A", PGID: 10, Label: Label{Kind: "task"}})
	tab := &fakeTable{procs: map[int]*fakeProc{
		10: {token: "A", readErr: os.ErrNotExist}, 11: {token: "A"},
	}}
	runSweep(t, l, tab.ops())
	if got := tab.killedPIDs(); !slices.Equal(got, []int{11}) {
		t.Errorf("killed %v, want [11]", got)
	}
}

func TestSweep_IgnoresZombies(t *testing.T) {
	l := testLedger(t)
	putRecord(t, l, record{Token: "A", PGID: 10, Label: Label{Kind: "task"}})
	tab := &fakeTable{procs: map[int]*fakeProc{10: {token: "A", zombie: true}}}
	ops := tab.ops()
	ops.deadline = time.Minute
	start := time.Now()
	logs := runSweep(t, l, ops)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("sweep waited %s on a zombie", elapsed)
	}
	if got := tab.killedPIDs(); len(got) != 0 {
		t.Errorf("killed %v, want nothing", got)
	}
	if errs := logs.at(slog.LevelError); len(errs) != 0 {
		t.Errorf("ERROR records for a zombie: %v", errs)
	}
}

func TestSweep_DropsOtherBootRecords(t *testing.T) {
	l := testLedger(t)
	putRecord(t, l, record{Token: "A", PGID: 10, BootID: "old", Label: Label{Kind: "task"}})
	ops := (&fakeTable{}).ops()
	ops.list = func() ([]int, error) {
		t.Fatal("list called with only another boot's records")
		return nil, nil
	}
	runSweep(t, l, ops)
	if left := ledgerFiles(t, l); len(left) != 0 {
		t.Errorf("other boot's record not deleted: %v", left)
	}
}

func TestSweep_DeletesTornRecords(t *testing.T) {
	l := testLedger(t)
	for name, body := range map[string]string{"x.json.tmp": `{"token":"x"`, "y.json": "{"} {
		if err := os.WriteFile(filepath.Join(l.dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	logs := runSweep(t, l, (&fakeTable{}).ops())
	if left := ledgerFiles(t, l); len(left) != 0 {
		t.Errorf("torn records left: %v", left)
	}
	errs := logs.at(slog.LevelError)
	if len(errs) != 2 {
		t.Fatalf("ERROR records = %v, want two", errs)
	}
	for _, e := range errs {
		if e["msg"] != "proctree: dropped an unreadable process tree record" || e["file"] == "" || e["error"] == "" {
			t.Errorf("ERROR = %v", e)
		}
	}
}

func TestSweep_SurvivorIsLoggedAndRecordDeleted(t *testing.T) {
	l := testLedger(t)
	putRecord(t, l, record{Token: "A", PGID: 10, Label: Label{Kind: "task"}})
	tab := &fakeTable{procs: map[int]*fakeProc{10: {token: "A", survives: true}}}
	ops := tab.ops()
	ops.deadline = 200 * time.Millisecond
	logs := runSweep(t, l, ops)
	errs := logs.at(slog.LevelError)
	if len(errs) != 1 || errs[0]["msg"] != "proctree: a process the previous worker left running survived SIGKILL" ||
		errs[0]["pid"] != "10" || errs[0]["token"] != "A" {
		t.Errorf("ERROR records = %v, want one naming pid 10 and token A", errs)
	}
	if left := ledgerFiles(t, l); len(left) != 0 {
		t.Errorf("record of a survivor not deleted: %v", left)
	}
}

func TestSweep_ListFailureIsLogged(t *testing.T) {
	l := testLedger(t)
	putRecord(t, l, record{Token: "A", PGID: 10, Label: Label{Kind: "task"}})
	ops := (&fakeTable{}).ops()
	ops.list = func() ([]int, error) { return nil, errors.New("no /proc") }
	logs := runSweep(t, l, ops)
	errs := logs.at(slog.LevelError)
	if len(errs) != 1 || errs[0]["msg"] != "proctree: could not list processes to sweep" || errs[0]["error"] != "no /proc" {
		t.Errorf("ERROR records = %v, want one naming the list failure", errs)
	}
	if left := ledgerFiles(t, l); len(left) != 0 {
		t.Errorf("records kept after a list failure: %v", left)
	}
}

func TestSweep_UnreadableDirIsReturned(t *testing.T) {
	l := &Ledger{dir: filepath.Join(t.TempDir(), "absent"), bootID: "boot-1"}
	if err := l.sweepWith(t.Context(), slog.New(&logCapture{}), (&fakeTable{}).ops()); err == nil {
		t.Error("sweepWith on a missing ledger dir = nil, want an error")
	}
}

func TestNilLedger_SweepIsNoOp(t *testing.T) {
	var l *Ledger
	if err := l.Sweep(t.Context(), slog.New(&logCapture{})); err != nil {
		t.Errorf("nil ledger Sweep = %v", err)
	}
}

func TestSweep_ReportsUnreadableGroupMembers(t *testing.T) {
	l := testLedger(t)
	putRecord(t, l, record{Token: "A", PGID: 10, Label: Label{Kind: "task", AttemptID: "att-a"}})
	tab := &fakeTable{procs: map[int]*fakeProc{
		10: {pgid: 10, envUnreadable: true},
		11: {pgid: 10, envUnreadable: true},
		12: {pgid: 99, envUnreadable: true}, // another group: not mentioned
		13: {token: "A", pgid: 10},
	}}
	logs := runSweep(t, l, tab.ops())
	if got := tab.killedPIDs(); !slices.Equal(got, []int{13}) {
		t.Errorf("killed %v, want only the readable marked process [13]", got)
	}
	var unchecked map[string]string
	for _, w := range logs.at(slog.LevelWarn) {
		if w["msg"] == "proctree: could not check processes in a previous worker's process group; they were left running" {
			unchecked = w
		}
	}
	if unchecked == nil {
		t.Fatalf("no WARN about the unreadable group members; WARNs: %v", logs.at(slog.LevelWarn))
	}
	if unchecked["pgid"] != "10" || unchecked["processes"] != "2" || unchecked["attempt_id"] != "att-a" || unchecked["hint"] == "" {
		t.Errorf("WARN = %v, want pgid 10, processes 2, attempt att-a and a hint", unchecked)
	}
}

func TestSweep_NeverKillsItself(t *testing.T) {
	l := testLedger(t)
	putRecord(t, l, record{Token: "A", PGID: 10, Label: Label{Kind: "task"}})
	tab := &fakeTable{procs: map[int]*fakeProc{10: {token: "A"}, 11: {token: "A"}}}
	ops := tab.ops()
	ops.self = 10 // the sweeping worker inherited a recorded token
	runSweep(t, l, ops)
	if got := tab.killedPIDs(); !slices.Equal(got, []int{11}) {
		t.Errorf("killed %v, want [11]: the sweep must never kill its own process", got)
	}
}

func TestSweep_BoundedWhenAReadHangs(t *testing.T) {
	l := testLedger(t)
	putRecord(t, l, record{Token: "A", PGID: 10, Label: Label{Kind: "task"}})
	tab := &fakeTable{procs: map[int]*fakeProc{10: {token: "A"}}}
	ops := tab.ops()
	ops.deadline = 100 * time.Millisecond
	ops.slack = 100 * time.Millisecond
	hang := make(chan struct{})
	t.Cleanup(func() { close(hang) })
	ops.inspect = func(int) (procInfo, error) {
		<-hang // a read stuck in the kernel, as on a dead NFS mount
		return procInfo{}, os.ErrNotExist
	}
	start := time.Now()
	logs := runSweep(t, l, ops)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("sweep took %s with a hung read, want it bounded by deadline+slack (200ms)", elapsed)
	}
	errs := logs.at(slog.LevelError)
	if len(errs) != 1 || errs[0]["msg"] != "proctree: the sweep did not finish in time; starting without it" || errs[0]["pid"] != "10" {
		t.Errorf("ERROR records = %v, want one naming the stuck pid 10", errs)
	}
}

func TestSweep_StopsOnContextCancel(t *testing.T) {
	l := testLedger(t)
	putRecord(t, l, record{Token: "A", PGID: 10, Label: Label{Kind: "task"}})
	tab := &fakeTable{procs: map[int]*fakeProc{10: {token: "A"}}}
	ops := tab.ops()
	ops.deadline = time.Minute
	hang := make(chan struct{})
	t.Cleanup(func() { close(hang) })
	ops.inspect = func(int) (procInfo, error) {
		<-hang
		return procInfo{}, os.ErrNotExist
	}
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(100*time.Millisecond, cancel)
	start := time.Now()
	if err := l.sweepWith(ctx, slog.New(&logCapture{}), ops); err != nil {
		t.Fatalf("sweepWith: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("sweep took %s after its context was canceled", elapsed)
	}
}
