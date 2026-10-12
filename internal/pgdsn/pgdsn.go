// SPDX-License-Identifier: AGPL-3.0-or-later

// Package pgdsn reads PostgreSQL connection strings (DSNs) the way the driver
// does, to keep their secrets out of places they must not reach: [Redact]
// for `sqi-server config print`, and [WithoutPassword] for a pg_dump command
// line, which any local user can read from the process list.
//
// It imports only the standard library, so any package can use it. The
// keyword/value tokenizer mirrors pgx's (and libpq's), because a regular
// expression cannot: see [parseKeywordValueDSN]. A URL-form DSN is checked for
// every way Go's [url.Parse] and the driver's libpq-style parser can disagree
// (see [urlDSNAmbiguous]), and refused where they can.
package pgdsn

import (
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// redactedPassword is the placeholder [Redact] writes in place of a
// keyword/value password, and after the scheme of a URL it fails closed on.
const redactedPassword = "<redacted>"

// redactedURLPassword is the placeholder [Redact] writes into a URL-form DSN
// in place of its password. It is the bare word because [url.URL] would
// percent-encode the angle brackets of [redactedPassword].
const redactedURLPassword = "redacted"

// queryPassword matches a URL-form DSN's password parameter (and sslpassword)
// in the raw query string. It works on the raw text because [url.Values] drops
// a pair that does not parse, which would leave that password in place.
var queryPassword = regexp.MustCompile(`(?i)((?:^|&)(?:ssl)?password=)[^&]+`)

// Redact returns dsn with its password (and sslpassword, the client key's
// passphrase) replaced by a placeholder, in either DSN form. A DSN without a
// password is returned unchanged.
//
// The form is chosen as pgx chooses it: a DSN is a URL only when it starts with
// exactly "postgres://" or "postgresql://", and anything else is keyword/value.
//
// It fails closed. A URL-form DSN whose password cannot be reliably located,
// because [url.Parse] refuses it or because [urlDSNAmbiguous] says that Go's
// reading of it and the PostgreSQL driver's can differ, is reduced to its scheme
// and a placeholder. A keyword/value DSN is tokenized as pgx tokenizes it (see
// [parseKeywordValueDSN]); one it cannot tokenize is reduced to the placeholder
// alone. A keyword/value DSN that does carry a password is rebuilt from its
// pairs, so its spacing and quoting may change, but pgx reads every other
// setting back exactly as before.
func Redact(dsn string) string {
	scheme, ok := dsnURLScheme(dsn)
	if !ok {
		return redactKeywordValueDSN(dsn)
	}
	u, err := url.Parse(dsn)
	if err != nil || urlDSNAmbiguous(u, dsn) {
		return scheme + "://" + redactedPassword
	}
	changed := false
	if pw, has := u.User.Password(); has && pw != "" {
		u.User = url.UserPassword(u.User.Username(), redactedURLPassword)
		changed = true
	}
	if queryPassword.MatchString(u.RawQuery) {
		u.RawQuery = queryPassword.ReplaceAllString(u.RawQuery, "${1}"+redactedURLPassword)
		changed = true
	}
	if !changed {
		return dsn
	}
	return u.String()
}

// kvPair is one keyword/value setting, as pgx reads it: the key trimmed of
// whitespace, the value with its quotes and backslash escapes removed.
type kvPair struct{ key, value string }

// kvSpace is pgx's whitespace set for keyword/value DSNs (pgconn's asciiSpace),
// which, unlike RE2's \s, includes the vertical tab.
const kvSpace = " \t\n\v\f\r"

// redactKeywordValueDSN is [Redact] for a keyword/value DSN. A DSN that
// [parseKeywordValueDSN] refuses becomes the bare placeholder; one with no
// password key is returned as it came; otherwise it is rebuilt pair by pair
// with each password value replaced.
func redactKeywordValueDSN(dsn string) string {
	pairs, ok := parseKeywordValueDSN(dsn)
	if !ok {
		return redactedPassword
	}
	if !slices.ContainsFunc(pairs, func(p kvPair) bool { return isPasswordKey(p.key) }) {
		return dsn
	}
	var b strings.Builder
	for i, p := range pairs {
		if i > 0 {
			b.WriteByte(' ')
		}
		value := p.value
		if isPasswordKey(p.key) {
			value = redactedPassword
		}
		b.WriteString(p.key)
		b.WriteByte('=')
		b.WriteString(quoteKeywordValue(value))
	}
	return b.String()
}

// isPasswordKey reports whether a keyword/value key holds a secret. pgx matches
// "password" and "sslpassword" exactly; this matches them in any case, a
// superset on the safe side (an upper-case PASSWORD is still the operator's
// password, sent to the server as a runtime parameter).
func isPasswordKey(key string) bool {
	return strings.EqualFold(key, "password") || strings.EqualFold(key, "sslpassword")
}

// parseKeywordValueDSN tokenizes a keyword/value DSN by the rules of pgx
// v5.11.0's parseKeywordValueSettings (pgconn/config.go), and reports false
// wherever pgx would return an error:
//
//   - a NUL byte anywhere;
//   - text with no "=" left, an empty key, or whitespace inside a key;
//   - an unterminated quoted value, including one whose last byte is an
//     escaping backslash.
//
// Whitespace is [kvSpace]. A key runs to the next "=" and is trimmed. Leading
// whitespace before a value is skipped; a value starting with "'" is quoted and
// ends at the next unescaped "'", otherwise it ends at whitespace. In both a
// backslash escapes the byte after it, whatever it is, and a trailing backslash
// in an unquoted value escapes the end of the string.
//
// It also refuses, beyond pgx, a key that is not made of letters, digits, "_",
// "." and "-". pgx passes any other key to the server as a runtime parameter,
// but no real one looks like that, and a mistyped URL (" postgres://u:pw@h/d?x=y"
// or "POSTGRES://…", which pgx reads as keyword/value) would otherwise carry its
// password into the output inside a key.
func parseKeywordValueDSN(s string) ([]kvPair, bool) {
	if strings.IndexByte(s, 0) >= 0 {
		return nil, false
	}
	var pairs []kvPair
	s = strings.TrimLeft(s, kvSpace)
	for s != "" {
		eq := strings.IndexByte(s, '=')
		if eq < 0 {
			return nil, false
		}
		key := strings.Trim(s[:eq], kvSpace)
		if !isKeywordValueKey(key) {
			return nil, false
		}
		s = strings.TrimLeft(s[eq+1:], kvSpace)
		value, rest, ok := cutKeywordValue(s)
		if !ok {
			return nil, false
		}
		pairs = append(pairs, kvPair{key: key, value: value})
		s = strings.TrimLeft(rest, kvSpace)
	}
	return pairs, true
}

// isKeywordValueKey reports whether key is non-empty and made only of letters,
// digits, "_", "." and "-" (see [parseKeywordValueDSN]). That also rejects the
// whitespace inside a key that pgx rejects.
func isKeywordValueKey(key string) bool {
	if key == "" {
		return false
	}
	for i := range len(key) {
		c := key[i]
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9',
			c == '_', c == '.', c == '-':
		default:
			return false
		}
	}
	return true
}

// cutKeywordValue reads one value from the start of s (leading whitespace
// already skipped) and returns it unescaped, with the text after it. It reports
// false for an unterminated quoted value.
func cutKeywordValue(s string) (value, rest string, ok bool) {
	if s == "" {
		return "", "", true
	}
	quoted := s[0] == '\''
	if quoted {
		s = s[1:]
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quoted && c == '\'':
			return b.String(), s[i+1:], true
		case !quoted && strings.IndexByte(kvSpace, c) >= 0:
			return b.String(), s[i:], true
		case c == '\\':
			i++
			if i == len(s) {
				// An escaped end: an error inside quotes, dropped outside.
				return b.String(), "", !quoted
			}
			c = s[i]
		}
		b.WriteByte(c)
	}
	return b.String(), "", !quoted
}

// quoteKeywordValue renders a value so that pgx reads it back unchanged: as it
// is when it holds nothing pgx treats specially, otherwise single-quoted with
// every "'" and "\" backslash-escaped.
func quoteKeywordValue(v string) string {
	if v != "" && !strings.ContainsAny(v, kvSpace+`'\`) {
		return v
	}
	var b strings.Builder
	b.WriteByte('\'')
	for i := range len(v) {
		if v[i] == '\'' || v[i] == '\\' {
			b.WriteByte('\\')
		}
		b.WriteByte(v[i])
	}
	b.WriteByte('\'')
	return b.String()
}

// urlDSNAmbiguous reports whether a URL-form DSN that [url.Parse] accepted may
// still be read differently by the driver (pgx follows libpq's URI parser, not
// RFC 3986), which would leave [Redact] redacting the wrong span:
//
//   - A "#" is a fragment to Go, so everything after it is out of the query,
//     but to pgx it is ordinary text: `?password=XQ#YR` has the password XQ#YR,
//     and `/db#x?password=hunter2` still has a query. So any "#" is refused.
//   - The authority is ambiguous (see [ambiguousAuthority]).
//   - A query key is spelled so the regex cannot see it, or a query pair has
//     no "=" (see [obscuredPasswordKey]).
//   - The query holds a ";", or a value holds a second "=" (see
//     [ambiguousQuery]).
func urlDSNAmbiguous(u *url.URL, dsn string) bool {
	return strings.Contains(dsn, "#") || ambiguousAuthority(u, dsn) ||
		obscuredPasswordKey(dsn) || ambiguousQuery(dsn)
}

// ambiguousQuery reports whether the DSN's query holds a ";" anywhere, or
// a pair, split on "&" as pgx and libpq split it, whose value holds a second
// "=". Either lets one reading of the query see a password key that another
// does not: pgx and libpq split on "&" alone, so `?a=x;password=y` is one pair
// (a) and `?;password=y` a runtime parameter named ";password", while a reader
// that also splits on ";" (net/url once did) sees a plain password key; and
// pgx and libpq reject `?a=b=password=y`, which a reader splitting a pair at
// its last "=" reads as a password. Refusing both leaves every reading of the
// query the same: one key, then one value, per "&" pair. A ";" in a value
// must be percent-encoded.
func ambiguousQuery(dsn string) bool {
	_, query, found := strings.Cut(dsn, "?")
	if !found {
		return false
	}
	if strings.Contains(query, ";") {
		return true
	}
	for pair := range strings.SplitSeq(query, "&") {
		if _, value, _ := strings.Cut(pair, "="); strings.Contains(value, "=") {
			return true
		}
	}
	return false
}

// obscuredPasswordKey reports whether the DSN's query has a password or
// sslpassword key that [queryPassword] cannot match, because the key is
// percent-encoded (pgx decodes keys: `pass%77ord=x` is a password) or padded
// with spaces (pgx trims them), or has a pair with no "=" at all, which could
// be the tail of a password the regex would stop short of (`password=ab&cd`;
// pgx rejects such a DSN, but it is refused here regardless). The query is
// split on "&" as pgx does; a query holding any ";" is refused before this
// matters (see [ambiguousQuery]).
func obscuredPasswordKey(dsn string) bool {
	_, query, found := strings.Cut(dsn, "?")
	if !found {
		return false
	}
	pairs := strings.FieldsFunc(query, func(r rune) bool { return r == '&' })
	for _, pair := range pairs {
		rawKey, _, hasValue := strings.Cut(pair, "=")
		if !hasValue {
			return true
		}
		key := rawKey
		if decoded, err := url.PathUnescape(rawKey); err == nil {
			key = decoded
		}
		name := strings.ToLower(strings.Trim(key, " "))
		if (name == "password" || name == "sslpassword") && !strings.EqualFold(rawKey, name) {
			return true
		}
	}
	return false
}

// ambiguousAuthority reports whether a URL-form DSN that parsed cannot be
// trusted to have had its userinfo, and so its password, located. [url.Parse]
// ends the authority at the first "#", "?" or "/", so an unescaped "@" in the
// password makes the parse come out wrong in one of two ways:
//
//   - A password that starts with digits and runs into one of those characters
//     (sqi:123#ss@db) reads as host:port with the rest a fragment, query or
//     path. No userinfo is parsed out of it, yet the DSN holds an "@".
//   - A password with an "@" before one of those characters (sqi:p@ss#word@db)
//     parses with userinfo, but only the part up to the first "@" is taken as
//     the password, and the rest leaks into the result.
//
// A well-formed DSN has at most one "@", the one that ends the userinfo, so
// more than one, or one with no userinfo to end, is ambiguous. That is
// deliberately conservative: it also refuses an unescaped "@" in a password
// that did parse correctly (sqi:p@ss@db), and an "@" in a query value, where
// the only cost is a less informative `config print`.
func ambiguousAuthority(u *url.URL, dsn string) bool {
	ats := strings.Count(dsn, "@")
	return ats > 1 || (ats == 1 && u.User == nil)
}

// dsnURLScheme reports whether dsn is in URL form and returns its scheme. As
// in pgx, that is a DSN starting with exactly "postgres://" or "postgresql://";
// pgx reads anything else, "POSTGRES://" included, as keyword/value.
func dsnURLScheme(dsn string) (string, bool) {
	for _, scheme := range []string{"postgres", "postgresql"} {
		if strings.HasPrefix(dsn, scheme+"://") {
			return scheme, true
		}
	}
	return "", false
}
