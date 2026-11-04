// SPDX-License-Identifier: AGPL-3.0-or-later

package driver_test

import (
	"context"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/loadtest/client"
	"github.com/uberware/sqi/internal/loadtest/driver"
	"github.com/uberware/sqi/internal/loadtest/internal/testserver"
	"github.com/uberware/sqi/internal/loadtest/scenario"
	"github.com/uberware/sqi/internal/loadtest/simworker"
)

// realTarget points the driver at an in-process sqi-server.
type realTarget struct {
	url, farm string
	queues    map[string]string
}

func (r realTarget) HTTPURL() string                  { return r.url }
func (realTarget) NATSURL() string                    { return "" }
func (realTarget) APIKey() string                     { return "" }
func (r realTarget) FarmID() string                   { return r.farm }
func (r realTarget) QueueIDs() map[string]string      { return r.queues }
func (realTarget) DataDir() string                    { return "" }
func (realTarget) Credentials() simworker.Credentials { return nil }
func (realTarget) TLS() simworker.TLSFiles            { return simworker.TLSFiles{} }
func (realTarget) NoopCommand() scenario.Command      { return scenario.Command{Path: "/bin/true"} }
func (realTarget) Close(context.Context) error        { return nil }

// TestDriver_AgainstARealServer submits the generated templates to a real
// sqi-server, watches the farm over the real WebSocket protocol, polls it over
// REST and cancels some jobs.
func TestDriver_AgainstARealServer(t *testing.T) {
	srv := testserver.Start(t, testserver.Options{})
	c := client.New(srv.HTTPURL())
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	farm, err := c.CreateFarm(ctx, "driver-e2e")
	if err != nil {
		t.Fatalf("CreateFarm: %v", err)
	}
	queue, err := c.CreateQueue(ctx, farm, "q", 0)
	if err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}
	tgt := realTarget{url: srv.HTTPURL(), farm: farm, queues: map[string]string{"q": queue}}

	jc := scenario.JobClass{Name: "shot", Weight: 1, Total: 6, Queue: "q", Steps: 2, TasksPerStep: 4, Chunk: 2}
	sc := &scenario.Scenario{JobDeps: 0.5, Jobs: []scenario.JobClass{jc}}
	ledger, m := driver.NewJobLedger(), driver.NewMetrics(nil)

	ui := driver.NewUIClients(srv.HTTPURL(), "", scenario.ClientsSpec{
		WebSocket:   scenario.WSSpec{Count: 2},
		RESTPollers: scenario.PollSpec{Count: 1, Interval: scenario.Duration{Duration: 50 * time.Millisecond}},
	}, ledger, m, driver.WithUIFarm(farm))
	uiCtx, stopUI := context.WithCancel(ctx)
	uiDone := make(chan error, 1)
	go func() { uiDone <- ui.Run(uiCtx) }()

	if err := driver.NewSubmitter(c, tgt, sc, ledger, m, nil).Run(ctx); err != nil {
		t.Fatalf("Submitter.Run: %v", err)
	}
	if got := m.StatusCounts(); got[201] != 6 || len(got) != 1 {
		t.Fatalf("ByStatus = %v, want six 201s", got)
	}
	if n := ledger.AcceptedCount(); n != 6 {
		t.Fatalf("accepted %d, want 6", n)
	}

	acts := driver.NewActions(c, tgt, []scenario.Action{{CancelJobs: &scenario.Fraction{Fraction: 0.5}}}, ledger, nil)
	if err := acts.Run(ctx, time.Now()); err != nil {
		t.Fatalf("Actions.Run: %v", err)
	}
	recs := acts.Records()
	if len(recs) != 1 || recs[0].Targeted != 3 || recs[0].Succeeded != 3 {
		t.Fatalf("action records = %+v, want 3 cancels that succeeded", recs)
	}

	// The server pushes nothing when a job is created or canceled before any
	// worker touches it; deleting a job announces its removal on the jobs
	// subject, so delete until a client has seen one.
	for _, j := range ledger.Accepted() {
		if m.WSEvents.Load() > 0 {
			break
		}
		if err := c.DeleteJob(ctx, j.ID); err != nil {
			t.Fatalf("DeleteJob: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	deadline := time.Now().Add(10 * time.Second)
	for m.WSEvents.Load() == 0 || m.REST[driver.EndpointJobsList].Count() == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("no traffic observed: ws events %d, jobs-list polls %d, ws errors %d, rest errors %d",
				m.WSEvents.Load(), m.REST[driver.EndpointJobsList].Count(), m.WSErrors.Load(), m.RESTErrors.Load())
		}
		time.Sleep(20 * time.Millisecond)
	}
	stopUI()
	if err := <-uiDone; err != nil {
		t.Fatalf("UIClients.Run: %v", err)
	}
	if m.WSLag.Count() == 0 {
		t.Error("no WebSocket lag recorded from the real server's pushes")
	}
	// The pollers may have asked for a job this test deleted, so REST errors
	// are expected; WebSocket ones are not.
	if m.WSErrors.Load() != 0 {
		t.Errorf("ws errors %d, want none", m.WSErrors.Load())
	}
}
