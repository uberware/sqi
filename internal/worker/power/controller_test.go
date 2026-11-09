// SPDX-License-Identifier: AGPL-3.0-or-later

package power

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	nats "github.com/nats-io/nats.go"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// fakeClock is a clock that moves by step on every reading.
type fakeClock struct {
	mu   sync.Mutex
	t    time.Time
	step time.Duration
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.t
	c.t = c.t.Add(c.step)
	return t
}

func (c *fakeClock) peek() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// harness is a controller wired to fakes that record what it did, in order.
type harness struct {
	t     *testing.T
	c     *controller
	clock *fakeClock

	mu      sync.Mutex
	events  []string
	replies []protocol.ControlReplyMsg

	busy      atomic.Int32
	runErr    error
	runGate   chan struct{} // when set, Run blocks until it is closed
	regErr    error
	runs      [][]string
	wakeMACs  []net.HardwareAddr
	wakeTo    []string
	regTimes  []time.Time
	resumeNow chan struct{} // closed by a test to fire the fake resume watcher
}

func (h *harness) add(e string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, e)
}

func (h *harness) snapshot() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.events)
}

func (h *harness) lastReply() protocol.ControlReplyMsg {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.replies) == 0 {
		h.t.Fatal("no reply was sent")
	}
	return h.replies[len(h.replies)-1]
}

func newHarness(t *testing.T, cfg Config) *harness {
	t.Helper()
	h := &harness{
		t:         t,
		clock:     &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
		resumeNow: make(chan struct{}),
	}
	d := Deps{
		WorkerID: "w1",
		Busy:     func() int { return int(h.busy.Load()) },
		Deregister: func(reason string, kind protocol.DeregisterKind) {
			h.add(fmt.Sprintf("deregister:%s:%s", reason, kind))
		},
		Reregister: func(context.Context) error {
			h.mu.Lock()
			h.regTimes = append(h.regTimes, h.clock.peek())
			h.mu.Unlock()
			h.add("reregister")
			return h.regErr
		},
		Run: func(_ context.Context, argv []string) error {
			h.add("run:" + strings.Join(argv, " "))
			h.mu.Lock()
			h.runs = append(h.runs, argv)
			gate, err := h.runGate, h.runErr
			h.mu.Unlock()
			if gate != nil {
				<-gate
			}
			return err
		},
		SendWake: func(_ context.Context, macs []net.HardwareAddr, targets []string) error {
			h.mu.Lock()
			h.wakeMACs, h.wakeTo = macs, targets
			h.mu.Unlock()
			h.add("wake")
			return nil
		},
		Now:    h.clock.Now,
		Logger: slog.New(slog.DiscardHandler),
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h.c = newController(ctx, cfg, d)
	h.c.defaults = func() ([]string, []string) { return []string{"default-sleep"}, []string{"default-off"} }
	h.c.pollEvery = time.Millisecond
	// By default the resume watcher fires when a test closes resumeNow.
	h.c.watchResume = func(ctx context.Context, onResume func()) {
		select {
		case <-h.resumeNow:
			onResume()
		case <-ctx.Done():
		}
	}
	return h
}

// send delivers one control message and waits for the work it starts to end.
func (h *harness) send(raw []byte) {
	h.t.Helper()
	h.c.handle(raw, func(r protocol.ControlReplyMsg) error {
		h.mu.Lock()
		h.replies = append(h.replies, r)
		h.mu.Unlock()
		h.add(fmt.Sprintf("reply:accepted=%t", r.Accepted))
		return nil
	})
}

func (h *harness) sendCmd(cmd protocol.ControlCommand) {
	h.t.Helper()
	h.send(controlJSON(h.t, protocol.ControlMsg{
		Version: protocol.ProtocolVersion, Type: protocol.TypeControl, Command: cmd,
	}))
}

func (h *harness) wait() {
	h.t.Helper()
	done := make(chan struct{})
	go func() { h.c.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		h.t.Fatalf("controller work did not finish; events: %v", h.snapshot())
	}
}

// waitFor polls until the harness has recorded event.
func (h *harness) waitFor(event string) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if slices.Contains(h.snapshot(), event) || slices.ContainsFunc(h.snapshot(), func(e string) bool { return strings.HasPrefix(e, event) }) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	h.t.Fatalf("event %q never happened; events: %v", event, h.snapshot())
}

func controlJSON(t *testing.T, m protocol.ControlMsg) []byte {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func optedIn() Config {
	return Config{AllowRemoteSleep: true, SleepCommand: []string{"my-sleep", "-now"}, ShutdownCommand: []string{"my-off"}}
}

func TestController_SleepNotOptedInRefused(t *testing.T) {
	for _, cmd := range []protocol.ControlCommand{protocol.ControlSleep, protocol.ControlShutdown} {
		t.Run(string(cmd), func(t *testing.T) {
			h := newHarness(t, Config{})
			h.sendCmd(cmd)
			h.wait()
			r := h.lastReply()
			if r.Accepted || !strings.Contains(r.Reason, "worker.power.allow_remote_sleep is false") {
				t.Fatalf("reply = %+v, want a refusal naming worker.power.allow_remote_sleep", r)
			}
			if got := h.snapshot(); !slices.Equal(got, []string{"reply:accepted=false"}) {
				t.Errorf("events = %v, want only the refusal: nothing may run or deregister", got)
			}
		})
	}
}

// A busy worker refuses even when the operator opted in: suspending the
// machine would kill the task.
func TestController_BusyWorkerRefusesSleep(t *testing.T) {
	for _, cmd := range []protocol.ControlCommand{protocol.ControlSleep, protocol.ControlShutdown} {
		t.Run(string(cmd), func(t *testing.T) {
			h := newHarness(t, optedIn())
			h.busy.Store(1)
			h.sendCmd(cmd)
			h.wait()
			r := h.lastReply()
			if r.Accepted || r.Reason != "busy" {
				t.Fatalf("reply = %+v, want a refusal with reason busy", r)
			}
			if got := h.snapshot(); !slices.Equal(got, []string{"reply:accepted=false"}) {
				t.Errorf("events = %v, want only the refusal", got)
			}
		})
	}
}

// A task that arrives between the reply and the machine going down must keep
// the machine up.
func TestController_BusyAfterAcceptAbortsBeforeRun(t *testing.T) {
	h := newHarness(t, optedIn())
	var calls atomic.Int32
	h.c.d.Busy = func() int {
		if calls.Add(1) == 1 {
			return 0
		}
		return 1
	}
	h.sendCmd(protocol.ControlSleep)
	h.wait()
	want := []string{
		"reply:accepted=true",
		"deregister:capacity group retirement: sleep:retired",
		"reregister",
	}
	if got := h.snapshot(); !slices.Equal(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

func TestController_SleepDeregistersRetiredThenRuns(t *testing.T) {
	h := newHarness(t, optedIn())
	h.sendCmd(protocol.ControlSleep)
	h.waitFor("run:")
	close(h.resumeNow)
	h.wait()
	want := []string{
		"reply:accepted=true",
		"deregister:capacity group retirement: sleep:retired",
		"run:my-sleep -now",
		"reregister",
	}
	if got := h.snapshot(); !slices.Equal(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

func TestController_ShutdownRunsAndDoesNothingAfter(t *testing.T) {
	h := newHarness(t, optedIn())
	h.sendCmd(protocol.ControlShutdown)
	h.wait()
	want := []string{
		"reply:accepted=true",
		"deregister:capacity group retirement: shutdown:retired",
		"run:my-off",
	}
	if got := h.snapshot(); !slices.Equal(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

func TestController_DefaultCommandUsedWhenNoneConfigured(t *testing.T) {
	h := newHarness(t, Config{AllowRemoteSleep: true})
	h.sendCmd(protocol.ControlShutdown)
	h.wait()
	if got := h.snapshot(); !slices.Contains(got, "run:default-off") {
		t.Fatalf("events = %v, want the per-OS default command", got)
	}
}

func TestController_NoDefaultCommandRefused(t *testing.T) {
	h := newHarness(t, Config{AllowRemoteSleep: true})
	h.c.defaults = func() ([]string, []string) { return nil, nil }
	h.sendCmd(protocol.ControlSleep)
	h.wait()
	r := h.lastReply()
	want := "no default sleep command on " + runtime.GOOS
	if r.Accepted || r.Reason != want {
		t.Fatalf("reply = %+v, want refusal %q", r, want)
	}
	if got := h.snapshot(); len(got) != 1 {
		t.Errorf("events = %v, want only the refusal", got)
	}
}

// Review focus 3: a command that fails to suspend must not leave the worker
// deregistered. It re-registers at once, without waiting for any resume.
func TestController_FailedSleepReregistersAtOnce(t *testing.T) {
	for _, cmd := range []protocol.ControlCommand{protocol.ControlSleep, protocol.ControlShutdown} {
		t.Run(string(cmd), func(t *testing.T) {
			h := newHarness(t, optedIn())
			h.runErr = errors.New("exit status 1")
			// A frozen clock: a controller that entered the 60 s wait would hang.
			h.clock.step = 0
			h.sendCmd(cmd)
			h.wait()
			got := h.snapshot()
			if len(got) < 2 || got[len(got)-1] != "reregister" || strings.Count(strings.Join(got, "|"), "reregister") != 1 {
				t.Fatalf("events = %v, want exactly one re-registration after the failed command", got)
			}
		})
	}
}

// A start failure is a failure like a non-zero exit.
func TestController_UnrunnableCommandReregisters(t *testing.T) {
	h := newHarness(t, optedIn())
	h.runErr = errors.New(`exec: "my-sleep": executable file not found`)
	h.sendCmd(protocol.ControlSleep)
	h.wait()
	if got := h.snapshot(); got[len(got)-1] != "reregister" {
		t.Fatalf("events = %v, want a re-registration", got)
	}
}

// Review focus 3: a sleep that returned success but never suspended the host
// (or whose resume went unseen) re-registers once 60 s have passed.
func TestController_NoResumeReregistersAfterFallback(t *testing.T) {
	h := newHarness(t, optedIn())
	h.clock.step = 20 * time.Second
	// The watcher never fires.
	h.c.watchResume = func(ctx context.Context, _ func()) { <-ctx.Done() }
	start := h.clock.peek()
	h.sendCmd(protocol.ControlSleep)
	h.wait()
	got := h.snapshot()
	if got[len(got)-1] != "reregister" || strings.Count(strings.Join(got, "|"), "reregister") != 1 {
		t.Fatalf("events = %v, want one re-registration at the end", got)
	}
	h.mu.Lock()
	regAt := h.regTimes[0]
	h.mu.Unlock()
	if regAt.Sub(start) < resumeFallback {
		t.Errorf("re-registered %s after the start, before the %s fallback", regAt.Sub(start), resumeFallback)
	}
}

func TestController_ResumeReregisters(t *testing.T) {
	t.Run("after the command returned", func(t *testing.T) {
		h := newHarness(t, optedIn())
		h.clock.step = 0
		h.sendCmd(protocol.ControlSleep)
		h.waitFor("run:")
		time.Sleep(20 * time.Millisecond) // let the wait begin
		if slices.Contains(h.snapshot(), "reregister") {
			t.Fatal("re-registered before any resume")
		}
		close(h.resumeNow)
		h.wait()
		if got := h.snapshot(); got[len(got)-1] != "reregister" {
			t.Fatalf("events = %v, want a re-registration", got)
		}
	})
	t.Run("while the command was still running", func(t *testing.T) {
		// systemctl suspend blocks across the suspend: the resume is seen
		// before the command returns, and must not be lost.
		h := newHarness(t, optedIn())
		h.clock.step = 0
		h.runGate = make(chan struct{})
		h.sendCmd(protocol.ControlSleep)
		h.waitFor("run:")
		close(h.resumeNow)
		time.Sleep(20 * time.Millisecond)
		close(h.runGate)
		h.wait()
		got := h.snapshot()
		if got[len(got)-1] != "reregister" || strings.Count(strings.Join(got, "|"), "reregister") != 1 {
			t.Fatalf("events = %v, want exactly one re-registration", got)
		}
	})
}

func TestController_SecondSleepRefused(t *testing.T) {
	h := newHarness(t, optedIn())
	h.clock.step = 0
	h.runErr = errors.New("exit status 1")
	h.runGate = make(chan struct{})
	h.sendCmd(protocol.ControlSleep)
	h.waitFor("run:")

	for _, cmd := range []protocol.ControlCommand{protocol.ControlSleep, protocol.ControlShutdown} {
		h.sendCmd(cmd)
		if r := h.lastReply(); r.Accepted || r.Reason != "already retiring" {
			t.Fatalf("second %s: reply = %+v, want refusal already retiring", cmd, r)
		}
	}
	close(h.runGate)
	h.wait()

	// The failed command re-registered, so the worker may be retired again.
	h.runGate = nil
	h.sendCmd(protocol.ControlSleep)
	if r := h.lastReply(); !r.Accepted {
		t.Fatalf("a sleep after the earlier one ended was refused: %+v", r)
	}
	h.wait()
}

func TestController_WakeRelaysPackets(t *testing.T) {
	h := newHarness(t, Config{}) // no opt-in, and busy: wake needs neither
	h.busy.Store(3)
	h.send(controlJSON(t, protocol.ControlMsg{
		Version: protocol.ProtocolVersion, Type: protocol.TypeControl, Command: protocol.ControlWake,
		MACs:    []string{"aa:bb:cc:dd:ee:ff", "not-a-mac", "11-22-33-44-55-66"},
		Targets: []string{"192.168.1.255:9"},
	}))
	h.wait()
	if r := h.lastReply(); !r.Accepted {
		t.Fatalf("reply = %+v, want accepted", r)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.wakeMACs) != 2 || h.wakeMACs[0].String() != "aa:bb:cc:dd:ee:ff" || h.wakeMACs[1].String() != "11:22:33:44:55:66" {
		t.Errorf("SendWake macs = %v, want the two valid ones", h.wakeMACs)
	}
	if !slices.Equal(h.wakeTo, []string{"192.168.1.255:9"}) {
		t.Errorf("SendWake targets = %v", h.wakeTo)
	}
	if slices.Contains(h.events, "deregister:capacity group retirement: wake:retired") || len(h.runs) != 0 {
		t.Errorf("wake must not deregister or run a command: %v", h.events)
	}
}

func TestController_WakeBadMACRefused(t *testing.T) {
	tests := []struct {
		name    string
		macs    []string
		targets []string
	}{
		{"unparseable", []string{"zz:zz"}, []string{"10.0.0.255:9"}},
		{"eight byte address", []string{"00:00:00:00:00:00:00:01"}, []string{"10.0.0.255:9"}},
		{"twenty byte address", []string{"00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:01"}, []string{"10.0.0.255:9"}},
		{"no macs", nil, []string{"10.0.0.255:9"}},
		{"no targets", []string{"aa:bb:cc:dd:ee:ff"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, Config{})
			h.send(controlJSON(t, protocol.ControlMsg{
				Version: protocol.ProtocolVersion, Type: protocol.TypeControl, Command: protocol.ControlWake,
				MACs: tt.macs, Targets: tt.targets,
			}))
			h.wait()
			if r := h.lastReply(); r.Accepted || r.Reason == "" {
				t.Fatalf("reply = %+v, want a refusal with a reason", r)
			}
			if slices.Contains(h.snapshot(), "wake") {
				t.Error("SendWake was called for a refused wake")
			}
		})
	}
}

func TestController_BadMessagesRefused(t *testing.T) {
	tests := []struct {
		name string
		raw  []byte
	}{
		{"not json", []byte("{nope")},
		{"wrong version", controlJSON(t, protocol.ControlMsg{Version: "1", Type: protocol.TypeControl, Command: protocol.ControlSleep})},
		{"wrong type", controlJSON(t, protocol.ControlMsg{Version: protocol.ProtocolVersion, Type: "heartbeat", Command: protocol.ControlSleep})},
		{"unknown command", controlJSON(t, protocol.ControlMsg{Version: protocol.ProtocolVersion, Type: protocol.TypeControl, Command: "reboot"})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, optedIn())
			h.send(tt.raw)
			h.wait()
			if r := h.lastReply(); r.Accepted || r.Reason == "" {
				t.Fatalf("reply = %+v, want a refusal with a reason", r)
			}
			if got := h.snapshot(); len(got) != 1 {
				t.Errorf("events = %v, want only the refusal", got)
			}
		})
	}
}

// A reply that cannot be delivered means the server will not treat the
// command as accepted, so the worker must not act on it.
func TestController_UndeliverableReplyDoesNotAct(t *testing.T) {
	h := newHarness(t, optedIn())
	h.c.handle(controlJSON(t, protocol.ControlMsg{
		Version: protocol.ProtocolVersion, Type: protocol.TypeControl, Command: protocol.ControlSleep,
	}), func(protocol.ControlReplyMsg) error { return errors.New("connection closed") })
	h.wait()
	if got := h.snapshot(); len(got) != 0 {
		t.Fatalf("events = %v, want none", got)
	}
	h.sendCmd(protocol.ControlSleep)
	if r := h.lastReply(); !r.Accepted {
		t.Fatalf("retiring flag stuck after an undelivered reply: %+v", r)
	}
	close(h.resumeNow)
	h.wait()
}

func TestNewControllerRejectsMissingDeps(t *testing.T) {
	_, err := Subscribe(context.Background(), nil, Config{}, Deps{WorkerID: "w"})
	if err == nil {
		t.Fatal("Subscribe with no Busy/Deregister/Reregister succeeded")
	}
}

// ── Subscribe against a real NATS server ─────────────────────────────────────

func startNATS(t *testing.T) *nats.Conn {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tcp, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("address type %T", ln.Addr())
	}
	port := tcp.Port
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	ns, err := natsserver.NewServer(&natsserver.Options{Host: "127.0.0.1", Port: port, NoLog: true, NoSigs: true})
	if err != nil {
		t.Fatal(err)
	}
	ns.Start()
	if !ns.ReadyForConnections(5 * time.Second) {
		t.Fatal("nats not ready")
	}
	t.Cleanup(ns.Shutdown)
	nc, err := nats.Connect(fmt.Sprintf("nats://127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	return nc
}

func TestSubscribe_AnswersRequestsOnControlSubject(t *testing.T) {
	nc := startNATS(t)
	var regs atomic.Int32
	var wakes atomic.Int32
	ctx := t.Context()
	sub, err := Subscribe(ctx, nc, Config{}, Deps{
		WorkerID:   "w-sub",
		Busy:       func() int { return 0 },
		Deregister: func(string, protocol.DeregisterKind) {},
		Reregister: func(context.Context) error { regs.Add(1); return nil },
		SendWake: func(context.Context, []net.HardwareAddr, []string) error {
			wakes.Add(1)
			return nil
		},
		Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if sub.Subject != bus.WorkerControlSubject("w-sub") {
		t.Fatalf("subscribed to %q", sub.Subject)
	}

	ask := func(m protocol.ControlMsg) protocol.ControlReplyMsg {
		t.Helper()
		data, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := nc.Request(sub.Subject, data, 5*time.Second)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		var r protocol.ControlReplyMsg
		if err := json.Unmarshal(resp.Data, &r); err != nil {
			t.Fatalf("reply %q: %v", resp.Data, err)
		}
		return r
	}

	r := ask(protocol.ControlMsg{Version: protocol.ProtocolVersion, Type: protocol.TypeControl, Command: protocol.ControlSleep})
	if r.Accepted || !strings.Contains(r.Reason, "allow_remote_sleep") {
		t.Errorf("sleep without opt-in: %+v", r)
	}
	r = ask(protocol.ControlMsg{
		Version: protocol.ProtocolVersion, Type: protocol.TypeControl, Command: protocol.ControlWake,
		MACs: []string{"aa:bb:cc:dd:ee:ff"}, Targets: []string{"127.0.0.1:9"},
	})
	if !r.Accepted {
		t.Errorf("wake: %+v", r)
	}
	deadline := time.Now().Add(5 * time.Second)
	for wakes.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if wakes.Load() != 1 {
		t.Errorf("SendWake called %d times, want 1", wakes.Load())
	}
}
