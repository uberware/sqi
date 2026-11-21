// SPDX-License-Identifier: AGPL-3.0-or-later

package testport

import (
	"context"
	"net"
	"strconv"
	"testing"
)

// TestFree_BelowEphemeralRange pins where the ports come from: outside every
// default ephemeral range, where an outgoing connection cannot take one.
func TestFree_BelowEphemeralRange(t *testing.T) {
	for _, p := range Free(t, 20) {
		if p < RangeLow || p >= RangeHigh {
			t.Errorf("port %d is outside [%d, %d)", p, RangeLow, RangeHigh)
		}
	}
}

// TestReserve_SkipsBusyPorts pins that a port something already listens on is
// not handed out, and that a range with too few free ports is an error.
func TestReserve_SkipsBusyPorts(t *testing.T) {
	ports, release, err := reserve(1, RangeLow, RangeHigh)
	if err != nil {
		t.Fatal(err)
	}
	release()
	l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(ports[0])))
	if err != nil {
		t.Fatalf("occupy port: %v", err)
	}
	t.Cleanup(func() { l.Close() })
	if got, release, err := reserve(1, ports[0], ports[0]+1); err == nil {
		release()
		t.Fatalf("reserve on a busy one-port range = %v, want an error", got)
	}
}

// TestFree_KeepsPortsReservedUntilCleanup pins that a port stays the caller's
// while nothing listens on it, as when a node it was given is restarted: no
// other caller, in this process or another, is handed it until the test that
// reserved it ends.
func TestFree_KeepsPortsReservedUntilCleanup(t *testing.T) {
	var port int
	t.Run("holder", func(t *testing.T) {
		port = Free(t, 1)[0]
		if got, release, err := reserve(1, port, port+1); err == nil {
			release()
			t.Errorf("reserve of a port Free handed out = %v, want an error while its test runs", got)
		}
	})
	got, release, err := reserve(1, port, port+1)
	if err != nil {
		t.Fatalf("reserve after the holder's test ended: %v", err)
	}
	release()
	if got[0] != port {
		t.Errorf("reserve = %v, want [%d]", got, port)
	}
}
