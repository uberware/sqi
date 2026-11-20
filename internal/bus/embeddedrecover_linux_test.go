// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build linux

package bus

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// fdChildEnv marks the child process TestRecovery_RealDescriptorExhaustion
// re-executes itself as: the child lowers its own descriptor limit, which
// must not touch the test binary running every other test.
const fdChildEnv = "SQI_TEST_FD_CHILD"

// TestRecovery_RealDescriptorExhaustion runs the embedded broker out of file
// descriptors for real, as chaos-server-fd-exhaustion does: SQI_TASK traffic
// that is acked as it arrives makes the stream open a new block file every
// time an ack empties the last one, and with no descriptor free that open
// fails and nats-server keeps the stream's write error. The broker must
// report the break and recover by itself once the limit is lifted. It pins
// the log formats detection matches against the real nats-server.
func TestRecovery_RealDescriptorExhaustion(t *testing.T) {
	if os.Getenv(fdChildEnv) == "1" {
		fdChild(t)
		return
	}
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestRecovery_RealDescriptorExhaustion$", "-test.count=1", "-test.v")
	cmd.Env = append(os.Environ(), fdChildEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child: %v\n%s", err, out)
	}
}

func fdChild(t *testing.T) {
	cfg := EmbeddedConfig{
		Addr:       net.JoinHostPort("127.0.0.1", itoa(freePort(t))),
		DataDir:    t.TempDir() + "/nats",
		MaxStoreMB: 64,
	}
	b := NewEmbedded(cfg, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))
	b.checkEvery = 500 * time.Millisecond
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Shutdown()

	c, err := b.NewClient()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.js.CreateOrUpdateConsumer(ctx, StreamTask, jetstream.ConsumerConfig{
		Durable: "fd-child", FilterSubject: "task.status.w1.>", AckPolicy: jetstream.AckExplicitPolicy,
	}); err != nil {
		t.Fatal(err)
	}
	var stored, consumed atomic.Int64
	go traffic(ctx, c.js, &stored)
	go drain(ctx, c.js, &consumed)
	eventuallyNil(t, 10*time.Second, "traffic", func() error {
		if consumed.Load() < 50 {
			return errors.New("not yet 50 messages through")
		}
		return nil
	})

	hold := exhaustDescriptors(t)
	eventuallyNil(t, 15*time.Second, "the break", func() error {
		if err := b.Check(ctx); err == nil || !strings.Contains(err.Error(), "cannot store") {
			return errors.New("Check does not report a stream that cannot store")
		}
		return nil
	})
	t.Logf("Check while exhausted: %v", b.Check(ctx))
	hold()

	eventuallyNil(t, 60*time.Second, "the recovery", func() error { return b.Check(ctx) })
	if restartsOf(b) < 1 {
		t.Fatal("Check passes again but the broker never restarted")
	}
	before := consumed.Load()
	eventuallyNil(t, 30*time.Second, "traffic after the recovery", func() error {
		if consumed.Load() < before+50 {
			return errors.New("not yet 50 messages through after the recovery")
		}
		return nil
	})
	t.Logf("recovered: %d stored, %d consumed", stored.Load(), consumed.Load())
}

// traffic publishes a status-like message every 20 ms until ctx ends,
// counting the ones the broker stored.
func traffic(ctx context.Context, js jetstream.JetStream, stored *atomic.Int64) {
	for ctx.Err() == nil {
		pctx, cancel := context.WithTimeout(ctx, time.Second)
		if _, err := js.Publish(pctx, "task.status.w1.job", []byte("status")); err == nil {
			stored.Add(1)
		}
		cancel()
		time.Sleep(20 * time.Millisecond)
	}
}

// drain fetches and acks from the durable until ctx ends.
func drain(ctx context.Context, js jetstream.JetStream, consumed *atomic.Int64) {
	for ctx.Err() == nil {
		cons, err := js.Consumer(ctx, StreamTask, "fd-child")
		if err != nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		batch, err := cons.Fetch(10, jetstream.FetchMaxWait(500*time.Millisecond))
		if err != nil {
			continue
		}
		for m := range batch.Messages() {
			if m.Ack() == nil {
				consumed.Add(1)
			}
		}
	}
}

// exhaustDescriptors lowers this process's soft descriptor limit to what is
// open plus 10 and takes those 10 too, as the redialing workers do in the
// chaos scenario. The returned function gives them back and restores the
// limit.
func exhaustDescriptors(t *testing.T) (restore func()) {
	t.Helper()
	var old syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &old); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	lim := syscall.Rlimit{Cur: uint64(len(entries) + 10), Max: old.Max}
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		t.Fatal(err)
	}
	var held []*os.File
	for {
		f, err := os.Open("/dev/null")
		if err != nil {
			break
		}
		held = append(held, f)
	}
	t.Logf("descriptor limit %d, %d extra held", lim.Cur, len(held))
	return func() {
		for _, f := range held {
			_ = f.Close()
		}
		if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &old); err != nil {
			t.Errorf("restore the descriptor limit: %v", err)
		}
	}
}
