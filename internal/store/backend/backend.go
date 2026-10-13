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
	MaxConns    int // PostgreSQL write pool size
	// AutoMigrate applies pending migrations before Open returns. The zero
	// Options never migrates, unlike sqlite.DefaultOptions and
	// postgres.DefaultOptions. The server sets it; the backup and worker CLIs
	// leave it false because they write into an existing schema and must
	// never create one; migrate drives goose itself.
	AutoMigrate bool
	ServerLock  bool // take the single-server lock (both drivers)
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
	StartCheckpointer(ctx context.Context, interval time.Duration, logger *slog.Logger) <-chan struct{}
}

// Open opens the store o names.
func Open(ctx context.Context, o Options) (Backend, error) {
	switch o.Driver {
	case "", DriverSQLite:
		st, err := sqlite.Open(ctx, o.SQLitePath, sqlite.Options{AutoMigrate: o.AutoMigrate, ServerLock: o.ServerLock})
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

// LockStore takes the single-server lock on the store o names and returns its
// release. Commands that must not run beside a server (restore, store copy,
// migrate up) call it; they then open the store with ServerLock false.
func LockStore(ctx context.Context, o Options) (func() error, error) {
	switch o.Driver {
	case "", DriverSQLite:
		l, err := sqlite.AcquireLock(o.SQLitePath)
		if err != nil {
			return nil, err
		}
		return l.Release, nil
	case DriverPostgres:
		l, err := postgres.LockDatabase(ctx, o.PostgresDSN, o.Logger)
		if err != nil {
			return nil, err
		}
		return l.Release, nil
	default:
		return nil, fmt.Errorf("store: unknown driver %q (want sqlite or postgres)", o.Driver)
	}
}

// Backup writes a backup of the store o names to out, which must not exist.
// It never opens the store, so it works whatever the schema version.
func Backup(ctx context.Context, o Options, out string) error {
	switch o.Driver {
	case "", DriverSQLite:
		return sqlite.BackupPath(ctx, o.SQLitePath, out)
	case DriverPostgres:
		return postgres.BackupDSN(ctx, o.PostgresDSN, out, o.Logger)
	default:
		return fmt.Errorf("store: unknown driver %q (want sqlite or postgres)", o.Driver)
	}
}

// Name is the driver's name as the health checker and logs report it.
func Name(driver string) string {
	if driver == "" {
		return DriverSQLite
	}
	return driver
}
