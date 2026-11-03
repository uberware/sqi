// SPDX-License-Identifier: AGPL-3.0-or-later

// Package redact strips credentials from broker URLs before they reach a log
// line, an error message, a terminal or a network advertisement.
//
// [url.URL.Redacted] is not enough for that: it masks a password but leaves a
// user name in place, and nats.go reads a user name with no password as an
// authentication token, so nats://t0ken@host comes out of it unchanged.
//
// It imports only the standard library, so the server, the worker and every
// command can use it.
package redact

import (
	"net/url"
	"strings"
)

// Placeholder stands in for a URL that cannot be parsed, which could hold
// anything.
const Placeholder = "REDACTED-URL"

// URL returns raw with its user information, a user and password or a bare
// token, replaced by REDACTED. A URL that does not parse, or that holds an @
// outside its user information, is replaced whole by [Placeholder]. An empty
// raw stays empty.
func URL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return Placeholder
	}
	if u.User == nil {
		// "user:pass@host:4222" parses as scheme "user" with the rest opaque,
		// so an @ that did not land in User may still be a credential.
		if strings.Contains(raw, "@") {
			return Placeholder
		}
		return raw
	}
	u.User = url.User("REDACTED")
	return u.String()
}

// URLs applies [URL] to each of urls and joins the results with sep.
func URLs(urls []string, sep string) string {
	return strings.Join(EachURL(urls), sep)
}

// EachURL applies [URL] to each of urls, returning nil for nil.
func EachURL(urls []string) []string {
	if urls == nil {
		return nil
	}
	out := make([]string, len(urls))
	for i, raw := range urls {
		out[i] = URL(raw)
	}
	return out
}

// List applies [URL] to each URL of a comma-separated list, the form
// nats.Connect takes several servers in.
func List(list string) string {
	return URLs(strings.Split(list, ","), ",")
}
