// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
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
