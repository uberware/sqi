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

// TestPick_SkipsBusyPorts pins that a port something already holds is not
// handed out, and that a range with too few free ports is an error.
func TestPick_SkipsBusyPorts(t *testing.T) {
	ports := Free(t, 1)
	l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(ports[0])))
	if err != nil {
		t.Fatalf("occupy port: %v", err)
	}
	t.Cleanup(func() { l.Close() })
	if got, err := Pick(1, ports[0], ports[0]+1); err == nil {
		t.Fatalf("Pick on a busy one-port range = %v, want an error", got)
	}
}
