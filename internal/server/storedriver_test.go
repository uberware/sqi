// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store/storetest"
)

func TestDefaultConfig_StoreIsSQLite(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.StoreDriver != "sqlite" {
		t.Errorf("DefaultConfig().StoreDriver = %q, want sqlite", cfg.StoreDriver)
	}
	if cfg.PostgresMaxConns != 16 {
		t.Errorf("DefaultConfig().PostgresMaxConns = %d, want 16", cfg.PostgresMaxConns)
	}
}

// TestServer_ReadyzNamesTheStoreDriver boots a real server on each store
// driver and reads /readyz. SQLite must keep reporting its checker as
// "sqlite", exactly as before this driver existed; PostgreSQL reports
// "postgres" and no "sqlite". It is also the one place the server's own start-up
// path (backend.Open, the readiness registration, the optional checkpointer) is
// driven on PostgreSQL, rather than the store alone.
func TestServer_ReadyzNamesTheStoreDriver(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(t *testing.T, cfg *Config)
		wantCheck string
		wantNot   string
	}{
		{
			name:      "sqlite",
			mutate:    func(*testing.T, *Config) {},
			wantCheck: "sqlite",
			wantNot:   "postgres",
		},
		{
			name: "postgres",
			mutate: func(t *testing.T, cfg *Config) {
				cfg.StoreDriver = "postgres"
				cfg.PostgresDSN = storetest.PostgresDSN(t) // skips without SQI_TEST_POSTGRES_DSN
				cfg.PostgresMaxConns = 4
			},
			wantCheck: "postgres",
			wantNot:   "sqlite",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, base := startTestServer(t, "http", func(cfg *Config) { tc.mutate(t, cfg) })

			resp, err := get(t, &http.Client{Timeout: 5 * time.Second}, base+"/readyz")
			if err != nil {
				t.Fatalf("GET /readyz: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			var body struct {
				Status string                       `json:"status"`
				Checks map[string]map[string]string `json:"checks"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatalf("decode /readyz: %v", err)
			}
			if resp.StatusCode != http.StatusOK || body.Status != "ok" {
				t.Fatalf("/readyz = %d %q (%v), want 200 ok", resp.StatusCode, body.Status, body.Checks)
			}
			if got := body.Checks[tc.wantCheck]["status"]; got != "ok" {
				t.Errorf("checks[%q] = %q, want ok (checks: %v)", tc.wantCheck, got, body.Checks)
			}
			if _, ok := body.Checks[tc.wantNot]; ok {
				t.Errorf("/readyz reports a %q check on the %s driver (checks: %v)", tc.wantNot, tc.name, body.Checks)
			}
		})
	}
}
