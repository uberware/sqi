// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"time"
)

// expiryWarnWindow is how close to NotAfter a certificate has to be before
// startup warns about it.
const expiryWarnWindow = 30 * 24 * time.Hour

// LoadServerTLS builds a server *tls.Config from a certificate/key pair,
// optionally requiring client certificates signed by clientCAFile.
//
// It is called once at startup with paths [Validate] has already checked, so
// the certificate is read exactly once: passing the paths on to
// ListenAndServeTLS instead would read them a second time, and a rotation
// landing between the two reads would serve a half-written file.
func LoadServerTLS(certFile, keyFile, clientCAFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load keypair %s/%s: %w", certFile, keyFile, err)
	}
	cfg := &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{cert},
	}
	if clientCAFile == "" {
		return cfg, nil
	}
	pool, err := certPool(clientCAFile)
	if err != nil {
		return nil, err
	}
	cfg.ClientCAs = pool
	cfg.ClientAuth = tls.RequireAndVerifyClientCert
	return cfg, nil
}

// certPool reads a PEM bundle into a certificate pool.
func certPool(caFile string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read CA file %s: %w", caFile, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("no valid certificates found in CA file %s", caFile)
	}
	return pool, nil
}

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

// leafOf returns the parsed leaf certificate of a loaded keypair.
func leafOf(pair tls.Certificate) (*x509.Certificate, error) {
	if pair.Leaf != nil {
		return pair.Leaf, nil
	}
	if len(pair.Certificate) == 0 {
		return nil, errors.New("keypair contains no certificate")
	}
	return x509.ParseCertificate(pair.Certificate[0])
}

// validateKeypair checks that certFile and keyFile load together and that the
// leaf has not expired. Every problem a certificate can have is caught here,
// at load, rather than at the first connection attempt.
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

	leaf, err := leafOf(pair)
	if err != nil {
		return []ValidationError{{
			Field:   certField,
			Message: fmt.Sprintf("cannot parse leaf certificate in %s: %s", certFile, err),
		}}
	}
	if time.Now().After(leaf.NotAfter) {
		return []ValidationError{{
			Field:   certField,
			Message: fmt.Sprintf("certificate %s expired at %s; issue a new one (`sqi-server tls init`) — a server that boots with an expired certificate is a server whose whole farm fails to connect", certFile, leaf.NotAfter.Format(time.RFC3339)),
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
		if _, err := certPool(cfg.ClientCAFile); err != nil {
			errs = append(errs, ValidationError{
				Field:   "nats.tls.client_ca_file",
				Message: err.Error(),
			})
		}
	}
	return errs
}

// ExpiringSoon reports whether the leaf certificate in certFile expires
// within 30 days, returning its NotAfter. It is advisory: startup warns,
// it does not refuse. Errors are swallowed because Validate has already
// rejected anything unloadable.
func ExpiringSoon(certFile, keyFile string) (time.Time, bool) {
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return time.Time{}, false
	}
	leaf, err := leafOf(pair)
	if err != nil {
		return time.Time{}, false
	}
	return leaf.NotAfter, time.Until(leaf.NotAfter) < expiryWarnWindow
}
