// SPDX-License-Identifier: AGPL-3.0-or-later

package bustest

import (
	"context"
	"net"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// TestStartCluster_RetriesOnPortCollision reproduces the race freePorts
// documents: a port reserved for a node is taken before the node binds it.
// StartCluster must notice the node's bind failure promptly, rather than
// waiting out readyTimeout, and come up on fresh ports.
func TestStartCluster_RetriesOnPortCollision(t *testing.T) {
	var squatter net.Listener
	var calls atomic.Int32
	orig := reservePorts
	t.Cleanup(func() { reservePorts = orig })
	reservePorts = func(t testing.TB, n int) []int {
		ports := orig(t, n)
		// Only the first attempt's client ports collide.
		if calls.Add(1) == 1 {
			l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(ports[0])))
			if err != nil {
				t.Fatalf("occupy port: %v", err)
			}
			squatter = l
			t.Cleanup(func() { l.Close() })
		}
		return ports
	}

	start := time.Now()
	c := StartCluster(t, 3, Options{})
	if squatter == nil {
		t.Fatal("collision was never injected")
	}
	if got := calls.Load(); got < 3 {
		t.Fatalf("reservePorts called %d times, want a second attempt's reservations too", got)
	}
	if took := time.Since(start); took > readyTimeout/2 {
		t.Fatalf("recovered after %s; the bind failure was not detected promptly", took)
	}
	if len(c.URLs) != 3 || len(c.nodes) != 3 {
		t.Fatalf("cluster has %d URLs and %d nodes, want 3 of each", len(c.URLs), len(c.nodes))
	}
}
