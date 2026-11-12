// SPDX-License-Identifier: AGPL-3.0-or-later

package netfault

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// echoServer echoes every connection until closed. accepted counts connections.
type echoServer struct {
	ln       net.Listener
	accepted atomic.Int64
	wg       sync.WaitGroup
}

func listenEcho(t *testing.T, addr string) *echoServer {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", addr)
	if err != nil {
		t.Fatalf("echo listen: %v", err)
	}
	s := &echoServer{ln: ln}
	s.wg.Go(func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			s.accepted.Add(1)
			s.wg.Go(func() {
				defer c.Close()
				_, _ = io.Copy(c, c) //nolint:errcheck // ends when the peer closes
			})
		}
	})
	t.Cleanup(s.close)
	return s
}

func (s *echoServer) addr() string { return s.ln.Addr().String() }

func (s *echoServer) close() {
	_ = s.ln.Close()
	s.wg.Wait()
}

func newProxy(t *testing.T, target string) *Proxy {
	t.Helper()
	p, err := Listen(target, 1)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

func dial(t *testing.T, addr string) net.Conn {
	t.Helper()
	c, err := (&net.Dialer{Timeout: 2 * time.Second}).DialContext(context.Background(), "tcp", addr)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func echoOnce(t *testing.T, c net.Conn, msg string, within time.Duration) error {
	t.Helper()
	setDeadline(t, c, within)
	if _, err := c.Write([]byte(msg)); err != nil {
		return err
	}
	got := make([]byte, len(msg))
	if _, err := io.ReadFull(c, got); err != nil {
		return err
	}
	if string(got) != msg {
		t.Fatalf("echoed %q, want %q", got, msg)
	}
	return nil
}

func TestProxy_PassThrough(t *testing.T) {
	e := listenEcho(t, "127.0.0.1:0")
	p := newProxy(t, e.addr())
	c := dial(t, p.Addr())
	if err := echoOnce(t, c, "hello", 2*time.Second); err != nil {
		t.Fatalf("echo: %v", err)
	}
}

func TestProxy_CutClosesAndRefuses(t *testing.T) {
	e := listenEcho(t, "127.0.0.1:0")
	p := newProxy(t, e.addr())
	c := dial(t, p.Addr())
	if err := echoOnce(t, c, "hi", 2*time.Second); err != nil {
		t.Fatalf("echo: %v", err)
	}

	p.Cut()
	setReadDeadline(t, c, time.Second)
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("read on a cut connection returned no error")
	} else if isTimeout(err) {
		t.Fatalf("a cut connection was not closed within 1s: %v", err)
	}

	c2 := dial(t, p.Addr())
	setReadDeadline(t, c2, time.Second)
	if _, err := c2.Read(make([]byte, 1)); err == nil {
		t.Fatal("a connection made while cut stayed open")
	} else if isTimeout(err) {
		t.Fatalf("a connection made while cut was not closed within 1s: %v", err)
	}

	p.Heal()
	c3 := dial(t, p.Addr())
	if err := echoOnce(t, c3, "back", 2*time.Second); err != nil {
		t.Fatalf("echo after Heal: %v", err)
	}
}

func TestProxy_BlackholeHoldsThenDelivers(t *testing.T) {
	e := listenEcho(t, "127.0.0.1:0")
	p := newProxy(t, e.addr())
	c := dial(t, p.Addr())
	if err := echoOnce(t, c, "warm", 2*time.Second); err != nil {
		t.Fatalf("echo: %v", err)
	}

	p.Blackhole()
	if _, err := c.Write([]byte("held")); err != nil {
		t.Fatalf("write: %v", err)
	}
	setReadDeadline(t, c, 500*time.Millisecond)
	if n, err := c.Read(make([]byte, 4)); err == nil {
		t.Fatalf("read %d bytes through a blackhole", n)
	} else if !isTimeout(err) {
		t.Fatalf("the blackholed connection was closed: %v", err)
	}

	p.Heal()
	setReadDeadline(t, c, time.Second)
	got := make([]byte, 4)
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatalf("held data not delivered after Heal: %v", err)
	}
	if string(got) != "held" {
		t.Fatalf("delivered %q, want held", got)
	}
}

func TestProxy_BlackholeAcceptsThenDialsOnHeal(t *testing.T) {
	e := listenEcho(t, "127.0.0.1:0")
	p := newProxy(t, e.addr())

	p.Blackhole()
	c := dial(t, p.Addr())
	if _, err := c.Write([]byte("early")); err != nil {
		t.Fatalf("write: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	if n := e.accepted.Load(); n != 0 {
		t.Fatalf("target saw %d connections while the proxy was blackholed", n)
	}

	p.Heal()
	setReadDeadline(t, c, 2*time.Second)
	got := make([]byte, 5)
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatalf("a connection accepted during the blackhole was not served after Heal: %v", err)
	}
	if string(got) != "early" {
		t.Fatalf("delivered %q, want early", got)
	}
	if n := e.accepted.Load(); n != 1 {
		t.Fatalf("target saw %d connections, want 1", n)
	}
}

func TestProxy_BlackholeThenDialFailureCloses(t *testing.T) {
	e := listenEcho(t, "127.0.0.1:0")
	addr := e.addr()
	p := newProxy(t, addr)

	p.Blackhole()
	c := dial(t, p.Addr())
	e.close()
	p.Heal()
	setReadDeadline(t, c, 2*time.Second)
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("read returned data from a target that is down")
	} else if isTimeout(err) {
		t.Fatalf("a connection whose dial failed on Heal was not closed: %v", err)
	}
}

func TestProxy_ShapeDelays(t *testing.T) {
	e := listenEcho(t, "127.0.0.1:0")
	p := newProxy(t, e.addr())
	c := dial(t, p.Addr())
	if err := echoOnce(t, c, "warm", 2*time.Second); err != nil {
		t.Fatalf("echo: %v", err)
	}

	p.Shape(200*time.Millisecond, 0, 0)
	start := time.Now()
	if err := echoOnce(t, c, "slow", 3*time.Second); err != nil {
		t.Fatalf("echo: %v", err)
	}
	// The lower bound proves the delay: the chunk is held 200ms each way. The
	// upper bound only catches a stuck link, so it is generous enough for the
	// race detector on a loaded shared runner.
	if d := time.Since(start); d < 400*time.Millisecond || d >= 3*time.Second {
		t.Fatalf("round trip took %v, want in [400ms, 3s)", d)
	}
}

func TestProxy_ShapePreservesOrder(t *testing.T) {
	e := listenEcho(t, "127.0.0.1:0")
	p := newProxy(t, e.addr())
	p.Shape(50*time.Millisecond, 40*time.Millisecond, 0)
	c := dial(t, p.Addr())
	setDeadline(t, c, 10*time.Second)

	const n = 100
	errc := make(chan error, 1)
	go func() {
		for i := range n {
			if _, err := fmt.Fprintf(c, "line %d\n", i); err != nil {
				errc <- err
				return
			}
			time.Sleep(time.Millisecond)
		}
		errc <- nil
	}()
	r := bufio.NewReader(c)
	for i := range n {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("read line %d: %v", i, err)
		}
		if want := fmt.Sprintf("line %d\n", i); line != want {
			t.Fatalf("line %d = %q, want %q", i, line, want)
		}
	}
	if err := <-errc; err != nil {
		t.Fatalf("write: %v", err)
	}
}

func TestProxy_BandwidthCap(t *testing.T) {
	e := listenEcho(t, "127.0.0.1:0")
	p := newProxy(t, e.addr())
	p.Shape(0, 0, 64*1024)
	c := dial(t, p.Addr())
	setDeadline(t, c, 30*time.Second)

	const size = 256 * 1024
	payload := make([]byte, size)
	for i := range payload {
		payload[i] = byte(i)
	}
	start := time.Now()
	werr := make(chan error, 1)
	go func() {
		_, err := c.Write(payload)
		werr <- err
	}()
	got := make([]byte, size)
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := <-werr; err != nil {
		t.Fatalf("write: %v", err)
	}
	if d := time.Since(start); d < 3*time.Second {
		t.Fatalf("256 KiB through a 64 KiB/s cap took %v, want >= 3s", d)
	}
	for i := range got {
		if got[i] != payload[i] {
			t.Fatalf("byte %d corrupted", i)
		}
	}
}

func TestProxy_TargetDownThenUp(t *testing.T) {
	e := listenEcho(t, "127.0.0.1:0")
	addr := e.addr()
	p := newProxy(t, addr)
	e.close()

	c := dial(t, p.Addr())
	setReadDeadline(t, c, 2*time.Second)
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("read returned data with the target down")
	} else if isTimeout(err) {
		t.Fatalf("a connection to a down target was not closed promptly: %v", err)
	}

	listenEcho(t, addr)
	c2 := dial(t, p.Addr())
	if err := echoOnce(t, c2, "up", 2*time.Second); err != nil {
		t.Fatalf("echo after the target came back: %v", err)
	}
}

func TestProxy_CloseEndsEverything(t *testing.T) {
	e := listenEcho(t, "127.0.0.1:0")
	p, err := Listen(e.addr(), 1)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	addr := p.Addr()
	c := dial(t, addr)
	if err := echoOnce(t, c, "x", 2*time.Second); err != nil {
		t.Fatalf("echo: %v", err)
	}
	// A parked connection (blackholed, never dialed) must end too.
	p.Blackhole()
	parked := dial(t, addr)

	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	for name, cc := range map[string]net.Conn{"carried": c, "parked": parked} {
		setReadDeadline(t, cc, time.Second)
		if _, err := cc.Read(make([]byte, 1)); err == nil {
			t.Fatalf("%s: read after Close returned data", name)
		} else if isTimeout(err) {
			t.Fatalf("%s: connection survived Close: %v", name, err)
		}
	}
	if nc, err := (&net.Dialer{Timeout: time.Second}).DialContext(context.Background(), "tcp", addr); err == nil {
		_ = nc.Close()
		t.Fatal("Addr still accepts after Close")
	}
	if err := p.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestProxy_CloseLeaksNoGoroutines(t *testing.T) {
	e := listenEcho(t, "127.0.0.1:0")
	before := runtime.NumGoroutine()

	p, err := Listen(e.addr(), 1)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	p.Shape(20*time.Millisecond, 10*time.Millisecond, 0)
	var conns []net.Conn
	for range 5 {
		c := dial(t, p.Addr())
		if err := echoOnce(t, c, "x", 2*time.Second); err != nil {
			t.Fatalf("echo: %v", err)
		}
		conns = append(conns, c)
	}
	p.Blackhole()
	for _, c := range conns {
		_, _ = c.Write([]byte("held")) //nolint:errcheck // the write is only to have data to hold
	}
	conns = append(conns, dial(t, p.Addr())) // parked, never dialed
	time.Sleep(50 * time.Millisecond)
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	for _, c := range conns {
		_ = c.Close()
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		// The echo server's per-connection goroutines end once the proxy's
		// target side closes.
		if n := runtime.NumGoroutine(); n <= before {
			return
		}
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<16)
			buf = buf[:runtime.Stack(buf, true)]
			t.Fatalf("goroutines: %d before, %d after Close\n%s", before, runtime.NumGoroutine(), buf)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func setDeadline(t *testing.T, c net.Conn, d time.Duration) {
	t.Helper()
	if err := c.SetDeadline(time.Now().Add(d)); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}
}

func setReadDeadline(t *testing.T, c net.Conn, d time.Duration) {
	t.Helper()
	if err := c.SetReadDeadline(time.Now().Add(d)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
}

// TestProxy_BlackholeHoldsTeardown checks that a partition hides one side
// closing, whether cleanly or by reset, from the other until Heal.
func TestProxy_BlackholeHoldsTeardown(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reset bool
	}{{"close", false}, {"reset", true}} {
		t.Run(tc.name, func(t *testing.T) {
			ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatalf("listen: %v", err)
			}
			t.Cleanup(func() { _ = ln.Close() })
			ended := make(chan error, 1)
			ready := make(chan struct{})
			go func() {
				c, err := ln.Accept()
				if err != nil {
					ended <- err
					return
				}
				defer c.Close()
				close(ready)
				_, err = io.Copy(io.Discard, c) // returns nil on EOF
				ended <- err
			}()

			p := newProxy(t, ln.Addr().String())
			c := dial(t, p.Addr())
			select {
			case <-ready:
			case <-time.After(2 * time.Second):
				t.Fatal("the target never saw the connection")
			}

			p.Blackhole()
			if tc.reset {
				tcp, ok := c.(*net.TCPConn)
				if !ok {
					t.Fatalf("dialed %T, want *net.TCPConn", c)
				}
				if err := tcp.SetLinger(0); err != nil {
					t.Fatalf("SetLinger: %v", err)
				}
			}
			_ = c.Close()
			select {
			case err := <-ended:
				t.Fatalf("the target saw the client's teardown through a blackhole (err=%v)", err)
			case <-time.After(500 * time.Millisecond):
			}

			p.Heal()
			select {
			case <-ended:
			case <-time.After(2 * time.Second):
				t.Fatal("the teardown was not delivered after Heal")
			}
		})
	}
}
