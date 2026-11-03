// SPDX-License-Identifier: AGPL-3.0-or-later

package discovery

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	workerconfig "github.com/uberware/sqi/internal/worker/config"
)

// ApplyTLS is what makes nats.tls_enabled's "auto" mode able to see a
// TLS-required broker: it folds a discovered tls:// nats_url into the
// worker's configuration, so the worker's own tls_* settings apply and a
// misconfiguration is logged with the setting that fixes it.

func applyFixture(t *testing.T, cfg workerconfig.NATSConfig, found Result) (workerconfig.NATSConfig, string) {
	t.Helper()
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ApplyTLS(context.Background(), &cfg, found, logger)
	return cfg, buf.String()
}

func TestApplyTLS_AutoEnablesOnAdvertisedBroker(t *testing.T) {
	got, logs := applyFixture(
		t,
		workerconfig.NATSConfig{TLSEnabled: workerconfig.TLSAuto},
		Result{NATSURL: "nats://srv:4222", NATSTLS: true},
	)
	if !got.UseTLS(false) {
		t.Error("auto did not enable TLS against a broker advertising a tls:// nats_url")
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

func TestApplyTLS_AutoWithCAStaysQuietAboutTheCA(t *testing.T) {
	_, logs := applyFixture(
		t,
		workerconfig.NATSConfig{TLSEnabled: workerconfig.TLSAuto, TLSCAFile: "/etc/sqi/ca.crt"},
		Result{NATSURL: "nats://srv:4222", NATSTLS: true},
	)
	if strings.Contains(logs, "no nats.tls_ca_file is configured") {
		t.Errorf("warned about a missing CA when one is configured:\n%s", logs)
	}
}

func TestApplyTLS_ForcedOffIsHonoredButExplained(t *testing.T) {
	got, logs := applyFixture(
		t,
		workerconfig.NATSConfig{TLSEnabled: workerconfig.TLSOff},
		Result{NATSURL: "nats://srv:4222", NATSTLS: true},
	)
	// Discovery must not override an explicit decision about the worker's own
	// TLS settings, even though a tls:// URL makes nats.go use TLS anyway.
	if got.TLSEnabled != workerconfig.TLSOff || got.UseTLS(true) {
		t.Error("discovery overrode an explicit nats.tls_enabled=false")
	}
	if !strings.Contains(logs, "are not applied") || strings.Contains(logs, "refused") {
		t.Errorf("log did not explain that the tls_* settings are not applied:\n%s", logs)
	}
}

func TestApplyTLS_PlaintextServerChangesNothing(t *testing.T) {
	// The default path: a plaintext farm must be untouched.
	got, logs := applyFixture(
		t,
		workerconfig.NATSConfig{TLSEnabled: workerconfig.TLSAuto},
		Result{NATSURL: "nats://srv:4222"},
	)
	if got.UseTLS(false) {
		t.Error("TLS was enabled against a plaintext server")
	}
	if logs != "" {
		t.Errorf("a plaintext discovery produced output:\n%s", logs)
	}
}

func TestApplyTLS_ExplicitURLIsANoOp(t *testing.T) {
	// An explicit nats.url discovers nothing, so Result carries no TLS state
	// and an operator who set the URL by hand keeps full control.
	got, logs := applyFixture(
		t,
		workerconfig.NATSConfig{TLSEnabled: workerconfig.TLSAuto},
		Result{NATSURL: "nats://explicit:4222"},
	)
	if got.UseTLS(false) || logs != "" {
		t.Errorf("explicit URL was not a no-op: cfg=%+v logs=%s", got, logs)
	}
}

func TestApplyTLS_WarnsOnHTTPServerURLAgainstHTTPSServer(t *testing.T) {
	_, logs := applyFixture(
		t,
		workerconfig.NATSConfig{TLSEnabled: workerconfig.TLSAuto, ServerURL: "http://srv:8080"},
		Result{NATSURL: "nats://srv:4222", HTTPTLS: true},
	)
	if !strings.Contains(logs, "server_url") {
		t.Errorf("no log warned about the http:// enrollment URL:\n%s", logs)
	}

	// An https:// server_url against the same server is correct and silent.
	_, quiet := applyFixture(
		t,
		workerconfig.NATSConfig{TLSEnabled: workerconfig.TLSAuto, ServerURL: "https://srv:8080"},
		Result{NATSURL: "nats://srv:4222", HTTPTLS: true},
	)
	if strings.Contains(quiet, "server_url") {
		t.Errorf("warned about a correct https:// server_url:\n%s", quiet)
	}
}
