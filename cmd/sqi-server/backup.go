// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/uberware/sqi/internal/pgdsn"
	"github.com/uberware/sqi/internal/store/backend"
)

var backupFlags struct {
	DBPath  string
	DSN     string
	OutPath string
}

// backupCmd creates an online backup of the state store: VACUUM INTO for
// SQLite, pg_dump for PostgreSQL. The server can be running or stopped.
var backupCmd = &cobra.Command{
	Use:   "backup",
	Short: "Create an online backup of the state store",
	Long: `Create a consistent, online backup of the sqi-server state store.

The server may be running or stopped — either works. The destination file
must not already exist.

SQLite: the backup is produced with SQLite's VACUUM INTO statement, which
snapshots the live database without taking an exclusive lock. The result is
an ordinary SQLite database file; restore it by stopping the server and
putting the file in place of store.sqlite_path.

PostgreSQL: the backup runs "pg_dump --format=custom" against the database,
so the PostgreSQL client tools must be on PATH, at a major version no older
than the server's. Restore the archive into an empty database with
pg_restore.

The store is chosen as follows: --dsn names a PostgreSQL database; --db names
a SQLite file; otherwise store.driver from the resolved configuration (the
root -c/--config file and SQI_STORE_DRIVER) decides. For PostgreSQL the
connection string is store.postgres.dsn (SQI_STORE_POSTGRES_DSN). For SQLite
the path is store.sqlite_path (SQI_STORE_SQLITE_PATH), falling back to the
legacy SQI_SQLITE_PATH environment variable and then to "sqi.db". The source
database must already exist and carry the schema — this command never
creates one.

Examples:
  sqi-server backup --db sqi.db --out sqi-backup-$(date +%Y%m%d).db
  sqi-server backup --dsn postgres://sqi@db.example.com/sqi --out sqi-$(date +%Y%m%d).dump
  pg_restore --dbname postgres://sqi@db.example.com/sqi_restored sqi-20261012.dump`,
	RunE: runBackup,
}

func init() {
	backupCmd.Flags().StringVar(
		&backupFlags.DBPath,
		"db", "sqi.db",
		"path to source SQLite database file (defaults to store.sqlite_path from config, then the deprecated SQI_SQLITE_PATH, then \"sqi.db\")",
	)
	backupCmd.Flags().StringVar(
		&backupFlags.DSN,
		"dsn", "",
		"PostgreSQL connection string (defaults to store.postgres.dsn when store.driver is postgres)",
	)
	backupCmd.Flags().StringVar(
		&backupFlags.OutPath,
		"out", "",
		"destination path for the backup file (must not already exist)",
	)
	if err := backupCmd.MarkFlagRequired("out"); err != nil {
		panic(err)
	}
}

func runBackup(cmd *cobra.Command, _ []string) error {
	if backupFlags.OutPath == "" {
		return errors.New("destination path is empty; use --out")
	}

	tgt, err := resolveStoreTarget(cmd, backupFlags.DBPath, backupFlags.DSN)
	if err != nil {
		return err
	}
	src := tgt.SQLitePath
	if tgt.Driver == backend.DriverPostgres {
		src = pgdsn.Redact(tgt.DSN)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	ctx := context.Background()

	logger.InfoContext(ctx, "backup: opening source database",
		slog.String("driver", backend.Name(tgt.Driver)), slog.String("src", src))
	st, err := openExistingStore(ctx, tgt)
	if err != nil {
		return err
	}
	defer st.Close()

	start := time.Now()
	logger.InfoContext(
		ctx, "backup: starting",
		slog.String("src", src),
		slog.String("dst", backupFlags.OutPath),
	)

	if err := st.Backup(ctx, backupFlags.OutPath); err != nil {
		return err
	}

	elapsed := time.Since(start).Round(time.Millisecond)
	logger.InfoContext(
		ctx, "backup: complete",
		slog.String("dst", backupFlags.OutPath),
		slog.Duration("elapsed", elapsed),
	)
	return nil
}
