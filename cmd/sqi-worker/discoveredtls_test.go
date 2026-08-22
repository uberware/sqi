// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	workerconfig "github.com/uberware/sqi/internal/worker/config"
	workerdiscovery "github.com/uberware/sqi/internal/worker/discovery"
)

// applyDiscoveredTLS is what makes nats.tls_enabled's "auto" mode able to see a
// TLS-required broker. Before it existed the server advertised tls=1 /
// nats_tls=1 over mDNS and nothing read them, so a discovering worker attempted
// plaintext and failed with nats.go's "secure connection not available" —
// naming neither the cause nor the setting that fixes it.

func applyFixture(t *testing.T, cfg workerconfig.NATSConfig, found workerdiscovery.Result) (workerconfig.NATSConfig, string) {
	t.Helper()
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	wc := workerconfig.WorkerConfig{NATS: cfg}
	applyDiscoveredTLS(context.Background(), &wc, found, logger)
	return wc.NATS, buf.String()
}

func TestApplyDiscoveredTLS_AutoEnablesOnAdvertisedBroker(t *testing.T) {
	got, logs := applyFixture(
		t,
		workerconfig.NATSConfig{TLSEnabled: workerconfig.TLSAuto},
		workerdiscovery.Result{NATSURL: "nats://srv:4222", NATSTLS: true},
	)
	if !got.UseTLS(false) {
		t.Error("auto did not enable TLS against a broker advertising nats_tls=1")
	}
	if !strings.Contains(logs, "enabling TLS") {
		t.Errorf("no log explained the switch:\n%s", logs)
	}
	// With no CA the system roots are used, which cannot verify a farm CA.
	// Saying so up front beats an unexplained verification failure.
	if !strings.Contains(logs, "nats.tls_ca_file") {
		t.Errorf("no log warned about the missing CA:\n%s", logs)
	}
}

func TestApplyDiscoveredTLS_AutoWithCAStaysQuietAboutTheCA(t *testing.T) {
	_, logs := applyFixture(
		t,
		workerconfig.NATSConfig{TLSEnabled: workerconfig.TLSAuto, TLSCAFile: "/etc/sqi/ca.crt"},
		workerdiscovery.Result{NATSURL: "nats://srv:4222", NATSTLS: true},
	)
	if strings.Contains(logs, "no nats.tls_ca_file is configured") {
		t.Errorf("warned about a missing CA when one is configured:\n%s", logs)
	}
}

func TestApplyDiscoveredTLS_ForcedOffIsHonoredButExplained(t *testing.T) {
	got, logs := applyFixture(
		t,
		workerconfig.NATSConfig{TLSEnabled: workerconfig.TLSOff},
		workerdiscovery.Result{NATSURL: "nats://srv:4222", NATSTLS: true},
	)
	// "false" means false: discovery must not override an explicit decision.
	if got.TLSEnabled != workerconfig.TLSOff || got.UseTLS(true) {
		t.Error("discovery overrode an explicit nats.tls_enabled=false")
	}
	if !strings.Contains(logs, "will be refused") {
		t.Errorf("no log warned that the connection is about to fail:\n%s", logs)
	}
}

func TestApplyDiscoveredTLS_PlaintextServerChangesNothing(t *testing.T) {
	// The default path: a plaintext farm must be untouched.
	got, logs := applyFixture(
		t,
		workerconfig.NATSConfig{TLSEnabled: workerconfig.TLSAuto},
		workerdiscovery.Result{NATSURL: "nats://srv:4222"},
	)
	if got.UseTLS(false) {
		t.Error("TLS was enabled against a plaintext server")
	}
	if logs != "" {
		t.Errorf("a plaintext discovery produced output:\n%s", logs)
	}
}

func TestApplyDiscoveredTLS_ExplicitURLIsANoOp(t *testing.T) {
	// An explicit nats.url discovers nothing, so Result carries no TLS state
	// and an operator who set the URL by hand keeps full control.
	got, logs := applyFixture(
		t,
		workerconfig.NATSConfig{TLSEnabled: workerconfig.TLSAuto},
		workerdiscovery.Result{NATSURL: "nats://explicit:4222"},
	)
	if got.UseTLS(false) || logs != "" {
		t.Errorf("explicit URL was not a no-op: cfg=%+v logs=%s", got, logs)
	}
}

func TestApplyDiscoveredTLS_WarnsOnHTTPServerURLAgainstHTTPSServer(t *testing.T) {
	_, logs := applyFixture(
		t,
		workerconfig.NATSConfig{TLSEnabled: workerconfig.TLSAuto, ServerURL: "http://srv:8080"},
		workerdiscovery.Result{NATSURL: "nats://srv:4222", HTTPTLS: true},
	)
	if !strings.Contains(logs, "server_url") {
		t.Errorf("no log warned about the http:// enrollment URL:\n%s", logs)
	}

	// An https:// server_url against the same server is correct and silent.
	_, quiet := applyFixture(
		t,
		workerconfig.NATSConfig{TLSEnabled: workerconfig.TLSAuto, ServerURL: "https://srv:8080"},
		workerdiscovery.Result{NATSURL: "nats://srv:4222", HTTPTLS: true},
	)
	if strings.Contains(quiet, "server_url") {
		t.Errorf("warned about a correct https:// server_url:\n%s", quiet)
	}
}
