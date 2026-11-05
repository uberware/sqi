// SPDX-License-Identifier: AGPL-3.0-or-later

package testserver

import (
	"context"
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
