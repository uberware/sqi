// SPDX-License-Identifier: AGPL-3.0-or-later

package backend_test

import (
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

func TestOpen_UnknownDriver(t *testing.T) {
	if _, err := backend.Open(t.Context(), backend.Options{Driver: "mysql"}); err == nil {
		t.Fatal("Open accepted driver mysql")
	}
}

func TestName(t *testing.T) {
	for in, want := range map[string]string{"": "sqlite", "sqlite": "sqlite", "postgres": "postgres"} {
		if got := backend.Name(in); got != want {
			t.Errorf("Name(%q) = %q, want %q", in, got, want)
		}
	}
}
