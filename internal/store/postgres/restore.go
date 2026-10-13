// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
)

// ErrUnreadableArchive means pg_restore could not read the file as an archive,
// or the archive's goose_db_version data could not be parsed.
var ErrUnreadableArchive = errors.New("postgres: unreadable archive")

// ArchiveSchemaVersion reads the sqi schema version recorded in a pg_dump
// custom-format archive without connecting to any database: pg_restore prints
// goose_db_version's data as a COPY block. 0 means the archive has no
// goose_db_version data (not an sqi backup). A file pg_restore cannot read, or
// whose goose data cannot be parsed, is [ErrUnreadableArchive]; a missing
// pg_restore, a canceled ctx, and a pg_restore older than the pg_dump that
// wrote the archive are other errors, since the archive may be fine.
func ArchiveSchemaVersion(ctx context.Context, archive string) (int64, error) {
	tool, err := findTool("pg_restore")
	if err != nil {
		return 0, err
	}
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, tool, "--data-only", "--table=goose_db_version", "--file=-", archive)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		switch {
		case ctx.Err() != nil:
			return 0, fmt.Errorf("postgres: read archive %s: %w", archive, ctx.Err())
		case strings.Contains(msg, "unsupported version"):
			// pg_restore reads only archive formats up to its own version's.
			return 0, fmt.Errorf("postgres: %s cannot read archive %s, which a newer pg_dump wrote: install a "+
				"pg_restore at least as new as the pg_dump that made the backup: %s", tool, archive, msg)
		}
		return 0, fmt.Errorf("%w %s: %w: %s", ErrUnreadableArchive, archive, err, msg)
	}
	v, err := parseGooseCopy(stdout.String())
	if err != nil {
		return 0, fmt.Errorf("%w %s: %w", ErrUnreadableArchive, archive, err)
	}
	return v, nil
}

// parseGooseCopy returns the highest applied version_id in a pg_restore SQL
// script's COPY block for goose_db_version, reading the column order from the
// COPY header.
func parseGooseCopy(script string) (int64, error) {
	sc := bufio.NewScanner(strings.NewReader(script))
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	verCol, appliedCol, in := -1, -1, false
	var maxV int64
	for sc.Scan() {
		line := sc.Text()
		if !in {
			if !strings.HasPrefix(line, "COPY ") || !strings.Contains(line, "goose_db_version") {
				continue
			}
			open, closeIdx := strings.Index(line, "("), strings.Index(line, ")")
			if open < 0 || closeIdx < open {
				return 0, fmt.Errorf("postgres: unreadable COPY header %q", line)
			}
			cols := strings.Split(line[open+1:closeIdx], ", ")
			verCol, appliedCol = slices.Index(cols, "version_id"), slices.Index(cols, "is_applied")
			if verCol < 0 || appliedCol < 0 {
				return 0, fmt.Errorf("postgres: goose_db_version lacks version_id or is_applied: %q", line)
			}
			in = true
			continue
		}
		if line == `\.` {
			break
		}
		f := strings.Split(line, "\t")
		if len(f) <= max(verCol, appliedCol) || f[appliedCol] != "t" {
			continue
		}
		v, err := strconv.ParseInt(f[verCol], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("postgres: goose version %q: %w", f[verCol], err)
		}
		maxV = max(maxV, v)
	}
	return maxV, sc.Err()
}

// RequireEmptyDatabase fails unless the database dsn names has no tables in
// its search path. restore loads an archive only into an empty database: sqi
// does not drop databases (it may lack the privilege, and it is the
// operator's decision).
//
// Tables are listed from pg_class, which every role can read in full, not
// information_schema, which hides tables the role has no privilege on.
func RequireEmptyDatabase(ctx context.Context, dsn string) error {
	db, err := openPool(dsn, 1)
	if err != nil {
		return err
	}
	defer db.Close()
	var n int
	var name sql.NullString
	if err := db.QueryRowContext(ctx, `
SELECT count(*), min(c.relname::text)
FROM   pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE  c.relkind IN ('r', 'p') AND n.nspname = ANY (current_schemas(false))`).Scan(&n, &name); err != nil {
		return fmt.Errorf("postgres: list tables: %w", err)
	}
	if n > 0 {
		return fmt.Errorf("restore needs an empty database, and this one has %d tables (%s, ...); "+
			"drop and recreate it, then run restore again", n, name.String)
	}
	return nil
}

// RequireMigratedDatabase fails unless the database dsn names already holds an
// sqi schema (an applied goose version). It creates nothing: restore --in-place
// uses it to refuse an empty database before anything could migrate one.
func RequireMigratedDatabase(ctx context.Context, dsn string) error {
	db, err := openPool(dsn, 1)
	if err != nil {
		return err
	}
	defer db.Close()
	v, err := SchemaVersion(ctx, db)
	if err != nil {
		return err
	}
	if v == 0 {
		return errors.New("the database has no sqi schema, so there is nothing to restore in place; " +
			"restore a backup into it instead")
	}
	return nil
}

// CheckRestoreTool finds pg_restore on PATH and refuses one older than the
// server dsn names, so a restore can report a tool problem before it reads the
// archive.
func CheckRestoreTool(ctx context.Context, dsn string) error {
	_, err := restoreTool(ctx, dsn)
	return err
}

// restoreTool is CheckRestoreTool, returning pg_restore's path.
func restoreTool(ctx context.Context, dsn string) (string, error) {
	tool, err := findTool("pg_restore")
	if err != nil {
		return "", err
	}
	db, err := openPool(dsn, 1)
	if err != nil {
		return "", err
	}
	defer db.Close()
	if err := checkToolAgainstServer(ctx, tool, "pg_restore", db); err != nil {
		return "", err
	}
	return tool, nil
}

// Restore loads a pg_dump custom-format archive into the empty database dsn
// names, in one transaction (a failed load leaves it empty), with the
// password passed as for backup. pg_restore must be at least the server's
// major version.
//
// Ownership and grants are not restored (--no-owner, --no-acl): the archive's
// roles need not exist in the target cluster, and under --exit-on-error one
// GRANT to a missing role would fail the whole load.
func Restore(ctx context.Context, dsn, archive string, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}
	dbArg, env, err := pgConnArgs(dsn)
	if err != nil {
		return err
	}
	tool, err := restoreTool(ctx, dsn)
	if err != nil {
		return err
	}
	// --no-password: a missing or wrong password fails rather than prompting.
	cmd := exec.CommandContext(ctx, tool, "--no-owner", "--no-acl", "--no-password", "--single-transaction",
		"--exit-on-error", dbArg, archive)
	// A later entry wins over an inherited PGPASSWORD (exec keeps the last).
	cmd.Env = append(os.Environ(), env...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("postgres: pg_restore failed: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	if msg := strings.TrimSpace(stderr.String()); msg != "" {
		log.WarnContext(ctx, "pg_restore wrote to stderr", "stderr", msg)
	}
	return nil
}
