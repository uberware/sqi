// SPDX-License-Identifier: AGPL-3.0-or-later

package chaos

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/loadtest/scenario"
	"github.com/uberware/sqi/internal/loadtest/target"
	"github.com/uberware/sqi/internal/store/backend"
)

// tolerance is how far from its planned time a fault action may land.
const tolerance = 80 * time.Millisecond

type call struct {
	at   time.Time
	name string
	arg  int
}

// fakeTarget records every fault call with its time.
type fakeTarget struct {
	mu               sync.Mutex
	calls            []call
	groups           map[string][]int
	incs             []target.Incarnation
	lanes            map[string]*fakeLane
	restartServerErr error
	// bootNeedsServer makes workers fail to boot while the server is down, as
	// sqi-worker does: a real worker's process exits at once, and the fleet's
	// restart fails.
	bootNeedsServer bool
	serverDown      bool
}

func newFakeTarget(workers map[string][]int, lanes ...string) *fakeTarget {
	t := &fakeTarget{groups: workers, lanes: map[string]*fakeLane{}}
	for _, ws := range workers {
		for _, w := range ws {
			t.incs = append(t.incs, target.Incarnation{ID: incID(w, 0), Worker: w, Started: time.Now()})
		}
	}
	slices.SortFunc(t.incs, func(a, b target.Incarnation) int { return a.Worker - b.Worker })
	for _, l := range lanes {
		t.lanes[l] = &fakeLane{}
	}
	return t
}

func incID(w, n int) string { return strconv.Itoa(w) + "." + strconv.Itoa(n) }

func (t *fakeTarget) record(name string, arg int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.calls = append(t.calls, call{at: time.Now(), name: name, arg: arg})
}

func (t *fakeTarget) snapshot() []call {
	t.mu.Lock()
	defer t.mu.Unlock()
	return slices.Clone(t.calls)
}

// named returns the calls called name, in order.
func (t *fakeTarget) named(name string) []call {
	var out []call
	for _, c := range t.snapshot() {
		if c.name == name {
			out = append(out, c)
		}
	}
	return out
}

func (t *fakeTarget) endWorker(i int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for j := len(t.incs) - 1; j >= 0; j-- {
		if t.incs[j].Worker == i {
			if t.incs[j].Ended.IsZero() {
				t.incs[j].Ended = time.Now()
			}
			return
		}
	}
}

func (t *fakeTarget) KillServer(context.Context) error {
	t.record("KillServer", -1)
	t.mu.Lock()
	defer t.mu.Unlock()
	t.serverDown = true
	return nil
}

// down reports whether a worker booting now would find no server.
func (t *fakeTarget) down() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.bootNeedsServer && t.serverDown
}

func (t *fakeTarget) TermServer(context.Context) error { t.record("TermServer", -1); return nil }
func (t *fakeTarget) StallServer() error               { t.record("StallServer", -1); return nil }
func (t *fakeTarget) ResumeServer() error              { t.record("ResumeServer", -1); return nil }

func (t *fakeTarget) RestartServer(context.Context) error {
	t.record("RestartServer", -1)
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.restartServerErr == nil {
		t.serverDown = false
	}
	return t.restartServerErr
}

func (t *fakeTarget) RealWorkers(group string) []int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return slices.Clone(t.groups[group])
}

func (t *fakeTarget) KillWorker(_ context.Context, i int) error {
	t.record("KillWorker", i)
	t.endWorker(i)
	return nil
}

func (t *fakeTarget) TermWorker(_ context.Context, i int) error {
	t.record("TermWorker", i)
	t.endWorker(i)
	return nil
}

func (t *fakeTarget) StallWorker(i int) error  { t.record("StallWorker", i); return nil }
func (t *fakeTarget) ResumeWorker(i int) error { t.record("ResumeWorker", i); return nil }

func (t *fakeTarget) RestartWorker(_ context.Context, i int) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for _, inc := range t.incs {
		if inc.Worker != i {
			continue
		}
		if inc.Ended.IsZero() {
			return target.ErrWorkerRunning
		}
		n++
	}
	t.calls = append(t.calls, call{at: time.Now(), name: "RestartWorker", arg: i})
	inc := target.Incarnation{ID: incID(i, n), Worker: i, Started: time.Now()}
	if t.bootNeedsServer && t.serverDown {
		inc.Ended = inc.Started // the process exited: no server to reach
	}
	t.incs = append(t.incs, inc)
	return nil
}

func (*fakeTarget) RealLive() int { return 0 }

func (t *fakeTarget) Incarnations() []target.Incarnation {
	t.mu.Lock()
	defer t.mu.Unlock()
	return slices.Clone(t.incs)
}

func (t *fakeTarget) Lane(group string) target.Lane {
	if l, ok := t.lanes[group]; ok {
		return l
	}
	return nil
}

func (*fakeTarget) MarkerDir() string                   { return "" }
func (*fakeTarget) StopForChecks(context.Context) error { return nil }
func (*fakeTarget) StoreOptions() backend.Options       { return backend.Options{} }
func (t *fakeTarget) laneCalls(group string) []laneCall { return t.lanes[group].snapshot() }
func (t *fakeTarget) laneOps(group string) []string     { return ops(t.laneCalls(group)) }
func (t *fakeTarget) laneState(group string) string     { return t.lanes[group].state() }
func (t *fakeTarget) count(name string) int             { return len(t.named(name)) }

// firstNamed is the first call called name.
func (t *fakeTarget) firstNamed(name string) (call, bool) {
	cs := t.named(name)
	if len(cs) == 0 {
		return call{}, false
	}
	return cs[0], true
}

var _ target.Faults = (*fakeTarget)(nil)

type laneCall struct {
	at     time.Time
	op     string
	delay  time.Duration
	jitter time.Duration
	bps    int
}

// fakeLane records its calls; its state is the last call's, the way a
// state-replacing proxy behaves.
type fakeLane struct {
	mu    sync.Mutex
	calls []laneCall
}

func (l *fakeLane) add(c laneCall) {
	l.mu.Lock()
	defer l.mu.Unlock()
	c.at = time.Now()
	l.calls = append(l.calls, c)
}

func (l *fakeLane) Cut()       { l.add(laneCall{op: "Cut"}) }
func (l *fakeLane) Blackhole() { l.add(laneCall{op: "Blackhole"}) }
func (l *fakeLane) Heal()      { l.add(laneCall{op: "Heal"}) }
func (l *fakeLane) Shape(delay, jitter time.Duration, bps int) {
	l.add(laneCall{op: "Shape", delay: delay, jitter: jitter, bps: bps})
}

func (l *fakeLane) snapshot() []laneCall {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.calls)
}

// state is "open" before any call, else the last call's op.
func (l *fakeLane) state() string {
	c := l.snapshot()
	if len(c) == 0 || c[len(c)-1].op == "Heal" {
		return "open"
	}
	return c[len(c)-1].op
}

func ops(cs []laneCall) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.op
	}
	return out
}

// fakeFleet records CrashAll and RestartAll in the target's call log.
type fakeFleet struct{ t *fakeTarget }

func (f fakeFleet) CrashAll() { f.t.record("CrashAll", -1) }
func (f fakeFleet) RestartAll(context.Context) error {
	f.t.record("RestartAll", -1)
	if f.t.down() {
		return errors.New("no servers available for connection")
	}
	return nil
}

func quiet() *slog.Logger { return slog.New(slog.DiscardHandler) }

func d(v time.Duration) scenario.Duration { return scenario.Duration{Duration: v} }

func groups(names ...string) []scenario.WorkerGroup {
	out := make([]scenario.WorkerGroup, len(names))
	for i, n := range names {
		out[i] = scenario.WorkerGroup{Name: n}
	}
	return out
}

// near fails unless got is within tolerance of start+want.
func near(t *testing.T, what string, start, got time.Time, want time.Duration) {
	t.Helper()
	off := got.Sub(start)
	if off < want-tolerance || off > want+tolerance {
		t.Errorf("%s at %s, want %s ± %s", what, off, want, tolerance)
	}
}

func newController(t *testing.T, faults []scenario.Fault, gs []scenario.WorkerGroup, ft *fakeTarget) *Controller {
	t.Helper()
	c, err := NewController(faults, gs, ft, fakeFleet{ft}, quiet())
	if err != nil {
		t.Fatalf("NewController: %v", err)
	}
	return c
}

func run(t *testing.T, c *Controller) (time.Time, error) {
	t.Helper()
	start := time.Now()
	return start, c.Run(context.Background(), start)
}

// asPOSIX makes stalls acceptable for the duration of the test.
func asPOSIX(t *testing.T) {
	t.Helper()
	old := goos
	goos = "linux"
	t.Cleanup(func() { goos = old })
}

func TestController_KillServerAndRestart(t *testing.T) {
	ft := newFakeTarget(nil)
	c := newController(t, []scenario.Fault{{
		At: d(100 * time.Millisecond), Kill: &scenario.ProcTarget{Server: true}, RestartAfter: d(200 * time.Millisecond),
	}}, groups("render"), ft)
	start, err := run(t, c)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	kill, ok := ft.firstNamed("KillServer")
	if !ok {
		t.Fatal("KillServer not called")
	}
	restart, ok := ft.firstNamed("RestartServer")
	if !ok {
		t.Fatal("RestartServer not called")
	}
	near(t, "KillServer", start, kill.at, 100*time.Millisecond)
	near(t, "RestartServer", start, restart.at, 300*time.Millisecond)
	recs := c.Records()
	if len(recs) != 1 {
		t.Fatalf("records = %d, want 1", len(recs))
	}
	r := recs[0]
	if r.Index != 0 || r.Kind != scenario.FaultKill || r.Target != "server" || !r.ServerRestart || r.Error != "" {
		t.Errorf("record = %+v", r)
	}
	if r.Healed.IsZero() || r.Healed.Before(r.Start) {
		t.Errorf("record Start %s Healed %s, want healed after start", r.Start, r.Healed)
	}
	ws := c.Windows()
	if len(ws) != 1 || !ws[0].Start.Equal(r.Start) || !ws[0].End.Equal(r.Healed) || len(ws[0].Groups) != 0 {
		t.Errorf("windows = %+v, want one over the whole farm from %s to %s", ws, r.Start, r.Healed)
	}
}

func TestController_KillWorkerNotRestarted(t *testing.T) {
	ft := newFakeTarget(map[string][]int{"render": {0, 1}})
	gs := []scenario.WorkerGroup{{Name: "render", Real: 2}}
	c := newController(t, []scenario.Fault{{Kill: &scenario.ProcTarget{Group: "render", Real: 1}}}, gs, ft)
	if _, err := run(t, c); err != nil {
		t.Fatalf("Run: %v", err)
	}
	kills := ft.named("KillWorker")
	if len(kills) != 1 || kills[0].arg != 0 {
		t.Errorf("KillWorker calls = %+v, want one of worker 0", kills)
	}
	if n := ft.count("RestartWorker"); n != 0 {
		t.Errorf("RestartWorker called %d times, want none", n)
	}
	r := c.Records()
	if len(r) != 1 || !r[0].Healed.IsZero() || r[0].Error != "" || r[0].Target != "group render (real 0)" || r[0].ServerRestart {
		t.Errorf("records = %+v, want one unhealed, error-free kill of group render (real 0)", r)
	}
	if ws := c.Windows(); len(ws) != 0 {
		t.Errorf("windows = %+v, want none for a worker fault", ws)
	}
}

func TestController_KillPicksRunningWorkers(t *testing.T) {
	ft := newFakeTarget(map[string][]int{"render": {0, 1, 2}})
	ft.endWorker(0)
	c := newController(t, []scenario.Fault{{
		Term: &scenario.ProcTarget{Group: "render", Real: 2}, RestartAfter: d(50 * time.Millisecond),
	}}, groups("render"), ft)
	if _, err := run(t, c); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var termed, restarted []int
	for _, c := range ft.named("TermWorker") {
		termed = append(termed, c.arg)
	}
	for _, c := range ft.named("RestartWorker") {
		restarted = append(restarted, c.arg)
	}
	slices.Sort(termed)
	slices.Sort(restarted)
	if !slices.Equal(termed, []int{1, 2}) || !slices.Equal(restarted, []int{1, 2}) {
		t.Errorf("termed %v restarted %v, want [1 2] both (worker 0 was not running)", termed, restarted)
	}
	if r := c.Records(); len(r) != 1 || r[0].Healed.IsZero() || r[0].Target != "group render (real 1,2)" {
		t.Errorf("records = %+v", r)
	}
}

func TestController_PartitionHeals(t *testing.T) {
	ft := newFakeTarget(nil, "render", "rack-a", "rack-b")
	c := newController(t, []scenario.Fault{{
		At: d(50 * time.Millisecond), Duration: d(150 * time.Millisecond),
		Partition: &scenario.NetTarget{Groups: []string{"rack-a", "rack-b"}},
	}}, groups("render", "rack-a", "rack-b"), ft)
	start, err := run(t, c)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, g := range []string{"rack-a", "rack-b"} {
		cs := ft.laneCalls(g)
		if got := ops(cs); !slices.Equal(got, []string{"Cut", "Heal"}) {
			t.Fatalf("lane %s calls = %v, want [Cut Heal]", g, got)
		}
		near(t, g+" Cut", start, cs[0].at, 50*time.Millisecond)
		near(t, g+" Heal", start, cs[1].at, 200*time.Millisecond)
	}
	if got := ft.laneOps("render"); len(got) != 0 {
		t.Errorf("lane render calls = %v, want none", got)
	}
	r := c.Records()
	if len(r) != 1 || r[0].Target != "groups [rack-a rack-b]" || r[0].Healed.IsZero() {
		t.Errorf("records = %+v", r)
	}
	ws := c.Windows()
	if len(ws) != 1 || !slices.Equal(ws[0].Groups, []string{"rack-a", "rack-b"}) {
		t.Errorf("windows = %+v, want one on rack-a and rack-b", ws)
	}
}

// TestController_LatencyOpensNoFaultWindow proves a latency fault, which only
// slows a lane and loses nothing, opens no window, while a partition on the
// same group does: a window relaxes the logs check, which only a fault that
// can lose lines may do.
func TestController_LatencyOpensNoFaultWindow(t *testing.T) {
	ft := newFakeTarget(nil, "rack-a", "rack-b")
	c := newController(t, []scenario.Fault{
		{
			Duration: d(100 * time.Millisecond),
			Latency:  &scenario.LatencySpec{Groups: []string{"rack-a"}, Delay: d(20 * time.Millisecond), BytesPerSec: 1000},
		},
		{
			At: d(50 * time.Millisecond), Duration: d(100 * time.Millisecond),
			Partition: &scenario.NetTarget{Groups: []string{"rack-b"}},
		},
	}, groups("rack-a", "rack-b"), ft)
	if _, err := run(t, c); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if r := c.Records(); len(r) != 2 || r[0].Kind != scenario.FaultLatency || r[0].Healed.IsZero() {
		t.Fatalf("records = %+v, want the latency fault recorded and healed", r)
	}
	ws := c.Windows()
	if len(ws) != 1 || !slices.Equal(ws[0].Groups, []string{"rack-b"}) {
		t.Errorf("windows = %+v, want only the partition's, on rack-b", ws)
	}
}

func TestController_EmptyGroupsMeansEveryLane(t *testing.T) {
	ft := newFakeTarget(nil, "render", "rack-b")
	c := newController(t, []scenario.Fault{{
		Duration: d(30 * time.Millisecond), Blackhole: &scenario.NetTarget{},
	}}, groups("render", "rack-b", "render"), ft)
	if _, err := run(t, c); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, g := range []string{"render", "rack-b"} {
		if got := ft.laneOps(g); !slices.Equal(got, []string{"Blackhole", "Heal"}) {
			t.Errorf("lane %s calls = %v, want [Blackhole Heal]", g, got)
		}
	}
	if r := c.Records(); len(r) != 1 || r[0].Target != "all" || r[0].ServerRestart {
		t.Errorf("records = %+v, want target all", r)
	}
	if ws := c.Windows(); len(ws) != 1 || !slices.Equal(ws[0].Groups, []string{"render", "rack-b"}) {
		t.Errorf("windows = %+v, want one on every group", ws)
	}
}

func TestController_FlapEndsHealed(t *testing.T) {
	ft := newFakeTarget(nil, "rack-b")
	c := newController(t, []scenario.Fault{{
		Duration: d(450 * time.Millisecond),
		Flap:     &scenario.FlapSpec{Groups: []string{"rack-b"}, Period: d(100 * time.Millisecond)},
	}}, groups("rack-b"), ft)
	start, err := run(t, c)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	cs := ft.laneCalls("rack-b")
	got := ops(cs)
	cuts := 0
	for i, op := range got {
		want := "Cut"
		if i%2 == 1 {
			want = "Heal"
		}
		if op != want {
			t.Fatalf("lane calls = %v, want Cut and Heal alternating", got)
		}
		if op == "Cut" {
			cuts++
		}
	}
	if cuts < 4 || got[len(got)-1] != "Heal" {
		t.Errorf("lane calls = %v, want at least 4 cuts and Heal last", got)
	}
	near(t, "last Heal", start, cs[len(cs)-1].at, 450*time.Millisecond)
}

func TestController_PowerCutOrder(t *testing.T) {
	for _, order := range []string{scenario.PowerCutServerFirst, scenario.PowerCutWorkersFirst} {
		t.Run(order, func(t *testing.T) {
			ft := newFakeTarget(map[string][]int{"render": {0}, "rack-b": {1}})
			c := newController(t, []scenario.Fault{{
				PowerCut: &scenario.PowerCut{Order: order, Down: d(100 * time.Millisecond), Gap: d(100 * time.Millisecond)},
			}}, groups("render", "rack-b"), ft)
			start, err := run(t, c)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			for _, name := range []string{"KillServer", "CrashAll"} {
				if cs := ft.named(name); len(cs) != 1 {
					t.Fatalf("%s called %d times, want once", name, len(cs))
				} else {
					near(t, name, start, cs[0].at, 0)
				}
			}
			kills := ft.named("KillWorker")
			if len(kills) != 2 {
				t.Fatalf("KillWorker calls = %+v, want workers 0 and 1", kills)
			}
			serverAt, workersAt := 100*time.Millisecond, 200*time.Millisecond
			if order == scenario.PowerCutWorkersFirst {
				serverAt, workersAt = workersAt, serverAt
			}
			rs, _ := ft.firstNamed("RestartServer")
			near(t, "RestartServer", start, rs.at, serverAt)
			ra, _ := ft.firstNamed("RestartAll")
			near(t, "RestartAll", start, ra.at, workersAt)
			restarts := ft.named("RestartWorker")
			if len(restarts) != 2 {
				t.Fatalf("RestartWorker calls = %+v, want workers 0 and 1", restarts)
			}
			for _, r := range restarts {
				near(t, "RestartWorker", start, r.at, workersAt)
			}
			if (order == scenario.PowerCutServerFirst) != rs.at.Before(ra.at) {
				t.Errorf("RestartServer at %s, RestartAll at %s: wrong order for %s", rs.at.Sub(start), ra.at.Sub(start), order)
			}
			r := c.Records()
			if len(r) != 1 || r[0].Target != "all" || !r[0].ServerRestart || r[0].Healed.IsZero() {
				t.Errorf("records = %+v", r)
			}
			if ws := c.Windows(); len(ws) != 1 || len(ws[0].Groups) != 0 {
				t.Errorf("windows = %+v, want one over the whole farm", ws)
			}
		})
	}
}

// TestController_PowerCutWorkersFirstSupervisesWorkers proves workers that
// fail to boot before the server is back are started again once it is, as
// their supervisor would, and the fault heals without an error.
func TestController_PowerCutWorkersFirstSupervisesWorkers(t *testing.T) {
	old := supervisorEvery
	supervisorEvery = 50 * time.Millisecond
	t.Cleanup(func() { supervisorEvery = old })
	ft := newFakeTarget(map[string][]int{"render": {0, 1}})
	ft.bootNeedsServer = true
	c := newController(t, []scenario.Fault{{
		PowerCut: &scenario.PowerCut{Order: scenario.PowerCutWorkersFirst, Down: d(100 * time.Millisecond), Gap: d(100 * time.Millisecond)},
	}}, groups("render"), ft)
	start, err := run(t, c)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rs, _ := ft.firstNamed("RestartServer")
	near(t, "RestartServer", start, rs.at, 200*time.Millisecond)
	for _, w := range []int{0, 1} {
		var at []time.Duration
		for _, r := range ft.named("RestartWorker") {
			if r.arg == w {
				at = append(at, r.at.Sub(start))
			}
		}
		if len(at) != 2 || at[0] > 200*time.Millisecond-tolerance || at[1] < rs.at.Sub(start) {
			t.Errorf("worker %d started at %v, want once at the boot (100ms) and once after the server (%s)", w, at, rs.at.Sub(start))
		}
	}
	fleet := ft.named("RestartAll")
	if len(fleet) != 2 || fleet[1].at.Before(rs.at) {
		t.Errorf("RestartAll calls = %+v, want one at the boot and one after the server", fleet)
	}
	running := c.running()
	if !running[0] || !running[1] {
		t.Errorf("running = %v, want both workers up", running)
	}
	if r := c.Records(); len(r) != 1 || r[0].Healed.IsZero() || r[0].Error != "" {
		t.Errorf("records = %+v, want one healed fault with no error", r)
	}
}

// TestController_PowerCutSupervisorGivesUp proves the supervisor stops after
// supervisorTries and the fault reports the workers that never started.
func TestController_PowerCutSupervisorGivesUp(t *testing.T) {
	oldEvery, oldTries := supervisorEvery, supervisorTries
	supervisorEvery, supervisorTries = 20*time.Millisecond, 3
	t.Cleanup(func() { supervisorEvery, supervisorTries = oldEvery, oldTries })
	ft := newFakeTarget(map[string][]int{"render": {0}})
	ft.bootNeedsServer = true
	ft.restartServerErr = errors.New("server would not start")
	c := newController(t, []scenario.Fault{{
		PowerCut: &scenario.PowerCut{Order: scenario.PowerCutWorkersFirst, Down: d(50 * time.Millisecond), Gap: d(50 * time.Millisecond)},
	}}, groups("render"), ft)
	if _, err := run(t, c); err == nil || !strings.Contains(err.Error(), "no servers available") {
		t.Fatalf("Run error = %v, want the fleet's last start failure", err)
	}
	if n := len(ft.named("RestartAll")); n != 1+3 {
		t.Errorf("RestartAll called %d times, want the boot plus 3 supervisor tries", n)
	}
}

func TestController_OverlappingFaultsEndHealed(t *testing.T) {
	ft := newFakeTarget(nil, "render", "rack-b")
	c := newController(t, []scenario.Fault{
		{Duration: d(400 * time.Millisecond), Partition: &scenario.NetTarget{Groups: []string{"rack-b"}}},
		{
			At: d(100 * time.Millisecond), Duration: d(100 * time.Millisecond),
			Latency: &scenario.LatencySpec{Groups: []string{"rack-b"}, Delay: d(50 * time.Millisecond)},
		},
		{At: d(150 * time.Millisecond), Kill: &scenario.ProcTarget{Server: true}, RestartAfter: d(100 * time.Millisecond)},
	}, groups("render", "rack-b"), ft)
	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- c.Run(context.Background(), start) }()

	time.Sleep(time.Until(start.Add(300 * time.Millisecond)))
	if s := ft.laneState("rack-b"); s != "Cut" {
		t.Errorf("lane rack-b at 300ms is %s, want Cut: the latency's end healed the partition", s)
	}
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	cs := ft.laneCalls("rack-b")
	if got := ops(cs); !slices.Equal(got, []string{"Cut", "Heal"}) {
		t.Fatalf("lane rack-b calls = %v, want [Cut Heal]", got)
	}
	near(t, "rack-b Heal", start, cs[1].at, 400*time.Millisecond)
	if got := ft.laneOps("render"); len(got) != 0 {
		t.Errorf("lane render calls = %v, want none", got)
	}
	kill, _ := ft.firstNamed("KillServer")
	restart, ok := ft.firstNamed("RestartServer")
	if !ok || restart.at.Before(kill.at) {
		t.Errorf("server killed at %s and restarted at %v, want a restart after the kill", kill.at.Sub(start), restart.at.Sub(start))
	}
	if !kill.at.After(cs[0].at) || !restart.at.Before(cs[1].at) {
		t.Errorf("server kill and restart are not inside the partition")
	}
	for _, r := range c.Records() {
		if r.Healed.IsZero() || r.Error != "" {
			t.Errorf("record %+v: want healed without error", r)
		}
	}
	if ws := c.Windows(); len(ws) != 2 {
		t.Errorf("windows = %+v, want two (the latency opens none)", ws)
	}
}

func TestController_PartitionEndsWhileLatencyActive(t *testing.T) {
	ft := newFakeTarget(nil, "rack-b")
	c := newController(t, []scenario.Fault{
		{
			Duration: d(400 * time.Millisecond),
			Latency: &scenario.LatencySpec{
				Groups: []string{"rack-b"}, Delay: d(50 * time.Millisecond), Jitter: d(10 * time.Millisecond), BytesPerSec: 1000,
			},
		},
		{At: d(100 * time.Millisecond), Duration: d(100 * time.Millisecond), Partition: &scenario.NetTarget{Groups: []string{"rack-b"}}},
	}, groups("rack-b"), ft)
	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- c.Run(context.Background(), start) }()

	time.Sleep(time.Until(start.Add(300 * time.Millisecond)))
	if s := ft.laneState("rack-b"); s != "Shape" {
		t.Errorf("lane rack-b at 300ms is %s, want Shape: the partition's end must fall back to the latency", s)
	}
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	cs := ft.laneCalls("rack-b")
	if got := ops(cs); !slices.Equal(got, []string{"Shape", "Cut", "Heal", "Shape", "Heal"}) {
		t.Fatalf("lane rack-b calls = %v, want [Shape Cut Heal Shape Heal]", got)
	}
	for _, i := range []int{0, 3} {
		if s := cs[i]; s.delay != 50*time.Millisecond || s.jitter != 10*time.Millisecond || s.bps != 1000 {
			t.Errorf("call %d = %+v, want the latency's shaping", i, s)
		}
	}
	near(t, "fallback Shape", start, cs[3].at, 200*time.Millisecond)
	near(t, "last Heal", start, cs[4].at, 400*time.Millisecond)
}

// TestLaneCtl_Precedence drives the lane bookkeeping directly: cut beats
// blackhole beats shape beats open, and the latest shape wins among shapes.
func TestLaneCtl_Precedence(t *testing.T) {
	fl := &fakeLane{}
	l := newLaneCtl(fl)
	shapeA := laneState{level: levelShape, delay: time.Second}
	shapeB := laneState{level: levelShape, delay: 2 * time.Second}
	steps := []struct {
		do      func()
		wantOps []string
		want    string
	}{
		{func() { l.add(1, shapeA) }, []string{"Shape"}, "Shape"},
		{func() { l.add(2, shapeB) }, []string{"Shape"}, "Shape"},
		{func() { l.add(3, laneState{level: levelBlackhole}) }, []string{"Blackhole"}, "Blackhole"},
		{func() { l.add(4, laneState{level: levelCut}) }, []string{"Cut"}, "Cut"},
		{func() { l.add(5, laneState{level: levelBlackhole}) }, nil, "Cut"},
		{func() { l.remove(4) }, []string{"Blackhole"}, "Blackhole"},
		{func() { l.remove(3) }, nil, "Blackhole"},
		{func() { l.remove(5) }, []string{"Heal", "Shape"}, "Shape"},
		{func() { l.remove(2) }, []string{"Shape"}, "Shape"},
		{func() { l.remove(1) }, []string{"Heal"}, "open"},
		{func() { l.remove(1) }, nil, "open"},
	}
	seen := 0
	for i, s := range steps {
		s.do()
		cs := fl.snapshot()
		if got := ops(cs[seen:]); !slices.Equal(got, s.wantOps) {
			t.Fatalf("step %d: calls %v, want %v", i, got, s.wantOps)
		}
		seen = len(cs)
		if got := fl.state(); got != s.want {
			t.Fatalf("step %d: lane %s, want %s", i, got, s.want)
		}
	}
	cs := fl.snapshot()
	if cs[0].delay != time.Second || cs[1].delay != 2*time.Second || cs[6].delay != 2*time.Second || cs[7].delay != time.Second {
		t.Errorf("shapes = %+v, want A, B, B again after the blackholes, then A", cs)
	}
	l.healAll()
	if n := len(fl.snapshot()); n != seen {
		t.Errorf("healAll on an open lane made %d calls, want none", n-seen)
	}
	l.add(6, laneState{level: levelCut})
	if got := fl.state(); got != "open" {
		t.Errorf("a fault added after healAll changed the lane to %s", got)
	}
}

func TestController_StallRejectedOnWindows(t *testing.T) {
	old := goos
	goos = "windows"
	t.Cleanup(func() { goos = old })
	ft := newFakeTarget(map[string][]int{"render": {0}})
	_, err := NewController([]scenario.Fault{
		{Duration: d(time.Second), Kill: &scenario.ProcTarget{Server: true}, RestartAfter: d(time.Second)},
		{Duration: d(time.Second), Stall: &scenario.ProcTarget{Group: "render"}},
	}, groups("render"), ft, nil, quiet())
	const want = "chaos: faults[1]: stall needs SIGSTOP, which Windows does not have"
	if err == nil || err.Error() != want {
		t.Fatalf("NewController error = %v, want %q", err, want)
	}
}

func TestController_RejectsBadFaults(t *testing.T) {
	ft := newFakeTarget(nil, "render")
	cases := []struct {
		name   string
		faults []scenario.Fault
		t      target.Faults
		want   string
	}{
		{"no kind", []scenario.Fault{{}}, ft, "chaos: faults[0]: "},
		{
			"no lane",
			[]scenario.Fault{{Duration: d(time.Second), Partition: &scenario.NetTarget{Groups: []string{"rack-b"}}}},
			ft,
			"chaos: faults[0]: worker group rack-b has no broker lane",
		},
		{
			"no target",
			[]scenario.Fault{{Kill: &scenario.ProcTarget{Server: true}, RestartAfter: d(time.Second)}},
			nil,
			"chaos: ",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewController(tc.faults, groups("render", "rack-b"), tc.t, nil, quiet())
			if err == nil || !strings.HasPrefix(err.Error(), tc.want) {
				t.Fatalf("NewController error = %v, want one starting %q", err, tc.want)
			}
		})
	}
}

func TestController_ErrorIsRecorded(t *testing.T) {
	ft := newFakeTarget(nil, "rack-b")
	ft.restartServerErr = errors.New("port taken")
	c := newController(t, []scenario.Fault{
		{Kill: &scenario.ProcTarget{Server: true}, RestartAfter: d(50 * time.Millisecond)},
		{At: d(20 * time.Millisecond), Duration: d(100 * time.Millisecond), Partition: &scenario.NetTarget{Groups: []string{"rack-b"}}},
	}, groups("rack-b"), ft)
	_, err := run(t, c)
	if err == nil || !strings.Contains(err.Error(), "port taken") || !strings.Contains(err.Error(), "faults[0]") {
		t.Fatalf("Run error = %v, want one naming faults[0] and the restart error", err)
	}
	recs := c.Records()
	if len(recs) != 2 {
		t.Fatalf("records = %+v, want two", recs)
	}
	if !strings.Contains(recs[0].Error, "port taken") || !recs[0].Healed.IsZero() {
		t.Errorf("record 0 = %+v, want the restart error and no heal", recs[0])
	}
	if recs[1].Error != "" || recs[1].Healed.IsZero() {
		t.Errorf("record 1 = %+v, want the partition healed without error", recs[1])
	}
	if got := ft.laneOps("rack-b"); !slices.Equal(got, []string{"Cut", "Heal"}) {
		t.Errorf("lane rack-b calls = %v, want [Cut Heal]", got)
	}
	ws := c.Windows()
	if len(ws) != 2 || ws[0].End.Before(recs[1].Healed) {
		t.Errorf("windows = %+v, want the unhealed server fault's window to run to now", ws)
	}
}

func TestController_HealAllOnCancel(t *testing.T) {
	asPOSIX(t)
	ft := newFakeTarget(map[string][]int{"render": {0, 1}}, "rack-b")
	c := newController(t, []scenario.Fault{
		{Duration: d(10 * time.Second), Partition: &scenario.NetTarget{Groups: []string{"rack-b"}}},
		{Duration: d(10 * time.Second), Stall: &scenario.ProcTarget{Group: "render", Real: 1}},
		{Kill: &scenario.ProcTarget{Server: true}, RestartAfter: d(10 * time.Second)},
		{At: d(10 * time.Second), Kill: &scenario.ProcTarget{Server: true}, RestartAfter: d(time.Second)},
	}, groups("render", "rack-b"), ft)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(150*time.Millisecond, cancel)
	start := time.Now()
	if err := c.Run(ctx, start); err == nil || !strings.Contains(err.Error(), "faults[2]") {
		t.Errorf("Run error = %v, want faults[2]'s interrupted restart", err)
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Errorf("Run took %s after the cancel", el)
	}
	if got := ft.laneOps("rack-b"); !slices.Equal(got, []string{"Cut", "Heal"}) {
		t.Errorf("lane rack-b calls = %v, want [Cut Heal]", got)
	}
	stalls, resumes := ft.named("StallWorker"), ft.named("ResumeWorker")
	if len(stalls) != 1 || len(resumes) != 1 || stalls[0].arg != 0 || resumes[0].arg != 0 {
		t.Errorf("stalls %+v resumes %+v, want worker 0 stalled and resumed", stalls, resumes)
	}
	if n := ft.count("KillServer"); n != 1 {
		t.Errorf("KillServer called %d times, want once (faults[3] never fired)", n)
	}
	if n := ft.count("RestartServer"); n != 0 {
		t.Errorf("RestartServer called %d times after the cancel", n)
	}
	recs := c.Records()
	if len(recs) != 3 {
		t.Fatalf("records = %+v, want the three fired faults", recs)
	}
	if recs[0].Healed.IsZero() || recs[1].Healed.IsZero() {
		t.Errorf("records = %+v, want the partition and the stall healed", recs)
	}
	if !recs[2].Healed.IsZero() || recs[2].Error == "" {
		t.Errorf("record 2 = %+v, want an interrupted, unhealed kill", recs[2])
	}

	// HealAll again, and on a controller that never ran, does nothing more.
	c.HealAll()
	if got := ft.laneOps("rack-b"); len(got) != 2 || ft.count("ResumeWorker") != 1 {
		t.Errorf("a second HealAll made calls: lane %v", got)
	}
	idle := newController(t, nil, groups("rack-b"), newFakeTarget(nil, "rack-b"))
	idle.HealAll()
	idle.HealAll()
	if r := idle.Records(); len(r) != 0 {
		t.Errorf("records of an idle controller = %+v", r)
	}
}

// TestController_HealAllWhileRunning heals mid-run, then lets the run end:
// nothing is left cut or stalled and no fault fires after the heal.
func TestController_HealAllWhileRunning(t *testing.T) {
	asPOSIX(t)
	ft := newFakeTarget(map[string][]int{"render": {0}}, "rack-b")
	c := newController(t, []scenario.Fault{
		{Duration: d(300 * time.Millisecond), Partition: &scenario.NetTarget{Groups: []string{"rack-b"}}},
		{Duration: d(300 * time.Millisecond), Stall: &scenario.ProcTarget{Server: true}},
		{At: d(200 * time.Millisecond), Duration: d(50 * time.Millisecond), Blackhole: &scenario.NetTarget{Groups: []string{"rack-b"}}},
	}, groups("render", "rack-b"), ft)
	time.AfterFunc(100*time.Millisecond, c.HealAll)
	if _, err := run(t, c); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := ft.laneOps("rack-b"); !slices.Equal(got, []string{"Cut", "Heal"}) {
		t.Errorf("lane rack-b calls = %v, want [Cut Heal]", got)
	}
	if ft.count("StallServer") != 1 || ft.count("ResumeServer") != 1 {
		t.Errorf("server stalled %d and resumed %d times, want once each", ft.count("StallServer"), ft.count("ResumeServer"))
	}
	r := c.Records()
	if len(r) != 2 {
		t.Errorf("records = %+v, want the two faults fired before the heal", r)
	}
	for _, rec := range r {
		if rec.ServerRestart {
			t.Errorf("record %+v marks a server restart; a stall keeps the server's process", rec)
		}
	}
}
