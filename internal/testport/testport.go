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
// Each port handed out is also reserved, by a lock file under the temporary
// directory, until the test that took it ends. Another caller, in this test
// binary or another one running at the same time, skips a reserved port, and
// never binds it to check it is free, so a port stays the test's while its
// server is down.
//
// It is a test helper. Production code must not import it.
package testport

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/uberware/sqi/internal/fslock"
)

// RangeLow and RangeHigh bound the ports [Free] hands out.
const (
	RangeLow  = 20000
	RangeHigh = 32768
)

// Free reserves n distinct loopback ports in [RangeLow, RangeHigh) that can be
// bound now, until t ends. Nothing listens on them: the caller binds them.
func Free(t testing.TB, n int) []int {
	t.Helper()
	ports, release, err := reserve(n, RangeLow, RangeHigh)
	if err != nil {
		t.Fatalf("testport: reserve ports: %v", err)
	}
	t.Cleanup(release)
	return ports
}

// reserve finds n distinct loopback ports in [lo, hi) that no other caller
// has reserved and that can be bound now, and reserves them until release is
// called. It tries them from a random starting point so callers running at
// once rarely try the same ones.
func reserve(n, lo, hi int) (ports []int, release func(), err error) {
	dir, err := lockDir()
	if err != nil {
		return nil, nil, err
	}
	var locks []*fslock.Lock
	release = func() {
		for _, l := range locks {
			l.Release() //nolint:errcheck // the lock is dropped when the file closes, which Release always does
		}
	}
	size := hi - lo
	first := rand.IntN(size) //nolint:gosec // spreads callers over the range; nothing depends on it being unpredictable
	for i := 0; i < size && len(ports) < n; i++ {
		port := lo + (first+i)%size
		lock, err := fslock.Acquire(filepath.Join(dir, strconv.Itoa(port)+".lock"))
		if err != nil {
			continue
		}
		if !bindable(port) {
			lock.Release() //nolint:errcheck // as above
			continue
		}
		locks = append(locks, lock)
		ports = append(ports, port)
	}
	if len(ports) < n {
		release()
		return nil, nil, fmt.Errorf("%d of %d ports free in [%d, %d)", len(ports), n, lo, hi)
	}
	return ports, release, nil
}

// bindable reports whether a listener can bind port now.
func bindable(port int) bool {
	l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return false
	}
	l.Close()
	return true
}

// lockDir is the directory of the reservation lock files, one per user so a
// directory another user created cannot lock this one out.
func lockDir() (string, error) {
	name := "sqi-testport"
	if uid := os.Getuid(); uid >= 0 {
		name += "-" + strconv.Itoa(uid)
	}
	dir := filepath.Join(os.TempDir(), name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("testport: %w", err)
	}
	return dir, nil
}
