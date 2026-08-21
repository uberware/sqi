// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/bus"
	workerconfig "github.com/uberware/sqi/internal/worker/config"
)

// freeTestPort asks the OS for an unused loopback TCP port, for booting a
// throwaway embedded broker.
func freeTestPort(t *testing.T) int {
	t.Helper()
	var lc net.ListenConfig
	l, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freeTestPort: listen: %v", err)
	}
	defer func() { _ = l.Close() }()
	addr, ok := l.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("freeTestPort: listener address is %T, want *net.TCPAddr", l.Addr())
	}
	return addr.Port
}

// startEmbeddedBroker boots a real embedded NATS broker on a throwaway
// loopback port and JetStream dir, with or without auth, and registers
// cleanup. A real broker is used rather than a fake so these tests exercise
// the actual nkey handshake, matching how [connectToBroker] behaves in
// production.
func startEmbeddedBroker(t *testing.T, auth bus.BrokerAuthConfig) *bus.Broker {
	t.Helper()
	b := bus.New(bus.BrokerConfig{
		Addr:    net.JoinHostPort("127.0.0.1", strconv.Itoa(freeTestPort(t))),
		DataDir: t.TempDir() + "/nats",
		Auth:    auth,
	}, slog.New(slog.DiscardHandler))

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("startEmbeddedBroker: Start: %v", err)
	}
	t.Cleanup(b.Shutdown)
	return b
}

// TestConnectToBroker_AuthOffFarmBootsWithNoCredential exercises the exact
// boot-time path runStart takes on the default, auth-off configuration: a
// worker with no credential file and no join token configured must still
// connect, exactly as it did before broker authentication existed. This is
// the regression this feature is not allowed to introduce.
func TestConnectToBroker_AuthOffFarmBootsWithNoCredential(t *testing.T) {
	b := startEmbeddedBroker(t, bus.BrokerAuthConfig{Enabled: false})

	var cfg workerconfig.WorkerConfig
	cfg.NATS.URL = b.ClientURL()
	cfg.NATS.CredentialFile = t.TempDir() + "/worker.nk" // deliberately does not exist
	cfg.NATS.MaxReconnectAttempts = 0
	cfg.NATS.ReconnectWait = 10 * time.Millisecond

	logger := slog.New(slog.DiscardHandler)
	nc, _, err := connectToBroker(context.Background(), cfg, "worker-a", logger)
	if err != nil {
		t.Fatalf("connectToBroker on an auth-off farm with no credential: %v", err)
	}
	defer nc.Close()
	if !nc.IsConnected() {
		t.Error("connection is not in the connected state")
	}
}

// TestConnectToBroker_AuthOnFarmWithNoCredentialNamesBothRemediations covers
// the case the boot-sequence exit message in cmd/sqi-worker/start.go exists
// for: a worker with neither a credential file nor a join token, against a
// broker that actually requires authentication. The failure must be fatal
// and must name both ways an operator can fix it.
func TestConnectToBroker_AuthOnFarmWithNoCredentialNamesBothRemediations(t *testing.T) {
	b := startEmbeddedBroker(t, bus.BrokerAuthConfig{Enabled: true})

	var cfg workerconfig.WorkerConfig
	cfg.NATS.URL = b.ClientURL()
	cfg.NATS.CredentialFile = t.TempDir() + "/worker.nk"
	cfg.NATS.MaxReconnectAttempts = 0
	cfg.NATS.ReconnectWait = 10 * time.Millisecond

	logger := slog.New(slog.DiscardHandler)
	_, _, err := connectToBroker(context.Background(), cfg, "worker-a", logger)
	if err == nil {
		t.Fatal("connectToBroker: want error against an auth-on broker with no credential, got nil")
	}
	if !strings.Contains(err.Error(), "no credential was found") {
		t.Errorf("error %q does not say no credential was found", err.Error())
	}
	if !strings.Contains(err.Error(), "sqi-worker keygen") {
		t.Errorf("error %q does not mention pre-provisioning a key", err.Error())
	}
	if !strings.Contains(err.Error(), "sqi-server worker token issue") {
		t.Errorf("error %q does not mention obtaining a join token", err.Error())
	}
}
