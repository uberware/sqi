// SPDX-License-Identifier: AGPL-3.0-or-later

package discovery

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"testing"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func TestPortFromAddr(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		addr    string
		want    int
		wantErr bool
	}{
		{name: "wildcard host", addr: "0.0.0.0:8080", want: 8080},
		{name: "loopback", addr: "127.0.0.1:4222", want: 4222},
		{name: "ipv6 wildcard", addr: "[::]:8080", want: 8080},
		{name: "hostname", addr: "example.local:9000", want: 9000},
		{name: "empty", addr: "", wantErr: true},
		{name: "missing port", addr: "127.0.0.1", wantErr: true},
		{name: "non-numeric port", addr: "127.0.0.1:http", wantErr: true},
		{name: "port too high", addr: "127.0.0.1:70000", wantErr: true},
		{name: "port zero", addr: "127.0.0.1:0", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := portFromAddr(tt.addr)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("portFromAddr(%q) = %d, want error", tt.addr, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("portFromAddr(%q) unexpected error: %v", tt.addr, err)
			}
			if got != tt.want {
				t.Errorf("portFromAddr(%q) = %d, want %d", tt.addr, got, tt.want)
			}
		})
	}
}

func TestBuildTXTRecords(t *testing.T) {
	t.Parallel()

	got := buildTXTRecords("abc-123", 8080, 4222, "render-01", "v0.1.0", false, false)
	want := []string{
		"id=abc-123",
		"http=8080",
		"nats=4222",
		"host=render-01",
		"version=v0.1.0",
	}
	if !slices.Equal(got, want) {
		t.Errorf("buildTXTRecords() = %v, want %v", got, want)
	}
}

// TestBuildTXTRecords_TLSKeysOmittedWhenOff pins that a plaintext farm's
// advertisement is byte-for-byte what it has always been: the TLS keys are
// ABSENT, not present-and-zero. Anything else would change what every
// existing client sees on an unchanged deployment.
func TestBuildTXTRecords_TLSKeysOmittedWhenOff(t *testing.T) {
	t.Parallel()

	for _, rec := range buildTXTRecords("id-1", 8080, 4222, "host", "0.4.0", false, false) {
		if strings.HasPrefix(rec, "tls=") || strings.HasPrefix(rec, "nats_tls=") {
			t.Errorf("plaintext advertisement contains %q; TLS keys must be absent, not zero-valued", rec)
		}
	}
}

func TestBuildTXTRecords_TLSKeysPresentWhenOn(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		httpTLS, natsTLS bool
		want             []string
		absent           []string
	}{
		{"both on", true, true, []string{"tls=1", "nats_tls=1"}, nil},
		{"http only", true, false, []string{"tls=1"}, []string{"nats_tls=1"}},
		{"nats only", false, true, []string{"nats_tls=1"}, []string{"tls=1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := buildTXTRecords("id-1", 8080, 4222, "host", "0.4.0", tt.httpTLS, tt.natsTLS)
			for _, w := range tt.want {
				if !slices.Contains(got, w) {
					t.Errorf("records %v missing %q", got, w)
				}
			}
			for _, a := range tt.absent {
				if slices.Contains(got, a) {
					t.Errorf("records %v unexpectedly contain %q", got, a)
				}
			}
		})
	}
}

func TestNew_Disabled(t *testing.T) {
	t.Parallel()

	// When disabled, New must succeed even with otherwise-invalid fields,
	// because no advertisement will be published.
	r, err := New(Config{Enabled: false, HTTPAddr: "garbage"}, discardLogger())
	if err != nil {
		t.Fatalf("New(disabled) unexpected error: %v", err)
	}
	if err := r.Start(context.Background()); err != nil {
		t.Fatalf("Start(disabled) unexpected error: %v", err)
	}
	if r.server != nil {
		t.Error("Start(disabled) should not register an mDNS server")
	}
	// Shutdown must be safe when nothing was registered.
	r.Shutdown()
}

func TestNew_EnabledValidationErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  Config
	}{
		{
			name: "empty instance name",
			cfg:  Config{Enabled: true, HTTPAddr: "0.0.0.0:8080", NATSAddr: "127.0.0.1:4222"},
		},
		{
			name: "bad http addr",
			cfg:  Config{Enabled: true, InstanceName: "sqi-server", HTTPAddr: "nope", NATSAddr: "127.0.0.1:4222"},
		},
		{
			name: "bad nats addr",
			cfg:  Config{Enabled: true, InstanceName: "sqi-server", HTTPAddr: "0.0.0.0:8080", NATSAddr: "nope"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := New(tt.cfg, discardLogger()); err == nil {
				t.Fatalf("New(%+v) = nil error, want error", tt.cfg)
			}
		})
	}
}

func TestNew_GeneratesInstanceID(t *testing.T) {
	t.Parallel()

	cfg := Config{
		Enabled:      true,
		InstanceName: "sqi-server",
		HTTPAddr:     "0.0.0.0:8080",
		NATSAddr:     "127.0.0.1:4222",
	}
	r, err := New(cfg, discardLogger())
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}
	if r.cfg.InstanceID == "" {
		t.Error("New() should generate a non-empty instance ID when none is provided")
	}
	if r.httpPort != 8080 {
		t.Errorf("httpPort = %d, want 8080", r.httpPort)
	}
	if r.natsPort != 4222 {
		t.Errorf("natsPort = %d, want 4222", r.natsPort)
	}
}

func TestNew_PreservesInstanceID(t *testing.T) {
	t.Parallel()

	cfg := Config{
		Enabled:      true,
		InstanceName: "sqi-server",
		HTTPAddr:     "0.0.0.0:8080",
		NATSAddr:     "127.0.0.1:4222",
		InstanceID:   "fixed-id",
	}
	r, err := New(cfg, discardLogger())
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}
	if r.cfg.InstanceID != "fixed-id" {
		t.Errorf("InstanceID = %q, want %q", r.cfg.InstanceID, "fixed-id")
	}
}

func TestHostname(t *testing.T) {
	t.Parallel()

	if h := hostname(); strings.TrimSpace(h) == "" {
		t.Error("hostname() returned an empty string")
	}
}
