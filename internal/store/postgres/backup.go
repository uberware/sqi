// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/uberware/sqi/internal/fsutil"
	"github.com/uberware/sqi/internal/pgdsn"
)

// BackupDSN writes a pg_dump custom-format archive of the database dsn names to
// out, which must not exist, without opening the store, so it works on a
// schema this binary is too old for. It runs the pg_dump found on PATH, which
// must be at least the server's major version. Restore with pg_restore.
//
// The password reaches pg_dump through PGPASSWORD, never its argument list,
// which any local user can read from the process list: see
// [pgDumpInvocation].
func BackupDSN(ctx context.Context, dsn, out string, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}
	db, err := openPool(dsn, 1)
	if err != nil {
		return err
	}
	defer db.Close()
	pgDump, err := preflight(out)
	if err != nil {
		return err
	}
	args, env, err := pgDumpInvocation(dsn, out)
	if err != nil {
		return err
	}
	if err := checkToolAgainstServer(ctx, pgDump, "pg_dump", db); err != nil {
		return err
	}
	return runPgDump(ctx, log, pgDump, args, env, out)
}

// runPgDump creates out exclusively, so a file that appeared after preflight
// is refused rather than overwritten (pg_dump opens --file without O_EXCL),
// and restricted to the writing user ([fsutil.CreateSecretExclusive]): the
// archive holds password hashes, API-key hashes and session tokens, and a mode
// alone protects nothing on Windows. pg_dump then writes into it, with args and
// env added to the inherited environment. On failure it removes out, which it
// created; pg_dump's stderr goes into the error, or, on success, to log at warn
// level.
func runPgDump(ctx context.Context, log *slog.Logger, pgDump string, args, env []string, out string) error {
	if err := fsutil.CreateSecretExclusive(out); errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("postgres: backup destination %s already exists", out)
	} else if err != nil {
		return fmt.Errorf("postgres: create backup destination %s: %w", out, err)
	}
	cmd := exec.CommandContext(ctx, pgDump, args...)
	// A later entry wins over an inherited PGPASSWORD (exec keeps the last).
	cmd.Env = append(os.Environ(), env...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	msg := strings.TrimSpace(stderr.String())
	if err != nil {
		// Never leave a partial archive behind.
		return removeCreated(out, fmt.Errorf("postgres: pg_dump failed: %w: %s", err, msg))
	}
	if msg != "" {
		log.WarnContext(ctx, "pg_dump wrote to stderr", "out", out, "stderr", msg)
	}
	return nil
}

// removeCreated removes out, a file this backup created, after err, and
// returns err with any failure to remove it.
func removeCreated(out string, err error) error {
	if rmErr := os.Remove(out); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
		return errors.Join(err, fmt.Errorf("postgres: remove partial archive %s: %w", out, rmErr))
	}
	return err
}

// pgConnArgs returns the --dbname argument and the environment additions for a
// PostgreSQL client tool (pg_dump, pg_restore) connecting to the database dsn
// names.
//
// The password, as pgx reads it (from the DSN, else PGPASSWORD or the password
// file), goes only into PGPASSWORD. The DSN goes on the command line with its
// password removed by [pgdsn.WithoutPassword], which parses it as the driver
// does; a DSN it cannot read reliably is refused rather than guessed at.
//
// A DSN that sets sslpassword (the client key's passphrase) is refused too, as
// is a PGSSLPASSWORD in the environment (pgx reads it; libpq does not): libpq
// reads the passphrase from no environment variable, so the only way to hand
// it to a tool would be its command line.
func pgConnArgs(dsn string) (dbnameArg string, env []string, err error) {
	// ParseConfig must run first: it rejects every DSN pgx cannot read, and
	// pgdsn's fail-closed checks assume a DSN that pgx accepts.
	cfg, err := pgconn.ParseConfig(dsn)
	if err != nil {
		return "", nil, fmt.Errorf("postgres: parse dsn: %w", err)
	}
	rest, ok := pgdsn.WithoutPassword(dsn)
	if !ok {
		return "", nil, errors.New("postgres: cannot pass this DSN to a PostgreSQL client tool safely; " +
			"use a plain URL or keyword/value DSN")
	}
	if pgdsn.SetsSSLPassword(dsn) || os.Getenv("PGSSLPASSWORD") != "" {
		return "", nil, errors.New("postgres: cannot use an sslpassword (in the DSN or PGSSLPASSWORD) with " +
			"pg_dump or pg_restore: they could only be given it on their command line; use an unencrypted client key")
	}
	if cfg.Password != "" {
		env = []string{"PGPASSWORD=" + cfg.Password}
	}
	return "--dbname=" + rest, env, nil
}

// pgDumpInvocation returns pg_dump's arguments for dumping the database dsn
// names into out, and the variables to add to its environment (see
// [pgConnArgs]).
func pgDumpInvocation(dsn, out string) (args, env []string, err error) {
	dbArg, env, err := pgConnArgs(dsn)
	if err != nil {
		return nil, nil, err
	}
	// --no-password: a missing or wrong password fails rather than prompting
	// on the terminal, which libpq opens directly whatever stdin is.
	return []string{"--format=custom", "--no-owner", "--no-password", "--file=" + out, dbArg}, env, nil
}

// preflight refuses an existing destination and finds pg_dump, before
// anything is written or the database is touched.
func preflight(out string) (string, error) {
	if _, err := os.Stat(out); err == nil {
		return "", fmt.Errorf("postgres: backup destination %s already exists", out)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("postgres: check backup destination %s: %w", out, err)
	}
	return findTool("pg_dump")
}

// findTool finds the PostgreSQL client tool name on PATH.
func findTool(name string) (string, error) {
	p, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("postgres: %s not found on PATH; install the PostgreSQL client tools, "+
			"at least the server's major version", name)
	}
	return p, nil
}

var toolVersion = regexp.MustCompile(`\(PostgreSQL\)\s+(\d+)`)

// parseToolMajor reads the major version from `pg_dump --version` (or
// pg_restore's) output.
func parseToolMajor(s string) (int, error) {
	m := toolVersion.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("postgres: cannot read the PostgreSQL client tool's version from %q", strings.TrimSpace(s))
	}
	return strconv.Atoi(m[1])
}

// checkToolVersion refuses a client tool older than the server: pg_dump cannot
// dump a newer server, and pg_restore cannot be trusted to load into one.
func checkToolVersion(tool string, major, serverVersionNum int) error {
	if serverMajor := serverVersionNum / 10000; major < serverMajor {
		return fmt.Errorf("postgres: %s %d is older than the server (%d); install %s %d or newer",
			tool, major, serverMajor, tool, serverMajor)
	}
	return nil
}

// checkToolAgainstServer runs `<tool> --version` and refuses a tool older than
// the server db is connected to. Backup and restore share it so the two cannot
// drift.
func checkToolAgainstServer(ctx context.Context, toolPath, tool string, db *sql.DB) error {
	verOut, err := exec.CommandContext(ctx, toolPath, "--version").Output()
	if err != nil {
		return fmt.Errorf("postgres: run %s --version: %w", tool, err)
	}
	major, err := parseToolMajor(string(verOut))
	if err != nil {
		return err
	}
	var serverNum string
	if err := db.QueryRowContext(ctx, `SHOW server_version_num`).Scan(&serverNum); err != nil {
		return fmt.Errorf("postgres: read server version: %w", err)
	}
	n, err := strconv.Atoi(serverNum)
	if err != nil {
		return fmt.Errorf("postgres: server_version_num %q: %w", serverNum, err)
	}
	return checkToolVersion(tool, major, n)
}
