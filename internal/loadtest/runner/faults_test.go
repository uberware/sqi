// SPDX-License-Identifier: AGPL-3.0-or-later

package runner

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

	"github.com/uberware/sqi/internal/loadtest/chaos"
	"github.com/uberware/sqi/internal/loadtest/client"
	"github.com/uberware/sqi/internal/loadtest/driver"
	"github.com/uberware/sqi/internal/loadtest/internal/sleep"
	"github.com/uberware/sqi/internal/loadtest/report"
	"github.com/uberware/sqi/internal/loadtest/scenario"
	"github.com/uberware/sqi/internal/loadtest/simworker"
	"github.com/uberware/sqi/internal/loadtest/target"
	"github.com/uberware/sqi/internal/loadtest/verify"
)

// plainTarget is a target with no fault injection.
type plainTarget struct{ closed bool }

func (*plainTarget) HTTPURL() string                    { return "http://127.0.0.1:1" }
func (*plainTarget) NATSURL() string                    { return "nats://127.0.0.1:1" }
func (*plainTarget) APIKey() string                     { return "" }
func (*plainTarget) FarmID() string                     { return "farm" }
func (*plainTarget) QueueIDs() map[string]string        { return map[string]string{"render": "q"} }
func (*plainTarget) DataDir() string                    { return "" }
func (*plainTarget) Credentials() simworker.Credentials { return nil }
func (*plainTarget) TLS() simworker.TLSFiles            { return simworker.TLSFiles{} }
func (*plainTarget) NoopCommand() scenario.Command      { return scenario.Command{} }
func (*plainTarget) PoolName(name string) string        { return name }
func (*plainTarget) PostgresDSN() string                { return "" }
func (*plainTarget) StoreLabel() string                 { return "fake" }
func (p *plainTarget) Close(context.Context) error {
	p.closed = true
	return nil
}

// latencyScenario is the smoke scenario, 10 s of 5 jobs without churn, with a
// latency fault on its one group from 2 s to 4 s.
func latencyScenario(t *testing.T) *scenario.Scenario {
	t.Helper()
	sc := smokeScenario(t, "10s")
	sc.Workers[0].Churn = scenario.Churn{}
	sc.Jobs[0].Total = 5
	sc.Faults = []scenario.Fault{{
		At:       scenario.Duration{Duration: 2 * time.Second},
		Duration: scenario.Duration{Duration: 2 * time.Second},
		Latency:  &scenario.LatencySpec{Groups: []string{sc.Workers[0].Name}, Delay: scenario.Duration{Duration: time.Millisecond}},
	}}
	if err := sc.Validate(scenario.TargetManaged); err != nil {
		t.Fatal(err)
	}
	return sc
}

func TestRun_FaultsRejectedWithoutFaultTarget(t *testing.T) {
	tgt := &plainTarget{}
	ctlMade := false
	rec, err := Run(t.Context(), Options{
		Scenario: latencyScenario(t), RunID: "no-faults", OutDir: t.TempDir(), Logger: testLogger(t),
		startTarget: func(context.Context, *run) (target.Target, error) { return tgt, nil },
		newController: func([]scenario.Fault, []scenario.WorkerGroup, target.Faults, chaos.Fleet, *slog.Logger) (faultController, error) {
			ctlMade = true
			return nil, errors.New("unreachable")
		},
	})
	if err == nil || !strings.Contains(err.Error(), "cannot inject") {
		t.Fatalf("Run = %v, %v; want an error saying the target cannot inject faults", rec, err)
	}
	if rec != nil {
		t.Errorf("Run returned a record for a run that could not be set up")
	}
	if ctlMade {
		t.Error("a fault controller was made for a target without faults")
	}
	if !tgt.closed {
		t.Error("the target was not closed")
	}
}

// slowHeal is a fault controller whose Run returns the heal duration after the
// end of submissions.
type slowHeal struct {
	duration, heal time.Duration

	mu       sync.Mutex
	start    time.Time
	returned time.Time
	healAll  int
}

func (s *slowHeal) Run(ctx context.Context, start time.Time) error {
	s.mu.Lock()
	s.start = start
	s.mu.Unlock()
	ok := sleep.Until(ctx, start.Add(s.duration+s.heal))
	s.mu.Lock()
	s.returned = time.Now()
	s.mu.Unlock()
	if !ok {
		return context.Cause(ctx)
	}
	return nil
}

func (s *slowHeal) Records() []report.FaultRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return []report.FaultRecord{{Index: 0, Kind: scenario.FaultLatency, Target: "groups [sim]", Start: s.start.Add(2 * time.Second), Healed: s.returned}}
}

func (s *slowHeal) Windows() []verify.FaultWindow {
	s.mu.Lock()
	defer s.mu.Unlock()
	return []verify.FaultWindow{{Start: s.start.Add(2 * time.Second), End: s.start.Add(4 * time.Second), Groups: []string{"sim"}}}
}

func (s *slowHeal) HealAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.healAll++
}

// TestRun_DrainWaitsForFaultsToHeal runs a fault scenario against a real
// sqi-server with a controller whose faults heal 2 s after submissions end:
// the drain must not start before they have.
func TestRun_DrainWaitsForFaultsToHeal(t *testing.T) {
	if testing.Short() {
		t.Skip("runs real sqi binaries; skipped with -short")
	}
	sc := latencyScenario(t)
	ctl := &slowHeal{duration: sc.Duration.Duration, heal: 2 * time.Second}
	var (
		mu         sync.Mutex
		firstDrain time.Time
		drained    []driver.SubmittedJob
	)
	var gotFaults target.Faults
	rec, err := Run(t.Context(), Options{
		Scenario: sc, RunID: "drain-after-heal", OutDir: t.TempDir(), Seed: 3, Managed: binaries(t), Logger: testLogger(t),
		newController: func(faults []scenario.Fault, _ []scenario.WorkerGroup, tf target.Faults, _ chaos.Fleet, _ *slog.Logger) (faultController, error) {
			if len(faults) != 1 {
				t.Errorf("controller made with %d faults, want 1", len(faults))
			}
			gotFaults = tf
			return ctl, nil
		},
		waitDrained: func(ctx context.Context, v verify.ServerView, farmID string, jobs []driver.SubmittedJob, timeout time.Duration) error {
			mu.Lock()
			if firstDrain.IsZero() {
				firstDrain, drained = time.Now(), slices.Clone(jobs)
			}
			mu.Unlock()
			return verify.WaitDrained(ctx, v, farmID, jobs, timeout)
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if gotFaults == nil || gotFaults.Lane(sc.Workers[0].Name) == nil {
		t.Errorf("the controller's target %v has no lane for group %s", gotFaults, sc.Workers[0].Name)
	}
	ctl.mu.Lock()
	returned, healAll := ctl.returned, ctl.healAll
	ctl.mu.Unlock()
	switch {
	case returned.IsZero():
		t.Fatal("the controller's Run never returned")
	case firstDrain.IsZero():
		t.Fatal("the run never waited for its jobs to drain")
	case firstDrain.Before(returned):
		t.Errorf("the drain started at %s, %s before the faults healed at %s", stamp(firstDrain), returned.Sub(firstDrain), stamp(returned))
	}
	if healAll == 0 {
		t.Error("teardown did not call HealAll")
	}
	if !rec.Complete {
		t.Fatal("the run is incomplete")
	}
	if len(drained) < 5 {
		t.Errorf("WaitDrained was given %d jobs, want the 5 accepted ones at least", len(drained))
	}
	if len(rec.Faults) != 1 || rec.Faults[0].Kind != scenario.FaultLatency {
		t.Errorf("record faults = %+v, want the controller's one record", rec.Faults)
	}
	var names []string
	for _, inv := range rec.Invariants {
		names = append(names, inv.Name)
		if !inv.OK {
			t.Errorf("invariant %s failed: %q", inv.Name, inv.Evidence)
		}
		if inv.Name == verify.NameLogs {
			if _, ok := inv.Counters[verify.CounterLogsLostInFault]; !ok {
				t.Errorf("the logs result carries no %s counter: %v", verify.CounterLogsLostInFault, inv.Counters)
			}
		}
	}
	want := []string{
		verify.NameTerminal, verify.NameOutcomes, verify.NameSingleAttempt, verify.NameLogs,
		verify.NameWorkersConverge, verify.NameCaps, verify.NameSubmissionAtomic,
		verify.NameStoreIntegrity, verify.NameExecutionsRecorded, verify.NameAttemptOrder,
	}
	if !slices.Equal(names, want) {
		t.Errorf("invariants %v, want %v", names, want)
	}
}

func stamp(t time.Time) string { return t.Format("15:04:05.000") }

func TestOrphanJobs(t *testing.T) {
	t0 := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	farm := map[string]client.Job{
		"j1": {ID: "j1", Name: "shot-1", CreatedAt: t0},
		// A submission that timed out after the server had created its job.
		"o1": {ID: "o1", Name: "shot-2", CreatedAt: t0.Add(time.Second)},
		// A second job with the name of a rejected submission, created later:
		// not an orphan; submission-atomic reports it.
		"o1b": {ID: "o1b", Name: "shot-2", CreatedAt: t0.Add(3 * time.Second)},
		// A job of an accepted submission's name but another ID.
		"dup": {ID: "dup", Name: "shot-1", CreatedAt: t0.Add(2 * time.Second)},
	}
	accepted := []driver.SubmittedJob{{ID: "j1", Name: "shot-1", Class: "shot", Status: 201}}
	rejected := []driver.SubmittedJob{
		{Name: "shot-2", Class: "shot", Status: 0},
		{Name: "shot-3", Class: "shot", Status: 503}, // created nothing
	}
	got := orphanJobs(farm, accepted, rejected)
	if len(got) != 1 || got[0].ID != "o1" || got[0].Name != "shot-2" || got[0].Class != "shot" || got[0].Accepted() {
		t.Errorf("orphanJobs = %+v, want shot-2 as o1, still not accepted", got)
	}
	if none := orphanJobs(map[string]client.Job{"j1": farm["j1"]}, accepted, rejected); none == nil || len(none) != 0 {
		t.Errorf("orphanJobs with none = %#v, want an empty, non-nil slice", none)
	}
}

func TestChaosConvergeWithin(t *testing.T) {
	sc := smokeScenario(t, "20s")
	base := convergeWithin(sc)
	grace := serverQuietTime(sc) + convergeMargin
	now := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	cases := []struct {
		name     string
		serverUp time.Time
		faults   []report.FaultRecord
		want     time.Duration
	}{
		{name: "long after the server started", serverUp: now.Add(-time.Hour), want: base},
		{name: "server just started", serverUp: now.Add(-time.Minute), want: grace - time.Minute},
		{
			name: "server restarted by a fault", serverUp: now.Add(-time.Hour),
			faults: []report.FaultRecord{
				{Kind: scenario.FaultKill, Target: "server", ServerRestart: true, Start: now.Add(-40 * time.Second), Healed: now.Add(-30 * time.Second)},
				{Kind: scenario.FaultKill, Target: "group sim (real 0)", Start: now.Add(-5 * time.Second), Healed: now},
			},
			want: grace - 30*time.Second,
		},
		{
			name: "power cut never healed", serverUp: now.Add(-time.Hour),
			faults: []report.FaultRecord{{Kind: scenario.FaultPowerCut, Target: "all", ServerRestart: true, Start: now.Add(-time.Minute)}},
			want:   grace,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := chaosConvergeWithin(sc, tc.serverUp, tc.faults, now); got != max(tc.want, base) {
				t.Errorf("chaosConvergeWithin = %s, want %s", got, max(tc.want, base))
			}
		})
	}
}

// identityFaults is a fault target that knows its real workers and its log.
type identityFaults struct {
	target.Faults

	ids map[int]string
}

func (identityFaults) ServerLog() string { return "server.log" }

func (f identityFaults) RealWorkerID(i int) (string, error) {
	if id, ok := f.ids[i]; ok {
		return id, nil
	}
	return "", errors.New("real worker " + strconv.Itoa(i) + " has not started")
}

// recordsOnly is a fault controller that has fired the records it holds.
type recordsOnly struct{ records []report.FaultRecord }

func (recordsOnly) Run(context.Context, time.Time) error { return nil }
func (r recordsOnly) Records() []report.FaultRecord      { return r.records }
func (recordsOnly) Windows() []verify.FaultWindow        { return nil }
func (recordsOnly) HealAll()                             {}

func TestStoreChecks_PassesIdentityAndDiskInputs(t *testing.T) {
	t0 := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	two := 2
	sc := &scenario.Scenario{
		Workers: []scenario.WorkerGroup{{Name: "render", Real: 2}},
		Faults: []scenario.Fault{
			{Revoke: &scenario.ProcTarget{Group: "render", Real: 1}},
			{DiskFull: &scenario.DiskFull{Volume: scenario.VolumeWorkers, Reason: "no space", MaxFailed: &two}},
			{DiskFull: &scenario.DiskFull{Volume: scenario.VolumeServer}},
			{Latency: &scenario.LatencySpec{}},
		},
	}
	records := []report.FaultRecord{
		{Index: 0, Kind: scenario.FaultRevoke, Workers: []string{"wid-1"}, Start: t0, Healed: t0.Add(time.Second)},
		{Index: 1, Kind: scenario.FaultDiskFull, Start: t0.Add(time.Minute), Healed: t0.Add(2 * time.Minute)},
		{Index: 2, Kind: scenario.FaultDiskFull, Start: t0.Add(3 * time.Minute), Healed: t0.Add(4 * time.Minute)},
		{Index: 3, Kind: scenario.FaultLatency, Start: t0, Healed: t0},
	}
	bound := 4
	sc.Expect.MaxReclaims = &bound
	r := &run{
		sc: sc, tgt: &plainTarget{}, ctl: recordsOnly{records},
		faults: identityFaults{ids: map[int]string{0: "wid-0", 1: "wid-1"}},
	}
	in := r.storeInput()
	if len(in.Revoked) != 1 || in.Revoked[0].ID != "wid-1" || !in.Revoked[0].At.Equal(t0) {
		t.Errorf("Revoked = %+v, want wid-1 at %s", in.Revoked, t0)
	}
	if !slices.Equal(in.RealIDs, []string{"wid-0", "wid-1"}) {
		t.Errorf("RealIDs = %v, want both real workers", in.RealIDs)
	}
	if len(in.DiskFaults) != 1 {
		t.Fatalf("DiskFaults = %+v, want only the workers-volume fault", in.DiskFaults)
	}
	w := in.DiskFaults[0]
	if !w.Start.Equal(t0.Add(time.Minute)) || !w.Healed.Equal(t0.Add(2*time.Minute)) || w.MaxFailed == nil || *w.MaxFailed != 2 ||
		w.Reason == nil || w.Reason.String() != "no space" {
		t.Errorf("DiskFaults[0] = %+v, want the fault's window, reason and bound", w)
	}
	if in.DiskErr != nil || in.SkewedGroups != nil || in.ServerLog != "" {
		t.Errorf("DiskErr = %v, SkewedGroups = %v, ServerLog = %q; want none", in.DiskErr, in.SkewedGroups, in.ServerLog)
	}
	want := []string{
		verify.NameStoreIntegrity, verify.NameExecutionsRecorded, verify.NameAttemptOrder,
		verify.NameRevoked, verify.NameDiskFull, verify.NameReclaims,
	}
	if got := chaos.CheckNames(in); !slices.Equal(got, want) {
		t.Errorf("CheckNames = %v, want %v", got, want)
	}

	t.Run("a real worker that never started leaves disk-full unmeasured", func(t *testing.T) {
		r.faults = identityFaults{ids: map[int]string{0: "wid-0"}}
		in := r.storeInput()
		if in.DiskErr == nil || !strings.Contains(in.DiskErr.Error(), "real worker 1 has not started") {
			t.Errorf("DiskErr = %v, want the worker's error", in.DiskErr)
		}
	})
	t.Run("a bad reason leaves disk-full unmeasured", func(t *testing.T) {
		bad := *sc
		bad.Faults = slices.Clone(sc.Faults)
		bad.Faults[1] = scenario.Fault{DiskFull: &scenario.DiskFull{Volume: scenario.VolumeWorkers, Reason: "("}}
		rr := &run{sc: &bad, tgt: &plainTarget{}, ctl: recordsOnly{records}, faults: identityFaults{}}
		if in := rr.storeInput(); in.DiskErr == nil || !strings.Contains(in.DiskErr.Error(), `reason "("`) {
			t.Errorf("DiskErr = %v, want the reason's compile error", in.DiskErr)
		}
	})
}

func TestSkewedGroups(t *testing.T) {
	skew := scenario.Duration{Duration: time.Hour}
	specs := []simworker.WorkerSpec{
		{ID: "a-0", Group: scenario.WorkerGroup{Name: "rack-a", ClockSkew: skew}},
		{ID: "a-1", Group: scenario.WorkerGroup{Name: "rack-a", ClockSkew: skew}},
		{ID: "b-0", Group: scenario.WorkerGroup{Name: "rack-b"}},
	}
	got := skewedGroups(specs)
	if len(got) != 1 || !slices.Equal(got["rack-a"], []string{"a-0", "a-1"}) {
		t.Errorf("skewedGroups = %v, want rack-a with both workers", got)
	}
	if got := skewedGroups(specs[2:]); got != nil {
		t.Errorf("skewedGroups with no skew = %v, want nil", got)
	}
}
