// SPDX-License-Identifier: AGPL-3.0-or-later

package natsclient

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	nats "github.com/nats-io/nats.go"
)

// resetProxy forwards connections to a broker, except that while resetting
// it resets each one it accepts. A broker that refuses a credential closes
// the connection with the client's PING still unread, which sends a reset,
// and on Windows the reset discards the -ERR the client had not read yet:
// the refusal arrives as a reset.
type resetProxy struct {
	ln        net.Listener
	target    string
	resetting atomic.Bool
	resets    atomic.Int32
	wg        sync.WaitGroup
}

func startResetProxy(t *testing.T, target string) *resetProxy {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &resetProxy{ln: ln, target: target}
	p.resetting.Store(true)
	p.wg.Add(1)
	go p.serve()
	t.Cleanup(func() {
		ln.Close()
		p.wg.Wait()
	})
	return p
}

func (p *resetProxy) url() string { return "nats://" + p.ln.Addr().String() }

func (p *resetProxy) serve() {
	defer p.wg.Done()
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		if p.resetting.Load() {
			p.resets.Add(1)
			if tc, ok := c.(*net.TCPConn); ok {
				tc.SetLinger(0) //nolint:errcheck // a close without linger is the reset under test
			}
			c.Close()
			continue
		}
		p.wg.Add(1)
		go p.forward(c)
	}
}

func (p *resetProxy) forward(c net.Conn) {
	defer p.wg.Done()
	defer c.Close()
	var d net.Dialer
	up, err := d.DialContext(context.Background(), "tcp", p.target)
	if err != nil {
		return
	}
	defer up.Close()
	done := make(chan struct{}, 2)
	go func() { io.Copy(up, c); done <- struct{}{} }() //nolint:errcheck // copy ends with either side
	go func() { io.Copy(c, up); done <- struct{}{} }() //nolint:errcheck // copy ends with either side
	<-done
}

// TestConnect_InitialDialRetriesThroughHandshakeResets covers a worker that
// boots while the credential store is down on a host where the broker's
// refusals arrive as connection resets: the resets are retried
// within the grace, as refusals are, and the dial connects once they stop.
func TestConnect_InitialDialRetriesThroughHandshakeResets(t *testing.T) {
	t.Parallel()
	seed, pub, enrolled := enrolledWorker(t)
	b := startTestBroker(t, enrolled)
	addr := b.ClientURL()[len("nats://"):]
	p := startResetProxy(t, addr)
	time.AfterFunc(time.Second, func() { p.resetting.Store(false) })
	logger, logs := newTestLogger()

	start := time.Now()
	nc, _, err := connect(context.Background(), reconnectingConfig(p.url()), "worker-a", seed, pub, logger, 2*time.Second)
	if err != nil {
		t.Fatalf("Connect through 1s of handshake resets: %v (resets %d); logs:\n%s", err, p.resets.Load(), logs)
	}
	defer nc.Close()
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Connect took %s, want under 5s", elapsed)
	}
	if p.resets.Load() == 0 {
		t.Fatal("the proxy reset no connection; the test proves nothing")
	}
	if len(logs.lines(slog.LevelWarn, "reset")) == 0 {
		t.Errorf("no WARN for the reset dials; logs:\n%s", logs)
	}
}

// TestConnect_InitialDialResetsPastGraceFail pins that resets are retried
// only for the grace: a broker that keeps resetting the handshake ends the
// dial with the reset, not a credential rejection, since a reset does not
// prove the credential was refused.
func TestConnect_InitialDialResetsPastGraceFail(t *testing.T) {
	t.Parallel()
	seed, pub, _ := enrolledWorker(t)
	p := startResetProxy(t, "127.0.0.1:1")
	grace := time.Second

	start := time.Now()
	_, _, err := connect(context.Background(), reconnectingConfig(p.url()), "worker-a", seed, pub, slog.New(slog.DiscardHandler), grace)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("Connect through endless resets: want error, got nil")
	}
	if elapsed < grace {
		t.Errorf("Connect gave up after %s, before the %s grace", elapsed, grace)
	}
	if elapsed > 10*time.Second {
		t.Errorf("Connect gave up after %s, long after the %s grace", elapsed, grace)
	}
	if !isConnReset(err) {
		t.Errorf("error %v is not the connection reset", err)
	}
	if errors.Is(err, nats.ErrAuthorization) {
		t.Errorf("error %v reports a credential refusal for a reset", err)
	}
}

// TestIsConnReset pins the classification: a reset is retried, a refused
// dial (nothing listening) is not.
func TestIsConnReset(t *testing.T) {
	t.Parallel()
	if isConnReset(nil) {
		t.Error("isConnReset(nil) = true")
	}
	if isConnReset(errors.New("boom")) {
		t.Error("isConnReset(other) = true")
	}
	d := net.Dialer{Timeout: time.Second}
	_, err := d.DialContext(context.Background(), "tcp", "127.0.0.1:1")
	if err == nil {
		t.Skip("something listens on port 1")
	}
	if isConnReset(err) {
		t.Errorf("isConnReset(%v) = true for a refused dial", err)
	}
}

// TestLogReset_NamesACredentialOnlyWhenTheWorkerHasOne pins the wording of
// the reset warning: a refusal of the worker's credential can arrive as a
// reset only when the worker presents one, so a worker without a key is not
// sent looking for a credential problem.
func TestLogReset_NamesACredentialOnlyWhenTheWorkerHasOne(t *testing.T) {
	for _, keyed := range []bool{true, false} {
		var logs syncLog
		lc := newLifecycle(context.Background(), slog.New(slog.NewTextHandler(&logs, nil)), keyed, time.Second, time.Minute)
		lc.logReset(errors.New("connection reset by peer"), time.Minute)
		got := logs.String()
		if mentions := strings.Contains(got, "credential"); mentions != keyed {
			t.Errorf("keyed=%v: reset warning mentions a credential = %v: %s", keyed, mentions, got)
		}
	}
}
