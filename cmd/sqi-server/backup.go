// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/uberware/sqi/internal/store/sqlite"
)

var backupFlags struct {
	DBPath  string
	OutPath string
}

// backupCmd creates an online backup of the SQLite database using VACUUM INTO.
// The source database can be live (server running) or offline.
var backupCmd = &cobra.Command{
	Use:   "backup",
	Short: "Create an online backup of the SQLite database",
	Long: `Create a consistent, online backup of the sqi-server SQLite database.

The backup is produced using SQLite's VACUUM INTO statement, which snapshots
the live database without taking an exclusive lock. The server may be running
or stopped — either works. The destination file must not already exist.

The source database path defaults to store.sqlite_path from the resolved
configuration (the root -c/--config file and SQI_STORE_SQLITE_PATH), falling
back to the legacy SQI_SQLITE_PATH environment variable and then to "sqi.db".
Pass --db to override it explicitly. The source database must already exist —
this command never creates one.

Example:
  sqi-server backup --db sqi.db --out sqi-backup-$(date +%Y%m%d).db`,
	RunE: runBackup,
}

func init() {
	backupCmd.Flags().StringVar(
		&backupFlags.DBPath,
		"db", "sqi.db",
		"path to source SQLite database file (defaults to store.sqlite_path from config, or SQI_SQLITE_PATH)",
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

	dbPath, err := resolveDBPath(backupFlags.DBPath, cmd != nil && cmd.Flags().Changed("db"))
	if err != nil {
		return err
	}
	if dbPath == "" {
		return errors.New("source database path is empty; use --db, set store.sqlite_path, or set SQI_STORE_SQLITE_PATH")
	}
	if err := requireExistingDB(dbPath); err != nil {
		return err
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	ctx := context.Background()

	logger.InfoContext(ctx, "backup: opening source database", slog.String("path", dbPath))
	st, err := sqlite.Open(ctx, dbPath, sqlite.Options{AutoMigrate: false})
	if err != nil {
		return fmt.Errorf("open source database: %w", err)
	}
	defer st.Close()

	start := time.Now()
	logger.InfoContext(
		ctx, "backup: starting",
		slog.String("src", dbPath),
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
