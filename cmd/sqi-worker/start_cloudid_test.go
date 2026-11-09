// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	workerconfig "github.com/uberware/sqi/internal/worker/config"
)

func TestReadCloudInstanceID(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		if _, err := io.WriteString(w, "node-1"); err != nil {
			return
		}
	}))
	defer srv.Close()

	t.Run("none reads nothing", func(t *testing.T) {
		got := readCloudInstanceID(context.Background(),
			workerconfig.CloudInstanceIDConfig{Source: "none", Endpoint: srv.URL}, slog.New(slog.DiscardHandler))
		if got != "" || hits.Load() != 0 {
			t.Fatalf("got %q after %d requests, want none", got, hits.Load())
		}
	})

	t.Run("a source reads the ID", func(t *testing.T) {
		got := readCloudInstanceID(context.Background(),
			workerconfig.CloudInstanceIDConfig{Source: "gcp", Endpoint: srv.URL}, slog.New(slog.DiscardHandler))
		if got != "node-1" {
			t.Fatalf("got %q, want node-1", got)
		}
	})

	t.Run("a failure is one warning and no ID", func(t *testing.T) {
		dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		url := dead.URL
		dead.Close()
		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, nil))
		got := readCloudInstanceID(context.Background(),
			workerconfig.CloudInstanceIDConfig{Source: "gcp", Endpoint: url}, logger)
		if got != "" || strings.Count(buf.String(), "level=WARN") != 1 {
			t.Fatalf("got %q with log %q, want no ID and exactly one warning", got, buf.String())
		}
	})
}
