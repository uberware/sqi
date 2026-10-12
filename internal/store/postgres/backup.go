// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/uberware/sqi/internal/pgdsn"
)

// Backup writes a pg_dump custom-format archive of the database to out, which
// must not exist. It runs the pg_dump found on PATH, which must be at least the
// server's major version. Restore with pg_restore.
//
// The password reaches pg_dump through PGPASSWORD, never its argument list,
// which any local user can read from the process list: see
// [pgDumpInvocation].
func (s *Store) Backup(ctx context.Context, out string) error {
	pgDump, err := preflight(out)
	if err != nil {
		return err
	}
	args, env, err := pgDumpInvocation(s.dsn, out)
	if err != nil {
		return err
	}
	verOut, err := exec.CommandContext(ctx, pgDump, "--version").Output()
	if err != nil {
		return fmt.Errorf("postgres: run pg_dump --version: %w", err)
	}
	dumpMajor, err := parsePgDumpMajor(string(verOut))
	if err != nil {
		return err
	}
	var serverNum string
	if err := s.rdb.QueryRowContext(ctx, `SHOW server_version_num`).Scan(&serverNum); err != nil {
		return fmt.Errorf("postgres: read server version: %w", err)
	}
	n, err := strconv.Atoi(serverNum)
	if err != nil {
		return fmt.Errorf("postgres: server_version_num %q: %w", serverNum, err)
	}
	if err := checkPgDumpVersion(dumpMajor, n); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, pgDump, args...)
	// A later entry wins over an inherited PGPASSWORD (exec keeps the last).
	cmd.Env = append(os.Environ(), env...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		err = fmt.Errorf("postgres: pg_dump failed: %w: %s", err, strings.TrimSpace(stderr.String()))
		// Never leave a partial archive behind.
		if rmErr := os.Remove(out); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
			err = errors.Join(err, fmt.Errorf("postgres: remove partial archive %s: %w", out, rmErr))
		}
		return err
	}
	return nil
}

// pgDumpInvocation returns pg_dump's arguments for dumping the database dsn
// names into out, and the variables to add to its environment.
//
// The password, as pgx reads it (from the DSN, else PGPASSWORD or the password
// file), goes only into PGPASSWORD. The DSN goes on the command line with its
// password removed by [pgdsn.WithoutPassword], which parses it as the driver
// does; a DSN it cannot read reliably is refused rather than guessed at.
//
// A DSN that sets sslpassword (the client key's passphrase) is refused too, as
// is a PGSSLPASSWORD in the environment (pgx reads it; libpq does not): libpq
// reads the passphrase from no environment variable, so the only way to hand
// it to pg_dump would be its command line.
func pgDumpInvocation(dsn, out string) (args, env []string, err error) {
	cfg, err := pgconn.ParseConfig(dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("postgres: parse dsn: %w", err)
	}
	rest, ok := pgdsn.WithoutPassword(dsn)
	if !ok {
		return nil, nil, errors.New("postgres: cannot pass this DSN to pg_dump safely; " +
			"use a plain URL or keyword/value DSN")
	}
	if pgdsn.SetsSSLPassword(dsn) || os.Getenv("PGSSLPASSWORD") != "" {
		return nil, nil, errors.New("postgres: cannot back up with an sslpassword (in the DSN or PGSSLPASSWORD): " +
			"pg_dump could only be given it on its command line; use an unencrypted client key")
	}
	// --no-password: a missing or wrong password fails rather than prompting
	// on the terminal, which libpq opens directly whatever stdin is.
	args = []string{"--format=custom", "--no-owner", "--no-password", "--file=" + out, "--dbname=" + rest}
	if cfg.Password != "" {
		env = []string{"PGPASSWORD=" + cfg.Password}
	}
	return args, env, nil
}

// preflight refuses an existing destination and finds pg_dump, before
// anything is written or the database is touched.
func preflight(out string) (string, error) {
	if _, err := os.Stat(out); err == nil {
		return "", fmt.Errorf("postgres: backup destination %s already exists", out)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("postgres: check backup destination %s: %w", out, err)
	}
	p, err := exec.LookPath("pg_dump")
	if err != nil {
		return "", errors.New("postgres: pg_dump not found on PATH; install the PostgreSQL client tools, " +
			"at least the server's major version")
	}
	return p, nil
}

var pgDumpVersion = regexp.MustCompile(`\(PostgreSQL\)\s+(\d+)`)

// parsePgDumpMajor reads the major version from `pg_dump --version` output.
func parsePgDumpMajor(s string) (int, error) {
	m := pgDumpVersion.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("postgres: cannot read pg_dump's version from %q", strings.TrimSpace(s))
	}
	return strconv.Atoi(m[1])
}

// checkPgDumpVersion refuses a pg_dump older than the server: it cannot dump a
// newer server.
func checkPgDumpVersion(dumpMajor, serverVersionNum int) error {
	if serverMajor := serverVersionNum / 10000; dumpMajor < serverMajor {
		return fmt.Errorf("postgres: pg_dump %d is older than the server (%d); install pg_dump %d or newer",
			dumpMajor, serverMajor, serverMajor)
	}
	return nil
}
