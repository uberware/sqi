// SPDX-License-Identifier: AGPL-3.0-or-later

// Package tlsutil holds the TLS loading primitives shared by every component
// that terminates or dials TLS: the API listener, the embedded broker, the
// worker's broker client and the worker's enrollment client.
//
// It exists because those callers live on both sides of an import boundary.
// internal/config pulls internal/auth/oidc, so the worker binary can never
// import it, and internal/bus is forbidden from importing internal/config at
// all. This package imports nothing internal, so all of them can reach it.
//
// It deliberately holds only LOADING. Validation of operator-supplied paths
// stays in internal/config, which is the one place that knows config keys and
// can name them in an error.
package tlsutil

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
)

// CertPool reads a PEM bundle into a certificate pool.
//
// Callers use this for the "empty means system roots, set means THAT CA only"
// rule: an empty path never reaches here, and a non-empty one produces a pool
// that pins the acceptable issuer rather than widening it.
func CertPool(caFile string) (*x509.CertPool, error) {
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

// ServerConfig builds a server *tls.Config from a certificate/key pair,
// optionally requiring client certificates signed by clientCAFile.
//
// It is called once at startup, with paths the caller has already validated,
// so the certificate is read exactly once: handing the paths to
// ListenAndServeTLS instead would read them a second time, and a rotation
// landing between the two reads would serve a half-written file.
func ServerConfig(certFile, keyFile, clientCAFile string) (*tls.Config, error) {
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
	pool, err := CertPool(clientCAFile)
	if err != nil {
		return nil, err
	}
	cfg.ClientCAs = pool
	cfg.ClientAuth = tls.RequireAndVerifyClientCert
	return cfg, nil
}

// LeafOf returns the parsed leaf certificate of a loaded keypair.
//
// tls.LoadX509KeyPair normally populates Leaf itself, but only when the
// x509keypairleaf GODEBUG is left at its default; under
// GODEBUG=x509keypairleaf=0 it stays nil, so the re-parse below is a real
// fallback rather than dead code.
func LeafOf(pair tls.Certificate) (*x509.Certificate, error) {
	if pair.Leaf != nil {
		return pair.Leaf, nil
	}
	if len(pair.Certificate) == 0 {
		return nil, errors.New("keypair contains no certificate")
	}
	return x509.ParseCertificate(pair.Certificate[0])
}
