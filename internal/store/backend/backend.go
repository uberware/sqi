// SPDX-License-Identifier: AGPL-3.0-or-later

// Package backend opens the state store the configuration names, so the
// server and the CLIs do not each switch on the driver.
package backend

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/postgres"
	"github.com/uberware/sqi/internal/store/sqlite"
)

// The drivers.
const (
	DriverSQLite   = "sqlite"
	DriverPostgres = "postgres"
)

// Options says which store to open and how.
type Options struct {
	Driver      string // DriverSQLite (also when empty) or DriverPostgres
	SQLitePath  string
	PostgresDSN string
	MaxConns    int  // PostgreSQL write pool size
	AutoMigrate bool // apply pending migrations
	ServerLock  bool // PostgreSQL only: take the single-server lock
	Logger      *slog.Logger
}

// Backend is a store plus what the server and the CLIs need beyond
// store.Store.
type Backend interface {
	store.Store
	Ping(ctx context.Context) error
	Backup(ctx context.Context, out string) error
}

// Checkpointer is implemented by a backend with a write-ahead log to fold back
// (SQLite).
type Checkpointer interface {
	StartCheckpointer(ctx context.Context, interval time.Duration, logger *slog.Logger)
}

// Open opens the store o names.
func Open(ctx context.Context, o Options) (Backend, error) {
	switch o.Driver {
	case "", DriverSQLite:
		st, err := sqlite.Open(ctx, o.SQLitePath, sqlite.Options{AutoMigrate: o.AutoMigrate})
		if err != nil {
			return nil, err
		}
		return st, nil
	case DriverPostgres:
		st, err := postgres.Open(ctx, o.PostgresDSN, postgres.Options{
			MaxConns: o.MaxConns, AutoMigrate: o.AutoMigrate, ServerLock: o.ServerLock, Logger: o.Logger,
		})
		if err != nil {
			return nil, err
		}
		return st, nil
	default:
		return nil, fmt.Errorf("store: unknown driver %q (want sqlite or postgres)", o.Driver)
	}
}

// Name is the driver's name as the health checker and logs report it.
func Name(driver string) string {
	if driver == "" {
		return DriverSQLite
	}
	return driver
}
