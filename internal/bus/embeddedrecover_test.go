// SPDX-License-Identifier: AGPL-3.0-or-later

package bus

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// switchProbe is a recovery probe a test turns on and off.
type switchProbe struct{ pass atomic.Bool }

func (p *switchProbe) probe(string) error {
	if p.pass.Load() {
		return nil
	}
	return errors.New("no space left on device")
}

// recoveryBroker starts an embedded broker whose recovery loop ticks every
// 20 ms, on clock and probe, logging to logger.
func recoveryBroker(t *testing.T, clock *fakeClock, probe *switchProbe, logger *slog.Logger) *Embedded {
	t.Helper()
	cfg := EmbeddedConfig{
		Addr:       net.JoinHostPort("127.0.0.1", itoa(freePort(t))),
		DataDir:    t.TempDir() + "/nats",
		MaxStoreMB: 64,
	}
	b := NewEmbedded(cfg, logger)
	b.checkEvery = 20 * time.Millisecond
	b.now = clock.now
	b.probe = probe.probe
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(b.Shutdown)
	return b
}

// injectBreak makes the current server's logger report that stream broke,
// as nats-server does.
func injectBreak(t *testing.T, b *Embedded, stream string) {
	t.Helper()
	b.mu.Lock()
	nl := b.natsLog
	b.mu.Unlock()
	if nl == nil {
		t.Fatal("no current server to report a break")
	}
	nl.Errorf("JetStream stream '%s > %s' critical write error: %v", DefaultAccount, stream, errors.New("lmb missing"))
}

func restartsOf(b *Embedded) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.restarts
}

func waitRestarts(t *testing.T, b *Embedded, want int) {
	t.Helper()
	eventuallyNil(t, 20*time.Second, "restarts", func() error {
		if got := restartsOf(b); got != want {
			return errors.New("restarts = " + itoa(got) + ", want " + itoa(want))
		}
		return nil
	})
}

// stayRestarts fails if the restart count moves off want within d.
func stayRestarts(t *testing.T, b *Embedded, want int, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if got := restartsOf(b); got != want {
			t.Fatalf("restarts = %d, want it to stay %d", got, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func checkSays(t *testing.T, b *Embedded, want ...string) {
	t.Helper()
	eventuallyNil(t, 5*time.Second, "Check", func() error {
		err := b.Check(t.Context())
		if err == nil {
			return errors.New("Check is nil")
		}
		for _, w := range want {
			if !strings.Contains(err.Error(), w) {
				return errors.New("Check = " + err.Error())
			}
		}
		return nil
	})
}

var t0 = time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

func TestRecovery_BreakDegradesReadinessThenRestarts(t *testing.T) {
	clock, probe := &fakeClock{t: t0}, &switchProbe{}
	b := recoveryBroker(t, clock, probe, slog.New(slog.DiscardHandler))
	port := boundPort(b)

	injectBreak(t, b, StreamTask)
	checkSays(t, b, StreamTask, "cannot store")
	stayRestarts(t, b, 0, 100*time.Millisecond)

	probe.pass.Store(true)
	waitRestarts(t, b, 1)
	eventuallyNil(t, 15*time.Second, "Check after the restart", func() error { return b.Check(t.Context()) })
	if got := boundPort(b); got != port {
		t.Errorf("port %d after the restart, was %d", got, port)
	}
}

func TestRecovery_ProbeFailingKeepsBrokenWithoutRestart(t *testing.T) {
	clock, probe := &fakeClock{t: t0}, &switchProbe{}
	h := &recordHandler{}
	b := recoveryBroker(t, clock, probe, slog.New(h))

	injectBreak(t, b, StreamTask)
	checkSays(t, b, StreamTask)
	stayRestarts(t, b, 0, 400*time.Millisecond) // about 20 ticks
	checkSays(t, b, StreamTask, "cannot store")
	if breaks, waits := countRecoveryLogs(h); breaks != 1 || waits != 1 {
		t.Errorf("logged %d break and %d cannot-restart records, want 1 and 1 (the clock did not move a minute)", breaks, waits)
	}

	// A minute later the probe, still failing, is logged once more.
	clock.set(t0.Add(recoverProbeLogEvery + time.Second))
	stayRestarts(t, b, 0, 400*time.Millisecond)
	checkSays(t, b, StreamTask, "cannot store")
	if breaks, waits := countRecoveryLogs(h); breaks != 1 || waits != 2 {
		t.Errorf("after a minute: logged %d break and %d cannot-restart records, want 1 and 2", breaks, waits)
	}
}

// countRecoveryLogs counts the break and cannot-restart records h holds.
func countRecoveryLogs(h *recordHandler) (breaks, waits int) {
	for _, r := range h.records() {
		switch r.Message {
		case msgBreak:
			breaks++
		case msgCannotRestart:
			waits++
		}
	}
	return breaks, waits
}

// TestRecovery_BackoffSpacesRepeatedRestarts pins the backoff: a break soon
// after a restart waits 30 s from it, the next 1 m, and 10 minutes without a
// restart reset it.
func TestRecovery_BackoffSpacesRepeatedRestarts(t *testing.T) {
	clock, probe := &fakeClock{t: t0}, &switchProbe{}
	probe.pass.Store(true)
	b := recoveryBroker(t, clock, probe, slog.New(slog.DiscardHandler))

	injectBreak(t, b, StreamTask)
	waitRestarts(t, b, 1) // the first restart of a break is immediate

	clock.set(t0.Add(5 * time.Second))
	injectBreak(t, b, StreamTask)
	stayRestarts(t, b, 1, 300*time.Millisecond)
	clock.set(t0.Add(31 * time.Second))
	waitRestarts(t, b, 2)

	second := t0.Add(31 * time.Second)
	clock.set(second.Add(5 * time.Second))
	injectBreak(t, b, StreamTask)
	clock.set(second.Add(59 * time.Second))
	stayRestarts(t, b, 2, 300*time.Millisecond)
	clock.set(second.Add(61 * time.Second))
	waitRestarts(t, b, 3)

	clock.set(second.Add(61*time.Second + 11*time.Minute))
	injectBreak(t, b, StreamTask)
	waitRestarts(t, b, 4)
}

// TestRecovery_FailedStartRetriesWithoutBackoff pins that a restart whose new
// server could not start is retried at the next passing probe: with no broker
// every worker is disconnected, so the backoff does not apply, and the failed
// attempt does not lengthen it either.
func TestRecovery_FailedStartRetriesWithoutBackoff(t *testing.T) {
	clock, probe := &fakeClock{t: t0}, &switchProbe{}
	probe.pass.Store(true)
	b := recoveryBroker(t, clock, probe, slog.New(slog.DiscardHandler))
	port := boundPort(b)

	injectBreak(t, b, StreamTask)
	waitRestarts(t, b, 1)

	var once sync.Once
	var hog net.Listener
	hogged := make(chan struct{})
	b.mu.Lock()
	b.afterStopHook = func() {
		once.Do(func() {
			var lc net.ListenConfig
			hog, _ = lc.Listen(context.Background(), "tcp", net.JoinHostPort("127.0.0.1", itoa(port))) //nolint:errcheck // a nil hog fails the test below
			close(hogged)
		})
	}
	b.mu.Unlock()

	clock.set(t0.Add(31 * time.Second))
	injectBreak(t, b, StreamTask)
	<-hogged
	if hog == nil {
		t.Fatal("could not occupy the broker's port")
	}
	// Check reports the break while it stands, so the failed attempt is
	// observed as no current server.
	eventuallyNil(t, 15*time.Second, "the failed restart", func() error {
		if boundPort(b) != 0 {
			return errors.New("a server is still current")
		}
		return nil
	})
	_ = hog.Close()
	clock.set(t0.Add(32 * time.Second)) // well inside the next step's minute
	waitRestarts(t, b, 2)
}

func TestRecovery_JetStreamDisabledIsABreak(t *testing.T) {
	clock, probe := &fakeClock{t: t0}, &switchProbe{}
	b := recoveryBroker(t, clock, probe, slog.New(slog.DiscardHandler))
	createServerDurables(t, b) // a stream that cannot be read is checked for them
	b.mu.Lock()
	ns := b.ns
	b.mu.Unlock()
	if err := ns.DisableJetStream(); err != nil {
		t.Fatal(err)
	}
	checkSays(t, b, "JetStream is disabled")

	probe.pass.Store(true)
	waitRestarts(t, b, 1)
	eventuallyNil(t, 15*time.Second, "Check after the restart", func() error { return b.Check(t.Context()) })
	b.mu.Lock()
	enabled := b.ns.JetStreamEnabled()
	b.mu.Unlock()
	if !enabled {
		t.Error("JetStream is still disabled after the restart")
	}
	requireStreams(t, b)
}

// TestRecovery_VerifyRecreatesStreamAndReportsConsumer pins the check after a
// restart: a stream that did not come back is created again, and a durable
// that existed before and did not come back keeps readiness degraded, naming
// it, without another restart.
func TestRecovery_VerifyRecreatesStreamAndReportsConsumer(t *testing.T) {
	clock, probe := &fakeClock{t: t0}, &switchProbe{}
	probe.pass.Store(true)
	b := recoveryBroker(t, clock, probe, slog.New(slog.DiscardHandler))
	c, err := b.NewClient()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	if _, err := c.js.CreateOrUpdateConsumer(t.Context(), StreamLogs, jetstream.ConsumerConfig{
		Durable: "logs-durable", AckPolicy: jetstream.AckExplicitPolicy,
	}); err != nil {
		t.Fatal(err)
	}

	streams := filepath.Join(b.cfg.DataDir, "jetstream", DefaultAccount, "streams")
	b.mu.Lock()
	b.afterStopHook = func() {
		for _, dir := range []string{filepath.Join(streams, StreamWorker), filepath.Join(streams, StreamLogs, "obs", "logs-durable")} {
			if err := os.RemoveAll(dir); err != nil {
				t.Errorf("remove %s: %v", dir, err)
			}
		}
	}
	b.mu.Unlock()

	injectBreak(t, b, StreamTask)
	waitRestarts(t, b, 1)
	checkSays(t, b, "logs-durable", "restart sqi-server")
	requireStreams(t, b)
	stayRestarts(t, b, 1, 300*time.Millisecond)
}

// requireStreams fails unless every one of sqi's streams exists.
func requireStreams(t *testing.T, b *Embedded) {
	t.Helper()
	c, err := b.NewClient()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for _, name := range StreamNames() {
		eventuallyNil(t, 15*time.Second, "stream "+name, func() error {
			_, err := c.js.Stream(t.Context(), name)
			return err
		})
	}
}

// createServerDurables creates the durable consumers a running sqi-server
// keeps, as the server does at startup.
func createServerDurables(t *testing.T, b *Embedded) {
	t.Helper()
	c, err := b.NewClient()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, consumers := RestoreTargets()
	for _, d := range consumers {
		if _, err := c.js.CreateOrUpdateConsumer(t.Context(), d.Stream, jetstream.ConsumerConfig{
			Durable: d.Name, AckPolicy: jetstream.AckExplicitPolicy,
		}); err != nil {
			t.Fatal(err)
		}
	}
}

// TestRecovery_JetStreamDisabledChecksTheServersConsumers pins the check after
// a restart from JetStream disabled, when no stream can be read beforehand to
// list its consumers: the consumers a running sqi-server keeps are checked,
// and one that did not come back keeps readiness degraded, naming it.
func TestRecovery_JetStreamDisabledChecksTheServersConsumers(t *testing.T) {
	clock, probe := &fakeClock{t: t0}, &switchProbe{}
	b := recoveryBroker(t, clock, probe, slog.New(slog.DiscardHandler))
	createServerDurables(t, b)
	streams := filepath.Join(b.cfg.DataDir, "jetstream", DefaultAccount, "streams")
	b.mu.Lock()
	ns := b.ns
	b.afterStopHook = func() {
		if err := os.RemoveAll(filepath.Join(streams, StreamTask, "obs", taskStatusConsumerName)); err != nil {
			t.Errorf("remove the consumer: %v", err)
		}
	}
	b.mu.Unlock()
	if err := ns.DisableJetStream(); err != nil {
		t.Fatal(err)
	}
	checkSays(t, b, "JetStream is disabled")

	probe.pass.Store(true)
	waitRestarts(t, b, 1)
	checkSays(t, b, StreamTask+"/"+taskStatusConsumerName, "restart sqi-server")
}

// TestRecovery_FailedStartsLogOncePerMinute pins that a restart whose new
// server keeps failing to start, retried at every passing check, logs its
// failure at most once a minute rather than at every attempt.
func TestRecovery_FailedStartsLogOncePerMinute(t *testing.T) {
	clock, probe := &fakeClock{t: t0}, &switchProbe{}
	probe.pass.Store(true)
	h := &recordHandler{}
	b := recoveryBroker(t, clock, probe, slog.New(h))
	port := boundPort(b)

	var attempts atomic.Int32
	var hog net.Listener
	b.mu.Lock()
	b.afterStopHook = func() {
		if attempts.Add(1) == 1 {
			var lc net.ListenConfig
			hog, _ = lc.Listen(context.Background(), "tcp", net.JoinHostPort("127.0.0.1", itoa(port))) //nolint:errcheck // a nil hog fails the test below
		}
	}
	b.mu.Unlock()

	injectBreak(t, b, StreamTask)
	eventuallyNil(t, 15*time.Second, "failed attempts", func() error {
		if n := attempts.Load(); n < 4 {
			return errors.New("attempts = " + itoa(int(n)))
		}
		return nil
	})
	if hog == nil {
		t.Fatal("could not occupy the broker's port")
	}
	count := func(msg string) int {
		n := 0
		for _, r := range h.records() {
			if r.Message == msg {
				n++
			}
		}
		return n
	}
	if got := count(msgRestartFailed); got != 1 {
		t.Errorf("logged %d restart failures over %d attempts within a minute, want 1", got, attempts.Load())
	}
	if got := count(msgRestarting); got != 1 {
		t.Errorf("logged %d restarting records over %d attempts, want 1", got, attempts.Load())
	}

	clock.set(t0.Add(61 * time.Second))
	eventuallyNil(t, 15*time.Second, "a second failure record after a minute", func() error {
		if got := count(msgRestartFailed); got != 2 {
			return errors.New("restart failures logged = " + itoa(got))
		}
		return nil
	})
	_ = hog.Close()
	waitRestarts(t, b, 1)
}

// TestRecovery_ShutdownDuringALoopRestart pins that Shutdown arriving while
// the recovery loop restarts the broker ends that restart and returns, with
// no server left running and no restart failure logged, since the restart
// ended because sqi-server is stopping.
func TestRecovery_ShutdownDuringALoopRestart(t *testing.T) {
	clock, probe := &fakeClock{t: t0}, &switchProbe{}
	probe.pass.Store(true)
	h := &recordHandler{}
	b := recoveryBroker(t, clock, probe, slog.New(h))
	port := boundPort(b)

	reached, proceed := make(chan struct{}), make(chan struct{})
	var once sync.Once
	b.mu.Lock()
	b.afterStopHook = func() {
		once.Do(func() {
			close(reached)
			<-proceed
		})
	}
	b.mu.Unlock()

	injectBreak(t, b, StreamTask)
	<-reached
	shut := make(chan struct{})
	go func() {
		b.Shutdown()
		close(shut)
	}()
	time.Sleep(100 * time.Millisecond) // time for Shutdown to cancel the loop
	close(proceed)
	select {
	case <-shut:
	case <-time.After(15 * time.Second):
		t.Fatal("Shutdown did not return")
	}
	for _, r := range h.records() {
		if r.Message == msgRestartFailed {
			t.Errorf("logged %q during Shutdown", r.Message)
		}
	}
	b.mu.Lock()
	ns := b.ns
	b.mu.Unlock()
	if ns != nil {
		t.Error("a server is current after Shutdown")
	}
	var lc net.ListenConfig
	l, err := lc.Listen(context.Background(), "tcp", net.JoinHostPort("127.0.0.1", itoa(port)))
	if err != nil {
		t.Fatalf("the broker's port is still taken after Shutdown: %v", err)
	}
	_ = l.Close()
}
