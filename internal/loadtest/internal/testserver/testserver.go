// SPDX-License-Identifier: AGPL-3.0-or-later

// Package testserver starts a full in-process sqi-server (SQLite, embedded NATS,
// scheduler and HTTP) for the load harness's own tests. It is importable only
// from under internal/loadtest.
package testserver

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/config"
	"github.com/uberware/sqi/internal/scheduler"
	"github.com/uberware/sqi/internal/server"
	"github.com/uberware/sqi/internal/store/backend"
)

const (
	startupTimeout  = 15 * time.Second
	shutdownTimeout = 60 * time.Second
)

// Options configures Start. It is empty for now.
type Options struct{}

// Server is a running in-process sqi-server.
type Server struct {
	httpAddr string
	natsAddr string
}

// HTTPURL returns the base URL of the REST and WebSocket API.
func (s *Server) HTTPURL() string { return "http://" + s.httpAddr }

// NATSURL returns the URL of the embedded NATS broker.
func (s *Server) NATSURL() string { return "nats://" + s.natsAddr }

// Start boots a server tuned for throughput (tight assign interval, rate
// limiting off) and returns once /readyz passes. It stops the server, and waits
// for Run to return, through tb.Cleanup.
func Start(tb testing.TB, _ Options) *Server {
	tb.Helper()

	// TempDir is registered before the stop cleanup below, so cleanups (LIFO)
	// stop the server before the directory is removed.
	tmpDir := tb.TempDir()

	httpAddr := fmt.Sprintf("127.0.0.1:%d", freePort(tb))
	natsAddr := fmt.Sprintf("127.0.0.1:%d", freePort(tb))

	cfg := server.Config{
		HTTPAddr:    httpAddr,
		CORSOrigins: []string{"*"},
		StoreDriver: backend.DriverSQLite,
		SQLitePath:  tmpDir + "/test.db",
		NATS: config.NATSConfig{Embedded: config.NATSEmbeddedConfig{
			Addr:       natsAddr,
			DataDir:    tmpDir + "/nats",
			MaxStoreMB: 128,
		}},
		CheckpointInterval: time.Minute,
		Scheduler: scheduler.Config{
			AssignInterval:         50 * time.Millisecond,
			AssignBatchSize:        50,
			AssignWorkers:          4,
			WorkerTimeout:          60 * time.Second,
			HeartbeatSweepInterval: 30 * time.Second,
		},
		DiscoveryEnabled: false,
		DisableRateLimit: true,
	}

	srv := server.New(cfg, slog.New(slog.DiscardHandler), nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()

	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(shutdownTimeout):
				tb.Logf("testserver: server did not stop within %s", shutdownTimeout)
			}
		})
	}
	tb.Cleanup(stop)

	select {
	case err := <-done:
		tb.Fatalf("testserver: server exited during startup: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if !waitForTCP(httpAddr, startupTimeout) {
		tb.Fatalf("testserver: HTTP not up on %s (NATS %s) within %s", httpAddr, natsAddr, startupTimeout)
	}
	if !waitForReadyz(httpAddr, startupTimeout) {
		tb.Fatalf("testserver: /readyz not ready within %s (HTTP %s, NATS %s)", startupTimeout, httpAddr, natsAddr)
	}
	return &Server{httpAddr: httpAddr, natsAddr: natsAddr}
}

var (
	handedOutMu    sync.Mutex
	handedOutPorts = map[int]struct{}{}
)

// freePort returns a loopback port the OS reports free, never the same one
// twice in a process: two listeners of one server sharing a port makes the
// second bind fail while a TCP probe of the first still succeeds.
func freePort(tb testing.TB) int {
	tb.Helper()
	const maxDraws = 100
	for range maxDraws {
		port := drawPort(tb)
		handedOutMu.Lock()
		_, seen := handedOutPorts[port]
		if !seen {
			handedOutPorts[port] = struct{}{}
		}
		handedOutMu.Unlock()
		if !seen {
			return port
		}
	}
	tb.Fatalf("testserver: %d port draws all returned a port already handed out", maxDraws)
	return 0
}

func drawPort(tb testing.TB) int {
	tb.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatalf("testserver: free port: %v", err)
	}
	tcpAddr, ok := ln.Addr().(*net.TCPAddr)
	if err := ln.Close(); err != nil {
		tb.Fatalf("testserver: close listener: %v", err)
	}
	if !ok {
		tb.Fatalf("testserver: unexpected address type %T", ln.Addr())
	}
	return tcpAddr.Port
}

func waitForTCP(addr string, timeout time.Duration) bool {
	dialer := &net.Dialer{Timeout: 200 * time.Millisecond}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := dialer.DialContext(context.Background(), "tcp", addr)
		if err == nil {
			_ = conn.Close()
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

func waitForReadyz(httpAddr string, timeout time.Duration) bool {
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+httpAddr+"/readyz", http.NoBody)
		if err == nil {
			resp, err := client.Do(req)
			if err == nil {
				_ = resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					return true
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}
