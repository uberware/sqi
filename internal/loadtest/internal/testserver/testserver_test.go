// SPDX-License-Identifier: AGPL-3.0-or-later

package testserver

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"
)

// TestStart_RecoversFromAPortTakenByAnotherProcess reproduces the CI failure:
// the HTTP port drawn for the server is held by a foreign listener (another
// test package's NATS node), so the server's bind fails while a TCP probe of
// the port still succeeds.
func TestStart_RecoversFromAPortTakenByAnotherProcess(t *testing.T) {
	var lc net.ListenConfig
	hog, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = hog.Close() })
	go func() {
		for {
			c, err := hog.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	taken, ok := hog.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("unexpected address type %T", hog.Addr())
	}

	first := true
	orig := nextPort
	t.Cleanup(func() { nextPort = orig })
	nextPort = func(tb testing.TB) int {
		if first {
			first = false
			return taken.Port
		}
		return orig(tb)
	}

	begin := time.Now()
	srv := Start(t, Options{})
	if d := time.Since(begin); d >= startupTimeout {
		t.Fatalf("Start took %s, as long as the startup timeout", d)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.HTTPURL()+"/readyz", http.NoBody)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("readyz: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("readyz status %d", resp.StatusCode)
	}
}

// TestStart_IgnoresAnotherServersReadiness covers a drawn HTTP port that
// another process's sqi-server took before this one bound it (H13f21): that
// server answers /readyz with 200 at once, before this server has even
// started its broker, so trusting the first 200 returned a Server whose HTTP
// URL was the other server's and whose NATS port nothing listened on yet.
func TestStart_IgnoresAnotherServersReadiness(t *testing.T) {
	var lc net.ListenConfig
	hog, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	other := &http.Server{
		Handler:           http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }),
		ReadHeaderTimeout: time.Second,
	}
	go other.Serve(hog) //nolint:errcheck // ends with the test
	t.Cleanup(func() { _ = other.Close() })
	taken, ok := hog.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("unexpected address type %T", hog.Addr())
	}

	first := true
	orig := nextPort
	t.Cleanup(func() { nextPort = orig })
	nextPort = func(tb testing.TB) int {
		if first {
			first = false
			return taken.Port
		}
		return orig(tb)
	}

	srv := Start(t, Options{})
	if want := fmt.Sprintf("http://127.0.0.1:%d", taken.Port); srv.HTTPURL() == want {
		t.Fatalf("Start returned the other server's URL %s", want)
	}
	d := net.Dialer{Timeout: time.Second}
	conn, err := d.DialContext(t.Context(), "tcp", srv.NATSURL()[len("nats://"):])
	if err != nil {
		t.Fatalf("NATS %s after Start: %v", srv.NATSURL(), err)
	}
	_ = conn.Close()
}
