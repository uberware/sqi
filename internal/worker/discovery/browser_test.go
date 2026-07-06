// SPDX-License-Identifier: AGPL-3.0-or-later

package discovery

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"testing"
	"time"

	"github.com/grandcat/zeroconf"
)

// testLogger returns a logger that only emits errors, keeping test output clean.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// buildZeroconfEntry constructs a minimal *zeroconf.ServiceEntry for testing.
// Instance/Service/Domain live in the embedded zeroconf.ServiceRecord, so
// they must be initialized via the embedded struct field name, not as promoted
// fields in the outer composite literal.
func buildZeroconfEntry(hostname string, txt []string, instance string) *zeroconf.ServiceEntry {
	return &zeroconf.ServiceEntry{
		ServiceRecord: zeroconf.ServiceRecord{
			Instance: instance,
			Service:  serviceType,
			Domain:   domain,
		},
		HostName: hostname,
		Text:     txt,
	}
}

// ── parseTXTRecords ───────────────────────────────────────────────────────────

func TestParseTXTRecords(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		records []string
		want    map[string]string
	}{
		{
			name:    "empty",
			records: nil,
			want:    map[string]string{},
		},
		{
			name:    "standard server records",
			records: []string{"id=abc123", "http=8080", "nats=4222", "host=myhost", "version=1.2.3"},
			want: map[string]string{
				"id":      "abc123",
				"http":    "8080",
				"nats":    "4222",
				"host":    "myhost",
				"version": "1.2.3",
			},
		},
		{
			name:    "record without equals sign",
			records: []string{"flagonly"},
			want:    map[string]string{"flagonly": ""},
		},
		{
			name:    "record with extra equals in value",
			records: []string{"key=val=ue"},
			want:    map[string]string{"key": "val=ue"},
		},
		{
			name:    "whitespace trimmed",
			records: []string{" id = 123 "},
			want:    map[string]string{"id": "123"},
		},
		{
			name:    "duplicate key first-write-wins",
			records: []string{"nats=4222", "nats=bad"},
			want:    map[string]string{"nats": "4222"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := parseTXTRecords(tc.records)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d records, want %d: %v", len(got), len(tc.want), got)
			}
			for k, wantV := range tc.want {
				if gotV, ok := got[k]; !ok || gotV != wantV {
					t.Errorf("key %q: got %q, want %q", k, gotV, wantV)
				}
			}
		})
	}
}

// ── entryToResult ─────────────────────────────────────────────────────────────

func TestEntryToResult(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		hostname string
		ipv4     []net.IP
		ipv6     []net.IP
		txt      []string
		wantURL  string
		wantErr  bool
	}{
		{
			name:     "valid hostname with nats port",
			hostname: "render01.local.",
			txt:      []string{"id=abc", "nats=4222", "version=1.0.0"},
			wantURL:  "nats://render01.local:4222",
		},
		{
			name:     "trailing dot stripped from hostname",
			hostname: "myhost.local.",
			txt:      []string{"nats=4222"},
			wantURL:  "nats://myhost.local:4222",
		},
		{
			name:     "hostname without trailing dot",
			hostname: "myhost.local",
			txt:      []string{"nats=4222"},
			wantURL:  "nats://myhost.local:4222",
		},
		{
			name:     "missing nats record",
			hostname: "render01.local.",
			txt:      []string{"id=abc"},
			wantErr:  true,
		},
		{
			name:     "invalid nats port string",
			hostname: "render01.local.",
			txt:      []string{"nats=notaport"},
			wantErr:  true,
		},
		{
			name:     "nats port out of range high",
			hostname: "render01.local.",
			txt:      []string{"nats=99999"},
			wantErr:  true,
		},
		{
			name:     "nats port zero",
			hostname: "render01.local.",
			txt:      []string{"nats=0"},
			wantErr:  true,
		},
		{
			name:     "no hostname no IP",
			hostname: "",
			txt:      []string{"nats=4222"},
			wantErr:  true,
		},
		{
			name:     "no hostname falls back to IPv4",
			hostname: "",
			ipv4:     []net.IP{net.ParseIP("192.168.1.100")},
			txt:      []string{"nats=4222"},
			wantURL:  "nats://192.168.1.100:4222",
		},
		{
			name:     "no hostname falls back to IPv6",
			hostname: "",
			ipv6:     []net.IP{net.ParseIP("2001:db8::1")},
			txt:      []string{"nats=4222"},
			wantURL:  "nats://[2001:db8::1]:4222",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			entry := buildZeroconfEntry(tc.hostname, tc.txt, "test-instance")
			if len(tc.ipv4) > 0 {
				entry.AddrIPv4 = tc.ipv4
			}
			if len(tc.ipv6) > 0 {
				entry.AddrIPv6 = tc.ipv6
			}

			result, err := entryToResult(entry)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got result %+v", result)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result.NATSURL != tc.wantURL {
				t.Errorf("NATSURL: got %q, want %q", result.NATSURL, tc.wantURL)
			}
		})
	}
}

func TestEntryToResultFields(t *testing.T) {
	t.Parallel()

	entry := buildZeroconfEntry(
		"render01.local.",
		[]string{"id=server-id-123", "nats=4222", "version=2.1.0", "http=8080"},
		"my-sqi-server",
	)

	result, err := entryToResult(entry)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.InstanceName != "my-sqi-server" {
		t.Errorf("InstanceName: got %q, want %q", result.InstanceName, "my-sqi-server")
	}
	if result.InstanceID != "server-id-123" {
		t.Errorf("InstanceID: got %q, want %q", result.InstanceID, "server-id-123")
	}
	if result.Version != "2.1.0" {
		t.Errorf("Version: got %q, want %q", result.Version, "2.1.0")
	}
}

// ── ResolveNATSURL ────────────────────────────────────────────────────────────

func TestResolveNATSURLExplicitBypassesMDNS(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	logger := testLogger()

	// With an explicit URL, mDNS must not be consulted regardless of the
	// mdnsEnabled flag — no network I/O should occur.
	for _, enabled := range []bool{true, false} {
		t.Run("mdns_enabled="+boolStr(enabled), func(t *testing.T) {
			t.Parallel()
			url, err := ResolveNATSURL(ctx, "nats://explicit:4222", enabled, time.Second, logger)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if url != "nats://explicit:4222" {
				t.Errorf("got %q, want %q", url, "nats://explicit:4222")
			}
		})
	}
}

func TestResolveNATSURLDisabledNoURL(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	logger := testLogger()

	_, err := ResolveNATSURL(ctx, "", false, time.Second, logger)
	if err == nil {
		t.Fatal("expected ErrDiscoveryDisabled, got nil")
	}
	if !errors.Is(err, ErrDiscoveryDisabled) {
		t.Errorf("got %v, want ErrDiscoveryDisabled", err)
	}
}

func TestBrowseMDNSTimeout(t *testing.T) {
	// This test runs a real (brief) mDNS browse and asserts the timeout path
	// returns ErrDiscoveryTimeout. It browses a service type nothing
	// advertises rather than "_sqi._tcp": a developer machine (or CI host)
	// legitimately running sqi-server would otherwise answer instantly and
	// turn this into a test of the local network's state, not of the timeout
	// path. Skipped in -short mode because it waits for the timeout to elapse.
	if testing.Short() {
		t.Skip("skipping mDNS network test in short mode")
	}

	ctx := context.Background()
	logger := testLogger()

	timeout := 200 * time.Millisecond
	start := time.Now()

	_, err := browseService(ctx, "_sqi-test-absent._tcp", timeout, logger)
	elapsed := time.Since(start)

	if !errors.Is(err, ErrDiscoveryTimeout) {
		t.Errorf("got %v, want ErrDiscoveryTimeout", err)
	}
	// Should not take significantly longer than the configured timeout.
	if elapsed > timeout+500*time.Millisecond {
		t.Errorf("browse took %v, expected close to %v", elapsed, timeout)
	}
}

func TestResolveNATSURLContextCancelled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately before any I/O

	logger := testLogger()
	_, err := ResolveNATSURL(ctx, "", true, 5*time.Second, logger)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
