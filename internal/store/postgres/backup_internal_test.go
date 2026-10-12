// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestBackup_RefusesExistingOut(t *testing.T) {
	out := filepath.Join(t.TempDir(), "exists.dump")
	if err := os.WriteFile(out, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := preflight(out); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("preflight(existing) = %v", err)
	}
}

func TestBackup_PgDumpMissing(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if _, err := preflight(filepath.Join(t.TempDir(), "x.dump")); err == nil || !strings.Contains(err.Error(), "pg_dump not found") {
		t.Fatalf("preflight without pg_dump = %v", err)
	}
}

func TestBackup_PgDumpTooOld(t *testing.T) {
	if err := checkPgDumpVersion(16, 180002); err == nil || !strings.Contains(err.Error(), "older than the server") {
		t.Fatalf("pg_dump 16 against server 18 = %v", err)
	}
	if err := checkPgDumpVersion(18, 160004); err != nil {
		t.Fatalf("pg_dump 18 against server 16 refused: %v", err)
	}
}

func TestParsePgDumpMajor(t *testing.T) {
	for in, want := range map[string]int{
		"pg_dump (PostgreSQL) 16.4\n":                      16,
		"pg_dump (PostgreSQL) 18.1 (Debian 18.1-1.pgdg)\n": 18,
	} {
		if got, err := parsePgDumpMajor(in); err != nil || got != want {
			t.Errorf("parsePgDumpMajor(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	if _, err := parsePgDumpMajor("garbage"); err == nil {
		t.Error("parsePgDumpMajor accepted garbage")
	}
}

// isolatePGEnv clears the environment pgx falls back to, so the password
// pg_dump is handed comes from the DSN under test alone.
func isolatePGEnv(t *testing.T) {
	t.Helper()
	for _, env := range []string{"PGHOST", "PGPORT", "PGDATABASE", "PGUSER", "PGPASSWORD", "PGSSLMODE", "PGSSLPASSWORD", "PGSERVICE"} {
		t.Setenv(env, "")
	}
	t.Setenv("PGPASSFILE", filepath.Join(t.TempDir(), "no-pgpass"))
}

// TestPgDumpInvocation_NoPasswordInArgv builds pg_dump's argument list for
// DSNs carrying a password, in both forms and in every spelling known to have
// leaked through a parser other than pgx's, and checks that the password is
// handed over through PGPASSWORD and never appears among the arguments.
func TestPgDumpInvocation_NoPasswordInArgv(t *testing.T) {
	isolatePGEnv(t)
	tests := []struct{ dsn, password, secret string }{
		// The plain forms.
		{"postgres://sqi:hunter2@db:5432/sqi?sslmode=require", "hunter2", "hunter2"},
		{"postgres://sqi@db/sqi?password=hunter2&sslmode=disable", "hunter2", "hunter2"},
		{"host=db user=sqi password=hunter2 dbname=sqi", "hunter2", "hunter2"},
		{"host=db password='p@ss word' dbname=sqi sslmode=verify-full", "p@ss word", "p@ss word"},
		{"postgresql://sqi:p%40ss@db/sqi", "p@ss", "p%40ss"},
		{"postgres://sqi:hunter2@h1:5432,h2:5432/sqi", "hunter2", "hunter2"},
		// The keyword/value spellings a regex leaked.
		{"host=db password='ab\\\ncd'", "ab\ncd", "cd"},
		{"host=db password=hun\\\nter2", "hun\nter2", "ter2"},
		{"host=db password\v=hunter2", "hunter2", "hunter2"},
		{"host=db password=\v'abc def'", "abc def", "def"},
		{"host=db password =\v hunter2", "hunter2", "hunter2"},
		{"host=db application_name=password= password =hunter2", "hunter2", "hunter2"},
		{"host=db\vpassword=hunter2\fdbname=sqi\r", "hunter2", "hunter2"},
		{`  host=db   password='a\'b'sslmode=disable  `, "a'b", "a'b"},
		{`host=db password='it\'s' dbname=sqi`, "it's", "it's"},
		{"host=db password=first password=second dbname=sqi", "second", "first"},
	}
	for _, tc := range tests {
		args, env, err := pgDumpInvocation(tc.dsn, "/backups/sqi.dump")
		if err != nil {
			t.Errorf("pgDumpInvocation(%q): %v", tc.dsn, err)
			continue
		}
		for _, a := range args {
			if strings.Contains(a, tc.secret) || strings.Contains(a, tc.password) {
				t.Errorf("pgDumpInvocation(%q): argument %q carries the password", tc.dsn, a)
			}
			// And structurally: the driver reads no password from what pg_dump
			// is given (the environment is isolated, so none comes from there).
			if dbname, ok := strings.CutPrefix(a, "--dbname="); ok {
				if cfg, err := pgconn.ParseConfig(dbname); err != nil || cfg.Password != "" {
					t.Errorf("pgDumpInvocation(%q): --dbname %q reads as password %q, err %v", tc.dsn, dbname, cfg.Password, err)
				}
			}
		}
		if !slices.Contains(env, "PGPASSWORD="+tc.password) {
			t.Errorf("pgDumpInvocation(%q): env %q does not hand over PGPASSWORD=%q", tc.dsn, env, tc.password)
		}
		if !slices.Contains(args, "--file=/backups/sqi.dump") || !slices.ContainsFunc(args, func(a string) bool {
			return strings.HasPrefix(a, "--dbname=")
		}) {
			t.Errorf("pgDumpInvocation(%q) = %q: missing --file or --dbname", tc.dsn, args)
		}
		if strings.Contains(tc.dsn, "sslmode") && !slices.ContainsFunc(args, func(a string) bool {
			return strings.HasPrefix(a, "--dbname=") && strings.Contains(a, "sslmode")
		}) {
			t.Errorf("pgDumpInvocation(%q) = %q: the DSN lost its sslmode", tc.dsn, args)
		}
	}
}

// TestPgDumpInvocation_Refusals covers the DSNs pg_dump is not run with: one
// pgx rejects, one whose password cannot be located reliably, and one with a
// client key passphrase, which libpq reads from no environment variable.
func TestPgDumpInvocation_Refusals(t *testing.T) {
	isolatePGEnv(t)
	tests := []struct{ dsn, want string }{
		{"host=db password='hunter2 dbname=sqi", "parse dsn"},
		{"postgres://sqi:hunter2@db/sqi?password=ab&cd", "parse dsn"},
		{"postgres://sqi:p@ss@db/sqi", "cannot pass this DSN to pg_dump safely"},
		{"postgres://sqi@db/sqi?application_name=a@b&password=hunter2", "cannot pass this DSN to pg_dump safely"},
		{"host=db sslpassword=hunter2 sslmode=disable", "sslpassword"},
		{"postgres://sqi@db/sqi?sslpassword=hunter2&sslmode=disable", "sslpassword"},
	}
	for _, tc := range tests {
		args, env, err := pgDumpInvocation(tc.dsn, "/backups/sqi.dump")
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("pgDumpInvocation(%q) = %v, want an error containing %q", tc.dsn, err, tc.want)
		}
		if args != nil || env != nil {
			t.Errorf("pgDumpInvocation(%q) refused but returned args %q env %q", tc.dsn, args, env)
		}
		if err != nil && strings.Contains(err.Error(), "hunter2") {
			t.Errorf("pgDumpInvocation(%q): the error carries the password: %v", tc.dsn, err)
		}
	}
}

// TestPgDumpInvocation_NoPassword sets no PGPASSWORD when there is none to
// hand over, leaving pg_dump to its own defaults.
func TestPgDumpInvocation_NoPassword(t *testing.T) {
	isolatePGEnv(t)
	args, env, err := pgDumpInvocation("host=db user=sqi dbname=sqi", "/b/sqi.dump")
	if err != nil {
		t.Fatal(err)
	}
	if len(env) != 0 {
		t.Errorf("env = %q, want nothing added", env)
	}
	want := []string{"--format=custom", "--no-owner", "--no-password", "--file=/b/sqi.dump", "--dbname=host=db user=sqi dbname=sqi"}
	if !slices.Equal(args, want) {
		t.Errorf("args = %q, want %q", args, want)
	}
}

// TestPgDumpInvocation_RefusesEnvSSLPassword refuses a client key passphrase
// pgx took from PGSSLPASSWORD, which libpq does not read.
func TestPgDumpInvocation_RefusesEnvSSLPassword(t *testing.T) {
	isolatePGEnv(t)
	t.Setenv("PGSSLPASSWORD", "hunter2")
	if _, _, err := pgDumpInvocation("host=db user=sqi dbname=sqi", "/b/sqi.dump"); err == nil ||
		!strings.Contains(err.Error(), "PGSSLPASSWORD") {
		t.Fatalf("pgDumpInvocation with PGSSLPASSWORD set = %v", err)
	}
}

// fakePgDumpEnv makes the test binary act as pg_dump (see TestFakePgDump):
// "ok" writes an archive and a warning, "fail" writes part of one and fails.
const fakePgDumpEnv = "SQI_TEST_FAKE_PG_DUMP"

// TestFakePgDump is not a test: run as a child with fakePgDumpEnv set, it
// stands in for pg_dump, writing to the path its --file argument names.
func TestFakePgDump(t *testing.T) {
	mode := os.Getenv(fakePgDumpEnv)
	if mode == "" {
		t.Skip("helper process for the runPgDump tests")
	}
	var file string
	for _, a := range os.Args {
		if f, ok := strings.CutPrefix(a, "--file="); ok {
			file = f
		}
	}
	if err := os.WriteFile(file, []byte("PGDMP"), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(3)
	}
	fmt.Fprintln(os.Stderr, "pg_dump: warning: fake "+mode)
	if mode == "fail" {
		os.Exit(1)
	}
	os.Exit(0)
}

// fakePgDump returns the command and leading arguments that run the test
// binary as pg_dump in the given mode.
func fakePgDump(t *testing.T, mode string) (string, []string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(fakePgDumpEnv, mode)
	return exe, []string{"-test.run=^TestFakePgDump$", "--"}
}

func TestRunPgDump_LogsStderrOnSuccess(t *testing.T) {
	exe, lead := fakePgDump(t, "ok")
	out := filepath.Join(t.TempDir(), "sqi.dump")
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	if err := runPgDump(t.Context(), log, exe, append(lead, "--file="+out), nil, out); err != nil {
		t.Fatalf("runPgDump: %v", err)
	}
	if b, err := os.ReadFile(out); err != nil || string(b) != "PGDMP" {
		t.Fatalf("archive = %q, %v", b, err)
	}
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "fake ok") {
		t.Fatalf("pg_dump's stderr was not logged at warn: %q", logs.String())
	}
	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(out); err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("archive mode = %v, %v; want 0600", fi.Mode().Perm(), err)
		}
	}
}

func TestRunPgDump_RemovesItsPartialArchive(t *testing.T) {
	exe, lead := fakePgDump(t, "fail")
	out := filepath.Join(t.TempDir(), "sqi.dump")
	err := runPgDump(t.Context(), slog.New(slog.DiscardHandler), exe, append(lead, "--file="+out), nil, out)
	if err == nil || !strings.Contains(err.Error(), "fake fail") {
		t.Fatalf("runPgDump = %v, want pg_dump's failure with its stderr", err)
	}
	if _, err := os.Stat(out); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("partial archive left behind: %v", err)
	}
}

// TestRunPgDump_LeavesAFileItDidNotCreate covers a destination that appeared
// after preflight: runPgDump refuses it without running pg_dump, and neither
// overwrites nor removes it.
func TestRunPgDump_LeavesAFileItDidNotCreate(t *testing.T) {
	exe, lead := fakePgDump(t, "ok")
	out := filepath.Join(t.TempDir(), "sqi.dump")
	if err := os.WriteFile(out, []byte("theirs"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := runPgDump(t.Context(), slog.New(slog.DiscardHandler), exe, append(lead, "--file="+out), nil, out)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("runPgDump over an existing file = %v", err)
	}
	if b, err := os.ReadFile(out); err != nil || string(b) != "theirs" {
		t.Fatalf("existing file = %q, %v; want it untouched", b, err)
	}
}
