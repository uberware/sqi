// SPDX-License-Identifier: AGPL-3.0-or-later

package discovery

import (
	"errors"
	"strings"
	"testing"
)

// TestCheckNATSURL pins the one statement of the advertised-URL contract that
// configuration, the responder and the worker's browser all apply.
func TestCheckNATSURL(t *testing.T) {
	// The longest URL whose record still fits one TXT string.
	host := strings.Repeat("h", MaxTXTString-NATSURLRecordLen("nats://:4222"))
	fits := "nats://" + host + ":4222"
	if NATSURLRecordLen(fits) != MaxTXTString {
		t.Fatalf("fixture: record length %d, want %d", NATSURLRecordLen(fits), MaxTXTString)
	}

	tests := []struct {
		name string
		raw  string
		want error
	}{
		{"nats scheme", "nats://broker.example:4222", nil},
		{"tls scheme", "tls://broker.example:4222", nil},
		{"IPv6 host", "nats://[::1]:4222", nil},
		{"record exactly at the limit", fits, nil},
		{"does not parse", "nats://bad host:4222", ErrNATSURLInvalid},
		{"no host", "nats://:4222", ErrNATSURLInvalid},
		{"empty", "", ErrNATSURLInvalid},
		{"user and password", "nats://u:p@broker.example:4222", ErrNATSURLCredentials},
		{"token", "nats://tok@broker.example:4222", ErrNATSURLCredentials},
		{"credentials win over a bad scheme", "http://u:p@broker.example:4222", ErrNATSURLCredentials},
		{"http scheme", "http://broker.example:4222", ErrNATSURLScheme},
		{"port 0", "nats://broker.example:0", ErrNATSURLPortZero},
		{"one byte over the limit", "nats://h" + host + ":4222", ErrNATSURLTooLong},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, err := CheckNATSURL(tt.raw)
			if !errors.Is(err, tt.want) || (tt.want == nil && err != nil) {
				t.Fatalf("CheckNATSURL(%q) = %v, want %v", tt.raw, err, tt.want)
			}
			if (u == nil) != errors.Is(err, ErrNATSURLInvalid) {
				t.Fatalf("CheckNATSURL(%q) URL = %v; want nil exactly when the error is ErrNATSURLInvalid", tt.raw, u)
			}
		})
	}
}
