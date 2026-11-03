// SPDX-License-Identifier: AGPL-3.0-or-later

package bus

import (
	"context"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// hammerChildEnv makes the test binary run [hammerShutdownChild] instead of
// the parent half of TestEmbedded_ShutdownUnderConnectHammerDoesNotPanic.
const hammerChildEnv = "SQI_TEST_SHUTDOWN_HAMMER_CHILD"

// hammerRounds is how many broker lifetimes the child runs; each round is a
// fresh chance to land a CONNECT in nats-server's shutdown window.
const hammerRounds = 4

// hammerClients connect in a loop, pausing hammerPause between connects. A
// flood would, under the race detector, spend the test's time on overload
// instead of in the shutdown window.
const (
	hammerClients = 8
	hammerPause   = 2 * time.Millisecond
)

// TestEmbedded_ShutdownUnderConnectHammerDoesNotPanic shuts an auth-on
// embedded broker down while clients keep sending CONNECTs that need the
// auth callout. nats-server v2.15.2 dereferences nil when it processes such a
// CONNECT after its system account has shut down, which a plain Shutdown
// allows before it closes the clients it has accepted; [stopServer] narrows
// that window. The panic would take sqi-server down before its store is
// closed.
//
// A panic inside nats-server takes the whole process down, so the broker
// runs in a child copy of this test binary and the parent checks how it
// exited.
func TestEmbedded_ShutdownUnderConnectHammerDoesNotPanic(t *testing.T) {
	if os.Getenv(hammerChildEnv) == "1" {
		hammerShutdownChild(t)
		return
	}
	if testing.Short() {
		t.Skip("runs several broker lifetimes in a subprocess")
	}
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestEmbedded_ShutdownUnderConnectHammerDoesNotPanic$", "-test.v")
	cmd.Env = append(os.Environ(), hammerChildEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil || strings.Contains(string(out), "panic:") {
		t.Fatalf("the broker did not shut down cleanly under a CONNECT hammer (%v):\n%s", err, tail(string(out), 60))
	}
	if !strings.Contains(string(out), "--- PASS: TestEmbedded_ShutdownUnderConnectHammerDoesNotPanic") {
		t.Fatalf("the child did not run the scenario:\n%s", tail(string(out), 60))
	}
}

func tail(s string, lines int) string {
	all := strings.Split(s, "\n")
	if len(all) > lines {
		all = all[len(all)-lines:]
	}
	return strings.Join(all, "\n")
}

// hammerShutdownChild is the child half: several rounds of an auth-on broker
// shut down under a stream of anonymous CONNECTs, each of which goes to the
// callout.
func hammerShutdownChild(t *testing.T) {
	var sent atomic.Int64
	for round := range hammerRounds {
		b := NewEmbedded(EmbeddedConfig{
			Addr:       net.JoinHostPort("127.0.0.1", itoa(freePort(t))),
			DataDir:    t.TempDir() + "/nats",
			MaxStoreMB: 64,
			Auth:       AuthConfig{Enabled: true, UserTTL: time.Hour, Lookup: newCredentialMap().lookup},
		}, slog.New(slog.DiscardHandler))
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		if err := b.Start(ctx); err != nil {
			cancel()
			t.Fatalf("round %d: Start: %v", round, err)
		}
		cancel()
		addr := strings.TrimPrefix(b.ClientURL(), "nats://")

		stop := make(chan struct{})
		var wg sync.WaitGroup
		for range hammerClients {
			wg.Go(func() {
				for {
					select {
					case <-stop:
						return
					case <-time.After(hammerPause):
					}
					hammerConnect(addr)
					sent.Add(1)
				}
			})
		}
		time.Sleep(200 * time.Millisecond)
		b.Shutdown()
		close(stop)
		wg.Wait()
	}
	t.Logf("%d CONNECTs sent across %d shutdowns", sent.Load(), hammerRounds)
}

// anonymousConnect is a CONNECT with no credentials, which an auth-on broker
// sends to the auth callout.
const anonymousConnect = "CONNECT {\"verbose\":false,\"pedantic\":false,\"protocol\":1}\r\nPING\r\n"

// hammerConnect opens a connection, sends an anonymous CONNECT, and leaves
// without waiting for the answer. Every failure is ignored: the broker may
// be stopping.
func hammerConnect(addr string) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return
	}
	defer conn.Close()
	if conn.SetDeadline(time.Now().Add(100*time.Millisecond)) != nil {
		return
	}
	if _, err := conn.Write([]byte(anonymousConnect)); err != nil {
		return
	}
}
