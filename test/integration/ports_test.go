// SPDX-License-Identifier: AGPL-3.0-or-later

package integration

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

// openPort0 listens on a loopback port the OS chooses and closes it when the
// test ends.
func openPort0(t *testing.T) net.Listener {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

// assertClosed fails the test unless ln is closed. It does not block on an
// open listener: Accept times out at once instead.
func assertClosed(t *testing.T, ln net.Listener) {
	t.Helper()
	tcp, ok := ln.(*net.TCPListener)
	if !ok {
		t.Fatalf("unexpected listener type %T", ln)
	}
	_ = tcp.SetDeadline(time.Now()) //nolint:errcheck // a closed listener refuses it, and Accept then says so
	if _, err := tcp.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("listener %s left open (Accept: %v)", ln.Addr(), err)
	}
}

func portOf(t *testing.T, ln net.Listener) int {
	t.Helper()
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("unexpected address type %T", ln.Addr())
	}
	return addr.Port
}

// A listener the harness keeps open must not take a port freePort has handed
// out: that port is released until its server binds it.
func TestListenClaimed_SkipsAPortAlreadyHandedOut(t *testing.T) {
	taken := openPort0(t)
	if !reservePort(portOf(t, taken)) {
		t.Fatalf("port %d was already reserved", portOf(t, taken))
	}
	fresh := openPort0(t)
	queue := []net.Listener{taken, fresh}
	listen := func() (net.Listener, error) {
		ln := queue[0]
		queue = queue[1:]
		return ln, nil
	}

	got, err := listenClaimed(listen)
	if err != nil {
		t.Fatalf("listenClaimed: %v", err)
	}
	if got != fresh {
		t.Fatalf("listenClaimed returned %s, want the listener on a port not handed out (%s)", got.Addr(), fresh.Addr())
	}
	assertClosed(t, taken)
	if reservePort(portOf(t, fresh)) {
		t.Fatalf("the kept listener's port %d was not reserved", portOf(t, fresh))
	}
}

// freePort must not hand out a port a kept listener holds.
func TestClaimPort_SkipsAPortAListenerHolds(t *testing.T) {
	held, err := listenClaimed(func() (net.Listener, error) { return openPort0(t), nil })
	if err != nil {
		t.Fatalf("listenClaimed: %v", err)
	}
	const unused = 7 // below the ephemeral range, so no listener here holds it
	draws := []int{portOf(t, held), unused}
	draw := func() (int, error) {
		p := draws[0]
		draws = draws[1:]
		return p, nil
	}

	got, err := claimPort(draw)
	if err != nil {
		t.Fatalf("claimPort: %v", err)
	}
	if got != unused {
		t.Fatalf("claimPort returned %d, want %d: %d is held by a listener", got, unused, portOf(t, held))
	}
}

func TestClaimPort_FailsWhenEveryDrawIsTaken(t *testing.T) {
	held := openPort0(t)
	reservePort(portOf(t, held))
	_, err := claimPort(func() (int, error) { return portOf(t, held), nil })
	if err == nil {
		t.Fatal("claimPort succeeded although every draw returned a reserved port")
	}
}

func TestListenClaimed_ClosesEveryRejectedListener(t *testing.T) {
	var opened []net.Listener
	listen := func() (net.Listener, error) {
		ln := openPort0(t)
		reservePort(portOf(t, ln))
		opened = append(opened, ln)
		return ln, nil
	}
	if _, err := listenClaimed(listen); err == nil {
		t.Fatal("listenClaimed succeeded although every port was reserved")
	}
	for _, ln := range opened {
		assertClosed(t, ln)
	}
}
