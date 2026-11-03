// SPDX-License-Identifier: AGPL-3.0-or-later

package bus

import (
	"log/slog"
	"net"
	"strings"
	"testing"
)

// TestEmbedded_AdvertiseURL pins the URL discovery tells workers to use
// against an embedded broker.
func TestEmbedded_AdvertiseURL(t *testing.T) {
	t.Run("advertise_url wins", func(t *testing.T) {
		b := startEmbedded(t, EmbeddedConfig{AdvertiseURL: "tls://nats.example.com:7422"})
		if got, err := b.AdvertiseURL("h.local"); err != nil || got != "tls://nats.example.com:7422" {
			t.Fatalf("AdvertiseURL = (%q, %v), want the configured URL", got, err)
		}
	})

	t.Run("the listener's port", func(t *testing.T) {
		b := startBroker(t)
		got, err := b.AdvertiseURL("h.local")
		if err != nil {
			t.Fatalf("AdvertiseURL: %v", err)
		}
		port := strings.TrimPrefix(got, "nats://h.local:")
		if port == got || port == "0" || port == "" {
			t.Fatalf("AdvertiseURL = %q, want nats://h.local:<the bound port>", got)
		}
		if want := b.ClientURL(); !strings.HasSuffix(want, ":"+port) {
			t.Fatalf("advertised port %s is not the listener's (%s)", port, want)
		}
	})

	t.Run("ipv6 host is bracketed", func(t *testing.T) {
		b := startBroker(t)
		got, err := b.AdvertiseURL("fe80::1")
		if err != nil {
			t.Fatalf("AdvertiseURL: %v", err)
		}
		if !strings.HasPrefix(got, "nats://[fe80::1]:") {
			t.Fatalf("AdvertiseURL = %q, want nats://[fe80::1]:<port>", got)
		}
	})

	t.Run("tls listener advertises tls://", func(t *testing.T) {
		dir, _ := farmCerts(t)
		b := startBrokerTLS(t, brokerTLSOf(dir, false), AuthConfig{})
		got, err := b.AdvertiseURL("h.local")
		if err != nil || !strings.HasPrefix(got, "tls://h.local:") {
			t.Fatalf("AdvertiseURL = (%q, %v), want tls://h.local:<port>", got, err)
		}
	})

	t.Run("before Start", func(t *testing.T) {
		b := NewEmbedded(EmbeddedConfig{Addr: net.JoinHostPort("127.0.0.1", "4222")}, slog.New(slog.DiscardHandler))
		if got, err := b.AdvertiseURL("h.local"); err == nil {
			t.Fatalf("AdvertiseURL before Start = %q, want an error", got)
		}
	})
}

// TestRemote_AdvertiseURL pins the URL discovery tells workers to use against
// an external cluster: the first advertise_urls entry, else the first of urls.
func TestRemote_AdvertiseURL(t *testing.T) {
	tests := []struct {
		name      string
		urls      []string
		advertise []string
		want      string
	}{
		{"advertise_urls: the first", []string{"tls://internal-a:4222", "tls://internal-b:4222"}, []string{"tls://public-a:4222", "tls://public-b:4222"}, "tls://public-a:4222"},
		{"without advertise_urls: the first of urls", []string{"nats://a:4222", "nats://b:4222"}, nil, "nats://a:4222"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewRemote(RemoteConfig{URLs: tt.urls, AdvertiseURLs: tt.advertise}, slog.New(slog.DiscardHandler))
			if got, err := r.AdvertiseURL("h.local"); err != nil || got != tt.want {
				t.Fatalf("AdvertiseURL = (%q, %v), want %q", got, err, tt.want)
			}
		})
	}
}
