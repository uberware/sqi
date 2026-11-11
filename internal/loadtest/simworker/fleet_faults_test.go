// SPDX-License-Identifier: AGPL-3.0-or-later

package simworker

import (
	"context"
	"errors"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/loadtest/client"
	"github.com/uberware/sqi/internal/loadtest/internal/testserver"
	"github.com/uberware/sqi/internal/loadtest/scenario"
	"github.com/uberware/sqi/internal/worker/protocol"
)

func faultGroup(name string, count int) scenario.WorkerGroup {
	return scenario.WorkerGroup{
		Name: name, Count: count, CPU: 2, RAMMb: 4096, OS: "linux",
		Heartbeat: scenario.Duration{Duration: 15 * time.Second},
	}
}

func hangBehavior() Behavior {
	return BehaviorFunc(func(*protocol.AssignMsg, *rand.Rand) TaskPlan {
		return TaskPlan{Duration: 10 * time.Millisecond, Outcome: OutcomeHang}
	})
}

// faultFleet starts a fleet of groups on a fresh in-process server whose tasks
// hang, and stops it at cleanup. It returns the fleet, its ledger and the
// server's client and farm.
func faultFleet(t *testing.T, groups ...scenario.WorkerGroup) (*Fleet, *Ledger, *client.Client, string) {
	t.Helper()
	srv := testserver.Start(t, testserver.Options{})
	c := client.New(srv.HTTPURL())
	farmID, err := c.CreateFarm(t.Context(), "fault-farm")
	if err != nil {
		t.Fatalf("CreateFarm: %v", err)
	}
	l := NewLedger()
	fl := NewFleet(FleetConfig{
		RunID: "r", FarmID: farmID, NATSURL: srv.NATSURL(), IDPrefix: "fault",
		Groups: groups, Seed: 7,
	}, hangBehavior(), l, nil)
	if err := fl.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		// Hung tasks never finish, so give the graceful part little time.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := fl.Stop(ctx); err != nil {
			t.Errorf("Stop: %v", err)
		}
	})
	return fl, l, c, farmID
}

func waitForCond(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", timeout, what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func instanceIDs(fl *Fleet) map[string]string {
	out := map[string]string{}
	for _, m := range fl.members {
		out[m.spec.ID] = m.worker.Load().InstanceID()
	}
	return out
}

func TestFleet_NATSURLForPerGroup(t *testing.T) {
	urls := map[string]string{"a": "nats://lane-a:1", "b": "nats://lane-b:2"}
	fl := NewFleet(FleetConfig{
		RunID: "r", FarmID: "f", NATSURL: "nats://default:3", IDPrefix: "p",
		NATSURLFor: func(g string) string { return urls[g] },
		Groups:     []scenario.WorkerGroup{faultGroup("a", 2), faultGroup("b", 1)},
	}, hangBehavior(), NewLedger(), nil)
	specs := fl.Specs()
	if len(specs) != 3 {
		t.Fatalf("Specs() has %d entries, want 3", len(specs))
	}
	for _, s := range specs {
		if want := urls[s.Group.Name]; s.NATSURL != want {
			t.Errorf("worker %s in group %s has URL %q, want %q", s.ID, s.Group.Name, s.NATSURL, want)
		}
	}

	plain := NewFleet(FleetConfig{
		RunID: "r", FarmID: "f", NATSURL: "nats://default:3", IDPrefix: "p",
		Groups: []scenario.WorkerGroup{faultGroup("a", 1)},
	}, hangBehavior(), NewLedger(), nil)
	if got := plain.Specs()[0].NATSURL; got != "nats://default:3" {
		t.Errorf("with no NATSURLFor the URL is %q, want the fleet's", got)
	}
}

func TestFleet_GroupOf(t *testing.T) {
	fl := NewFleet(FleetConfig{
		RunID: "r", FarmID: "f", NATSURL: "nats://unused", IDPrefix: "p",
		Groups: []scenario.WorkerGroup{faultGroup("render", 2), faultGroup("comp", 1)},
	}, hangBehavior(), NewLedger(), nil)
	for id, want := range map[string]string{"p-render-1": "render", "p-render-2": "render", "p-comp-1": "comp", "p-comp-2": "", "": "", "other": ""} {
		if got := fl.GroupOf(id); got != want {
			t.Errorf("GroupOf(%q) = %q, want %q", id, got, want)
		}
	}
}

// submitJobs submits n one-task jobs.
func submitJobs(t *testing.T, c *client.Client, farmID string, n int) {
	t.Helper()
	queueID, err := c.CreateQueue(t.Context(), farmID, "fault-queue", 0)
	if err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}
	for range n {
		if _, _, err := c.SubmitJob(t.Context(), client.SubmitRequest{FarmID: farmID, QueueID: queueID, Template: []byte(oneTaskTemplate)}); err != nil {
			t.Fatalf("SubmitJob: %v", err)
		}
	}
}

func TestFleet_CrashAllThenRestartAll(t *testing.T) {
	fl, l, c, farmID := faultFleet(t, faultGroup("a", 4), faultGroup("b", 2))
	submitJobs(t, c, farmID, 6)
	waitForCond(t, 30*time.Second, "an attempt in flight", func() bool { return countKindIn(l, KindRunning) > 0 })
	before := instanceIDs(fl)

	fl.CrashAll()
	if got := fl.Live(); got != 0 {
		t.Fatalf("Live() = %d after CrashAll, want 0", got)
	}
	if got := fl.ChurnCounts().Crashes; got != 6 {
		t.Errorf("Crashes = %d, want 6", got)
	}
	leased, crashed := map[string]bool{}, map[string]bool{}
	for _, e := range l.Events() {
		switch e.Kind {
		case KindLeased:
			leased[e.AttemptID] = true
		case KindCrashed:
			crashed[e.AttemptID] = true
		}
	}
	if len(crashed) == 0 {
		t.Error("no crashed event for the attempts in flight")
	}
	for a := range leased {
		if !crashed[a] {
			t.Errorf("attempt %s was leased and never recorded as crashed", a)
		}
	}

	// Crashing again finds nothing running.
	fl.CrashAll()
	if got := fl.ChurnCounts().Crashes; got != 6 {
		t.Errorf("Crashes = %d after a second CrashAll, want still 6", got)
	}

	if err := fl.RestartAll(t.Context()); err != nil {
		t.Fatalf("RestartAll: %v", err)
	}
	if got := fl.Live(); got != 6 {
		t.Fatalf("Live() = %d after RestartAll, want 6", got)
	}
	for id, was := range before {
		if now := instanceIDs(fl)[id]; now == was || now == "" {
			t.Errorf("worker %s has instance ID %q after RestartAll, was %q; want a new one", id, now, was)
		}
	}
	if got := fl.ChurnCounts().Restarts; got != 6 {
		t.Errorf("Restarts = %d, want 6", got)
	}
}

func TestFleet_RestartAllLeavesRunningWorkersAlone(t *testing.T) {
	fl, _, _, _ := faultFleet(t, faultGroup("a", 3))
	before := instanceIDs(fl)
	if err := fl.RestartAll(t.Context()); err != nil {
		t.Fatalf("RestartAll: %v", err)
	}
	after := instanceIDs(fl)
	for id, was := range before {
		if after[id] != was {
			t.Errorf("running worker %s restarted: %q -> %q", id, was, after[id])
		}
	}
	if got := fl.ChurnCounts().Restarts; got != 0 {
		t.Errorf("Restarts = %d, want 0", got)
	}
}

func TestFleet_RestartAllReportsFailures(t *testing.T) {
	fl, _, _, _ := faultFleet(t, faultGroup("a", 3))
	fl.CrashAll()
	// One worker's broker has gone away for good.
	bad := fl.members[1]
	bad.worker.Load().spec.NATSURL = "nats://127.0.0.1:1"

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	err := fl.RestartAll(ctx)
	if err == nil || !strings.Contains(err.Error(), bad.spec.ID) {
		t.Fatalf("RestartAll() = %v, want an error naming %s", err, bad.spec.ID)
	}
	if got := fl.Live(); got != 2 {
		t.Errorf("Live() = %d, want the other 2 restarted", got)
	}
}

func TestFleet_RestartAllHonorsContext(t *testing.T) {
	fl, _, _, _ := faultFleet(t, faultGroup("a", 2))
	fl.CrashAll()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := fl.RestartAll(ctx); err == nil {
		t.Error("RestartAll with a canceled context returned nil")
	} else if !errors.Is(err, context.Canceled) {
		t.Errorf("RestartAll() = %v, want a canceled-context error", err)
	}
}

// CrashAll and RestartAll serialize with churn events: run together, under the
// race detector, the fleet ends with every worker up.
func TestFleet_RestartAllWhileChurning(t *testing.T) {
	g := faultGroup("c", 4)
	g.Churn = scenario.Churn{
		CrashPerHour: 36000, DisconnectPerHour: 18000,
		RestartAfter: scenario.Duration{Duration: 50 * time.Millisecond},
	}
	fl, _, _, _ := faultFleet(t, g)

	churnCtx, cancelChurn := context.WithCancel(t.Context())
	defer cancelChurn()
	churnDone := make(chan struct{})
	go func() {
		defer close(churnDone)
		fl.RunChurn(churnCtx)
	}()

	for range 4 {
		time.Sleep(150 * time.Millisecond)
		fl.CrashAll()
		time.Sleep(50 * time.Millisecond)
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		if err := fl.RestartAll(ctx); err != nil {
			t.Errorf("RestartAll: %v", err)
		}
		cancel()
	}

	cancelChurn()
	select {
	case <-churnDone:
	case <-time.After(45 * time.Second):
		t.Fatal("RunChurn did not return after its context ended")
	}
	if err := fl.RestartAll(t.Context()); err != nil {
		t.Fatalf("final RestartAll: %v", err)
	}
	if got := fl.Live(); got != 4 {
		t.Errorf("Live() = %d after churn stopped and RestartAll, want 4", got)
	}
	cc := fl.ChurnCounts()
	t.Logf("counts: %+v", cc)
	if cc.Failures != 0 {
		t.Errorf("%d churn events failed", cc.Failures)
	}
	if cc.Restarts != cc.Crashes {
		t.Errorf("Restarts = %d, Crashes = %d: every crash should have been restarted", cc.Restarts, cc.Crashes)
	}
}

// Nothing restarts once Stop has begun.
func TestFleet_RestartAllAfterStopDoesNothing(t *testing.T) {
	fl, _, _, _ := faultFleet(t, faultGroup("a", 2))
	fl.CrashAll()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := fl.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := fl.RestartAll(t.Context()); err != nil {
		t.Errorf("RestartAll after Stop = %v, want nil", err)
	}
	if got := fl.Live(); got != 0 {
		t.Errorf("Live() = %d, want 0 after Stop", got)
	}
}
