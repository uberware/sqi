// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/uberware/sqi/internal/restore"
)

var restoreFlags struct {
	DBPath, DSN, NATSDataDir string
	InPlace                  bool
}

// restoreCmd puts a backup in place of the state store and makes the farm
// safe to resume from it.
var restoreCmd = &cobra.Command{
	Use:   "restore [BACKUP]",
	Short: "Restore the state store from a backup and make the farm safe to resume",
	Long: `Restore the sqi-server state store from a backup made by "sqi-server backup",
with the server stopped.

restore:
  1. takes the single-server lock (refused while a server runs);
  2. checks the backup: an intact sqi backup no newer than this binary;
  3. puts it in place — SQLite: the live database and its -wal/-shm files are
     renamed to <name>.pre-restore-<time>; PostgreSQL: the configured database
     must be empty, and the archive is loaded with pg_restore;
  4. applies any newer migrations;
  5. fences: every task the backup records as assigned or running goes back to
     ready (its attempt closes as a worker shutdown; no retry is used), because
     the workers that held those tasks have moved on;
  6. renames nats.data_dir to <dir>.pre-restore-<time>: JetStream's messages
     belong to a different moment than the restored database.

Work that ran after the backup was taken runs again. Nothing is deleted; remove
the .pre-restore- copies once the farm is healthy.

--in-place skips steps 2 and 3, for a database restored by other means
(PostgreSQL point-in-time recovery, a volume snapshot): it fences and moves the
NATS directory aside so that database is safe to start a server on.

The store is chosen as for backup (--db, --dsn, store.driver). The NATS
directory is nats.data_dir from the same configuration the server reads, or
--nats-data-dir; a relative path is resolved from the current directory, as the
server resolves it.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runRestore,
}

func init() {
	addStoreFlags(restoreCmd.Flags(), &restoreFlags.DBPath, &restoreFlags.DSN, "path to the SQLite database to restore into")
	restoreCmd.Flags().BoolVar(&restoreFlags.InPlace, "in-place", false,
		"fence and quarantine the database already in place, without a backup file")
	restoreCmd.Flags().StringVar(&restoreFlags.NATSDataDir, "nats-data-dir", "",
		"JetStream directory to move aside (defaults to nats.data_dir from config)")
}

func runRestore(cmd *cobra.Command, args []string) error {
	backup := ""
	if len(args) == 1 {
		backup = args[0]
	}
	if restoreFlags.InPlace == (backup != "") {
		return errors.New("give a backup file, or --in-place, not both")
	}
	explicitNATS := flagChanged(cmd, "nats-data-dir")
	if explicitNATS && restoreFlags.NATSDataDir == "" {
		return errors.New("--nats-data-dir is empty; restore must move the JetStream directory aside, " +
			"so name it (or omit the flag to use nats.data_dir)")
	}
	c, err := loadCLIConfig()
	if err != nil {
		return err
	}
	if err := requireRestoreConfig(cmd, c, explicitNATS); err != nil {
		return err
	}
	tgt, err := c.target(cmd, restoreFlags.DBPath, restoreFlags.DSN)
	if err != nil {
		return err
	}
	natsDir := restoreFlags.NATSDataDir
	if !explicitNATS {
		natsDir = c.cfg.NATS.DataDir
	}
	if natsDir == "" {
		return errors.New("nats.data_dir is empty; set it, or pass --nats-data-dir")
	}
	res, err := restore.Run(cmd.Context(), restore.Options{
		Driver: tgt.Driver, SQLitePath: tgt.SQLitePath, PostgresDSN: tgt.DSN,
		NATSDataDir: natsDir, Backup: backup, InPlace: restoreFlags.InPlace,
	})
	if err != nil {
		return err
	}
	printRestoreSummary(os.Stdout, res)
	return nil
}

// requireRestoreConfig refuses a configuration that failed to load (a broken
// discovered file, or an SQI_* variable that does not parse) unless the
// operator named both the store and the NATS directory. Other CLI commands
// fall back to built-in defaults there, but restore renames and replaces
// files, so a guessed default would restore into, and quarantine, the wrong
// places.
func requireRestoreConfig(cmd *cobra.Command, c cliConfig, explicitNATS bool) error {
	if c.loadErr == nil {
		return nil
	}
	if (flagChanged(cmd, "db") || flagChanged(cmd, "dsn")) && explicitNATS {
		return nil
	}
	return fmt.Errorf("could not load configuration: %w; restore will not guess the store or nats.data_dir "+
		"from built-in defaults: fix the configuration, or pass both the store (--db or --dsn) and --nats-data-dir",
		c.loadErr)
}

// printRestoreSummary tells the operator what restore did and where the
// previous state went.
func printRestoreSummary(w io.Writer, r restore.Result) {
	if r.BackupVersion > 0 {
		fmt.Fprintf(w, "restored a schema-%d backup; schema is now %d\n", r.BackupVersion, r.Version)
	}
	for _, p := range r.MovedAside {
		fmt.Fprintf(w, "previous database file moved to %s\n", p)
	}
	fmt.Fprintf(w, "fenced %d attempt(s): their tasks are ready again\n", r.Fenced)
	switch {
	case r.NATSMovedTo != "":
		fmt.Fprintf(w, "NATS data directory moved to %s\n", r.NATSMovedTo)
	case r.NATSDir != "":
		fmt.Fprintf(w, "no NATS data directory at %s; nothing to move (check nats.data_dir if you expected one)\n", r.NATSDir)
	}
}
