// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/uberware/sqi/internal/store/storecopy"
)

var storeCopyFlags struct{ From, To string }

var storeCmd = &cobra.Command{
	Use:   "store",
	Short: "State store maintenance",
}

var storeCopyCmd = &cobra.Command{
	Use:   "copy",
	Short: "Copy a SQLite state store into an empty PostgreSQL database",
	Long: `Copy every row of a SQLite state store into an empty PostgreSQL database, with
the server stopped, then verify each table by row count and digest before
committing. A failure leaves the PostgreSQL database with no rows.

--from defaults to store.sqlite_path, and --to to store.postgres.dsn, so with
store.driver set to postgres and the DSN configured, "sqi-server store copy" needs
no flags. The SQLite database must be at this binary's schema version (run
"sqi-server migrate up" first); the PostgreSQL database must be empty or freshly
migrated, and hold no table sqi does not own. Both databases are locked for the
copy, so it refuses to run while a server uses either. Leave the password out of
--to (the process list shows it) and supply it with PGPASSWORD or a password file.

Afterwards set store.driver: postgres and start the server. If it will run with a
different (or empty) nats.data_dir than the SQLite server had, run
"sqi-server restore --in-place" first so no task waits on a report that is gone.

PostgreSQL to SQLite is not supported.`,
	Args: cobra.NoArgs,
	RunE: runStoreCopy,
}

func init() {
	storeCopyCmd.Flags().StringVar(&storeCopyFlags.From, "from", "sqi.db",
		"SQLite database to copy (defaults to store.sqlite_path from config, then the deprecated SQI_SQLITE_PATH, then \"sqi.db\")")
	storeCopyCmd.Flags().StringVar(&storeCopyFlags.To, "to", "",
		"PostgreSQL connection string (defaults to store.postgres.dsn); leave the password out, since the process "+
			"list shows it, and supply it via PGPASSWORD or a password file")
	storeCmd.AddCommand(storeCopyCmd)
}

func runStoreCopy(cmd *cobra.Command, _ []string) error {
	explicitTo := flagChanged(cmd, "to")
	if explicitTo && storeCopyFlags.To == "" {
		return errors.New("--to is empty; name the PostgreSQL database to copy into")
	}
	c, err := loadCLIConfig()
	if err != nil {
		return err
	}
	// A configuration that failed to load has lost store.sqlite_path and
	// store.postgres.dsn; copying from a guessed default is not what the
	// operator asked for.
	if c.loadErr != nil && (!flagChanged(cmd, "from") || !explicitTo) {
		return fmt.Errorf("could not load configuration: %w; store copy will not guess the databases from built-in "+
			"defaults: fix the configuration, or pass both --from and --to", c.loadErr)
	}
	from := c.dbPath(storeCopyFlags.From, flagChanged(cmd, "from"))
	to := storeCopyFlags.To
	if !explicitTo {
		to = c.cfg.Store.Postgres.DSN
	}
	if to == "" {
		return errors.New("no PostgreSQL DSN; use --to, set store.postgres.dsn, or set SQI_STORE_POSTGRES_DSN")
	}
	res, err := storecopy.Copy(cmd.Context(), storecopy.Options{SQLitePath: from, PostgresDSN: to})
	if err != nil {
		return err
	}
	var total int64
	for _, tr := range res.Tables {
		fmt.Fprintf(os.Stdout, "%-22s %d rows\n", tr.Table, tr.Rows)
		total += tr.Rows
	}
	fmt.Fprintf(os.Stdout, "copied and verified %d rows in %s; set store.driver: postgres and start the server\n",
		total, res.Elapsed.Round(time.Millisecond))
	return nil
}
