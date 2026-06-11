// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import "testing"

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
