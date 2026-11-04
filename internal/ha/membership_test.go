// SPDX-License-Identifier: AGPL-3.0-or-later

package ha_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/ha"
	"github.com/uberware/sqi/internal/store"
)

// member reads id's membership row, failing the test when it is missing.
func (h *harness) member(t *testing.T, id string) store.ServerMember {
	t.Helper()
	members, _, err := h.db.ListServerMembers(context.Background())
	if err != nil {
		t.Fatalf("ListServerMembers: %v", err)
	}
	for _, m := range members {
		if m.InstanceID == id {
			return m
		}
	}
	t.Fatalf("no member row for %q", id)
	return store.ServerMember{}
}

func TestMembership_UpsertsEveryThirdTTL(t *testing.T) {
	h := newHarness(t)
	bus := &broker{up: true, since: h.t0}
	a := h.elector("a", withBroker(bus))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() {
		a.RunMembership(ctx)
		close(done)
	}()

	// The first upsert is immediate, and records who this server is.
	h.clk.waitTimers(t, 1)
	m := h.member(t, "a")
	startUS := h.t0.UnixMicro()
	if m.LastSeenUS != startUS {
		t.Errorf("first LastSeenUS = %d, want %d (at once)", m.LastSeenUS, startUS)
	}
	if m.BrokerOKUS == nil || *m.BrokerOKUS != startUS {
		t.Errorf("BrokerOKUS = %v, want %d while the broker is up", m.BrokerOKUS, startUS)
	}
	if m.Name != "name-a" || m.Host != "host-a" || m.PID != 42 || m.Version != "v0.0.0-test" {
		t.Errorf("member row = %+v, want the configured identity", m)
	}

	// Nothing more until a third of the lease has passed.
	h.clk.Advance(testTTL/3 - time.Millisecond)
	h.clk.waitTimers(t, 1)
	if got := h.member(t, "a").LastSeenUS; got != startUS {
		t.Errorf("LastSeenUS before ttl/3 = %d, want unchanged %d", got, startUS)
	}

	// The broker drops: the next upsert refreshes LastSeenUS and keeps the
	// time the broker was last seen.
	bus.set(false, h.clk.Now())
	h.clk.Advance(time.Millisecond)
	h.clk.waitTimers(t, 1)
	m = h.member(t, "a")
	if want := h.t0.Add(testTTL / 3).UnixMicro(); m.LastSeenUS != want {
		t.Errorf("LastSeenUS at ttl/3 = %d, want %d", m.LastSeenUS, want)
	}
	if m.BrokerOKUS == nil || *m.BrokerOKUS != startUS {
		t.Errorf("BrokerOKUS with the broker down = %v, want the earlier %d", m.BrokerOKUS, startUS)
	}

	// The broker is back: both stamps move together again.
	bus.set(true, h.clk.Now())
	h.clk.Advance(testTTL / 3)
	h.clk.waitTimers(t, 1)
	m = h.member(t, "a")
	want := h.t0.Add(2 * testTTL / 3).UnixMicro()
	if m.LastSeenUS != want || m.BrokerOKUS == nil || *m.BrokerOKUS != want {
		t.Errorf("stamps at 2ttl/3 = %d / %v, want both %d", m.LastSeenUS, m.BrokerOKUS, want)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(settleTimeout):
		t.Fatal("RunMembership did not return after cancel")
	}
}

func TestMembership_RunsWhatEverTheRole(t *testing.T) {
	h := newHarness(t)
	// Another server leads; this one is a standby with no broker.
	if _, ok, err := h.db.AcquireLeadership(context.Background(), "other", testTTL); err != nil || !ok {
		t.Fatalf("seed lease: ok %v, err %v", ok, err)
	}
	bus := &broker{up: false, since: h.t0}
	a := h.elector("a", withBroker(bus))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go a.RunMembership(ctx)
	h.clk.waitTimers(t, 1)
	m := h.member(t, "a")
	if m.BrokerOKUS != nil {
		t.Errorf("BrokerOKUS = %v for a member that never reached the broker, want nil", *m.BrokerOKUS)
	}
	if m.LastSeenUS != h.t0.UnixMicro() {
		t.Errorf("LastSeenUS = %d, want %d", m.LastSeenUS, h.t0.UnixMicro())
	}
}

func TestMembership_SurvivesUpsertErrors(t *testing.T) {
	h := newHarness(t)
	var mu sync.Mutex
	calls := 0
	h.st.upsert = func(ctx context.Context, m store.ServerMember) error {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			return errors.New("connection refused")
		}
		return h.db.UpsertServerMember(ctx, m)
	}
	a := h.elector("a")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go a.RunMembership(ctx)

	h.clk.waitTimers(t, 1)
	members, _, err := h.db.ListServerMembers(context.Background())
	if err != nil || len(members) != 0 {
		t.Fatalf("members after a failed upsert = %v, %v; want none", members, err)
	}
	h.clk.Advance(testTTL / 3)
	h.clk.waitTimers(t, 1)
	if got, want := h.member(t, "a").LastSeenUS, h.t0.Add(testTTL/3).UnixMicro(); got != want {
		t.Errorf("LastSeenUS after the retry = %d, want %d", got, want)
	}
}

func TestStandbyHasBroker(t *testing.T) {
	const nowUS = int64(1_000_000_000)
	ttlUS := testTTL.Microseconds()
	stamp := func(us int64) *int64 { return &us }
	for _, tc := range []struct {
		name    string
		members []store.ServerMember
		want    bool
	}{
		{"no members", nil, false},
		{"only self, fresh", []store.ServerMember{{InstanceID: "self", BrokerOKUS: stamp(nowUS)}}, false},
		{"standby never reached a broker", []store.ServerMember{{InstanceID: "s1"}}, false},
		{"standby stamped exactly ttl ago", []store.ServerMember{{InstanceID: "s1", BrokerOKUS: stamp(nowUS - ttlUS)}}, true},
		{"standby stamped just over ttl ago", []store.ServerMember{{InstanceID: "s1", BrokerOKUS: stamp(nowUS - ttlUS - 1)}}, false},
		{"one fresh standby among stale ones", []store.ServerMember{
			{InstanceID: "s1", BrokerOKUS: stamp(nowUS - 2*ttlUS)},
			{InstanceID: "self", BrokerOKUS: stamp(nowUS)},
			{InstanceID: "s2", BrokerOKUS: stamp(nowUS - 1)},
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ha.StandbyHasBroker(tc.members, "self", nowUS, testTTL); got != tc.want {
				t.Errorf("StandbyHasBroker = %v, want %v", got, tc.want)
			}
		})
	}
}
