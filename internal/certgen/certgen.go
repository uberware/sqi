// SPDX-License-Identifier: AGPL-3.0-or-later

// Package certgen generates the certificate material a farm needs to run
// sqi over TLS: a farm certificate authority, server certificates for the
// API listener and the embedded broker, and client certificates for the
// optional worker mTLS path.
//
// It exists because SANs and extended key usage are exactly what
// hand-rolled openssl invocations get wrong, and getting them wrong fails
// at worker-connect time — far from the mistake — rather than at load.
//
// The same generator backs `sqi-server tls init` and the test suites, so
// the code path under test is the code path that ships. Tests therefore
// never need checked-in certificate fixtures, which expire and break CI on
// a date nobody chose.
package certgen

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"time"
)

// CA is a generated certificate authority held in memory.
type CA struct {
	Cert    *x509.Certificate
	Key     *ecdsa.PrivateKey
	CertPEM []byte
	KeyPEM  []byte
}

// Leaf is a generated end-entity certificate held in memory.
type Leaf struct {
	CertPEM []byte
	KeyPEM  []byte
}

// serialNumber returns a random 128-bit certificate serial number.
func serialNumber() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	n, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, fmt.Errorf("certgen: generate serial: %w", err)
	}
	return n, nil
}

// encode marshals a certificate DER blob and its key into PEM form.
func encode(der []byte, key *ecdsa.PrivateKey) (certPEM, keyPEM []byte, err error) {
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("certgen: marshal key: %w", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}

// NewCA generates a self-signed certificate authority valid for validFor.
func NewCA(commonName string, validFor time.Duration) (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("certgen: generate CA key: %w", err)
	}
	serial, err := serialNumber()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(validFor),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("certgen: create CA certificate: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("certgen: parse CA certificate: %w", err)
	}
	certPEM, keyPEM, err := encode(der, key)
	if err != nil {
		return nil, err
	}
	return &CA{Cert: cert, Key: key, CertPEM: certPEM, KeyPEM: keyPEM}, nil
}

// LoadCA reconstructs a [CA] from PEM material previously written by
// [WriteCA], so certificates can be issued from a farm CA that already exists.
//
// Without this, `tls init` is the only way to get a CA and it refuses to
// overwrite one — which left no way at all to add a worker to an established
// mTLS farm, or to rotate a server certificate, short of replacing the CA and
// reissuing everything.
func LoadCA(certPEM, keyPEM []byte) (*CA, error) {
	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil || certBlock.Type != "CERTIFICATE" {
		return nil, errors.New("certgen: no CERTIFICATE block in the CA certificate")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("certgen: parse CA certificate: %w", err)
	}
	// A leaf cannot sign. Accepting one would mint certificates that every peer
	// rejects, discovered only at the first handshake.
	if !cert.IsCA {
		return nil, errors.New("certgen: the certificate is not a CA (BasicConstraints CA:FALSE)")
	}

	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, errors.New("certgen: no PEM block in the CA key")
	}
	key, err := parseECKey(keyBlock)
	if err != nil {
		return nil, err
	}

	// The pair must actually belong together: a mismatched key produces
	// signatures nothing can verify.
	pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		return nil, errors.New("certgen: CA certificate does not carry an ECDSA public key")
	}
	if !pub.Equal(key.Public()) {
		return nil, errors.New("certgen: CA key does not match the CA certificate")
	}

	return &CA{Cert: cert, Key: key, CertPEM: certPEM, KeyPEM: keyPEM}, nil
}

// parseECKey accepts the EC key encodings openssl and Go produce.
func parseECKey(block *pem.Block) (*ecdsa.PrivateKey, error) {
	if key, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	generic, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("certgen: parse CA key: %w", err)
	}
	key, ok := generic.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("certgen: CA key is %T, want an ECDSA key", generic)
	}
	return key, nil
}

// newLeaf signs an end-entity certificate with ca.
func (ca *CA) newLeaf(commonName string, hosts []string, usage x509.ExtKeyUsage, validFor time.Duration) (*Leaf, error) {
	return ca.newLeafAt(commonName, hosts, usage, time.Now().Add(-time.Hour), validFor+time.Hour)
}

// newLeafAt is newLeaf with an explicit validity start.
func (ca *CA) newLeafAt(commonName string, hosts []string, usage x509.ExtKeyUsage, notBefore time.Time, validFor time.Duration) (*Leaf, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("certgen: generate leaf key: %w", err)
	}
	serial, err := serialNumber()
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             notBefore,
		NotAfter:              notBefore.Add(validFor),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{usage},
		BasicConstraintsValid: true,
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
			continue
		}
		tmpl.DNSNames = append(tmpl.DNSNames, h)
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, &key.PublicKey, ca.Key)
	if err != nil {
		return nil, fmt.Errorf("certgen: create leaf certificate: %w", err)
	}
	certPEM, keyPEM, err := encode(der, key)
	if err != nil {
		return nil, err
	}
	return &Leaf{CertPEM: certPEM, KeyPEM: keyPEM}, nil
}

// NewServerCert signs a server certificate covering hosts. Entries that
// parse as an IP address become IP SANs; the rest become DNS SANs.
//
// The validity window starts an hour in the past, so a certificate is usable
// immediately on a host whose clock runs slightly behind the issuer's.
func (ca *CA) NewServerCert(hosts []string, validFor time.Duration) (*Leaf, error) {
	return ca.NewServerCertNotBefore(hosts, time.Now().Add(-time.Hour), validFor+time.Hour)
}

// NewServerCertNotBefore is [CA.NewServerCert] with an explicit start to the
// validity window, for a certificate minted ahead of a scheduled rollout.
func (ca *CA) NewServerCertNotBefore(hosts []string, notBefore time.Time, validFor time.Duration) (*Leaf, error) {
	cn := "sqi-server"
	if len(hosts) > 0 {
		cn = hosts[0]
	}
	return ca.newLeafAt(cn, hosts, x509.ExtKeyUsageServerAuth, notBefore, validFor)
}

// NewClientCert signs a client certificate for the worker mTLS path. The
// common name is informational: sqi does not bind a certificate to a worker
// identity — the nkey does that. See docs/tls.md.
func (ca *CA) NewClientCert(commonName string, validFor time.Duration) (*Leaf, error) {
	return ca.newLeaf(commonName, nil, x509.ExtKeyUsageClientAuth, validFor)
}
