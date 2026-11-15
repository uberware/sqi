// SPDX-License-Identifier: AGPL-3.0-or-later

package target

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/uberware/sqi/internal/loadtest/scenario"
)

func TestDSNVia(t *testing.T) {
	const addr = "127.0.0.1:54321"
	tests := []struct {
		name, dsn string
		wantErr   string
		check     func(t *testing.T, cfg *pgconn.Config)
	}{
		{
			name: "url keeps user, password, database and query",
			dsn:  "postgres://u:p%40ss@db:5432/x?sslmode=disable&application_name=sqi",
			check: func(t *testing.T, cfg *pgconn.Config) {
				t.Helper()
				if cfg.User != "u" || cfg.Password != "p@ss" || cfg.Database != "x" {
					t.Errorf("user %q password %q database %q", cfg.User, cfg.Password, cfg.Database)
				}
				if cfg.RuntimeParams["application_name"] != "sqi" {
					t.Errorf("application_name = %q, want sqi", cfg.RuntimeParams["application_name"])
				}
				if cfg.TLSConfig != nil {
					t.Error("sslmode=disable was lost")
				}
			},
		},
		{
			name: "postgresql scheme with no port",
			dsn:  "postgresql://u@db/x",
			check: func(t *testing.T, cfg *pgconn.Config) {
				t.Helper()
				if cfg.User != "u" || cfg.Database != "x" {
					t.Errorf("user %q database %q", cfg.User, cfg.Database)
				}
			},
		},
		{
			name: "url naming host and port in the query",
			dsn:  "postgres://u@db:5432/x?host=other&port=6543&sslmode=disable",
			check: func(t *testing.T, cfg *pgconn.Config) {
				t.Helper()
				if cfg.Database != "x" {
					t.Errorf("database %q", cfg.Database)
				}
			},
		},
		{
			name: "keyword value",
			dsn:  "host=db port=5432 dbname=x user=u sslmode=disable",
			check: func(t *testing.T, cfg *pgconn.Config) {
				t.Helper()
				if cfg.User != "u" || cfg.Database != "x" {
					t.Errorf("user %q database %q", cfg.User, cfg.Database)
				}
			},
		},
		{name: "several hosts in a url", dsn: "postgres://a:1,b:2/x", wantErr: "several"},
		{name: "several hosts as keywords", dsn: "host=a,b port=1,2 dbname=x", wantErr: "several"},
		{name: "unix socket", dsn: "host=/var/run/postgresql dbname=x", wantErr: "TCP"},
		{name: "unparsable", dsn: "postgres://u@db:notaport/x", wantErr: "DSN"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := dsnVia(tt.dsn, addr)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("dsnVia(%q) = %q, %v; want an error containing %q", tt.dsn, got, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("dsnVia(%q): %v", tt.dsn, err)
			}
			cfg, err := pgconn.ParseConfig(got)
			if err != nil {
				t.Fatalf("ParseConfig(%q): %v", got, err)
			}
			if cfg.Host != "127.0.0.1" || cfg.Port != 54321 {
				t.Errorf("host %q port %d, want 127.0.0.1 and 54321", cfg.Host, cfg.Port)
			}
			for _, fb := range cfg.Fallbacks {
				if fb.Host != cfg.Host || fb.Port != cfg.Port {
					t.Errorf("fallback %s:%d reaches past the proxy", fb.Host, fb.Port)
				}
			}
			tt.check(t, cfg)
		})
	}
}

// pgScenario is a postgres scenario holding one database fault.
func pgScenario(mode string) *scenario.Scenario {
	sc := testScenario(scenario.ServerSpec{Store: "postgres"})
	sc.Faults = []scenario.Fault{{Database: &scenario.DatabaseFault{Mode: mode}}}
	return sc
}

func TestManaged_StoreOptionsBypassTheDatabaseProxy(t *testing.T) {
	const dsn = "postgres://u:p@db.invalid:5432/sqi_load_x?sslmode=disable"
	m, err := newManaged(ManagedConfig{RunDir: t.TempDir(), Scenario: pgScenario(scenario.DatabaseStall), PostgresDSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	m.pgDSN = dsn
	if err := m.startDatabaseProxy(t.Context()); err != nil {
		t.Fatalf("startDatabaseProxy: %v", err)
	}
	t.Cleanup(func() { m.closeLanes() }) //nolint:errcheck // cleanup

	if m.dbProxy == nil {
		t.Fatal("no database proxy for a postgres scenario with a database fault")
	}
	port := m.dbProxy.Addr()[strings.LastIndex(m.dbProxy.Addr(), ":")+1:]
	var got string
	for _, e := range m.serverEnv() {
		if v, ok := strings.CutPrefix(e, "SQI_STORE_POSTGRES_DSN="); ok {
			got = v
		}
	}
	if !strings.Contains(got, "127.0.0.1:"+port) {
		t.Errorf("server DSN = %q, want one through the proxy at port %s", got, port)
	}
	if opts := m.StoreOptions(); opts.PostgresDSN != dsn {
		t.Errorf("StoreOptions().PostgresDSN = %q, want the direct %q", opts.PostgresDSN, dsn)
	}
	if m.PostgresDSN() != dsn {
		t.Errorf("PostgresDSN() = %q, want the direct %q", m.PostgresDSN(), dsn)
	}
}

func TestManaged_NoDatabaseProxyWithoutADatabaseFault(t *testing.T) {
	const dsn = "postgres://u:p@db.invalid:5432/x?sslmode=disable"
	sc := testScenario(scenario.ServerSpec{Store: "postgres"})
	m, err := newManaged(ManagedConfig{RunDir: t.TempDir(), Scenario: sc, PostgresDSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	m.pgDSN = dsn
	if err := m.startDatabaseProxy(t.Context()); err != nil {
		t.Fatal(err)
	}
	if m.dbProxy != nil || m.serverDSN != "" {
		t.Errorf("proxy %v, server DSN %q for a scenario with no database fault", m.dbProxy, m.serverDSN)
	}
	for _, e := range m.serverEnv() {
		if v, ok := strings.CutPrefix(e, "SQI_STORE_POSTGRES_DSN="); ok && v != dsn {
			t.Errorf("server DSN = %q, want the direct one", v)
		}
	}
}
