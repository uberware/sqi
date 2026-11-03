// SPDX-License-Identifier: AGPL-3.0-or-later

package discovery

import (
	"errors"
	"net/url"
)

// NATSURLKey is the TXT record key the broker URL is advertised under.
const NATSURLKey = "nats_url"

// MaxTXTString is the DNS TXT character-string limit; one "key=value"
// record cannot exceed it.
const MaxTXTString = 255

// The ways an advertised broker URL can break the contract workers rely on.
// [CheckNATSURL] wraps exactly one of them.
var (
	// ErrNATSURLInvalid: the URL does not parse or names no host.
	ErrNATSURLInvalid = errors.New("must be nats://host:port or tls://host:port")
	// ErrNATSURLCredentials: mDNS would publish them in plaintext.
	ErrNATSURLCredentials = errors.New("carries credentials, which mDNS would publish in plaintext")
	// ErrNATSURLScheme: the scheme is neither nats nor tls.
	ErrNATSURLScheme = errors.New("must use the nats:// or tls:// scheme")
	// ErrNATSURLPortZero: no worker can dial port 0.
	ErrNATSURLPortZero = errors.New("names port 0, which no worker can dial")
	// ErrNATSURLTooLong: the record would not fit one TXT string.
	ErrNATSURLTooLong = errors.New("is too long for its DNS TXT record")
)

// NATSURLRecordLen is the length of the TXT record that advertises raw.
func NATSURLRecordLen(raw string) int {
	return len(NATSURLKey) + len("=") + len(raw)
}

// CheckNATSURL is the one statement of what an advertised broker URL must
// be: it parses, names a host, carries no credentials, uses the nats or tls
// scheme, does not name port 0, and fits its TXT record. Configuration,
// the responder and the worker's browser all apply it. It returns the parsed
// URL and an error wrapping one of the ErrNATSURL errors, checked in that
// order; the URL is nil only for [ErrNATSURLInvalid].
func CheckNATSURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	switch {
	case err != nil || u.Hostname() == "":
		return nil, ErrNATSURLInvalid
	case u.User != nil:
		return u, ErrNATSURLCredentials
	case u.Scheme != "nats" && u.Scheme != "tls":
		return u, ErrNATSURLScheme
	case u.Port() == "0":
		return u, ErrNATSURLPortZero
	case NATSURLRecordLen(raw) > MaxTXTString:
		return u, ErrNATSURLTooLong
	}
	return u, nil
}
