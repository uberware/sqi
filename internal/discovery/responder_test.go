// SPDX-License-Identifier: AGPL-3.0-or-later

package discovery

import (
	"context"
	"log/slog"
	"net"
	"slices"
	"strings"
	"testing"

	"github.com/grandcat/zeroconf"
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

	got := buildTXTRecords("abc-123", 8080, "nats://render-01.local:4222", "render-01", "v0.1.0", false)
	want := []string{
		"id=abc-123",
		"http=8080",
		"nats_url=nats://render-01.local:4222",
		"host=render-01",
		"version=v0.1.0",
	}
	if !slices.Equal(got, want) {
		t.Errorf("buildTXTRecords() = %v, want %v", got, want)
	}
}

// TestBuildTXTRecords_AdvertisesNATSURLOnly pins that the broker is
// advertised as one nats_url record, with no nats= port record and no
// nats_tls= record.
func TestBuildTXTRecords_AdvertisesNATSURLOnly(t *testing.T) {
	t.Parallel()

	got := buildTXTRecords("id-1", 8080, "tls://h:4222", "h", "0.4.0", true)
	if !slices.Contains(got, "nats_url=tls://h:4222") {
		t.Errorf("records %v missing nats_url=tls://h:4222", got)
	}
	for _, rec := range got {
		if strings.HasPrefix(rec, "nats=") || strings.HasPrefix(rec, "nats_tls=") {
			t.Errorf("records %v contain %q; the broker is advertised only as nats_url", got, rec)
		}
	}
}

// TestBuildTXTRecords_TLSKeysOmittedWhenOff pins that a plaintext farm's
// advertisement has no tls key at all, rather than tls=0, so clients see no
// extra record on a farm without TLS.
func TestBuildTXTRecords_TLSKeysOmittedWhenOff(t *testing.T) {
	t.Parallel()

	for _, rec := range buildTXTRecords("id-1", 8080, "nats://host:4222", "host", "0.4.0", false) {
		if strings.HasPrefix(rec, "tls=") {
			t.Errorf("plaintext advertisement contains %q; the tls key must be absent, not zero-valued", rec)
		}
	}
}

func TestBuildTXTRecords_HTTPTLSKeyPresentWhenOn(t *testing.T) {
	t.Parallel()

	got := buildTXTRecords("id-1", 8080, "tls://host:4222", "host", "0.4.0", true)
	if !slices.Contains(got, "tls=1") {
		t.Errorf("records %v missing tls=1", got)
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
			cfg:  Config{Enabled: true, HTTPAddr: "0.0.0.0:8080", NATSURL: "nats://h:4222"},
		},
		{
			name: "bad http addr",
			cfg:  Config{Enabled: true, InstanceName: "sqi-server", HTTPAddr: "nope", NATSURL: "nats://h:4222"},
		},
		{
			name: "no broker url",
			cfg:  Config{Enabled: true, InstanceName: "sqi-server", HTTPAddr: "0.0.0.0:8080", NATSURL: ""},
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

func TestNew_RejectsUnusableBrokerURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		url  string
	}{
		{"empty", ""},
		{"no scheme", "h:4222"},
		{"http scheme", "http://h:4222"},
		{"empty host", "nats://:4222"},
		{"record over 255 bytes", "nats://" + strings.Repeat("h", 250) + ":4222"},
		{"user and password", "nats://sqi:hunter2@h:4222"},
		{"token", "nats://s3cr3tt0ken@h:4222"},
		{"unparseable with a token", "nats://s3cr3tt0ken@h:4222\x7f"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := Config{Enabled: true, InstanceName: "sqi-server", HTTPAddr: "0.0.0.0:8080", NATSURL: tt.url}
			_, err := New(cfg, discardLogger())
			if err == nil {
				t.Fatalf("New with NATSURL %q = nil error, want error", tt.url)
			}
			for _, secret := range []string{"hunter2", "s3cr3tt0ken"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("New error %q repeats the credential %q", err, secret)
				}
			}
		})
	}

	// The limit is on the whole record: exactly 255 bytes passes.
	ok := "nats://" + strings.Repeat("h", 255-len("nats_url=")-len("nats://")-len(":4222")) + ":4222"
	cfg := Config{Enabled: true, InstanceName: "sqi-server", HTTPAddr: "0.0.0.0:8080", NATSURL: ok}
	if _, err := New(cfg, discardLogger()); err != nil {
		t.Fatalf("New with a 255-byte record = %v, want nil", err)
	}
}

func TestNew_GeneratesInstanceID(t *testing.T) {
	t.Parallel()

	cfg := Config{
		Enabled:      true,
		InstanceName: "sqi-server",
		HTTPAddr:     "0.0.0.0:8080",
		NATSURL:      "nats://h:4222",
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
}

func TestNew_PreservesInstanceID(t *testing.T) {
	t.Parallel()

	cfg := Config{
		Enabled:      true,
		InstanceName: "sqi-server",
		HTTPAddr:     "0.0.0.0:8080",
		NATSURL:      "nats://h:4222",
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

// TestSRVHostFor pins the host the SRV record and a derived broker URL name.
// macOS reports its hostname already in the .local domain; appending .local
// again advertised "<name>.local.local", a name no certificate from
// `sqi-server tls init` covers.
func TestSRVHostFor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		hostname string
		want     string
	}{
		{hostname: "myhost", want: "myhost.local"},
		{hostname: "Robins-Mac-Studio.local", want: "Robins-Mac-Studio.local"},
		{hostname: "Robins-Mac-Studio.local.", want: "Robins-Mac-Studio.local"},
		{hostname: "Host.LOCAL", want: "Host.local"},
		{hostname: "render01.example.com", want: "render01.example.com.local"},
		{hostname: "mylocal", want: "mylocal.local"},
	}
	for _, tt := range tests {
		t.Run(tt.hostname, func(t *testing.T) {
			t.Parallel()
			if got := srvHostFor(tt.hostname); got != tt.want {
				t.Errorf("srvHostFor(%q) = %q, want %q", tt.hostname, got, tt.want)
			}
		})
	}
}

// TestStart_AdvertisesTheHostsAddresses: Start names the host itself, so the
// SRV record carries no doubled ".local", and passes the addresses explicitly.
// Given none, the underlying responder takes a reply's addresses from the
// interface the query arrived on, which it cannot learn on Windows (the
// network stack there reports no interface for a received packet), so every
// reply to a query carried no address and a worker browsing after the
// startup announcements found nothing. Not parallel: it replaces
// registerProxy.
func TestStart_AdvertisesTheHostsAddresses(t *testing.T) {
	want := interfaceIPs(nil)
	if len(want) == 0 {
		t.Skip("this host has no advertisable address")
	}
	var gotHost string
	var gotIPs []string
	orig := registerProxy
	registerProxy = func(_, _, _ string, _ int, host string, ips, _ []string, _ []net.Interface) (*zeroconf.Server, error) {
		gotHost, gotIPs = host, ips
		return nil, nil
	}
	t.Cleanup(func() { registerProxy = orig })

	r, err := New(Config{
		Enabled:      true,
		InstanceName: "test",
		HTTPAddr:     ":8080",
		NATSURL:      "nats://localhost:4222",
	}, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := r.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if want := hostLabel(hostname()); gotHost != want {
		t.Errorf("host = %q, want %q", gotHost, want)
	}
	if !slices.Equal(gotIPs, want) {
		t.Errorf("ips = %q, want this host's advertisable addresses %q", gotIPs, want)
	}
}

// TestAdvertisedIPs pins the address rules the responder applies to each
// interface, which are the underlying responder's own: loopback addresses
// are never advertised, IPv4 always is, and an interface's link-local IPv6
// addresses are advertised only when it has no global one.
func TestAdvertisedIPs(t *testing.T) {
	t.Parallel()

	cidr := func(s string) net.Addr {
		ip, n, err := net.ParseCIDR(s)
		if err != nil {
			t.Fatalf("ParseCIDR(%q): %v", s, err)
		}
		n.IP = ip
		return n
	}
	tests := []struct {
		name  string
		addrs []net.Addr
		want  []string
	}{
		{
			name:  "loopback excluded",
			addrs: []net.Addr{cidr("127.0.0.1/8"), cidr("::1/128")},
			want:  nil,
		},
		{
			name:  "global IPv6 preferred over link-local",
			addrs: []net.Addr{cidr("192.168.1.5/24"), cidr("fe80::1/64"), cidr("2001:db8::5/64")},
			want:  []string{"192.168.1.5", "2001:db8::5"},
		},
		{
			name:  "link-local IPv6 when there is no global one",
			addrs: []net.Addr{cidr("127.0.0.1/8"), cidr("fe80::1/64")},
			want:  []string{"fe80::1"},
		},
		{
			name:  "non-IPNet addresses ignored",
			addrs: []net.Addr{&net.IPAddr{IP: net.ParseIP("10.0.0.1")}, cidr("10.0.0.2/8")},
			want:  []string{"10.0.0.2"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := advertisedIPs(tt.addrs); !slices.Equal(got, tt.want) {
				t.Errorf("advertisedIPs = %q, want %q", got, tt.want)
			}
		})
	}
}
