// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/nats-io/nkeys"

	"github.com/uberware/sqi/internal/brokerauth"
	"github.com/uberware/sqi/internal/discovery"
	"github.com/uberware/sqi/internal/redact"
)

// validateNATSExternal checks the nats.external block, and the callout keys it
// needs when worker authentication is on. discoveryEnabled says whether
// discovery advertises a broker URL at all.
func validateNATSExternal(cfg NATSConfig, discoveryEnabled bool) []ValidationError {
	ex := cfg.External
	var errs []ValidationError
	errs = append(errs, validateExternalURLs(ex)...)
	errs = append(errs, validateExternalAdvertiseURLs(ex)...)
	if discoveryEnabled {
		errs = append(errs, validateAdvertisedFirstURL(ex)...)
	}
	errs = append(errs, validateExternalAccount(ex.Account)...)
	if ex.NkeySeedFile != "" && ex.CredsFile != "" {
		errs = append(errs, ValidationError{
			Field:   "nats.external.nkey_seed_file",
			Message: "nats.external.nkey_seed_file and nats.external.creds_file are mutually exclusive; set only one",
		})
	}
	switch ex.StreamReplicas {
	case 1, 3, 5:
	default:
		errs = append(errs, ValidationError{
			Field:   "nats.external.stream_replicas",
			Message: fmt.Sprintf("must be 1, 3 or 5, got %d; set SQI_NATS_EXTERNAL_STREAM_REPLICAS or nats.external.stream_replicas", ex.StreamReplicas),
		})
	}
	if ex.ConnectTimeout <= 0 {
		errs = append(errs, ValidationError{
			Field:   "nats.external.connect_timeout",
			Message: fmt.Sprintf("must be > 0, got %s; set SQI_NATS_EXTERNAL_CONNECT_TIMEOUT or nats.external.connect_timeout", ex.ConnectTimeout),
		})
	}
	errs = append(errs, validateReadable("nats.external.nkey_seed_file", ex.NkeySeedFile)...)
	errs = append(errs, validateReadable("nats.external.creds_file", ex.CredsFile)...)
	errs = append(errs, validateReadable("nats.external.tls.ca_file", ex.TLS.CAFile)...)
	if ex.TLS.CertFile != "" || ex.TLS.KeyFile != "" {
		// A client certificate is optional, but half of one is a mistake.
		errs = append(errs, validateKeypair("nats.external.tls.cert_file", "nats.external.tls.key_file", ex.TLS.CertFile, ex.TLS.KeyFile)...)
	}
	if cfg.Auth.Enabled {
		errs = append(errs, validateCallout(ex.Callout)...)
	}
	return errs
}

// validateExternalAccount checks nats.external.account. AUTH and SYS are
// accounts the generated cluster config also defines, and $G is nats-server's
// global account; a name that collides with one would merge or clobber it.
func validateExternalAccount(account string) []ValidationError {
	const field = "nats.external.account"
	switch {
	case account == "":
		return []ValidationError{{Field: field, Message: "must not be empty; set SQI_NATS_EXTERNAL_ACCOUNT or nats.external.account"}}
	case brokerauth.ReservedAccount(account):
		return []ValidationError{{Field: field, Message: fmt.Sprintf("%q is reserved for a NATS account sqi or nats-server defines; choose another name", account)}}
	case !brokerauth.ValidAccountName(account):
		return []ValidationError{{Field: field, Message: fmt.Sprintf("%q must use only letters, digits, '_' and '-'; set SQI_NATS_EXTERNAL_ACCOUNT or nats.external.account", account)}}
	}
	return nil
}

// validateExternalURLs checks nats.external.urls: non-empty, well formed, one
// scheme throughout, and consistent with the TLS settings.
func validateExternalURLs(ex NATSExternalConfig) []ValidationError {
	const field = "nats.external.urls"
	if len(ex.URLs) == 0 {
		return []ValidationError{{
			Field:   field,
			Message: "must list at least one broker URL in external mode; set SQI_NATS_EXTERNAL_URLS or nats.external.urls",
		}}
	}
	var errs []ValidationError
	var tlsCount, plainCount int
	for _, raw := range ex.URLs {
		u, err := url.Parse(raw)
		switch {
		case err != nil || u.Hostname() == "":
			errs = append(errs, ValidationError{Field: field, Message: fmt.Sprintf("invalid broker URL %q; want tls://host:port or nats://host:port", redact.URL(raw))})
		case u.Scheme == "tls":
			tlsCount++
		case u.Scheme == "nats":
			plainCount++
		default:
			errs = append(errs, ValidationError{Field: field, Message: fmt.Sprintf("broker URL %q must use the tls:// or nats:// scheme", redact.URL(raw))})
		}
	}
	if tlsCount > 0 && plainCount > 0 {
		errs = append(errs, ValidationError{
			Field:   field,
			Message: "mixes tls:// and nats:// URLs; use one scheme for every broker URL",
		})
	}
	if plainCount > 0 && tlsCount == 0 {
		errs = append(errs, tlsWithPlainURLs(ex.TLS)...)
	}
	return errs
}

// tlsWithPlainURLs rejects TLS settings beside nats:// URLs. Any of them would
// make the client require TLS on a connection the URLs say is plaintext, so
// the result depends on which the broker happens to offer.
func tlsWithPlainURLs(t NATSClientTLSConfig) []ValidationError {
	var errs []ValidationError
	for _, f := range []struct{ key, value string }{
		{"ca_file", t.CAFile},
		{"cert_file", t.CertFile},
		{"key_file", t.KeyFile},
		{"server_name", t.ServerName},
	} {
		if f.value == "" {
			continue
		}
		errs = append(errs, ValidationError{
			Field: "nats.external.tls." + f.key,
			Message: fmt.Sprintf("is set but every nats.external.urls entry is nats://; "+
				"use tls:// URLs for a TLS connection, or unset %s", f.key),
		})
	}
	return errs
}

// validateCallout checks the callout keys, which external mode with worker
// authentication requires.
func validateCallout(c NATSCalloutConfig) []ValidationError {
	var errs []ValidationError
	if c.NkeySeedFile == "" {
		errs = append(errs, ValidationError{
			Field:   "nats.external.callout.nkey_seed_file",
			Message: "is required when nats.auth.enabled is true in external mode; set SQI_NATS_EXTERNAL_CALLOUT_NKEY_SEED_FILE or nats.external.callout.nkey_seed_file",
		})
	}
	errs = append(errs, validateReadable("nats.external.callout.nkey_seed_file", c.NkeySeedFile)...)
	errs = append(errs, validateReadable("nats.external.callout.system_creds_file", c.SystemCredsFile)...)
	if c.IssuerSeedFile == "" {
		return append(errs, ValidationError{
			Field:   "nats.external.callout.issuer_seed_file",
			Message: "is required when nats.auth.enabled is true in external mode; set SQI_NATS_EXTERNAL_CALLOUT_ISSUER_SEED_FILE or nats.external.callout.issuer_seed_file",
		})
	}
	return append(errs, validateIssuerSeed(c.IssuerSeedFile)...)
}

// validateIssuerSeed requires the file to hold an account nkey seed. A user
// seed is the likely mistake, so the error names it.
func validateIssuerSeed(path string) []ValidationError {
	const field = "nats.external.callout.issuer_seed_file"
	data, err := os.ReadFile(path)
	if err != nil {
		return []ValidationError{{Field: field, Message: fmt.Sprintf("cannot read %s: %s", path, err)}}
	}
	kp, err := nkeys.FromSeed([]byte(strings.TrimSpace(string(data))))
	if err != nil {
		return []ValidationError{{Field: field, Message: fmt.Sprintf("%s does not hold a valid nkey seed (want an account nkey seed, SA...): %s", path, err)}}
	}
	defer kp.Wipe()
	pub, err := kp.PublicKey()
	if err != nil {
		return []ValidationError{{Field: field, Message: fmt.Sprintf("%s: %s", path, err)}}
	}
	if !nkeys.IsValidPublicAccountKey(pub) {
		return []ValidationError{{
			Field: field,
			Message: fmt.Sprintf("%s holds a %s, but the issuer must be an account nkey seed (SA...); "+
				"a user seed (SU...) is the usual mistake", path, nkeyKind(pub)),
		}}
	}
	return nil
}

// nkeyKind names the type of a public nkey for an error message.
func nkeyKind(pub string) string {
	switch {
	case nkeys.IsValidPublicUserKey(pub):
		return "user nkey seed"
	case nkeys.IsValidPublicOperatorKey(pub):
		return "operator nkey seed"
	case nkeys.IsValidPublicServerKey(pub):
		return "server nkey seed"
	default:
		return "non-account nkey seed"
	}
}

// validateReadable checks that a non-empty path exists and can be opened.
func validateReadable(field, path string) []ValidationError {
	if path == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return []ValidationError{{Field: field, Message: fmt.Sprintf("cannot read %s: %s", path, err)}}
	}
	_ = f.Close()
	return nil
}

// validateAdvertiseURL checks one URL discovery would hand to workers
// against [discovery.CheckNATSURL], with a message naming the fix. Length is
// left to [validateAdvertiseRecordLength], since only the first URL is
// advertised. wantScheme, when non-empty, is the scheme the broker listener
// uses; a mismatch would send workers to the wrong transport.
// Messages show the URL redacted.
func validateAdvertiseURL(field, raw, wantScheme, hint string) []ValidationError {
	u, err := discovery.CheckNATSURL(raw)
	shown := redact.URL(raw)
	msg := ""
	switch {
	case errors.Is(err, discovery.ErrNATSURLInvalid):
		msg = fmt.Sprintf("invalid advertised URL %q; want tls://host:port or nats://host:port", shown)
	case errors.Is(err, discovery.ErrNATSURLCredentials):
		msg = fmt.Sprintf("advertised URL %q must not carry credentials (user:password@ or token@): discovery publishes it in plaintext "+
			"over mDNS; remove them, since workers authenticate with their own credentials", shown)
	case errors.Is(err, discovery.ErrNATSURLScheme):
		msg = fmt.Sprintf("advertised URL %q must use the tls:// or nats:// scheme", shown)
	case wantScheme != "" && u.Scheme != wantScheme:
		msg = fmt.Sprintf("advertised URL %q uses %s:// but %s; use %s://", shown, u.Scheme, hint, wantScheme)
	case errors.Is(err, discovery.ErrNATSURLPortZero):
		msg = fmt.Sprintf("advertised URL %q names port 0, which no worker can dial; give the broker's real port", shown)
	default:
		return nil
	}
	return []ValidationError{{Field: field, Message: msg}}
}

// validateAdvertiseRecordLength rejects a first advertised URL too long for
// the nats_url TXT record. Only the first is advertised. remedy says how to
// fix it.
func validateAdvertiseRecordLength(field, first, remedy string) []ValidationError {
	if n := discovery.NATSURLRecordLen(first); n > discovery.MaxTXTString {
		return []ValidationError{{Field: field, Message: fmt.Sprintf(
			"advertised URL is %d bytes as a %q record, over the %d-byte DNS TXT limit; %s",
			n, discovery.NATSURLKey+"=...", discovery.MaxTXTString, remedy,
		)}}
	}
	return nil
}

// shorterURL is the remedy for an advertised URL that is too long.
const shorterURL = "use a shorter URL"

// validateExternalAdvertiseURLs checks nats.external.advertise_urls, which
// discovery tells workers to use in place of urls. Each entry must be a
// well-formed broker URL whose scheme agrees with urls.
func validateExternalAdvertiseURLs(ex NATSExternalConfig) []ValidationError {
	const field = "nats.external.advertise_urls"
	if len(ex.AdvertiseURLs) == 0 {
		return nil
	}
	// urls are validated on their own; the scheme to agree with is only
	// known when they parse.
	want := ""
	if len(ex.URLs) > 0 {
		if u, err := url.Parse(ex.URLs[0]); err == nil && (u.Scheme == "tls" || u.Scheme == "nats") {
			want = u.Scheme
		}
	}
	var errs []ValidationError
	for _, raw := range ex.AdvertiseURLs {
		errs = append(errs, validateAdvertiseURL(field, raw, want, "nats.external.urls uses "+want+"://")...)
	}
	return append(errs, validateAdvertiseRecordLength(field, ex.AdvertiseURLs[0], shorterURL)...)
}

// validateAdvertisedFirstURL holds nats.external.urls[0] to the rules for an
// advertised URL when it is the one discovery advertises, which it is while
// nats.external.advertise_urls is empty. Credentials stay allowed in urls
// whenever advertise_urls is set, because only that is advertised. A URL that
// does not parse or has the wrong scheme is left to [validateExternalURLs].
func validateAdvertisedFirstURL(ex NATSExternalConfig) []ValidationError {
	const field = "nats.external.urls"
	if len(ex.AdvertiseURLs) > 0 || len(ex.URLs) == 0 {
		return nil
	}
	first := ex.URLs[0]
	_, err := discovery.CheckNATSURL(first)
	shown := redact.URL(first)
	switch {
	case errors.Is(err, discovery.ErrNATSURLCredentials):
		return []ValidationError{{Field: field, Message: fmt.Sprintf(
			"the first URL %q carries credentials (user:password@ or token@), and with nats.external.advertise_urls "+
				"empty discovery would publish it in plaintext over mDNS; set nats.external.advertise_urls to a "+
				"credential-free URL of the same broker, or set discovery.enabled: false", shown,
		)}}
	case errors.Is(err, discovery.ErrNATSURLPortZero):
		return []ValidationError{{Field: field, Message: fmt.Sprintf(
			"the first URL %q names port 0, and with nats.external.advertise_urls empty discovery would advertise it; "+
				"give the broker's real port", shown,
		)}}
	case errors.Is(err, discovery.ErrNATSURLTooLong):
		return validateAdvertiseRecordLength(field, first,
			"set nats.external.advertise_urls to a shorter URL of the same broker, or set discovery.enabled: false")
	}
	return nil
}

// validateEmbeddedAdvertiseURL checks nats.embedded.advertise_url, whose
// scheme must match whether the embedded listener requires TLS.
func validateEmbeddedAdvertiseURL(cfg NATSEmbeddedConfig) []ValidationError {
	const field = "nats.embedded.advertise_url"
	if cfg.AdvertiseURL == "" {
		return nil
	}
	want, hint := "nats", "nats.embedded.tls.enabled is false"
	if cfg.TLS.Enabled {
		want, hint = "tls", "nats.embedded.tls.enabled is true"
	}
	errs := validateAdvertiseURL(field, cfg.AdvertiseURL, want, hint)
	return append(errs, validateAdvertiseRecordLength(field, cfg.AdvertiseURL, shorterURL)...)
}
