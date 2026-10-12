// SPDX-License-Identifier: AGPL-3.0-or-later

package pgdsn_test

import (
	"maps"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/uberware/sqi/internal/pgdsn"
)

func TestWithoutPassword(t *testing.T) {
	tests := []struct{ in, want string }{
		// The two forms, with the password where each can carry it.
		{"postgres://sqi:hunter2@db:5432/sqi?sslmode=require", "postgres://sqi@db:5432/sqi?sslmode=require"},
		{"postgres://sqi@db/sqi?password=hunter2&sslmode=disable", "postgres://sqi@db/sqi?sslmode=disable"},
		{"host=db user=sqi password=hunter2 dbname=sqi", "host=db user=sqi dbname=sqi"},
		{"host=db password='p@ss word' dbname=sqi sslmode=verify-full", "host=db dbname=sqi sslmode=verify-full"},
		// URL form: the text is kept byte for byte but for the removed spans.
		{"postgresql://sqi:p%40ss@db/sqi", "postgresql://sqi@db/sqi"},
		{"postgres://:hunter2@db/sqi", "postgres://db/sqi"},
		{"postgres://sqi:@db/sqi", "postgres://sqi@db/sqi"},
		{"postgres://sqi:hunter2@h1:5432,h2:5432/sqi", "postgres://sqi@h1:5432,h2:5432/sqi"},
		{"postgres://sqi@db/sqi?password=hunter2", "postgres://sqi@db/sqi"},
		{"postgres://sqi@db?password=hunter2&application_name=a%20b", "postgres://sqi@db?application_name=a%20b"},
		{"postgres://sqi@db/sqi?sslmode=disable&password=x&application_name=a", "postgres://sqi@db/sqi?sslmode=disable&application_name=a"},
		{"postgres://sqi@db/sqi?sslpassword=x&sslmode=require&password=y", "postgres://sqi@db/sqi?sslmode=require"},
		{"postgres://sqi@db/sqi?PASSWORD=hunter2", "postgres://sqi@db/sqi"},
		{"postgresql:///sqi?host=/var/run/postgresql", "postgresql:///sqi?host=/var/run/postgresql"},
		{"postgres://sqi@db/sqi", "postgres://sqi@db/sqi"},
		// Keyword/value form: rebuilt from its pairs when a password goes,
		// returned byte for byte when there is none.
		{"host=db password=a password=b dbname=sqi", "host=db dbname=sqi"},
		{`host=db password=x application_name=a\ b`, `host=db application_name='a b'`},
		{"host=db application_name=password= password =hunter2", "host=db application_name=password="},
		{"host=db PASSWORD=hunter2 sslpassword=s dbname=sqi", "host=db dbname=sqi"},
		{"password=hunter2", ""},
		{"host=db\v dbname=sqi  ", "host=db\v dbname=sqi  "},
		{"", ""},
	}
	for _, tc := range tests {
		got, ok := pgdsn.WithoutPassword(tc.in)
		if !ok || got != tc.want {
			t.Errorf("WithoutPassword(%q) = %q, %v; want %q, true", tc.in, got, ok, tc.want)
		}
	}
}

// TestWithoutPassword_RefusesWhereRedactFailsClosed pins the two functions to
// one reading of a DSN: wherever Redact cannot locate the password reliably,
// WithoutPassword refuses rather than guess.
func TestWithoutPassword_RefusesWhereRedactFailsClosed(t *testing.T) {
	refused := []string{
		"postgres://sqi:hun ter2@db/sqi",
		"postgres://sqi:hunter2%zz@db/sqi",
		"postgres://sqi:123#ss@db/sqi",
		"postgres://sqi:123?ss@db/sqi",
		"postgres://sqi:123/ss@db/sqi",
		"postgres://sqi:p@ss#word@db/sqi",
		"postgres://sqi:p@ss/word@db/sqi",
		"postgres://sqi:p@ss?word@db/sqi",
		"postgres://sqi:p@ss@db/sqi",
		"postgres://sqi@db/sqi?password=XQ#YR",
		"postgres://sqi@db/sqi#x?password=hunter2",
		"postgres://sqi@db/sqi?pass%77ord=hunter2",
		"postgres://sqi@db/sqi?ssl%70assword=hunter2",
		"postgres://sqi@db/sqi?password =hunter2",
		"postgres://sqi@db/sqi?password=ab&cd",
		"postgres://db/sqi?application_name=a@b",
		// A ";" splits a query pair to net/url of old, so this reads as a pair with
		// no "=", which Redact refuses too.
		"postgres://sqi@db/sqi?sslmode=require&password=hun;ter2",
		"host=db password='hunter2 dbname=sqi",
		`host=db password='hunter2\'`,
		"host=db password=hunter2 \x00",
		"host=db password=hunter2 dbname",
		"host=db =hunter2",
		"host=db pass word=hunter2",
		"POSTGRES://sqi:hunter2@db/sqi",
		" postgres://sqi:hunter2@db/sqi?sslmode=require",
	}
	for _, dsn := range refused {
		if got, ok := pgdsn.WithoutPassword(dsn); ok || got != "" {
			t.Errorf("WithoutPassword(%q) = %q, %v; want a refusal", dsn, got, ok)
		}
	}
	// And the rule itself, over every input either function has a test for.
	all := append([]string{}, refused...)
	for _, tc := range secretDSNs {
		all = append(all, tc.in)
	}
	all = append(all, keywordValueDSNs...)
	for _, dsn := range all {
		redacted := pgdsn.Redact(dsn)
		failedClosed := redacted == "<redacted>" || strings.HasSuffix(redacted, "://<redacted>")
		if _, ok := pgdsn.WithoutPassword(dsn); failedClosed && ok {
			t.Errorf("Redact(%q) fails closed but WithoutPassword accepts it", dsn)
		}
	}
}

// TestWithoutPassword_NoSecretSurvives runs every DSN known to have leaked a
// secret through a parser other than pgx's: each is refused or loses it.
func TestWithoutPassword_NoSecretSurvives(t *testing.T) {
	for _, tc := range secretDSNs {
		got, ok := pgdsn.WithoutPassword(tc.in)
		if ok && strings.Contains(got, tc.secret) {
			t.Errorf("WithoutPassword(%q) = %q, which still carries %q", tc.in, got, tc.secret)
		}
		if !ok && got != "" {
			t.Errorf("WithoutPassword(%q) refused but returned %q", tc.in, got)
		}
	}
}

// TestWithoutPassword_PgxReadsTheSameSettings checks the result against pgx
// itself: pgx must read it with every setting the input had except the
// password and sslpassword, and with no password at all.
func TestWithoutPassword_PgxReadsTheSameSettings(t *testing.T) {
	isolatePGEnv(t)
	dsns := append([]string{
		"postgres://sqi:hunter2@db:5432/sqi?sslmode=require",
		"postgresql://sqi:p%40ss@db/sqi?sslmode=disable",
		"postgres://:hunter2@db/sqi",
		"postgres://sqi:hunter2@h1:5432,h2:5433/sqi?sslmode=disable",
		"postgres://sqi@db/sqi?password=hunter2&sslmode=disable&application_name=a%20b",
		"postgres://sqi@db?sslpassword=x&sslmode=disable",
		"postgresql:///sqi?host=/var/run/postgresql&password=x",
		"postgres://sqi@db/sqi?options=-c%20search_path%3Dx&password=x",
	}, keywordValueDSNs...)
	for _, dsn := range dsns {
		want, err := pgconn.ParseConfig(dsn)
		if err != nil {
			t.Fatalf("pgx rejects test input %q: %v", dsn, err)
		}
		rest, ok := pgdsn.WithoutPassword(dsn)
		if !ok {
			t.Errorf("WithoutPassword(%q) refused a DSN pgx accepts", dsn)
			continue
		}
		got, err := pgconn.ParseConfig(rest)
		if err != nil {
			t.Errorf("WithoutPassword(%q) = %q, which pgx rejects: %v", dsn, rest, err)
			continue
		}
		if got.Password != "" {
			t.Errorf("WithoutPassword(%q) = %q: pgx reads password %q", dsn, rest, got.Password)
		}
		if got.Host != want.Host || got.Port != want.Port || got.Database != want.Database || got.User != want.User {
			t.Errorf("WithoutPassword(%q) = %q: pgx reads %s:%d/%s user %q, want %s:%d/%s user %q", dsn, rest,
				got.Host, got.Port, got.Database, got.User, want.Host, want.Port, want.Database, want.User)
		}
		if (got.TLSConfig == nil) != (want.TLSConfig == nil) || len(got.Fallbacks) != len(want.Fallbacks) {
			t.Errorf("WithoutPassword(%q) = %q: pgx reads a different sslmode or host list", dsn, rest)
		}
		wantParams := maps.Clone(want.RuntimeParams)
		// An upper-case PASSWORD is a runtime parameter to pgx (and an error to
		// libpq); it goes too, on the safe side.
		delete(wantParams, "PASSWORD")
		if !maps.Equal(got.RuntimeParams, wantParams) {
			t.Errorf("WithoutPassword(%q) = %q: pgx reads runtime params %v, want %v", dsn, rest, got.RuntimeParams, wantParams)
		}
	}
}

func TestSetsSSLPassword(t *testing.T) {
	tests := []struct {
		dsn  string
		want bool
	}{
		{"host=db sslpassword=secret dbname=sqi", true},
		{"host=db SSLPASSWORD=secret dbname=sqi", true},
		{"host=db sslpassword='' dbname=sqi", false},
		{"host=db password=x dbname=sqi", false},
		{"host=db application_name=sslpassword=x", false},
		{"postgres://sqi@db/sqi?sslpassword=secret", true},
		{"postgres://sqi@db/sqi?sslmode=require&sslpassword=secret", true},
		{"postgres://sqi@db/sqi?sslpassword=", false},
		{"postgres://sqi:pw@db/sqi?sslmode=require", false},
		{"postgres://sqi@db/sqi", false},
		// A DSN that cannot be read reliably might carry one: fail closed.
		{"postgres://sqi@db/sqi?ssl%70assword=secret", true},
		{"host=db sslpassword='secret", true},
	}
	for _, tc := range tests {
		if got := pgdsn.SetsSSLPassword(tc.dsn); got != tc.want {
			t.Errorf("SetsSSLPassword(%q) = %v, want %v", tc.dsn, got, tc.want)
		}
	}
}
