// SPDX-License-Identifier: AGPL-3.0-or-later

// Package testport hands tests loopback ports a server can bind again after
// it stops: a broker or node restarted on its old port must find that port
// free while it is down.
//
// A port in the operating system's ephemeral range is not: any outgoing
// connection can be given it as its source port, and many tests running at
// once open thousands of them. The ports here are drawn from below every
// default ephemeral range (Linux 32768 to 60999, Windows and macOS 49152 to
// 65535), so only a listener can take one.
//
// It is a test helper. Production code must not import it.
package testport

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net"
	"strconv"
	"testing"
)

// RangeLow and RangeHigh bound the ports [Free] hands out.
const (
	RangeLow  = 20000
	RangeHigh = 32768
)

// Free reserves n distinct loopback ports in [RangeLow, RangeHigh) and
// releases them. Another process can still bind one before the caller does.
func Free(t testing.TB, n int) []int {
	t.Helper()
	ports, err := Pick(n, RangeLow, RangeHigh)
	if err != nil {
		t.Fatalf("testport: reserve ports: %v", err)
	}
	return ports
}

// Pick finds n distinct loopback ports in [lo, hi) that can be bound now,
// trying them from a random starting point so tests running at once rarely
// try the same ones. It holds each port until all n are found.
func Pick(n, lo, hi int) ([]int, error) {
	size := hi - lo
	var listeners []net.Listener
	defer func() {
		for _, l := range listeners {
			l.Close()
		}
	}()
	ports := make([]int, 0, n)
	first := rand.IntN(size) //nolint:gosec // spreads tests over the range; nothing depends on it being unpredictable
	for i := 0; i < size && len(ports) < n; i++ {
		port := lo + (first+i)%size
		l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			continue
		}
		listeners = append(listeners, l)
		ports = append(ports, port)
	}
	if len(ports) < n {
		return nil, fmt.Errorf("%d of %d ports free in [%d, %d)", len(ports), n, lo, hi)
	}
	return ports, nil
}
