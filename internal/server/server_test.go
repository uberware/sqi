// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/config"
)

func TestBrowseURL(t *testing.T) {
	tests := []struct {
		name  string
		addr  string
		tlsOn bool
		want  string
	}{
		{"ipv4 wildcard becomes localhost", "0.0.0.0:8080", false, "http://localhost:8080"},
		{"ipv6 wildcard becomes localhost", "[::]:8080", false, "http://localhost:8080"},
		{"empty host becomes localhost", ":8080", false, "http://localhost:8080"},
		{"explicit loopback is preserved", "127.0.0.1:8080", false, "http://127.0.0.1:8080"},
		{"explicit lan ip is preserved", "192.168.1.10:9000", false, "http://192.168.1.10:9000"},
		{"non host:port surfaced as-is", "not-an-addr", false, "http://not-an-addr"},
		{"tls wildcard becomes https localhost", "0.0.0.0:8080", true, "https://localhost:8080"},
		{"tls explicit host keeps scheme", "sqi.example:8443", true, "https://sqi.example:8443"},
		{"tls ipv6 wildcard", "[::]:8080", true, "https://localhost:8080"},
		{"tls non host:port surfaced as-is", "not-an-addr", true, "https://not-an-addr"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := browseURL(tt.addr, tt.tlsOn); got != tt.want {
				t.Errorf("browseURL(%q, %v) = %q, want %q", tt.addr, tt.tlsOn, got, tt.want)
			}
		})
	}
}

func TestWarnIfBrokerUnauthenticated(t *testing.T) {
	tests := []struct {
		name        string
		addr        string
		authEnabled bool
		wantWarn    bool
	}{
		{"non-loopback, auth off", "0.0.0.0:4222", false, true},
		// An empty host means "all interfaces", exactly as 0.0.0.0 does —
		// isLoopbackHost("") returning false is what makes both warn.
		{"empty host, auth off", ":4222", false, true},
		{"specific LAN ip, auth off", "192.168.1.10:4222", false, true},
		{"ipv6 any, auth off", "[::]:4222", false, true},
		{"loopback v4, auth off", "127.0.0.1:4222", false, false},
		{"loopback v6, auth off", "[::1]:4222", false, false},
		{"localhost name, auth off", "localhost:4222", false, false},
		{"non-loopback, auth on", "0.0.0.0:4222", true, false},
		{"unparseable addr, auth off", "not-an-addr", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

			n := config.DefaultConfig().NATS
			n.Embedded.Addr = tt.addr
			n.Auth.Enabled = tt.authEnabled
			warnIfBrokerUnauthenticated(context.Background(), n, logger)

			got := strings.Contains(buf.String(), "broker is unauthenticated")
			if got != tt.wantWarn {
				t.Errorf("warn emitted = %v, want %v; log was %q", got, tt.wantWarn, buf.String())
			}
		})
	}
}

func TestWarnIfBrokerUnauthenticated_External(t *testing.T) {
	tests := []struct {
		name        string
		urls        []string
		authEnabled bool
		wantWarn    bool
	}{
		{"loopback, auth off", []string{"nats://127.0.0.1:4222"}, false, true},
		{"cluster, auth off", []string{"tls://a:4222", "tls://b:4222"}, false, true},
		{"auth on", []string{"nats://127.0.0.1:4222"}, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
			n := config.DefaultConfig().NATS
			n.Mode = config.NATSModeExternal
			n.External.URLs = tt.urls
			n.Auth.Enabled = tt.authEnabled

			warnIfBrokerUnauthenticated(context.Background(), n, logger)

			out := buf.String()
			if got := strings.Contains(out, "broker is unauthenticated"); got != tt.wantWarn {
				t.Fatalf("warn emitted = %v, want %v; log was %q", got, tt.wantWarn, out)
			}
			if tt.wantWarn && (strings.Contains(out, "nats.embedded.addr") || !strings.Contains(out, tt.urls[0])) {
				t.Errorf("external WARN %q must name the broker URLs and not advise on the embedded listener", out)
			}
		})
	}
}

// TestBrokerURLLogsRedactCredentials checks the two log lines that name the
// external broker's URLs: neither may carry a password or a bare token, which
// nats.go accepts in the user part of a URL.
func TestBrokerURLLogsRedactCredentials(t *testing.T) {
	n := config.DefaultConfig().NATS
	n.Mode = config.NATSModeExternal
	n.External.URLs = []string{"nats://sqi:hunter2@a:4222", "nats://s3cr3tt0ken@b:4222", "nats://c:4222"}

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	warnIfBrokerUnauthenticated(context.Background(), n, logger)
	logger.InfoContext(context.Background(), "sqi-server starting", natsStartAttr(n))

	out := buf.String()
	for _, secret := range []string{"hunter2", "s3cr3tt0ken", "sqi:"} {
		if strings.Contains(out, secret) {
			t.Fatalf("log repeats the credential %q:\n%s", secret, out)
		}
	}
	if got := strings.Count(out, "nats://REDACTED@a:4222,nats://REDACTED@b:4222,nats://c:4222"); got != 2 {
		t.Fatalf("redacted URL list appears %d times, want 2:\n%s", got, out)
	}
}
