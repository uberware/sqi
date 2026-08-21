// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

func TestBrowseURL(t *testing.T) {
	tests := []struct {
		name string
		addr string
		want string
	}{
		{"ipv4 wildcard becomes localhost", "0.0.0.0:8080", "http://localhost:8080"},
		{"ipv6 wildcard becomes localhost", "[::]:8080", "http://localhost:8080"},
		{"empty host becomes localhost", ":8080", "http://localhost:8080"},
		{"explicit loopback is preserved", "127.0.0.1:8080", "http://127.0.0.1:8080"},
		{"explicit lan ip is preserved", "192.168.1.10:9000", "http://192.168.1.10:9000"},
		{"non host:port surfaced as-is", "not-an-addr", "http://not-an-addr"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := browseURL(tt.addr); got != tt.want {
				t.Errorf("browseURL(%q) = %q, want %q", tt.addr, got, tt.want)
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

			warnIfBrokerUnauthenticated(context.Background(), tt.addr, tt.authEnabled, logger)

			got := strings.Contains(buf.String(), "broker is unauthenticated")
			if got != tt.wantWarn {
				t.Errorf("warn emitted = %v, want %v; log was %q", got, tt.wantWarn, buf.String())
			}
		})
	}
}
