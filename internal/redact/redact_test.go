// SPDX-License-Identifier: AGPL-3.0-or-later

package redact_test

import (
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/redact"
)

func TestURL(t *testing.T) {
	tests := []struct {
		name, raw, want string
	}{
		{"no credentials", "tls://nats.example.com:4222", "tls://nats.example.com:4222"},
		{"user and password", "nats://alice:s3cret@a:4222", "nats://REDACTED@a:4222"},
		{"token only", "nats://t0ken@a:4222", "nats://REDACTED@a:4222"},
		{"empty password", "nats://alice:@a:4222", "nats://REDACTED@a:4222"},
		{"escaped password", "nats://alice:p%40ss@a:4222", "nats://REDACTED@a:4222"},
		{"unparseable", "nats://to ken@a:4222\x7f", redact.Placeholder},
		{"credentials without a scheme", "alice:s3cret@a:4222", redact.Placeholder},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := redact.URL(tt.raw)
			if got != tt.want {
				t.Fatalf("URL(%q) = %q, want %q", tt.raw, got, tt.want)
			}
			for _, secret := range []string{"s3cret", "t0ken", "alice", "p%40ss", "to ken"} {
				if strings.Contains(got, secret) {
					t.Fatalf("URL(%q) = %q, which still holds %q", tt.raw, got, secret)
				}
			}
		})
	}
}

func TestURLs(t *testing.T) {
	got := redact.URLs([]string{"nats://u:p@a:4222", "nats://tok@b:4222", "nats://c:4222"}, ",")
	if want := "nats://REDACTED@a:4222,nats://REDACTED@b:4222,nats://c:4222"; got != want {
		t.Fatalf("URLs = %q, want %q", got, want)
	}
}

func TestList(t *testing.T) {
	got := redact.List("nats://u:p@a:4222,nats://tok@b:4222")
	if want := "nats://REDACTED@a:4222,nats://REDACTED@b:4222"; got != want {
		t.Fatalf("List = %q, want %q", got, want)
	}
}
