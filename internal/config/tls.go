// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"time"

	"github.com/uberware/sqi/internal/tlsutil"
)

// expiryWarnWindow is how close to NotAfter a certificate has to be before
// startup warns about it.
const expiryWarnWindow = 30 * 24 * time.Hour

// missingPathErrors reports the "must be set" errors for an enabled TLS block
// with an empty certificate or key path.
func missingPathErrors(certField, keyField, certFile, keyFile string) []ValidationError {
	var errs []ValidationError
	msg := fmt.Sprintf("must be set when TLS is enabled; set %s and %s, or generate a pair with `sqi-server tls init`", certField, keyField)
	if certFile == "" {
		errs = append(errs, ValidationError{Field: certField, Message: msg})
	}
	if keyFile == "" {
		errs = append(errs, ValidationError{Field: keyField, Message: msg})
	}
	return errs
}

// validateKeypair checks that certFile and keyFile load together and that the
// leaf's validity window covers now.
//
// This covers loadability, pairing and validity dates — NOT every problem a
// certificate can have. A SAN or hostname mismatch is still only discovered
// when a peer verifies the certificate, because it depends on the name that
// peer used, which is not knowable here.
func validateKeypair(certField, keyField, certFile, keyFile string) []ValidationError {
	if certFile == "" || keyFile == "" {
		return missingPathErrors(certField, keyField, certFile, keyFile)
	}

	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		msg := err.Error()
		if os.IsNotExist(err) {
			msg = fmt.Sprintf("no such file: %s", err)
		}
		return []ValidationError{{
			Field:   certField,
			Message: fmt.Sprintf("cannot load certificate/key pair (%s, %s): %s", certFile, keyFile, msg),
		}}
	}

	leaf, err := tlsutil.LeafOf(pair)
	if err != nil {
		return []ValidationError{{
			Field:   certField,
			Message: fmt.Sprintf("cannot parse leaf certificate in %s: %s", certFile, err),
		}}
	}
	now := time.Now()
	if now.After(leaf.NotAfter) {
		return []ValidationError{{
			Field:   certField,
			Message: fmt.Sprintf("certificate %s expired at %s; issue a new one (`sqi-server tls init`) — a server that boots with an expired certificate is a server whose whole farm fails to connect", certFile, leaf.NotAfter.Format(time.RFC3339)),
		}}
	}
	// Not yet valid fails exactly like expired at the peer, and for a reason
	// (a clock skew, a certificate minted for a future rollout) that is even
	// harder to read from a handshake error.
	if now.Before(leaf.NotBefore) {
		return []ValidationError{{
			Field:   certField,
			Message: fmt.Sprintf("certificate %s is not valid until %s; check the system clock, or wait", certFile, leaf.NotBefore.Format(time.RFC3339)),
		}}
	}
	return nil
}

// validateHTTPTLS checks the http.tls block.
func validateHTTPTLS(cfg TLSConfig) []ValidationError {
	if !cfg.Enabled {
		return nil
	}
	return validateKeypair("http.tls.cert_file", "http.tls.key_file", cfg.CertFile, cfg.KeyFile)
}

// validateNATSTLS checks the nats.tls block.
func validateNATSTLS(cfg NATSTLSConfig) []ValidationError {
	if !cfg.Enabled {
		if cfg.ClientCAFile != "" {
			return []ValidationError{{
				Field:   "nats.tls.client_ca_file",
				Message: "requires nats.tls.enabled: true; client certificates cannot be verified on a plaintext listener",
			}}
		}
		return nil
	}
	errs := validateKeypair("nats.tls.cert_file", "nats.tls.key_file", cfg.CertFile, cfg.KeyFile)
	if cfg.ClientCAFile != "" {
		if _, err := tlsutil.CertPool(cfg.ClientCAFile); err != nil {
			errs = append(errs, ValidationError{
				Field:   "nats.tls.client_ca_file",
				Message: err.Error(),
			})
		}
	}
	return errs
}

// ExpiringSoon reports whether the leaf certificate in certFile expires
// within 30 days, returning its NotAfter.
//
// It inspects the FIRST CERTIFICATE block, which the docs require to be the
// leaf. A chain written the other way round would have this report the CA's
// expiry instead — harmless, but it would warn about the wrong date. It is advisory: startup warns, it
// does not refuse. Errors are swallowed because Validate has already rejected
// anything unloadable.
//
// It takes only the certificate: the private key has no bearing on expiry, and
// asking for it would imply otherwise.
func ExpiringSoon(certFile string) (time.Time, bool) {
	raw, err := os.ReadFile(certFile)
	if err != nil {
		return time.Time{}, false
	}
	block, _ := pemDecode(raw)
	if block == nil {
		return time.Time{}, false
	}
	leaf, err := x509.ParseCertificate(block)
	if err != nil {
		return time.Time{}, false
	}
	return leaf.NotAfter, time.Until(leaf.NotAfter) < expiryWarnWindow
}

// pemDecode returns the DER bytes of the first CERTIFICATE block in raw.
func pemDecode(raw []byte) ([]byte, bool) {
	for len(raw) > 0 {
		block, rest := pem.Decode(raw)
		if block == nil {
			return nil, false
		}
		if block.Type == "CERTIFICATE" {
			return block.Bytes, true
		}
		raw = rest
	}
	return nil, false
}
