// SPDX-License-Identifier: AGPL-3.0-or-later

package obs

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/uberware/sqi/internal/health"
	workmetrics "github.com/uberware/sqi/internal/worker/metrics"
)

func newTestServer(t *testing.T, pprof bool, ready bool) *Server {
	t.Helper()
	reg := health.NewRegistry()
	if !ready {
		reg.Register("nats", health.CheckerFunc(func(context.Context) error {
			return io.ErrUnexpectedEOF
		}))
	}
	logger := slog.New(slog.DiscardHandler)
	return New("127.0.0.1:0", pprof, logger, workmetrics.New(), reg, TLSConfig{})
}

func TestBuildMux_HealthzAlwaysOK(t *testing.T) {
	s := newTestServer(t, false, false) // failing checker, but liveness ignores it
	mux := s.buildMux(context.Background())
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("/healthz = %d, want 200", rec.Code)
	}
}

func TestBuildMux_ReadyzReflectsCheckers(t *testing.T) {
	for _, tc := range []struct {
		name  string
		ready bool
		want  int
	}{
		{"ready", true, http.StatusOK},
		{"not-ready", false, http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t, false, tc.ready)
			mux := s.buildMux(context.Background())
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/readyz", nil))
			if rec.Code != tc.want {
				t.Errorf("/readyz = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

func TestBuildMux_MetricsServed(t *testing.T) {
	s := newTestServer(t, false, true)
	mux := s.buildMux(context.Background())
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("/metrics = %d, want 200", rec.Code)
	}
}

func TestBuildMux_PprofGating(t *testing.T) {
	// pprof off → route not registered (404).
	off := newTestServer(t, false, true).buildMux(context.Background())
	rec := httptest.NewRecorder()
	off.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/debug/pprof/", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("pprof off: /debug/pprof/ = %d, want 404", rec.Code)
	}

	// pprof on → index responds 200.
	on := newTestServer(t, true, true).buildMux(context.Background())
	rec = httptest.NewRecorder()
	on.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/debug/pprof/", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("pprof on: /debug/pprof/ = %d, want 200", rec.Code)
	}
}

func TestShutdown_NilServerIsSafe(t *testing.T) {
	s := newTestServer(t, false, true)
	s.Shutdown(context.Background()) // httpServer never set → must not panic
}
