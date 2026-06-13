// SPDX-License-Identifier: AGPL-3.0-or-later

package natsclient

import (
	"context"
	"log/slog"
	"testing"
	"time"

	workerconfig "github.com/uberware/sqi/internal/worker/config"
)

func TestExponentialBackoff_GrowsAndCaps(t *testing.T) {
	base := 100 * time.Millisecond
	fn := exponentialBackoff(base)

	// Attempt 0 ≈ base ± 20%.
	d0 := fn(0)
	if d0 < time.Duration(float64(base)*0.8) || d0 > time.Duration(float64(base)*1.2) {
		t.Errorf("attempt 0 delay %v outside base ±20%%", d0)
	}

	// Large attempt counts are capped at maxBackoff (+ up to +20% jitter).
	dBig := fn(1000)
	if dBig > time.Duration(float64(maxBackoff)*1.2) {
		t.Errorf("attempt 1000 delay %v exceeds capped maxBackoff+jitter", dBig)
	}
	if dBig < 0 {
		t.Errorf("delay must never be negative, got %v", dBig)
	}
}

func TestExponentialBackoff_NeverNegative(t *testing.T) {
	fn := exponentialBackoff(time.Millisecond)
	for i := range 50 {
		if d := fn(i); d < 0 {
			t.Fatalf("attempt %d produced negative delay %v", i, d)
		}
	}
}

func TestBuildTLSOptions(t *testing.T) {
	cases := []struct {
		name     string
		cfg      workerconfig.NATSConfig
		wantOpts bool
		wantErr  bool
	}{
		{"no tls", workerconfig.NATSConfig{}, false, false},
		{"insecure skip verify", workerconfig.NATSConfig{InsecureSkipVerify: true}, true, false},
		{"cert without key errors", workerconfig.NATSConfig{TLSCertFile: "/x.crt"}, false, true},
		{"bad ca file errors", workerconfig.NATSConfig{TLSCAFile: "/does/not/exist.pem"}, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts, err := buildTLSOptions(tc.cfg)
			if tc.wantErr && err == nil {
				t.Fatal("want error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !tc.wantErr {
				if tc.wantOpts && len(opts) == 0 {
					t.Error("expected TLS options, got none")
				}
				if !tc.wantOpts && len(opts) != 0 {
					t.Errorf("expected no TLS options, got %d", len(opts))
				}
			}
		})
	}
}

func TestConnect_DialErrorIsReturned(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	cfg := workerconfig.NATSConfig{
		URL:                  "nats://127.0.0.1:1", // nothing listens on port 1
		MaxReconnectAttempts: 0,
		ReconnectWait:        10 * time.Millisecond,
	}
	_, _, err := Connect(context.Background(), cfg, logger)
	if err == nil {
		t.Fatal("Connect to dead port: want error, got nil")
	}
}
