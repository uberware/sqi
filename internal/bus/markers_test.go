// SPDX-License-Identifier: AGPL-3.0-or-later

package bus

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	nats "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

func TestMarkerConstants(t *testing.T) {
	if SubjectMarkerWorker != "sqi.marker.worker" || SubjectMarkerTask != "sqi.marker.task" {
		t.Errorf("marker subjects = %q, %q", SubjectMarkerWorker, SubjectMarkerTask)
	}
	if WorkerConsumerMaxDeliver != 11 || TaskStatusConsumerMaxDeliver != 10 {
		t.Errorf("MaxDeliver = %d, %d, want 11, 10", WorkerConsumerMaxDeliver, TaskStatusConsumerMaxDeliver)
	}
	if WorkerConsumerAckWait != 10*time.Second || TaskStatusConsumerAckWait != 30*time.Second {
		t.Errorf("AckWait = %v, %v, want 10s, 30s", WorkerConsumerAckWait, TaskStatusConsumerAckWait)
	}
	if WorkerStreamMaxAge != 2*time.Minute || TaskStreamMaxAge != 24*time.Hour {
		t.Errorf("MaxAge = %v, %v, want 2m, 24h", WorkerStreamMaxAge, TaskStreamMaxAge)
	}
}

// TestStreamsAndConsumers_UseTheExportedSettings pins that the provisioned
// streams and consumers carry the exported values, so the constants the
// scheduler reads cannot drift from what the broker enforces.
func TestStreamsAndConsumers_UseTheExportedSettings(t *testing.T) {
	b := startBroker(t)
	c := newClient(t, b)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for name, want := range map[string]time.Duration{StreamWorker: WorkerStreamMaxAge, StreamTask: TaskStreamMaxAge} {
		s, err := c.js.Stream(ctx, name)
		if err != nil {
			t.Fatalf("Stream(%s): %v", name, err)
		}
		if got := s.CachedInfo().Config.MaxAge; got != want {
			t.Errorf("%s MaxAge = %v, want %v", name, got, want)
		}
	}

	noop := func(jetstream.Msg) {}
	for _, tc := range []struct {
		stream, name string
		ackWait      time.Duration
		consume      func() (jetstream.ConsumeContext, error)
	}{
		{StreamTask, taskStatusConsumerName, TaskStatusConsumerAckWait, func() (jetstream.ConsumeContext, error) { return c.ConsumeTaskStatus(ctx, noop) }},
		{StreamWorker, workerConsumerName, WorkerConsumerAckWait, func() (jetstream.ConsumeContext, error) { return c.ConsumeWorker(ctx, noop) }},
	} {
		cc, err := tc.consume()
		if err != nil {
			t.Fatalf("consume %s: %v", tc.name, err)
		}
		t.Cleanup(cc.Stop)
		consumer, err := c.js.Consumer(ctx, tc.stream, tc.name)
		if err != nil {
			t.Fatalf("Consumer(%s): %v", tc.name, err)
		}
		if got := consumer.CachedInfo().Config.AckWait; got != tc.ackWait {
			t.Errorf("%s AckWait = %v, want %v", tc.name, got, tc.ackWait)
		}
	}
}

func TestPublishIngestMarkers_LandInBothStreams(t *testing.T) {
	b := startBroker(t)
	c := newClient(t, b)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := c.PublishIngestMarkers(ctx, 5*time.Second); err != nil {
		t.Fatalf("PublishIngestMarkers: %v", err)
	}

	for stream, subject := range map[string]string{StreamWorker: SubjectMarkerWorker, StreamTask: SubjectMarkerTask} {
		s, err := c.js.Stream(ctx, stream)
		if err != nil {
			t.Fatalf("Stream(%s): %v", stream, err)
		}
		raw, err := s.GetLastMsgForSubject(ctx, subject)
		if err != nil {
			t.Fatalf("%s: no marker on %s: %v", stream, subject, err)
		}
		if len(raw.Data) != 0 {
			t.Errorf("%s marker body = %q, want empty", stream, raw.Data)
		}
		info, err := s.Info(ctx)
		if err != nil {
			t.Fatalf("Info(%s): %v", stream, err)
		}
		if raw.Sequence != info.State.LastSeq {
			t.Errorf("%s: marker is sequence %d, stream's last is %d", stream, raw.Sequence, info.State.LastSeq)
		}
	}
}

// TestPublishIngestMarkers_ReturnsBothErrors: a failed publish does not stop
// the other from being attempted, and both failures come back.
func TestPublishIngestMarkers_ReturnsBothErrors(t *testing.T) {
	b := startBroker(t)
	c := newClient(t, b)
	c.Close()
	err := c.PublishIngestMarkers(context.Background(), time.Second)
	if err == nil {
		t.Fatal("PublishIngestMarkers on a closed client returned nil")
	}
	for _, subject := range []string{SubjectMarkerWorker, SubjectMarkerTask} {
		if !strings.Contains(err.Error(), subject) {
			t.Errorf("error %q does not name %s", err, subject)
		}
	}
}

// TestPublishMarkers_EachHasItsOwnTimeout: a publish that stalls for its
// whole timeout does not shorten the other's, so a stall on one stream cannot
// keep the other stream's marker from being published.
func TestPublishMarkers_EachHasItsOwnTimeout(t *testing.T) {
	const timeout = 50 * time.Millisecond
	type call struct {
		subject string
		start   time.Time
		left    time.Duration // time to the deadline when the publish started
	}
	var calls []call
	publish := func(ctx context.Context, subject string, data []byte) error {
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Errorf("publish of %s has no deadline", subject)
		}
		if data != nil {
			t.Errorf("publish of %s carries %q, want an empty body", subject, data)
		}
		calls = append(calls, call{subject: subject, start: time.Now(), left: time.Until(deadline)})
		if subject == SubjectMarkerWorker {
			<-ctx.Done() // the worker stream stalls for the whole timeout
			return ctx.Err()
		}
		return nil
	}

	err := publishMarkers(context.Background(), timeout, publish)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want the stalled publish's deadline", err)
	}
	if len(calls) != 2 || calls[0].subject != SubjectMarkerWorker || calls[1].subject != SubjectMarkerTask {
		t.Fatalf("publishes = %+v, want the worker marker then the task marker", calls)
	}
	if left := calls[1].left; left < timeout/2 {
		t.Errorf("the task marker's publish started with %v to its deadline, want about %v", left, timeout)
	}
}

// TestInterruptedAt_SetOnReconnect: zero until something happens, then
// recorded when the broker goes away and again when it comes back.
func TestInterruptedAt_SetOnReconnect(t *testing.T) {
	port := freePort(t)
	srv := startPlainServer(t, "127.0.0.1", port)
	defer func() { srv.Shutdown() }()

	c, err := NewClient("nats://"+net.JoinHostPort("127.0.0.1", itoa(port)), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(c.Close)

	if got := c.InterruptedAt(); !got.IsZero() {
		t.Fatalf("InterruptedAt right after connect = %v, want zero", got)
	}

	srv.Shutdown()
	waitUntil(t, 5*time.Second, "InterruptedAt to record the disconnect", func() bool {
		return !c.InterruptedAt().IsZero()
	})
	disconnected := c.InterruptedAt()

	back := time.Now()
	srv = startPlainServer(t, "127.0.0.1", port)
	waitUntil(t, 15*time.Second, "the client to reconnect", func() bool { return !c.BusUpSince().IsZero() })
	if got := c.InterruptedAt(); got.Before(back) || !got.After(disconnected) {
		t.Errorf("InterruptedAt after reconnect = %v, want after the disconnect %v and not before %v", got, disconnected, back)
	}
}

func TestConsumeErrHandler_RecordsInterruption(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"no heartbeat", jetstream.ErrNoHeartbeat, true},
		{"leadership changed", jetstream.ErrConsumerLeadershipChanged, true},
		{"no responders", nats.ErrNoResponders, true},
		{"wrapped no heartbeat", errors.Join(errors.New("x"), jetstream.ErrNoHeartbeat), true},
		{"connection closed", jetstream.ErrConnectionClosed, false},
		{"nats connection closed", nats.ErrConnectionClosed, false},
		{"other", errors.New("boom"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &Client{logger: slog.New(slog.DiscardHandler)}
			c.consumeErrHandler("test")(nil, tc.err)
			if got := !c.InterruptedAt().IsZero(); got != tc.want {
				t.Errorf("interrupted = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestNewClient_SlowConsumerRecordsInterruption drives a real subscription
// past its pending limit and checks the error handler NewClient installs.
func TestNewClient_SlowConsumerRecordsInterruption(t *testing.T) {
	port := freePort(t)
	srv := startPlainServer(t, "127.0.0.1", port)
	defer srv.Shutdown()
	url := "nats://" + net.JoinHostPort("127.0.0.1", itoa(port))

	c, err := NewClient(url, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(c.Close)

	release := make(chan struct{})
	sub, err := c.nc.Subscribe("slow", func(*nats.Msg) { <-release })
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(func() { close(release) })
	if err := sub.SetPendingLimits(2, -1); err != nil {
		t.Fatalf("SetPendingLimits: %v", err)
	}

	pub, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("connect publisher: %v", err)
	}
	t.Cleanup(pub.Close)
	for range 20 {
		if err := pub.Publish("slow", []byte("x")); err != nil {
			t.Fatalf("Publish: %v", err)
		}
	}
	if err := pub.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	waitUntil(t, 5*time.Second, "InterruptedAt to record the slow consumer", func() bool {
		return !c.InterruptedAt().IsZero()
	})
}
