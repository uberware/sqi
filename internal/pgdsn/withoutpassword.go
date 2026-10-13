// SPDX-License-Identifier: AGPL-3.0-or-later

package pgdsn

import (
	"net/url"
	"strings"
)

// WithoutPassword returns dsn with its password and sslpassword removed, for a
// place the DSN can be seen but its secrets must not be: a pg_dump command
// line. It reads dsn exactly as [Redact] does, and refuses (rest "", ok false)
// wherever [Redact] fails closed: a URL-form DSN that [url.Parse] refuses or
// that [urlDSNAmbiguous] says the driver may read differently, and a
// keyword/value DSN that [parseKeywordValueDSN] cannot tokenize.
//
// A URL-form DSN keeps every byte but those removed: the password of the
// userinfo (with its ":"; with the "@" too when no user name is left), and
// every query pair whose key is password or sslpassword in any case. A
// keyword/value DSN with no such key is returned as it came; otherwise it is
// rebuilt from its other pairs, as [Redact] rebuilds it. Either way pgx (and
// libpq, whose parsers pgx's mirror) reads back every other setting as before.
//
// An upper-case PASSWORD key goes too: pgx sends it to the server as a runtime
// parameter and libpq rejects it, and it is most likely a password either way.
func WithoutPassword(dsn string) (rest string, ok bool) {
	rest, _, ok = withoutSecrets(dsn)
	return rest, ok
}

// SetsSSLPassword reports whether dsn sets a non-empty sslpassword, the client
// key's passphrase, or cannot be read reliably enough to tell (see
// [WithoutPassword]), so a caller with no safe way to pass one on can refuse.
func SetsSSLPassword(dsn string) bool {
	_, ssl, ok := withoutSecrets(dsn)
	return ssl || !ok
}

// withoutSecrets is [WithoutPassword], also reporting whether a non-empty
// sslpassword was removed.
func withoutSecrets(dsn string) (rest string, sslPassword, ok bool) {
	scheme, isURL := dsnURLScheme(dsn)
	if !isURL {
		return keywordValueWithoutSecrets(dsn)
	}
	u, err := url.Parse(dsn)
	if err != nil || urlDSNAmbiguous(u, dsn) {
		return "", false, false
	}
	body := dsn[len(scheme)+len("://"):]
	if u.User != nil {
		// [ambiguousAuthority] has made this the only "@" in the DSN, and the
		// end of the userinfo to both Go and the driver.
		at := strings.IndexByte(body, '@')
		user, _, _ := strings.Cut(body[:at], ":")
		body = body[at+1:]
		if user != "" {
			body = user + "@" + body
		}
	}
	// No "?" precedes the "@" (Go would have ended the authority there and
	// found no userinfo), so the first "?" left starts the query for both.
	head, query, hasQuery := strings.Cut(body, "?")
	if !hasQuery {
		return scheme + "://" + body, false, true
	}
	var kept []string
	for pair := range strings.SplitSeq(query, "&") {
		key, value, _ := strings.Cut(pair, "=")
		if !isPasswordKey(key) {
			kept = append(kept, pair)
			continue
		}
		// [obscuredPasswordKey] has refused every other spelling of the key.
		sslPassword = sslPassword || (strings.EqualFold(key, "sslpassword") && value != "")
	}
	if len(kept) > 0 {
		head += "?" + strings.Join(kept, "&")
	}
	return scheme + "://" + head, sslPassword, true
}

// keywordValueWithoutSecrets is [withoutSecrets] for a keyword/value DSN.
func keywordValueWithoutSecrets(dsn string) (rest string, sslPassword, ok bool) {
	pairs, ok := parseKeywordValueDSN(dsn)
	if !ok {
		return "", false, false
	}
	kept := pairs[:0]
	for _, p := range pairs {
		if isPasswordKey(p.key) {
			sslPassword = sslPassword || (strings.EqualFold(p.key, "sslpassword") && p.value != "")
			continue
		}
		kept = append(kept, p)
	}
	if len(kept) == len(pairs) {
		return dsn, false, true
	}
	return joinKeywordValue(kept), sslPassword, true
}
