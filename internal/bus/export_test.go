// SPDX-License-Identifier: AGPL-3.0-or-later

package bus

import (
	"context"
	"log/slog"
	"testing"
	"time"

	nats "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// AssertWorkerGrants is [assertWorkerGrants], for the external test package:
// the forgery checks run unchanged against both broker modes.
var AssertWorkerGrants = assertWorkerGrants

// SetBrokerVersion makes the remote broker read the broker's version through
// f instead of from INFO, until the test ends. Tests using it must not run in
// parallel with other remote-broker tests.
func SetBrokerVersion(t testing.TB, f func(*nats.Conn) string) {
	t.Helper()
	prev := brokerVersion
	brokerVersion = f
	t.Cleanup(func() { brokerVersion = prev })
}

// CloseRemoteCallout closes r's callout connection, as a lost connection
// that will not come back would leave it.
func CloseRemoteCallout(r *Remote) {
	r.mu.Lock()
	nc := r.callout
	r.mu.Unlock()
	if nc != nil {
		nc.Close()
	}
}

// CloseRemoteSystem closes r's system connection, as a lost connection that
// will not come back would leave it.
func CloseRemoteSystem(r *Remote) {
	r.mu.Lock()
	nc := r.system
	r.mu.Unlock()
	if nc != nil {
		nc.Close()
	}
}

// SetRemoteSystemSubjects makes r's Revoke send its CONNZ and VARZ requests
// to connz and varz, so a test can stand in a subject no node answers.
func SetRemoteSystemSubjects(r *Remote, connz, varz string) {
	r.mu.Lock()
	r.connzSubject, r.varzSubject = connz, varz
	r.mu.Unlock()
}

// Remote's system request subjects, for tests that restore one.
const (
	ConnzPingSubject = connzPingSubject
	VarzPingSubject  = varzPingSubject
)

// The guard's timing, shortened so its tests run in seconds. Set before any
// test starts; the refresh goroutines only read them.
func init() {
	guardTTL = 3 * time.Second
	guardRefresh = time.Second
}

// GuardTTL is the TTL the guard bucket uses in tests.
func GuardTTL() time.Duration { return guardTTL }

// PauseGuardRefresh stops g from refreshing, as a process frozen past the TTL
// would, until the returned function is called.
func PauseGuardRefresh(g *Guard) (resume func()) {
	g.pause.Lock()
	return g.pause.Unlock
}

// StopGuardRefresh ends g's refresh without releasing the key, as a killed
// process would.
func StopGuardRefresh(g *Guard) { g.stop() }

// DisableRemoteGuard makes r skip the one-server-per-account guard at Start,
// for a test that runs two servers on one cluster.
func DisableRemoteGuard(r *Remote) { r.noGuard = true }

// RemoteGuard is r's account guard, nil before Start.
func RemoteGuard(r *Remote) *Guard {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.guard
}

// RestoreLockGuard is l's account guard.
func RestoreLockGuard(l *RestoreLock) *Guard { return l.guard }

// AcquireGuard is acquireGuard as a server takes it, reconciling the bucket.
func AcquireGuard(ctx context.Context, js jetstream.JetStream, replicas int, h GuardHolder, logger *slog.Logger) (*Guard, error) {
	return acquireGuard(ctx, js, replicas, h, logger, true, GuardOptions{})
}

// AcquireGuardWith is AcquireGuard with opts.
func AcquireGuardWith(ctx context.Context, js jetstream.JetStream, replicas int, h GuardHolder, logger *slog.Logger, opts GuardOptions) (*Guard, error) {
	return acquireGuard(ctx, js, replicas, h, logger, true, opts)
}
