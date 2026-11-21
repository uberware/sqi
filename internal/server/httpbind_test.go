// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"context"
	"log/slog"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/config"
	"github.com/uberware/sqi/internal/scheduler"
)

// TestRun_FailsWhenTheHTTPPortIsTaken pins that a server whose HTTP port is
// held by another process fails to start, naming the address, rather than
// running on with no REST API, UI or readiness endpoint.
func TestRun_FailsWhenTheHTTPPortIsTaken(t *testing.T) {
	var lc net.ListenConfig
	hog, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = hog.Close() })

	tmp := t.TempDir()
	cfg := Config{
		HTTPAddr: hog.Addr().String(),
		NATS: config.NATSConfig{Embedded: config.NATSEmbeddedConfig{
			Addr: freeLoopbackAddr(t), DataDir: filepath.Join(tmp, "nats"), MaxStoreMB: 64,
		}},
		SQLitePath:         filepath.Join(tmp, "test.db"),
		CheckpointInterval: time.Minute,
		Scheduler:          scheduler.DefaultConfig(),
	}
	srv := New(cfg, slog.New(slog.DiscardHandler), nil)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Run with a taken HTTP port returned nil")
		}
		if !strings.Contains(err.Error(), hog.Addr().String()) {
			t.Errorf("Run error %q does not name the HTTP address %s", err, hog.Addr())
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Run with a taken HTTP port kept running")
	}
}
