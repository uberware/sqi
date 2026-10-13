// SPDX-License-Identifier: AGPL-3.0-or-later

package backend_test

import (
	"path/filepath"
	"testing"

	"github.com/uberware/sqi/internal/store/backend"
	"github.com/uberware/sqi/internal/store/storetest"
)

func TestOpen_SQLiteByDefault(t *testing.T) {
	st, err := backend.Open(t.Context(), backend.Options{SQLitePath: t.TempDir() + "/test.db", AutoMigrate: true})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, ok := st.(backend.Checkpointer); !ok {
		t.Error("a SQLite backend does not offer the checkpointer")
	}
	if err := st.Ping(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestOpen_Postgres(t *testing.T) {
	dsn := storetest.PostgresDSN(t)
	st, err := backend.Open(t.Context(), backend.Options{Driver: "postgres", PostgresDSN: dsn, MaxConns: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, ok := st.(backend.Checkpointer); ok {
		t.Error("a PostgreSQL backend offers a WAL checkpointer")
	}
	if err := st.Ping(t.Context()); err != nil {
		t.Fatal(err)
	}
}

// TestOpen_ErrorReturnsNilBackend pins that every failing Open returns a nil
// Backend interface, not a non-nil interface holding a nil *Store: a caller's
// "if st != nil { st.Close() }" would otherwise call Close on a nil store.
// None of the cases needs a database, so the test runs everywhere.
func TestOpen_ErrorReturnsNilBackend(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts backend.Options
	}{
		{"unknown driver", backend.Options{Driver: "mysql"}},
		{"postgres with an empty DSN", backend.Options{Driver: "postgres"}},
		{"sqlite in a directory that does not exist", backend.Options{
			SQLitePath: filepath.Join(t.TempDir(), "missing", "dir", "test.db"), AutoMigrate: true,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := backend.Open(t.Context(), tc.opts)
			if err == nil {
				_ = st.Close()
				t.Fatalf("Open(%+v) succeeded", tc.opts)
			}
			if st != nil {
				t.Fatalf("Open(%+v) returned a non-nil Backend %#v with error %v", tc.opts, st, err)
			}
		})
	}
}

func TestName(t *testing.T) {
	for in, want := range map[string]string{"": "sqlite", "sqlite": "sqlite", "postgres": "postgres"} {
		if got := backend.Name(in); got != want {
			t.Errorf("Name(%q) = %q, want %q", in, got, want)
		}
	}
}
