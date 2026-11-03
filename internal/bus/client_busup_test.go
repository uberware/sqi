// SPDX-License-Identifier: AGPL-3.0-or-later

package bus

import (
	"context"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	nats "github.com/nats-io/nats.go"
)

// startPlainServer runs a bare nats-server on addr (JetStream is not needed
// to connect a Client) and returns it.
func startPlainServer(t *testing.T, host string, port int) *natsserver.Server {
	t.Helper()
	srv, err := natsserver.NewServer(&natsserver.Options{Host: host, Port: port, NoLog: true, NoSigs: true})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(10 * time.Second) {
		srv.Shutdown()
		t.Fatal("nats-server not ready")
	}
	return srv
}

func waitUntil(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", within, what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestBusUpSince_TracksConnectAndReconnect pins the signal the scheduler's
// liveness grace reads: non-zero once connected, zero while the broker is
// gone, and a later time once the client has reconnected.
func TestBusUpSince_TracksConnectAndReconnect(t *testing.T) {
	port := freePort(t)
	srv := startPlainServer(t, "127.0.0.1", port)
	defer func() { srv.Shutdown() }()

	c, err := NewClient("nats://"+net.JoinHostPort("127.0.0.1", itoa(port)), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(c.Close)

	first := c.BusUpSince()
	if first.IsZero() {
		t.Fatal("BusUpSince is zero right after connect")
	}

	srv.Shutdown()
	waitUntil(t, 5*time.Second, "BusUpSince to clear after the broker stopped", func() bool {
		return c.BusUpSince().IsZero()
	})

	srv = startPlainServer(t, "127.0.0.1", port)
	waitUntil(t, 15*time.Second, "BusUpSince to be set again after the broker returned", func() bool {
		return !c.BusUpSince().IsZero()
	})
	if second := c.BusUpSince(); !second.After(first) {
		t.Errorf("BusUpSince after reconnect = %v, want after the first connect %v", second, first)
	}
}

// TestBusUpSince_SilentPartitionIsSeenAsDown: a link that stops carrying
// traffic without closing (a black-holed route, a hung broker host) must read
// as down within about 20 s, not after the client library's default two
// minutes of pings, or the scheduler would judge every worker by heartbeats
// that were never delivered while BusUpSince still says "up".
func TestBusUpSince_SilentPartitionIsSeenAsDown(t *testing.T) {
	port := freePort(t)
	srv := startPlainServer(t, "127.0.0.1", port)
	defer srv.Shutdown()
	p := startBlackholeProxy(t, net.JoinHostPort("127.0.0.1", itoa(port)))

	c, err := NewClient("nats://"+p.addr, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(c.Close)
	if c.BusUpSince().IsZero() {
		t.Fatal("BusUpSince is zero right after connect")
	}

	p.blackhole.Store(true) // traffic stops; no connection is closed
	start := time.Now()
	waitUntil(t, 25*time.Second, "BusUpSince to clear after the link went silent", func() bool {
		return c.BusUpSince().IsZero()
	})
	if d := time.Since(start); d > 20*time.Second {
		t.Errorf("silent partition detected after %v, want <= 20s", d)
	}
}

// blackholeProxy forwards TCP between a client and an upstream until
// blackhole is set, after which it reads and discards in both directions
// without closing anything: the failure a dropped route produces.
type blackholeProxy struct {
	addr      string
	blackhole atomic.Bool
}

func startBlackholeProxy(t *testing.T, upstream string) *blackholeProxy {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	p := &blackholeProxy{addr: ln.Addr().String()}

	var mu sync.Mutex
	var conns []net.Conn
	track := func(c net.Conn) {
		mu.Lock()
		conns = append(conns, c)
		mu.Unlock()
	}
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
	})

	go func() {
		for {
			client, err := ln.Accept()
			if err != nil {
				return
			}
			server, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(context.Background(), "tcp", upstream)
			if err != nil {
				_ = client.Close()
				continue
			}
			track(client)
			track(server)
			go p.pipe(server, client)
			go p.pipe(client, server)
		}
	}()
	return p
}

func (p *blackholeProxy) pipe(dst, src net.Conn) {
	buf := make([]byte, 32*1024)
	for {
		n, err := src.Read(buf)
		if n > 0 && !p.blackhole.Load() {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// TestNewClient_CallerHandlersStillRunAndBusUpSinceStillUpdates: a caller's
// own reconnect and disconnect handlers are chained, not replaced by, the
// ones that maintain BusUpSince (nats.go keeps only the last of each kind).
func TestNewClient_CallerHandlersStillRunAndBusUpSinceStillUpdates(t *testing.T) {
	port := freePort(t)
	srv := startPlainServer(t, "127.0.0.1", port)
	defer func() { srv.Shutdown() }()

	var disconnects, reconnects atomic.Int32
	c, err := NewClient(
		"nats://"+net.JoinHostPort("127.0.0.1", itoa(port)), slog.New(slog.DiscardHandler),
		nats.DisconnectErrHandler(func(*nats.Conn, error) { disconnects.Add(1) }),
		nats.ReconnectHandler(func(*nats.Conn) { reconnects.Add(1) }),
	)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(c.Close)

	srv.Shutdown()
	waitUntil(t, 5*time.Second, "BusUpSince to clear", func() bool { return c.BusUpSince().IsZero() })
	waitUntil(t, 5*time.Second, "the caller's disconnect handler", func() bool { return disconnects.Load() == 1 })

	srv = startPlainServer(t, "127.0.0.1", port)
	waitUntil(t, 15*time.Second, "BusUpSince to be set again", func() bool { return !c.BusUpSince().IsZero() })
	waitUntil(t, 5*time.Second, "the caller's reconnect handler", func() bool { return reconnects.Load() == 1 })
}

// TestClientState_InitialUpDoesNotUndoAnEarlierDisconnect pins the
// construction race: a disconnect handled between nats.Connect returning and
// the constructor's own bookkeeping must not be overwritten by "up".
func TestClientState_InitialUpDoesNotUndoAnEarlierDisconnect(t *testing.T) {
	c := &Client{}
	c.markDown()
	c.markInitialUp()
	if !c.BusUpSince().IsZero() {
		t.Error("markInitialUp overwrote a disconnect that had already been recorded")
	}

	c = &Client{}
	c.markInitialUp()
	if c.BusUpSince().IsZero() {
		t.Error("markInitialUp recorded nothing on a client with no earlier events")
	}
}

// TestNewClient_PingSettingsDetectAPartitionWithinAWorkerTimeout pins the
// options the silent-partition detection relies on.
func TestNewClient_PingSettingsDetectAPartitionWithinAWorkerTimeout(t *testing.T) {
	port := freePort(t)
	srv := startPlainServer(t, "127.0.0.1", port)
	defer srv.Shutdown()
	c, err := NewClient("nats://"+net.JoinHostPort("127.0.0.1", itoa(port)), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(c.Close)

	o := c.nc.Opts
	if o.PingInterval != 5*time.Second || o.MaxPingsOut != 2 {
		t.Errorf("PingInterval/MaxPingsOut = %v/%d, want 5s/2", o.PingInterval, o.MaxPingsOut)
	}
}
