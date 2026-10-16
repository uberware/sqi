// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestParseGooseCopy(t *testing.T) {
	cases := []struct {
		name, in string
		want     int64
	}{
		{"applied rows", "SET x;\nCOPY public.goose_db_version (id, version_id, is_applied, tstamp) FROM stdin;\n" +
			"1\t0\tt\t2026-10-01 00:00:00\n2\t37\tt\t2026-10-01 00:00:01\n3\t38\tt\t2026-10-02 00:00:00\n\\.\n", 38},
		{"unapplied rows ignored", "COPY public.goose_db_version (id, version_id, is_applied, tstamp) FROM stdin;\n" +
			"1\t37\tt\tx\n2\t38\tf\tx\n\\.\n", 37},
		{"column order read from the header", "COPY public.goose_db_version (is_applied, version_id, id, tstamp) FROM stdin;\n" +
			"t\t37\t1\tx\n\\.\n", 37},
		{"no goose table", "SET x;\n", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseGooseCopy(tc.in)
			if err != nil || got != tc.want {
				t.Fatalf("parseGooseCopy = %d, %v; want %d", got, err, tc.want)
			}
		})
	}
}

func TestParseGooseCopy_Refusals(t *testing.T) {
	for name, in := range map[string]string{
		"header without the columns": "COPY public.goose_db_version (id, tstamp) FROM stdin;\n1\tx\n\\.\n",
		"header without parentheses": "COPY public.goose_db_version FROM stdin;\n",
		"version not a number":       "COPY public.goose_db_version (version_id, is_applied) FROM stdin;\nabc\tt\n\\.\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseGooseCopy(in); err == nil {
				t.Fatal("parseGooseCopy accepted it")
			}
		})
	}
}

func TestPgConnArgs_KeepsThePasswordOffTheCommandLine(t *testing.T) {
	isolatePGEnv(t)
	arg, env, err := pgConnArgs("postgres://sqi:s3cret@db.example.com:5432/sqi?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(arg, "s3cret") {
		t.Fatalf("dbname arg %q carries the password", arg)
	}
	if len(env) != 1 || env[0] != "PGPASSWORD=s3cret" {
		t.Fatalf("env = %v", env)
	}
}

func TestCheckToolVersion_NamesTheTool(t *testing.T) {
	err := checkToolVersion("pg_restore", 16, 180002)
	if err == nil || !strings.Contains(err.Error(), "pg_restore 16 is older than the server (18)") {
		t.Fatalf("pg_restore 16 against server 18 = %v", err)
	}
}

func TestFindTool_NamesTheTool(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if _, err := findTool("pg_restore"); err == nil || !strings.Contains(err.Error(), "pg_restore not found on PATH") {
		t.Fatalf("findTool(pg_restore) without it = %v", err)
	}
}

// A failed load's stderr reaches the error and the log, so the server's
// continuation lines, which quote row values, are dropped — whatever language
// the server writes them in — and pg_restore's own lines are kept.
func TestWithoutRowData(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"empty", "", ""},
		{
			"own lines only", "pg_restore: warning: errors ignored on restore: 1\n",
			"pg_restore: warning: errors ignored on restore: 1",
		},
		{
			"a COPY failure",
			"pg_restore: error: COPY failed for table \"users\": ERROR:  duplicate key value violates unique constraint \"users_pkey\"\n" +
				"DETAIL:  Key (id)=(u1) already exists.\n" +
				"CONTEXT:  COPY users, line 1: \"u1\tadmin\t$argon2id$v=19$m=65536,t=3,p=4$c2FsdA$aGFzaA\"\n" +
				"pg_restore: detail: Command was: COPY public.users (id, username, password_hash) FROM stdin;\n",
			"pg_restore: error: COPY failed for table \"users\": ERROR:  duplicate key value violates unique constraint \"users_pkey\"\n" +
				"pg_restore: detail: Command was: COPY public.users (id, username, password_hash) FROM stdin;\n" +
				"(2 line(s) of server detail omitted: they can quote row data)",
		},
		{
			"a translated server message, CRLF",
			"pg_restore: error: COPY failed for table \"users\": FEHLER:  doppelter Schlüsselwert\r\n" +
				"DETAIL:  Schlüssel »(id)=(u1)« existiert bereits.\r\n",
			"pg_restore: error: COPY failed for table \"users\": FEHLER:  doppelter Schlüsselwert\n" +
				"(1 line(s) of server detail omitted: they can quote row data)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := withoutRowData(tc.in); got != tc.want {
				t.Errorf("withoutRowData =\n%s\nwant\n%s", got, tc.want)
			}
		})
	}
}

// The "unsupported version" match must hold whatever the operator's locale:
// a pg_restore that would answer in German still reports a newer archive as a
// tool problem, not as an unreadable archive. The fake pg_restore answers in
// English only when LC_ALL=C, as a real one with a German catalog would.
func TestArchiveSchemaVersion_MatchesTheToolErrorInAnyLocale(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake pg_restore is a POSIX shell script")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"if [ \"$LC_ALL\" = C ]; then\n" +
		"  echo 'pg_restore: error: unsupported version (1.99) in file header' >&2\n" +
		"else\n" +
		"  echo 'pg_restore: Fehler: nicht unterstützte Version (1.99) im Dateikopf' >&2\n" +
		"fi\n" +
		"exit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "pg_restore"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("LC_ALL", "de_DE.UTF-8")
	t.Setenv("LANGUAGE", "de")

	_, err := ArchiveSchemaVersion(t.Context(), filepath.Join(dir, "sqi.dump"))
	if err == nil || errors.Is(err, ErrUnreadableArchive) || !strings.Contains(err.Error(), "newer pg_dump") {
		t.Fatalf("ArchiveSchemaVersion under a German locale = %v, want a tool error naming a newer pg_dump", err)
	}
}
