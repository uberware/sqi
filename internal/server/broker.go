// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/config"
	"github.com/uberware/sqi/internal/restore"
	"github.com/uberware/sqi/internal/store"
)

// credentialLookup is the auth-callout responder's view of the store: the
// worker ID of the active credential holding publicKey. A key with no active
// credential (never enrolled, or revoked) is [bus.ErrUnknownCredential]; any
// other store error is passed through, and the responder refuses the
// connection for it too.
func credentialLookup(st store.WorkerCredentialStore) bus.CredentialLookup {
	return func(ctx context.Context, publicKey string) (string, error) {
		c, err := st.GetActiveWorkerCredentialByPublicKey(ctx, publicKey)
		if errors.Is(err, store.ErrNotFound) {
			return "", bus.ErrUnknownCredential
		}
		if err != nil {
			return "", fmt.Errorf("look up worker credential: %w", err)
		}
		return c.WorkerID, nil
	}
}

// newBroker builds the broker the configuration selects: the embedded broker
// (the default), or a connection to an operator-run cluster.
func (s *Server) newBroker() (bus.Broker, error) {
	switch s.cfg.NATS.Mode {
	case config.NATSModeEmbedded, "":
		return bus.NewEmbedded(embeddedBrokerConfig(s.cfg, s.store), s.logger), nil
	case config.NATSModeExternal:
		cfg := RemoteBrokerConfig(s.cfg.NATS)
		cfg.Auth.Lookup = credentialLookup(s.store)
		return bus.NewRemote(cfg, s.logger), nil
	default:
		return nil, fmt.Errorf("unknown nats.mode %q; want %s or %s", s.cfg.NATS.Mode, config.NATSModeEmbedded, config.NATSModeExternal)
	}
}

// RemoteBrokerConfig maps the nats configuration onto the external broker's
// own view of it. internal/bus does not import internal/config, so the fields
// are copied one by one. Auth.Lookup is left nil: the caller supplies the
// store-backed credential lookup.
func RemoteBrokerConfig(c config.NATSConfig) bus.RemoteConfig {
	e := c.External
	return bus.RemoteConfig{
		URLs:           slices.Clone(e.URLs),
		Account:        e.Account,
		NkeySeedFile:   e.NkeySeedFile,
		CredsFile:      e.CredsFile,
		StreamReplicas: e.StreamReplicas,
		ConnectTimeout: e.ConnectTimeout,
		TLS: bus.ClientTLSConfig{
			CAFile:     e.TLS.CAFile,
			CertFile:   e.TLS.CertFile,
			KeyFile:    e.TLS.KeyFile,
			ServerName: e.TLS.ServerName,
		},
		Auth: bus.RemoteAuthConfig{
			AuthConfig: bus.AuthConfig{
				Enabled: c.Auth.Enabled,
				UserTTL: c.Auth.UserTTL,
			},
			CalloutNkeySeedFile: e.Callout.NkeySeedFile,
			IssuerSeedFile:      e.Callout.IssuerSeedFile,
			SystemCredsFile:     e.Callout.SystemCredsFile,
		},
	}
}

// embeddedBrokerConfig maps the nats configuration onto the embedded broker's
// own view of it. internal/bus does not import internal/config, so the fields
// are copied one by one.
func embeddedBrokerConfig(cfg Config, st store.WorkerCredentialStore) bus.EmbeddedConfig {
	e := cfg.NATS.Embedded
	return bus.EmbeddedConfig{
		Addr:       e.Addr,
		DataDir:    e.DataDir,
		MaxStoreMB: e.MaxStoreMB,
		TLS: bus.BrokerTLSConfig{
			Enabled:      e.TLS.Enabled,
			CertFile:     e.TLS.CertFile,
			KeyFile:      e.TLS.KeyFile,
			ClientCAFile: e.TLS.ClientCAFile,
		},
		Auth: bus.AuthConfig{
			Enabled: cfg.NATS.Auth.Enabled,
			UserTTL: cfg.NATS.Auth.UserTTL,
			Lookup:  credentialLookup(st),
		},
	}
}

// reconcileBrokerLayout compares the broker this server just started against
// the one the database last ran against. When they differ, in-flight messages
// for open attempts (status reports, log chunks, cancels) stayed behind in the
// old broker or account, which is the hazard "sqi-server restore" exists for,
// so it applies the same fence: every open attempt closes as failed, its task
// returns to ready, and no retry is consumed. Only then is the new identity
// recorded, so a failed fence is retried on the next start.
//
// A database with no recorded identity counts as a switch, which also covers
// one last run by a build whose streams lived in the global account. On a
// database with no open attempts the fence does nothing.
//
// It must run after the broker starts and before any consumer or lease
// subscription can process a message.
func (s *Server) reconcileBrokerLayout(ctx context.Context) error {
	current := s.broker.Identity()
	recorded, err := s.store.GetBrokerIdentity(ctx)
	if err != nil {
		return fmt.Errorf("read recorded broker identity: %w", err)
	}
	if recorded != current {
		now := time.Now().UTC()
		n, err := restore.Fence(ctx, s.store, now)
		if err != nil {
			return fmt.Errorf("fence open attempts for the broker switch: %w", err)
		}
		if err := s.store.SetBrokerIdentity(ctx, current, now); err != nil {
			return fmt.Errorf("record broker identity: %w", err)
		}
		s.logIdentityRecorded(ctx, recorded, current, n)
	}
	s.logLeftoverGlobalStreams(ctx)
	return nil
}

// logIdentityRecorded says what reconcileBrokerLayout just did. A new
// database records its first identity with nothing to fence; a database an
// older build ran on records one after fencing what it left open; any other
// is a broker switch.
func (s *Server) logIdentityRecorded(ctx context.Context, recorded, current string, fenced int) {
	switch {
	case recorded == "" && fenced == 0:
		s.logger.InfoContext(ctx, "bus: broker identity recorded",
			slog.String("identity", current))
	case recorded == "":
		s.logger.InfoContext(ctx, "bus: no broker identity recorded; open attempts fenced",
			slog.Int("fenced_attempts", fenced),
			slog.String("identity", current))
	default:
		s.logger.InfoContext(ctx, "bus: broker layout changed; open attempts fenced",
			slog.Int("fenced_attempts", fenced),
			slog.String("previous_identity", recorded),
			slog.String("identity", current))
	}
}

// legacyGlobalStreamsDir is where nats-server keeps the streams of the global
// account ($G) under storeDir. Builds before the SQI account kept sqi's
// streams there.
func legacyGlobalStreamsDir(storeDir string) string {
	return filepath.Join(storeDir, "jetstream", "$G")
}

// logLeftoverGlobalStreams logs once when an embedded broker's store still
// holds the global account's streams from an older build. Nothing reads them
// any more. They are not deleted: that is the operator's call.
func (s *Server) logLeftoverGlobalStreams(ctx context.Context) {
	emb, ok := s.broker.(interface{ StoreDir() string })
	if !ok || emb.StoreDir() == "" {
		return
	}
	dir := legacyGlobalStreamsDir(emb.StoreDir())
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return
	}
	s.logger.InfoContext(ctx, "bus: found JetStream streams from an older build; they are abandoned and safe to delete",
		slog.String("path", dir))
}
