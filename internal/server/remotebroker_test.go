// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/bus/bustest"
	"github.com/uberware/sqi/internal/config"
	"github.com/uberware/sqi/internal/store/fake"
)

// syncLog is a log sink safe for nats.go callback goroutines.
type syncLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *syncLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *syncLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// externalNATS is an external-mode nats block pointing at c, auth off.
func externalNATS(c *bustest.Cluster) config.NATSConfig {
	n := config.DefaultConfig().NATS
	n.Mode = config.NATSModeExternal
	n.External.URLs = c.URLs
	n.External.NkeySeedFile = c.DataSeed
	n.External.ConnectTimeout = 10 * time.Second
	return n
}

func TestRemoteBrokerConfig_MapsEveryField(t *testing.T) {
	n := config.DefaultConfig().NATS
	n.Mode = config.NATSModeExternal
	n.Auth.Enabled = true
	n.Auth.UserTTL = 2 * time.Hour
	n.External = config.NATSExternalConfig{
		URLs:           []string{"tls://a:4222", "tls://b:4222"},
		AdvertiseURLs:  []string{"tls://pub:4222"},
		Account:        "FARM",
		NkeySeedFile:   "/k/data.nk",
		CredsFile:      "/k/data.creds",
		StreamReplicas: 3,
		ConnectTimeout: 45 * time.Second,
		TLS:            config.NATSClientTLSConfig{CAFile: "/t/ca", CertFile: "/t/c", KeyFile: "/t/k", ServerName: "nats.example"},
		Callout:        config.NATSCalloutConfig{NkeySeedFile: "/k/auth.nk", IssuerSeedFile: "/k/issuer.nk", SystemCredsFile: "/k/sys.creds"},
	}
	got := RemoteBrokerConfig(n)

	if strings.Join(got.URLs, ",") != "tls://a:4222,tls://b:4222" ||
		strings.Join(got.AdvertiseURLs, ",") != "tls://pub:4222" || got.Account != "FARM" ||
		got.NkeySeedFile != "/k/data.nk" || got.CredsFile != "/k/data.creds" ||
		got.StreamReplicas != 3 || got.ConnectTimeout != 45*time.Second {
		t.Errorf("connection fields = %+v", got)
	}
	if got.TLS != (bus.ClientTLSConfig{CAFile: "/t/ca", CertFile: "/t/c", KeyFile: "/t/k", ServerName: "nats.example"}) {
		t.Errorf("tls = %+v", got.TLS)
	}
	a := got.Auth
	if !a.Enabled || a.UserTTL != 2*time.Hour || a.Lookup != nil ||
		a.CalloutNkeySeedFile != "/k/auth.nk" || a.IssuerSeedFile != "/k/issuer.nk" || a.SystemCredsFile != "/k/sys.creds" {
		t.Errorf("auth = %+v; want every field mapped and Lookup left for the caller", a)
	}
	n.External.URLs[0] = "changed"
	n.External.AdvertiseURLs[0] = "changed"
	if got.URLs[0] != "tls://a:4222" || got.AdvertiseURLs[0] != "tls://pub:4222" {
		t.Error("RemoteBrokerConfig shares a URL slice with the configuration")
	}
}

func TestEmbeddedBrokerConfig_MapsAdvertiseURL(t *testing.T) {
	cfg := Config{NATS: config.DefaultConfig().NATS}
	cfg.NATS.Embedded.AdvertiseURL = "tls://nats.example.com:7422"
	if got := embeddedBrokerConfig(cfg, fake.New()).AdvertiseURL; got != "tls://nats.example.com:7422" {
		t.Errorf("AdvertiseURL = %q, want the configured nats.embedded.advertise_url", got)
	}
}

func TestNewBroker_SelectsByMode(t *testing.T) {
	tests := []struct {
		mode string
		want string
	}{
		{config.NATSModeEmbedded, "*bus.Embedded"},
		{"", "*bus.Embedded"},
		{config.NATSModeExternal, "*bus.Remote"},
	}
	for _, tt := range tests {
		t.Run(tt.mode, func(t *testing.T) {
			s := &Server{cfg: Config{NATS: config.DefaultConfig().NATS}, logger: slog.New(slog.DiscardHandler), store: fake.New()}
			s.cfg.NATS.Mode = tt.mode
			b, err := s.newBroker()
			if err != nil {
				t.Fatalf("newBroker: %v", err)
			}
			var got string
			switch b.(type) {
			case *bus.Embedded:
				got = "*bus.Embedded"
			case *bus.Remote:
				got = "*bus.Remote"
			default:
				got = "other"
			}
			if got != tt.want {
				t.Fatalf("newBroker in mode %q = %T, want %s", tt.mode, b, tt.want)
			}
		})
	}

	t.Run("unknown mode", func(t *testing.T) {
		s := &Server{cfg: Config{NATS: config.DefaultConfig().NATS}, logger: slog.New(slog.DiscardHandler), store: fake.New()}
		s.cfg.NATS.Mode = "cluster"
		if _, err := s.newBroker(); err == nil {
			t.Fatal("newBroker accepted an unknown mode")
		}
	})
}

// TestServer_ExternalAuthOffWarns starts an external-mode broker with auth off
// on a loopback cluster. The WARN fires anyway: sqi cannot tell whether an
// external cluster's port is reachable beyond this host.
func TestServer_ExternalAuthOffWarns(t *testing.T) {
	c := bustest.StartCluster(t, 1, bustest.Options{})
	var logs syncLog
	s := &Server{
		cfg:    Config{NATS: externalNATS(c)},
		logger: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})),
		store:  fake.New(),
	}
	b, err := s.startBroker(t.Context())
	if err != nil {
		t.Fatalf("startBroker: %v", err)
	}
	t.Cleanup(b.Shutdown)
	if _, ok := b.(*bus.Remote); !ok {
		t.Fatalf("startBroker in external mode = %T, want *bus.Remote", b)
	}

	var warns []string
	for line := range strings.SplitSeq(logs.String(), "\n") {
		if strings.Contains(line, "level=WARN") && strings.Contains(line, "broker is unauthenticated") {
			warns = append(warns, line)
		}
	}
	if len(warns) != 1 {
		t.Fatalf("unauthenticated-broker WARN lines = %d, want 1; log:\n%s", len(warns), logs.String())
	}
	if !strings.Contains(warns[0], c.URLs[0]) {
		t.Errorf("WARN %q does not name the broker URL %s", warns[0], c.URLs[0])
	}
}

// TestServer_ExternalAuthWithoutSystemCredsWarns starts an external-mode
// broker with worker authentication on and no system credential. Revocation
// can then only take effect at a user JWT's expiry, and the startup WARN names
// that bound.
func TestServer_ExternalAuthWithoutSystemCredsWarns(t *testing.T) {
	c := bustest.StartCluster(t, 1, bustest.Options{AuthEnabled: true})
	n := externalNATS(c)
	n.Auth.Enabled = true
	n.Auth.UserTTL = time.Hour
	n.External.Callout.NkeySeedFile = c.CalloutSeed
	n.External.Callout.IssuerSeedFile = c.IssuerSeed

	countWarns := func(n config.NATSConfig) (int, string) {
		t.Helper()
		var logs syncLog
		s := &Server{
			cfg:    Config{NATS: n},
			logger: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})),
			store:  fake.New(),
		}
		b, err := s.startBroker(t.Context())
		if err != nil {
			t.Fatalf("startBroker: %v", err)
		}
		b.Shutdown()
		count := 0
		for line := range strings.SplitSeq(logs.String(), "\n") {
			if strings.Contains(line, "level=WARN") && strings.Contains(line, "revocation takes effect within 1h") {
				count++
			}
		}
		return count, logs.String()
	}

	if got, logs := countWarns(n); got != 1 {
		t.Fatalf("bounded-revocation WARN lines = %d, want 1; log:\n%s", got, logs)
	}
	n.External.Callout.SystemCredsFile = c.SystemCreds
	if got, logs := countWarns(n); got != 0 {
		t.Fatalf("bounded-revocation WARN lines with a system credential = %d, want 0; log:\n%s", got, logs)
	}
}

func TestShortDuration(t *testing.T) {
	for _, tt := range []struct {
		in   time.Duration
		want string
	}{
		{time.Hour, "1h"},
		{90 * time.Minute, "1h30m"},
		{24 * time.Hour, "24h"},
		{time.Minute, "1m"},
		{90 * time.Second, "1m30s"},
		{time.Hour + time.Second, "1h0m1s"},
	} {
		if got := shortDuration(tt.in); got != tt.want {
			t.Errorf("shortDuration(%s) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestRun_StartFailureReleasesExternalBroker fails startup after the external
// broker has started (discovery refuses the advertised URL, the last step of
// start) and then starts a second broker on the same account at once, as a
// supervisor's immediate retry would. A failed start that kept the broker
// would hold the account's guard until its lease expired, and the retry would
// be refused naming its own host.
func TestRun_StartFailureReleasesExternalBroker(t *testing.T) {
	c := bustest.StartCluster(t, 1, bustest.Options{})

	cfg := DefaultConfig()
	cfg.HTTPAddr = freeLoopbackAddr(t)
	cfg.NATS = externalNATS(c)
	cfg.NATS.External.AdvertiseURLs = []string{"http://not-a-broker:4222"}
	cfg.SQLitePath = filepath.Join(t.TempDir(), "test.db")
	cfg.DiscoveryEnabled = true
	logger := slog.New(slog.DiscardHandler)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	srv := New(cfg, logger, nil)
	// A failed start leaves the store open, since the process exits. Windows
	// cannot remove an open database file when the test directory is cleaned
	// up, so release it here, once the checkpointer has finished.
	t.Cleanup(func() {
		cancel()
		if srv.checkpointDone != nil {
			<-srv.checkpointDone
		}
		if srv.store != nil {
			if err := srv.store.Close(); err != nil {
				t.Errorf("close the store left open by the failed start: %v", err)
			}
		}
	})
	err := srv.Run(ctx)
	if err == nil || !strings.Contains(err.Error(), "discovery") {
		t.Fatalf("Run = %v, want a discovery failure at startup", err)
	}

	retry := bus.NewRemote(RemoteBrokerConfig(cfg.NATS), logger)
	defer retry.Shutdown()
	if err := retry.Start(ctx); err != nil {
		t.Fatalf("a retry on the same account was refused after the failed start: %v", err)
	}
}
