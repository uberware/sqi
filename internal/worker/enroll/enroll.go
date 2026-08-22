// SPDX-License-Identifier: AGPL-3.0-or-later

// Package enroll obtains the nkey broker credential sqi-worker authenticates
// with: loading an existing seed from disk when one is present, and
// otherwise enrolling with sqi-server over REST when a join token is
// configured.
//
// Enrollment runs over REST rather than NATS on purpose: the broker's entire
// job is to refuse unauthenticated connections, so it cannot also be the
// channel a worker gets its first credential over. mDNS already advertises
// the HTTP port, so zero-configuration discovery still works.
package enroll

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/uberware/sqi/internal/brokerauth"
)

// ErrNoCredential is returned when no credential file exists and no join
// token is configured to obtain one. It is not inherently fatal to a boot on
// a farm that does not require worker authentication — see
// [EnsureCredential]'s doc for how callers should treat it.
var ErrNoCredential = errors.New("worker: no credential and no join token")

// Config holds everything EnsureCredential needs to load or obtain a
// credential. It is passed by value, deliberately separate from the broader
// worker configuration struct, so that a seed never has to travel through —
// and risk being logged by — general-purpose configuration plumbing.
type Config struct {
	// WorkerID is this worker's stable, server-correlated identity.
	WorkerID string

	// CredentialFile is the path to this worker's nkey seed file.
	CredentialFile string

	// JoinToken is a worker enrollment token, used once to obtain a
	// credential.
	JoinToken string

	// JoinTokenFile is a path to a file containing a join token, and takes
	// precedence over JoinToken when both are set.
	JoinTokenFile string

	// ServerURL is the sqi-server HTTP base URL used for enrollment.
	ServerURL string

	// HTTPClient performs the enrollment request. When nil, one is built from
	// TLSCAFile and InsecureSkipVerify below.
	HTTPClient *http.Client

	// TLSCAFile is the CA that verifies the server's certificate at
	// ServerURL. Empty means the system roots; set means THAT CA only, so a
	// farm CA pins the acceptable issuer rather than widening it.
	TLSCAFile string

	// InsecureSkipVerify disables verification of the server's certificate.
	// Development only.
	InsecureSkipVerify bool
}

// httpClient returns the client used for the enrollment request.
//
// It exists because http.DefaultClient cannot trust a private farm CA, and
// enrollment is the one REST call a worker makes BEFORE it has any credential
// at all — so without this a farm with an HTTPS server could not bootstrap.
func httpClient(cfg Config) (*http.Client, error) {
	if cfg.HTTPClient != nil {
		return cfg.HTTPClient, nil
	}
	tlsCfg := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: cfg.InsecureSkipVerify, //nolint:gosec // G402: explicitly requested via nats.server_tls_insecure_skip_verify
	}
	if cfg.TLSCAFile != "" {
		pemBytes, err := os.ReadFile(cfg.TLSCAFile)
		if err != nil {
			return nil, fmt.Errorf("worker: read enrollment CA file %s: %w", cfg.TLSCAFile, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pemBytes) {
			return nil, fmt.Errorf("worker: no valid certificates found in enrollment CA file %s", cfg.TLSCAFile)
		}
		tlsCfg.RootCAs = pool
	}
	return &http.Client{
		Timeout:   30 * time.Second,
		Transport: &http.Transport{TLSClientConfig: tlsCfg},
	}, nil
}

// enrollRequest is the body of POST /api/v1/workers/enroll.
type enrollRequest struct {
	JoinToken string `json:"join_token"`
	WorkerID  string `json:"worker_id"`
	PublicKey string `json:"public_key"`
}

// EnsureCredential returns this worker's nkey seed and public key: loading an
// existing credential file if one is present, and otherwise enrolling with
// sqi-server over REST using a configured join token.
//
// The order follows the spec exactly: an existing seed is loaded first — an
// already-enrolled worker must never re-enroll just because a join token is
// still configured — and only when no seed exists does enrollment happen,
// using whichever token is configured. When neither a seed nor a token is
// available, EnsureCredential returns ErrNoCredential rather than silently
// connecting with nothing: it is the caller's job to decide whether that is
// fatal (a farm that requires authentication) or fine (an auth-off farm,
// where the caller should proceed with no credential and let the broker
// itself decide whether to accept the connection).
//
// A credential is written to disk only after the server confirms enrollment
// succeeded — a failed enrollment must never leave a seed behind that a
// later boot would silently reuse.
func EnsureCredential(ctx context.Context, cfg Config, logger *slog.Logger) (seed []byte, publicKey string, err error) {
	seed, loadErr := brokerauth.LoadSeed(cfg.CredentialFile)
	switch {
	case loadErr == nil:
		publicKey, err = brokerauth.PublicKeyFromSeed(seed)
		if err != nil {
			return nil, "", err
		}
		logger.InfoContext(ctx, "enroll: loaded existing credential", slog.String("path", cfg.CredentialFile))
		return seed, publicKey, nil
	case errors.Is(loadErr, os.ErrNotExist):
		// Fall through to enrollment below.
	default:
		return nil, "", loadErr
	}

	token, err := resolveJoinToken(cfg)
	if err != nil {
		return nil, "", err
	}
	if token == "" {
		return nil, "", ErrNoCredential
	}
	// ServerURL is never derived from mDNS discovery — enrollment needs an
	// HTTP base URL, and mDNS discovery here only ever resolves a NATS URL
	// (see internal/worker/discovery). Fail before any HTTP attempt so the
	// operator sees a config key to fix rather than a raw
	// "unsupported protocol scheme" error.
	if cfg.ServerURL == "" {
		return nil, "", errors.New(
			"worker: enrollment requires nats.server_url (env SQI_WORKER_NATS_SERVER_URL) to be set; it is not derived from mDNS discovery",
		)
	}

	seed, publicKey, err = brokerauth.GenerateSeed()
	if err != nil {
		return nil, "", err
	}

	if err := enrollWithServer(ctx, cfg, token, publicKey); err != nil {
		return nil, "", err
	}

	if err := brokerauth.SaveSeed(cfg.CredentialFile, seed); err != nil {
		return nil, "", err
	}
	logger.InfoContext(ctx, "enroll: obtained new credential", slog.String("path", cfg.CredentialFile))
	return seed, publicKey, nil
}

// resolveJoinToken returns the join token to enroll with, preferring
// JoinTokenFile over JoinToken when both are set. An empty return with a nil
// error means no token is configured at all.
func resolveJoinToken(cfg Config) (string, error) {
	if cfg.JoinTokenFile != "" {
		data, err := os.ReadFile(cfg.JoinTokenFile)
		if err != nil {
			return "", fmt.Errorf("worker: read join token file %s: %w", cfg.JoinTokenFile, err)
		}
		return strings.TrimSpace(string(data)), nil
	}
	return cfg.JoinToken, nil
}

// enrollWithServer posts an enrollment request for publicKey to
// cfg.ServerURL and maps the response to an error naming both the cause and
// the remediation. A nil return means the server confirmed enrollment.
func enrollWithServer(ctx context.Context, cfg Config, token, publicKey string) error {
	body, err := json.Marshal(enrollRequest{
		JoinToken: token,
		WorkerID:  cfg.WorkerID,
		PublicKey: publicKey,
	})
	if err != nil {
		return fmt.Errorf("worker: encode enrollment request: %w", err)
	}

	url := strings.TrimRight(cfg.ServerURL, "/") + "/api/v1/workers/enroll"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("worker: build enrollment request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client, err := httpClient(cfg)
	if err != nil {
		return err
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("worker: enrollment request to %s: %w", url, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body) //nolint:errcheck // draining a response body before close; nothing actionable on failure
		_ = resp.Body.Close()
	}()

	switch resp.StatusCode {
	case http.StatusCreated, http.StatusOK:
		return nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return errors.New(
			"worker: enrollment refused: the join token is unknown, expired, or already used — issue a new one with `sqi-server worker token issue`",
		)
	case http.StatusConflict:
		return fmt.Errorf(
			"worker: worker id %s is already enrolled with a different key; revoke it with `sqi-server worker revoke %s` or clear this worker's data dir",
			cfg.WorkerID, cfg.WorkerID,
		)
	default:
		return fmt.Errorf("worker: enrollment failed: server returned %s", resp.Status)
	}
}
