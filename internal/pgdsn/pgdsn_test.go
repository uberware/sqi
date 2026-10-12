// SPDX-License-Identifier: AGPL-3.0-or-later

package pgdsn_test

import (
	"go/parser"
	"go/token"
	"maps"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/uberware/sqi/internal/pgdsn"
)

func TestRedact(t *testing.T) {
	tests := []struct{ in, secret string }{
		{"postgres://sqi:hunter2@db:5432/sqi?sslmode=require", "hunter2"},
		{"postgresql://sqi:p%40ss@db/sqi", "p%40ss"},
		{"postgres://sqi@db/sqi?password=hunter2", "hunter2"},
		{"host=db user=sqi password=hunter2 dbname=sqi", "hunter2"},
		{"host=db password='p@ss word' dbname=sqi", "p@ss word"},
		{`host=db password='it\'s' dbname=sqi`, `it\'s`},
		// Beyond the plain forms: a keyword the operator capitalised or spaced out,
		// an unquoted value with a backslash-escaped space, an unterminated quote,
		// a multi-host URL, and URLs that url.Parse refuses.
		{"host=db PASSWORD = hunter2 dbname=sqi", "hunter2"},
		{`host=db password=hun\ ter2 dbname=sqi`, `hun\ ter2`},
		{"host=db password='hunter2 dbname=sqi", "hunter2"},
		{"postgres://sqi:hunter2@h1:5432,h2:5432/sqi", "hunter2"},
		{"postgres://sqi:hun ter2@db/sqi", "hunter2"},
		{"postgres://sqi:hunter2%zz@db/sqi", "hunter2"},
		{"POSTGRES://sqi:hunter2@db/sqi", "hunter2"},
		// A password that starts with digits and then runs into "#", "?" or "/"
		// reads as host:port with a fragment, query or path after it, so no
		// userinfo is parsed out; the "@" is what gives it away.
		{"postgres://sqi:123#ss@db/sqi", "123#ss"},
		{"postgres://sqi:123?ss@db/sqi", "123?ss"},
		{"postgres://sqi:123/ss@db/sqi", "123/ss"},
		// An unescaped "@" in the password before one of those characters parses
		// with userinfo, but only up to the first "@", leaking the rest.
		{"postgres://sqi:p@ss#word@db/sqi", "word"},
		{"postgres://sqi:p@ss/word@db/sqi", "word"},
		{"postgres://sqi:p@ss?word@db/sqi", "word"},
		{"postgres://sqi:1@2#3@db/sqi", "2#3"},
		// pgx reads a URL the way libpq does, not as RFC 3986. A "#" does not
		// start a fragment there, so it can sit inside a query password or hide
		// a query altogether.
		{"postgres://sqi@db/sqi?password=XQ#YR", "YR"},
		{"postgres://sqi@db/sqi#x?password=hunter2", "hunter2"},
		{"postgres://sqi@db/sqi?sslpassword=XQ#YR", "YR"},
		// pgx percent-decodes query keys, so an encoded name is still a password.
		{"postgres://sqi@db/sqi?pass%77ord=hunter2", "hunter2"},
		{"postgres://sqi@db/sqi?%70assword=hunter2", "hunter2"},
		{"postgres://sqi@db/sqi?ssl%70assword=hunter2", "hunter2"},
		{"postgres://sqi@db/sqi?sslmode=disable&pass%77ord=hunter2", "hunter2"},
		{"postgres://sqi@db/sqi?application_name=x;pass%77ord=hunter2", "hunter2"},
		// pgx trims spaces around a key, so a padded name is still a password.
		{"postgres://sqi@db/sqi?password =hunter2", "hunter2"},
		// An upper-case key is not a password to pgx, but the regex still
		// redacts it, which is the safe side.
		{"postgres://sqi@db/sqi?PASSWORD=hunter2", "hunter2"},
		// The client key's passphrase is a secret too, as is a query password
		// url.Values would drop for its semicolon.
		{"host=db sslpassword=hunter2 dbname=sqi", "hunter2"},
		{"postgres://sqi@db/sqi?sslpassword=hunter2&sslmode=require", "hunter2"},
		{"postgres://sqi@db/sqi?sslmode=require&password=hun;ter2", "hun;ter2"},
		// pgx reads keyword/value text with its own rules, which no regex
		// mirrored: a backslash escapes any byte (a line feed included), "\v"
		// is whitespace, whitespace may follow "=" and precede a quote, and
		// "password=" inside another key's value is not a key at all.
		{"host=db password='ab\\\ncd'", "cd"},
		{"host=db password=hun\\\nter2", "ter2"},
		{"host=db password\v=hunter2", "hunter2"},
		{"host=db password=\v'abc def'", "def"},
		{"host=db password =\v hunter2", "hunter2"},
		{"host=db application_name=password= password =hunter2", "hunter2"},
		// A query pair with no "=" is refused (pgx rejects the DSN anyway).
		{"postgres://sqi@db/sqi?password=ab&cd", "&cd"},
	}
	for _, tc := range tests {
		got := pgdsn.Redact(tc.in)
		if strings.Contains(got, tc.secret) || !strings.Contains(got, "redacted") {
			t.Errorf("Redact(%q) = %q", tc.in, got)
		}
	}
	if got := pgdsn.Redact("postgres://sqi@db/sqi"); got != "postgres://sqi@db/sqi" {
		t.Errorf("a DSN with no password changed: %q", got)
	}
}

func TestRedact_KeepsEverythingElse(t *testing.T) {
	tests := []struct{ in, want string }{
		{"postgres://sqi:hunter2@db:5432/sqi?sslmode=require", "postgres://sqi:redacted@db:5432/sqi?sslmode=require"},
		{"host=db user=sqi password=hunter2 dbname=sqi", "host=db user=sqi password=<redacted> dbname=sqi"},
		{"host=db password='p@ss word' dbname=sqi", "host=db password=<redacted> dbname=sqi"},
		// No password: returned byte for byte.
		{"host=db user=sqi dbname=sqi", "host=db user=sqi dbname=sqi"},
		{"postgres:///sqi?host=/var/run/postgresql", "postgres:///sqi?host=/var/run/postgresql"},
		{"postgres://sqi:@db/sqi", "postgres://sqi:@db/sqi"},
		{"", ""},
		// Failing closed reduces the DSN to its scheme and a placeholder. This
		// includes a DSN whose "@" sits outside any userinfo, which is
		// indistinguishable from a truncated authority.
		{"postgres://sqi:123#ss@db/sqi", "postgres://<redacted>"},
		{"postgres://sqi:p@ss#word@db/sqi", "postgres://<redacted>"},
		{"postgres://sqi@db/sqi?pass%77ord=hunter2", "postgres://<redacted>"},
		{"postgres://sqi@db/sqi?password=XQ#YR", "postgres://<redacted>"},
		// A literal key is redacted in place, not failed closed.
		{"postgres://sqi@db/sqi?sslmode=disable&password=hunter2", "postgres://sqi@db/sqi?sslmode=disable&password=redacted"},
		{"postgres://sqi@db/sqi?sslmode=require&application_name=x", "postgres://sqi@db/sqi?sslmode=require&application_name=x"},
		{"postgres://sqi:hunter2%zz@db/sqi", "postgres://<redacted>"},
		{"postgres://db/sqi?application_name=a@b", "postgres://<redacted>"},
		{"postgres://sqi@db/sqi?password=ab&cd", "postgres://<redacted>"},
		// A keyword/value DSN with a password is rebuilt from its pairs, quoting
		// only the values that need it; one pgx cannot parse fails closed to the
		// bare placeholder, as does one with a key no real setting has (which
		// is how pgx reads a URL whose scheme is not exactly lower case).
		{"host=db application_name=password= password =hunter2", "host=db application_name=password= password=<redacted>"},
		{"host=db password\v=hunter2", "host=db password=<redacted>"},
		{"  host=db \t application_name='a b'\vPASSWORD = x  ", "host=db application_name='a b' PASSWORD=<redacted>"},
		{`host='' password=x options='-c a=\\\'b'`, `host='' password=<redacted> options='-c a=\\\'b'`},
		{`host=db password=x application_name=a\ b`, `host=db password=<redacted> application_name='a b'`},
		{"host=db password='hunter2 dbname=sqi", "<redacted>"},
		{`host=db password='hunter2\'`, "<redacted>"},
		{"host=db password=hunter2 \x00", "<redacted>"},
		{"host=db password=hunter2 dbname", "<redacted>"},
		{"host=db =hunter2", "<redacted>"},
		{"host=db pass word=hunter2", "<redacted>"},
		{"POSTGRES://sqi:hunter2@db/sqi", "<redacted>"},
		{" postgres://sqi:hunter2@db/sqi?sslmode=require", "<redacted>"},
		// No password: returned byte for byte, whatever its spacing.
		{"host=db\v dbname=sqi  ", "host=db\v dbname=sqi  "},
	}
	for _, tc := range tests {
		if got := pgdsn.Redact(tc.in); got != tc.want {
			t.Errorf("Redact(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestRedact_PgxReadsTheSameSettings checks the keyword/value rebuild against
// pgx itself: every DSN here is one pgx accepts, and pgx must read the redacted
// DSN with the same settings and no password but the placeholder.
func TestRedact_PgxReadsTheSameSettings(t *testing.T) {
	// pgx falls back to the environment and the password file for anything the
	// DSN leaves out, which must not leak into the comparison.
	for _, env := range []string{
		"PGHOST", "PGPORT", "PGDATABASE", "PGUSER", "PGPASSWORD", "PGAPPNAME",
		"PGSSLMODE", "PGSSLPASSWORD", "PGSERVICE", "PGSERVICEFILE", "PGOPTIONS",
	} {
		t.Setenv(env, "")
	}
	t.Setenv("PGPASSFILE", filepath.Join(t.TempDir(), "no-pgpass"))

	dsns := []string{
		// The inputs the regex used to leak.
		"host=db password='ab\\\ncd'",
		"host=db password=hun\\\nter2",
		"host=db password\v=hunter2",
		"host=db password=\v'abc def'",
		"host=db password =\v hunter2",
		"host=db application_name=password= password =hunter2",
		// Ordinary and awkward ones.
		"host=db user=sqi password=hunter2 dbname=sqi sslmode=disable",
		"host=db port=5433 dbname=sqi user=sqi password='p@ss word' sslmode=require",
		`host=db application_name='it\'s a \\ test' password=x database=sqi`,
		`host=db application_name=a\ b password='' dbname=sqi`,
		"host=db sslpassword=secret sslmode=disable user=sqi",
		"host=db password=a password=b dbname=sqi",
		"host=db options='-c search_path=x' password=hunter2",
		"host=db user= password=hunter2 dbname=sqi",
		`host=db password=x dbname=trail\`,
		`host=db dbname=trail\ password=x`,
		"host=db\vpassword=hunter2\fdbname=sqi\r",
		`  host=db   password='a\'b'sslmode=disable  `,
		"host=db PASSWORD=hunter2 dbname=sqi",
		"host=db dbname=sqi",
	}
	for _, dsn := range dsns {
		want, err := pgconn.ParseConfig(dsn)
		if err != nil {
			t.Fatalf("pgx rejects test input %q: %v", dsn, err)
		}
		redacted := pgdsn.Redact(dsn)
		got, err := pgconn.ParseConfig(redacted)
		if err != nil {
			t.Errorf("Redact(%q) = %q, which pgx rejects: %v", dsn, redacted, err)
			continue
		}
		if got.Password != "" && got.Password != "<redacted>" {
			t.Errorf("Redact(%q) = %q: pgx reads password %q", dsn, redacted, got.Password)
		}
		if got.Host != want.Host || got.Port != want.Port || got.Database != want.Database || got.User != want.User {
			t.Errorf("Redact(%q) = %q: pgx reads %s:%d/%s user %q, want %s:%d/%s user %q", dsn, redacted,
				got.Host, got.Port, got.Database, got.User, want.Host, want.Port, want.Database, want.User)
		}
		if (got.TLSConfig == nil) != (want.TLSConfig == nil) || len(got.Fallbacks) != len(want.Fallbacks) {
			t.Errorf("Redact(%q) = %q: pgx reads a different sslmode", dsn, redacted)
		}
		wantParams := maps.Clone(want.RuntimeParams)
		gotParams := maps.Clone(got.RuntimeParams)
		// An upper-case PASSWORD is a runtime parameter to pgx, redacted on purpose.
		if _, ok := wantParams["PASSWORD"]; ok {
			wantParams["PASSWORD"] = "<redacted>"
		}
		if !maps.Equal(gotParams, wantParams) {
			t.Errorf("Redact(%q) = %q: pgx reads runtime params %v, want %v", dsn, redacted, gotParams, wantParams)
		}
	}
}

// TestStdlibOnly keeps the package a leaf any binary can import: its non-test
// files import only the standard library (no pgx, nothing internal).
func TestStdlibOnly(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			if first, _, _ := strings.Cut(path, "/"); strings.Contains(first, ".") {
				t.Errorf("%s imports %s; pgdsn must import only the standard library", name, path)
			}
		}
	}
}
