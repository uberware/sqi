// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	nats "github.com/nats-io/nats.go"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// A wake goes through the first awake worker, by ID, whose network holds the
// target's address, with the target's directed broadcast and the limited
// broadcast; the server sends no packet itself when that relay accepts.
func TestActuator_WakesCandidatesThroughRelayOnSameSubnet(t *testing.T) {
	f := newActFixture(t, nil)
	g := createGroup(t, f.st, wolGroup("lan", nil))
	addNamedMember(t, f.st, g.ID, lanWorker("m1", "192.168.1.21", "aa:bb:cc:dd:ee:01"), false)
	// Relays serve another queue, so they absorb none of the demand.
	other := lanWorker("a-other-subnet", "10.1.0.5", "aa:bb:cc:dd:ee:02")
	other.QueueID = "elsewhere"
	registerOnline(t, f.st, other)
	relay := lanWorker("relay", "192.168.1.50", "aa:bb:cc:dd:ee:03")
	relay.QueueID = "elsewhere"
	registerOnline(t, f.st, relay)
	submitReady(t, f.st, 1, store.Step{})

	f.sweep(t, time.Now().UTC())

	calls := f.bus.sent()
	if len(calls) != 1 {
		t.Fatalf("control requests = %+v, want one wake to the relay", calls)
	}
	c := calls[0]
	if c.workerID != "relay" || c.msg.Command != protocol.ControlWake || c.msg.Version != protocol.ProtocolVersion ||
		c.msg.Type != protocol.TypeControl {
		t.Errorf("request = %+v, want a wake to relay", c)
	}
	if !slices.Equal(c.msg.MACs, []string{"aa:bb:cc:dd:ee:01"}) {
		t.Errorf("MACs = %v, want m1's", c.msg.MACs)
	}
	if want := []string{"192.168.1.255:9", "255.255.255.255:9"}; !slices.Equal(c.msg.Targets, want) {
		t.Errorf("Targets = %v, want %v", c.msg.Targets, want)
	}
	if sends := f.packets.all(); len(sends) != 0 {
		t.Errorf("direct sends = %+v, want none", sends)
	}
	if got := f.worker(t, "m1").PowerState; got != store.PowerWaking {
		t.Errorf("m1 = %s, want waking", got)
	}
	if got := actionsTotal(t, f, "lan", "wake", "ok"); got != 1 {
		t.Errorf("actions_total{wake,ok} = %v, want 1", got)
	}
	if la := lastAction(t, f.s, g.ID); la.Action != "wake" || la.Result != "ok" {
		t.Errorf("LastAction = %+v, want wake ok", la)
	}
}

// When the relay does not accept, the server sends the packets itself, to the
// limited broadcast, the directed broadcast and every configured send_to.
func TestActuator_WakeFallsBackToDirectWhenRelayRefuses(t *testing.T) {
	f := newActFixture(t, nil)
	f.bus.setAnswer(func(string, protocol.ControlMsg) ([]byte, error) {
		return controlReply(false, "malformed control message"), nil
	})
	g := createGroup(t, f.st, wolGroup("lan", nil))
	addNamedMember(t, f.st, g.ID, lanWorker("m1", "192.168.1.21", "aa:bb:cc:dd:ee:01"), false)
	relay := lanWorker("relay", "192.168.1.50", "aa:bb:cc:dd:ee:03")
	relay.QueueID = "elsewhere"
	registerOnline(t, f.st, relay)
	submitReady(t, f.st, 1, store.Step{})

	f.sweep(t, time.Now().UTC())

	if n := len(f.bus.sentCommand(protocol.ControlWake)); n != 1 {
		t.Fatalf("relay requests = %d, want 1", n)
	}
	sends := f.packets.all()
	if len(sends) != 1 {
		t.Fatalf("direct sends = %+v, want one", sends)
	}
	if !slices.Equal(sends[0].macs, []string{"aa:bb:cc:dd:ee:01"}) {
		t.Errorf("MACs = %v, want m1's", sends[0].macs)
	}
	if want := []string{"255.255.255.255:9", "192.168.1.255:9", "10.0.0.255:9"}; !slices.Equal(sends[0].targets, want) {
		t.Errorf("targets = %v, want %v", sends[0].targets, want)
	}
	if got := actionsTotal(t, f, "lan", "wake", "ok"); got != 1 {
		t.Errorf("actions_total{wake,ok} = %v, want 1", got)
	}
}

// A member that never registered an interface with a MAC and an IPv4 address
// cannot be woken: the wake fails visibly and the member stays waking, for
// the boot timeout to settle.
func TestActuator_WakeWithNoRecordedMACFails(t *testing.T) {
	f := newActFixture(t, nil)
	g := createGroup(t, f.st, wolGroup("lan", nil))
	bare := lanWorker("m1", "192.168.1.21", "aa:bb:cc:dd:ee:01")
	bare.Interfaces = []store.NetInterface{{Name: "eth0", MAC: "aa:bb:cc:dd:ee:01", Addrs: []string{"fe80::1/64"}}}
	addNamedMember(t, f.st, g.ID, bare, false)
	submitReady(t, f.st, 1, store.Step{})

	f.sweep(t, time.Now().UTC())

	if c := f.bus.sent(); len(c) != 0 {
		t.Errorf("control requests = %+v, want none", c)
	}
	if p := f.packets.all(); len(p) != 0 {
		t.Errorf("packets = %+v, want none", p)
	}
	if got := f.worker(t, "m1").PowerState; got != store.PowerWaking {
		t.Errorf("m1 = %s, want waking", got)
	}
	if got := actionsTotal(t, f, "lan", "wake", "failed"); got != 1 {
		t.Errorf("actions_total{wake,failed} = %v, want 1", got)
	}
	la := lastAction(t, f.s, g.ID)
	const want = "no wake-on-LAN address recorded; the worker must register once while awake"
	if la.Action != "wake" || la.Result != "failed" || !strings.Contains(la.Message, want) {
		t.Errorf("LastAction = %+v, want wake failed with %q", la, want)
	}
}

// A woken machine that does not register within the boot timeout becomes
// wake_failed, is counted, and is not woken again until its backoff of two
// boot timeouts has passed.
func TestActuator_WakeTimesOutToWakeFailedThenBacksOff(t *testing.T) {
	f := newActFixture(t, nil)
	g := createGroup(t, f.st, wolGroup("lan", nil))
	addNamedMember(t, f.st, g.ID, lanWorker("m1", "192.168.1.21", "aa:bb:cc:dd:ee:01"), false)
	submitReady(t, f.st, 1, store.Step{})
	t0 := time.Now().UTC()

	f.sweep(t, t0)
	if n := len(f.packets.all()); n != 1 {
		t.Fatalf("sends after the first tick = %d, want 1", n)
	}
	failedAt := t0.Add(g.BootTimeout + time.Second)
	f.sweep(t, failedAt)
	if w := f.worker(t, "m1"); w.PowerState != store.PowerWakeFailed {
		t.Fatalf("m1 = %s, want wake_failed", w.PowerState)
	}
	if got := bootTimeouts(t, f, "lan"); got != 1 {
		t.Errorf("boot_timeouts_total = %v, want 1", got)
	}
	if !strings.Contains(f.logs.String(), "level=WARN") || !strings.Contains(f.logs.String(), "worker_id=m1") {
		t.Errorf("the failed wake is not logged at WARN:\n%s", f.logs.String())
	}

	f.sweep(t, failedAt.Add(g.BootTimeout))
	f.sweep(t, failedAt.Add(2*g.BootTimeout-time.Second))
	if n := len(f.packets.all()); n != 1 {
		t.Fatalf("sends within the backoff = %d, want still 1", n)
	}
	f.sweep(t, failedAt.Add(2*g.BootTimeout+time.Second))
	if n := len(f.packets.all()); n != 2 {
		t.Errorf("sends after the backoff = %d, want 2", n)
	}
	if got := f.worker(t, "m1").PowerState; got != store.PowerWaking {
		t.Errorf("m1 after the backoff = %s, want waking", got)
	}
}

// The MACs are every 6-byte MAC on an interface with an IPv4 address, and the
// target is the interface whose address is the worker's registered IP, or the
// first IPv4 address recorded.
func TestWakeOnLAN_AddressesAndTarget(t *testing.T) {
	tests := []struct {
		name       string
		w          store.Worker
		wantMACs   []string
		wantTarget string
	}{
		{
			name: "registered IP picks its interface",
			w: store.Worker{IPAddress: "192.168.2.9", Interfaces: []store.NetInterface{
				{Name: "eth0", MAC: "aa:bb:cc:dd:ee:01", Addrs: []string{"10.0.0.4/8"}},
				{Name: "eth1", MAC: "aa:bb:cc:dd:ee:02", Addrs: []string{"fe80::2/64", "192.168.2.9/24"}},
			}},
			wantMACs:   []string{"aa:bb:cc:dd:ee:01", "aa:bb:cc:dd:ee:02"},
			wantTarget: "192.168.2.9/24",
		},
		{
			name: "no match takes the first IPv4",
			w: store.Worker{IPAddress: "172.16.0.1", Interfaces: []store.NetInterface{
				{Name: "eth0", MAC: "aa:bb:cc:dd:ee:01", Addrs: []string{"fe80::1/64"}},
				{Name: "eth1", MAC: "aa:bb:cc:dd:ee:02", Addrs: []string{"10.0.0.4/8"}},
			}},
			wantMACs:   []string{"aa:bb:cc:dd:ee:02"},
			wantTarget: "10.0.0.4/8",
		},
		{
			name: "long and malformed MACs are skipped",
			w: store.Worker{Interfaces: []store.NetInterface{
				{Name: "ib0", MAC: "00:00:00:00:fe:80:00:00:00:00:00:00:02:00:5e:10:00:00:00:01", Addrs: []string{"10.1.0.1/16"}},
				{Name: "eth0", MAC: "not-a-mac", Addrs: []string{"10.2.0.1/16"}},
				{Name: "eth1", MAC: "aa:bb:cc:dd:ee:03", Addrs: []string{"10.3.0.1/16"}},
			}},
			wantMACs:   []string{"aa:bb:cc:dd:ee:03"},
			wantTarget: "10.1.0.1/16",
		},
		{
			name: "duplicate MACs once",
			w: store.Worker{Interfaces: []store.NetInterface{
				{Name: "eth0", MAC: "AA:BB:CC:DD:EE:01", Addrs: []string{"10.0.0.4/8"}},
				{Name: "eth0.5", MAC: "aa:bb:cc:dd:ee:01", Addrs: []string{"10.5.0.4/16"}},
			}},
			wantMACs:   []string{"aa:bb:cc:dd:ee:01"},
			wantTarget: "10.0.0.4/8",
		},
		{
			name: "IPv6 only",
			w: store.Worker{Interfaces: []store.NetInterface{
				{Name: "eth0", MAC: "aa:bb:cc:dd:ee:01", Addrs: []string{"fe80::1/64"}},
			}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			macs, target, ok := wakeAddresses(tc.w)
			var got []string
			for _, m := range macs {
				got = append(got, m.String())
			}
			if !slices.Equal(got, tc.wantMACs) {
				t.Errorf("MACs = %v, want %v", got, tc.wantMACs)
			}
			if tc.wantTarget == "" {
				if ok {
					t.Errorf("target = %v, want none", target)
				}
				return
			}
			if !ok || target.String() != tc.wantTarget {
				t.Errorf("target = %v (%v), want %s", target, ok, tc.wantTarget)
			}
		})
	}
}

// A relay is an online, enabled, awake worker of the target's farm, other
// than the target, with an interface whose network holds the target's
// address; the lowest ID wins.
func TestWakeOnLAN_RelayChoice(t *testing.T) {
	target := lanWorker("target", "192.168.1.21", "aa:bb:cc:dd:ee:01")
	onSubnet := func(id string, mut func(*store.Worker)) store.Worker {
		w := lanWorker(id, "192.168.1.60", "aa:bb:cc:dd:ee:09")
		w.Status = store.WorkerStatusOnline
		w.PowerState = store.PowerAwake
		if mut != nil {
			mut(&w)
		}
		return w
	}
	tests := []struct {
		name   string
		relays []store.Worker
		want   string
	}{
		{"lowest ID on the subnet", []store.Worker{onSubnet("r2", nil), onSubnet("r1", nil)}, "r1"},
		{"self excluded", []store.Worker{onSubnet("target", nil)}, ""},
		{"offline excluded", []store.Worker{onSubnet("r1", func(w *store.Worker) { w.Status = store.WorkerStatusOffline })}, ""},
		{"disabled excluded", []store.Worker{onSubnet("r1", func(w *store.Worker) { w.Disabled = true })}, ""},
		{"retiring excluded", []store.Worker{onSubnet("r1", func(w *store.Worker) { w.PowerState = store.PowerRetiring })}, ""},
		{"other farm excluded", []store.Worker{onSubnet("r1", func(w *store.Worker) { w.FarmID = "farm-2" })}, ""},
		{"other subnet excluded", []store.Worker{onSubnet("r1", func(w *store.Worker) {
			w.Interfaces[0].Addrs = []string{"192.168.2.60/24"}
		})}, ""},
		{"wider network holds the target", []store.Worker{onSubnet("r1", func(w *store.Worker) {
			w.Interfaces[0].Addrs = []string{"192.168.0.7/16"}
		})}, "r1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, addr, ok := wakeAddresses(target)
			if !ok {
				t.Fatal("target has no address")
			}
			got, found := pickRelay(target, addr.Addr(), tc.relays)
			if tc.want == "" {
				if found {
					t.Errorf("relay = %s, want none", got.ID)
				}
				return
			}
			if !found || got.ID != tc.want {
				t.Errorf("relay = %q (%v), want %s", got.ID, found, tc.want)
			}
		})
	}
}

// A retire reply decides the result: accepted is ok, as is a worker already
// carrying out the same command; a refusal names the worker's reason; no
// reply in time is a timeout; anything else fails.
func TestWakeOnLAN_RetireReplies(t *testing.T) {
	tests := []struct {
		name        string
		reply       []byte
		err         error
		wantResult  string
		wantMessage string
	}{
		{"accepted", controlReply(true, ""), nil, resultOK, "sleep accepted"},
		{"already retiring", controlReply(false, "already retiring"), nil, resultOK, "sleep already under way"},
		{"refused", controlReply(false, "busy"), nil, resultRefused, "worker refused: busy"},
		{"deadline", nil, context.DeadlineExceeded, resultTimeout, "no reply to the sleep request within 10s"},
		{"nats timeout", nil, nats.ErrTimeout, resultTimeout, "no reply to the sleep request within 10s"},
		{"no responders", nil, nats.ErrNoResponders, resultFailed, "sleep request failed"},
		{"garbled reply", []byte("{"), nil, resultFailed, "unreadable reply"},
		{"other error", nil, errors.New("connection closed"), resultFailed, "connection closed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := &wolProvider{request: func(context.Context, string, []byte) ([]byte, error) { return tc.reply, tc.err }}
			g := wolGroup("lan", nil)
			got := p.retire(t.Context(), g, []store.Worker{{ID: "m1"}})
			r, ok := got["m1"]
			if !ok || len(got) != 1 {
				t.Fatalf("results = %v, want one for m1", got)
			}
			if r.Result != tc.wantResult || !strings.Contains(r.Message, tc.wantMessage) {
				t.Errorf("result = %+v, want %s containing %q", r, tc.wantResult, tc.wantMessage)
			}
		})
	}
}
