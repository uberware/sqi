// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/config"
	"github.com/uberware/sqi/internal/redact"
	"github.com/uberware/sqi/internal/restore"
	"github.com/uberware/sqi/internal/server"
)

var restoreFlags struct {
	DBPath, DSN, NATSDataDir string
	InPlace, SkipNATSPurge   bool
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
  6. clears the broker's messages, which belong to a different moment than the
     restored database:
     - embedded broker: renames nats.embedded.data_dir to
       <dir>.pre-restore-<time>;
     - external broker (nats.mode: external): purges sqi's four streams
       (SQI_TASK, SQI_LOGS, SQI_WORKER, SQI_CANCEL) and deletes the server's
       three durable consumers (sqi-task-status-srv, sqi-task-logs-srv,
       sqi-worker-srv) on the cluster, as the configured data credential.

Work that ran after the backup was taken runs again. Nothing in the store is
deleted; remove the .pre-restore- copies once the farm is healthy.

--in-place skips steps 2 and 3, for a database restored by other means
(PostgreSQL point-in-time recovery, a volume snapshot): it fences and clears the
broker's messages so that database is safe to start a server on.

The store is chosen as for backup (--db, --dsn, store.driver). With an embedded
broker the NATS directory is nats.embedded.data_dir from the same configuration
the server reads, or --nats-data-dir; a relative path is resolved from the
current directory, as the server resolves it.

With an external broker, restore connects and takes the account's one-server
guard before it changes anything, and holds it to the end, so no sqi-server can
start on the account meanwhile. It refuses, with nothing changed, while a server
holds the account, or when it cannot reach the broker. --skip-nats-purge
restores without the purge and prints what to purge by hand, which must be done
before a server starts. --nats-data-dir does not apply to an external broker.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runRestore,
}

func init() {
	addStoreFlags(restoreCmd.Flags(), &restoreFlags.DBPath, &restoreFlags.DSN, "path to the SQLite database to restore into")
	restoreCmd.Flags().BoolVar(&restoreFlags.InPlace, "in-place", false,
		"fence and quarantine the database already in place, without a backup file")
	restoreCmd.Flags().StringVar(&restoreFlags.NATSDataDir, "nats-data-dir", "",
		"JetStream directory to move aside (defaults to nats.embedded.data_dir from config; embedded broker only)")
	restoreCmd.Flags().BoolVar(&restoreFlags.SkipNATSPurge, "skip-nats-purge", false,
		"external broker only: restore without purging sqi's JetStream state, and print what to purge by hand")
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
			"so name it (or omit the flag to use nats.embedded.data_dir)")
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
	o := restore.Options{
		Driver: tgt.Driver, SQLitePath: tgt.SQLitePath, PostgresDSN: tgt.DSN,
		Backup: backup, InPlace: restoreFlags.InPlace,
	}
	release, err := restoreBroker(cmd, c.cfg.NATS, explicitNATS, &o)
	if err != nil {
		return err
	}
	defer release()
	res, err := restore.Run(cmd.Context(), o)
	if err != nil {
		return err
	}
	printRestoreSummary(os.Stdout, res)
	if restoreFlags.SkipNATSPurge {
		printManualPurge(os.Stdout, c.cfg.NATS.External)
	}
	return nil
}

// restoreBroker sets how restore clears the broker's messages: the embedded
// broker's directory to move aside, or the external broker's purge. For the
// purge it takes the broker account first, before restore changes anything,
// and returns a function giving it back; that function must be called.
func restoreBroker(cmd *cobra.Command, nc config.NATSConfig, explicitNATS bool, o *restore.Options) (release func(), err error) {
	if nc.Mode != config.NATSModeExternal {
		if restoreFlags.SkipNATSPurge {
			return nil, errors.New("--skip-nats-purge applies only to an external broker (nats.mode: external); " +
				"an embedded broker's directory is moved aside, not purged")
		}
		o.NATSDataDir = restoreFlags.NATSDataDir
		if !explicitNATS {
			o.NATSDataDir = nc.Embedded.DataDir
		}
		if o.NATSDataDir == "" {
			return nil, errors.New("nats.embedded.data_dir is empty; set it, or pass --nats-data-dir")
		}
		return func() {}, nil
	}
	if explicitNATS {
		return nil, errors.New("--nats-data-dir applies only to an embedded broker; with nats.mode: external, " +
			"restore purges sqi's JetStream state on the cluster instead")
	}
	if restoreFlags.SkipNATSPurge {
		return func() {}, nil
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	lock, err := bus.NewRemote(server.RemoteBrokerConfig(nc), logger).LockForRestore(cmd.Context())
	switch {
	case errors.Is(err, bus.ErrGuardHeld):
		return nil, fmt.Errorf("restore changed nothing: %w; stop that sqi-server before restoring", err)
	case err != nil:
		return nil, fmt.Errorf("restore changed nothing: it must purge sqi's JetStream state on the external broker "+
			"and cannot: %w; fix the broker connection, or pass --skip-nats-purge to restore without the purge "+
			"and purge by hand (restore then prints what)", err)
	}
	o.PurgeNATS = lock.Purge
	return lock.Release, nil
}

// requireRestoreConfig refuses a configuration that failed to load (a broken
// discovered file, or an SQI_* variable that does not parse) unless the
// operator named both the store and the NATS directory. Other CLI commands
// fall back to built-in defaults there, but restore renames and replaces
// files, so a guessed default would restore into, and quarantine, the wrong
// places.
//
// The defaults are an embedded broker's, so the fallback is refused outright
// when SQI_NATS_MODE asks for an external one: it would skip the purge the
// cluster needs and report success.
func requireRestoreConfig(cmd *cobra.Command, c cliConfig, explicitNATS bool) error {
	if c.loadErr == nil {
		return nil
	}
	if envNATSModeExternal() {
		return fmt.Errorf("could not load configuration: %w; SQI_NATS_MODE is external, and restore cannot purge "+
			"the cluster without the nats.external configuration: fix the configuration", c.loadErr)
	}
	if (flagChanged(cmd, "db") || flagChanged(cmd, "dsn")) && explicitNATS {
		return nil
	}
	return fmt.Errorf("could not load configuration: %w; restore will not guess the store or nats.embedded.data_dir "+
		"from built-in defaults: fix the configuration, or, with an embedded broker only, pass both the store "+
		"(--db or --dsn) and --nats-data-dir; with nats.mode: external the configuration must be fixed, because "+
		"restore cannot purge the cluster without it and --nats-data-dir does not apply",
		c.loadErr)
}

// envNATSModeExternal reports whether SQI_NATS_MODE asks for an external
// broker. It is read directly because a configuration that failed to load
// has lost it. Case and surrounding space are ignored, erring toward refusal.
func envNATSModeExternal() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("SQI_NATS_MODE")), config.NATSModeExternal)
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
	case r.NATSPurged:
		fmt.Fprintln(w, "purged sqi's JetStream state on the external broker: its streams are empty and the server's durable consumers deleted")
	case r.NATSMovedTo != "":
		fmt.Fprintf(w, "NATS data directory moved to %s\n", r.NATSMovedTo)
	case r.NATSDir != "":
		fmt.Fprintf(w, "no NATS data directory at %s; nothing to move (check nats.embedded.data_dir if you expected one)\n", r.NATSDir)
	}
}

// printManualPurge tells the operator who passed --skip-nats-purge what to
// purge on the external broker before a server starts, with an example
// using the nats CLI as sqi's data user.
func printManualPurge(w io.Writer, ex config.NATSExternalConfig) {
	streams, consumers := bus.RestoreTargets()
	fmt.Fprintf(w, "sqi's JetStream state on the external broker was NOT purged (--skip-nats-purge). "+
		"Before starting a server, in account %s:\n", ex.Account)
	fmt.Fprintf(w, "  purge the streams %s\n", strings.Join(streams, ", "))
	named := make([]string, 0, len(consumers))
	for _, c := range consumers {
		named = append(named, c.Name+" (stream "+c.Stream+")")
	}
	fmt.Fprintf(w, "  delete the durable consumers %s\n", strings.Join(named, ", "))
	conn := natsCLIConnFlags(ex)
	fmt.Fprintln(w, "for example, with the nats CLI as sqi's data user:")
	for _, s := range streams {
		fmt.Fprintf(w, "  nats stream purge %s -f%s\n", s, conn)
	}
	for _, c := range consumers {
		fmt.Fprintf(w, "  nats consumer rm %s %s -f%s\n", c.Stream, c.Name, conn)
	}
}

// natsCLIConnFlags are the nats CLI flags connecting as ex's data user, each
// with a leading space and quoted for a POSIX shell. Credentials in a URL are
// replaced by a placeholder, never printed.
func natsCLIConnFlags(ex config.NATSExternalConfig) string {
	var b strings.Builder
	add := func(flag, value string) {
		if value != "" {
			fmt.Fprintf(&b, " %s %s", flag, shellQuote(value))
		}
	}
	add("--server", redact.URLs(ex.URLs, ","))
	add("--nkey", ex.NkeySeedFile)
	add("--creds", ex.CredsFile)
	add("--tlsca", ex.TLS.CAFile)
	add("--tlscert", ex.TLS.CertFile)
	add("--tlskey", ex.TLS.KeyFile)
	return b.String()
}

// shellSafe matches a word a POSIX shell takes literally.
var shellSafe = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// shellQuote quotes s as one literal POSIX shell word: single quotes, inside
// which the shell expands nothing. A single quote in s closes the quoting, is
// written escaped, and reopens it.
func shellQuote(s string) string {
	if shellSafe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
