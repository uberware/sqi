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

// testClock is a clock a test moves by hand.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

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
func recoveryBroker(t *testing.T, clock *testClock, probe *switchProbe, logger *slog.Logger) *Embedded {
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
	clock, probe := &testClock{t: t0}, &switchProbe{}
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
	clock, probe := &testClock{t: t0}, &switchProbe{}
	h := &recordHandler{}
	b := recoveryBroker(t, clock, probe, slog.New(h))

	injectBreak(t, b, StreamTask)
	checkSays(t, b, StreamTask)
	stayRestarts(t, b, 0, 400*time.Millisecond) // about 20 ticks
	checkSays(t, b, StreamTask, "cannot store")

	var breaks, waits int
	for _, r := range h.records() {
		switch r.Message {
		case msgBreak:
			breaks++
		case msgCannotRestart:
			waits++
		}
	}
	if breaks != 1 || waits != 1 {
		t.Errorf("logged %d break and %d cannot-restart records, want 1 and 1 (the clock did not move a minute)", breaks, waits)
	}
}

// TestRecovery_BackoffSpacesRepeatedRestarts pins the backoff: a break soon
// after a restart waits 30 s from it, the next 1 m, and 10 minutes without a
// restart reset it.
func TestRecovery_BackoffSpacesRepeatedRestarts(t *testing.T) {
	clock, probe := &testClock{t: t0}, &switchProbe{}
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
	clock, probe := &testClock{t: t0}, &switchProbe{}
	probe.pass.Store(true)
	b := recoveryBroker(t, clock, probe, slog.New(slog.DiscardHandler))
	port := boundPort(b)

	injectBreak(t, b, StreamTask)
	waitRestarts(t, b, 1)

	var once sync.Once
	var hog net.Listener
	hogged := make(chan struct{})
	b.mu.Lock()
	b.stageHook = func(s string) {
		if s != stageStopped {
			return
		}
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
	// The break, not the missing server, is what Check reports while it
	// stands; the failed attempt shows as no current server.
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
	clock, probe := &testClock{t: t0}, &switchProbe{}
	b := recoveryBroker(t, clock, probe, slog.New(slog.DiscardHandler))
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
	clock, probe := &testClock{t: t0}, &switchProbe{}
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
	b.stageHook = func(s string) {
		if s != stageStopped {
			return
		}
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
