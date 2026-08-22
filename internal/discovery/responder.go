// SPDX-License-Identifier: AGPL-3.0-or-later

// Package discovery advertises the running sqi-server on the local network via
// multicast DNS (mDNS / DNS-SD) so that workers and the sqi CLI can find it
// without manual address configuration.
//
// The server publishes a single "_sqi._tcp" service instance whose SRV record
// points at the HTTP API port — the canonical, externally-bound sqi endpoint
// (the embedded NATS broker defaults to loopback). Additional connection
// details, including the NATS port, are carried in TXT records:
//
//	id=<instance-id>   unique per process; lets clients dedupe re-announcements
//	http=<http-port>   REST + WebSocket API port (also the SRV port)
//	nats=<nats-port>   embedded NATS JetStream port for worker connections
//	host=<hostname>    advertising host's reported name
//	version=<version>  server build version
//
// Advertisement is toggleable via configuration (see [Config.Enabled]) for
// environments that prohibit multicast, such as most cloud VPCs.
package discovery

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"

	"github.com/google/uuid"
	"github.com/grandcat/zeroconf"

	"github.com/uberware/sqi/internal/version"
)

const (
	// serviceType is the DNS-SD service type advertised by sqi-server.
	serviceType = "_sqi._tcp"

	// domain is the mDNS domain. "local." is the standard DNS-SD multicast
	// domain and must include the trailing dot.
	domain = "local."
)

// Config holds the parameters needed to advertise the sqi-server instance.
type Config struct {
	// Enabled controls whether the responder publishes an mDNS record.
	// When false, [New] still succeeds but [Responder.Start] is a no-op.
	Enabled bool

	// InstanceName is the DNS-SD service instance name (the human-readable
	// label shown in service browsers). Each sqi-server on the same subnet
	// should use a distinct name. Must not be empty when Enabled is true.
	InstanceName string

	// HTTPAddr is the server's HTTP listen address ("host:port"). The port is
	// extracted and advertised as the SRV record port.
	HTTPAddr string

	// NATSAddr is the embedded NATS listen address ("host:port"). The port is
	// extracted and advertised in the "nats" TXT record.
	NATSAddr string

	// InstanceID uniquely identifies this server process. When empty, [New]
	// generates a random UUID. Advertised in the "id" TXT record.
	InstanceID string

	// HTTPTLS reports whether the REST/WebSocket listener is TLS-terminated,
	// advertised as the "tls" TXT record.
	//
	// Consumed by internal/worker/discovery, which surfaces it as
	// Result.HTTPTLS. Enrollment never uses a discovered URL, so a worker uses
	// this to warn that its http:// nats.server_url points at an HTTPS server
	// rather than to rewrite anything.
	HTTPTLS bool

	// NATSTLS reports whether the embedded broker requires TLS, advertised as
	// the "nats_tls" TXT record.
	//
	// This is the one signal that can tell a worker its broker needs TLS
	// without anything being configured locally, so it feeds
	// nats.tls_enabled's "auto" mode directly: a discovering worker turns TLS
	// on rather than attempting plaintext and failing with an error that names
	// neither the cause nor the fix.
	NATSTLS bool
}

// Responder owns the lifecycle of the mDNS service advertisement. Create one
// with [New], then call [Responder.Start] and [Responder.Shutdown].
type Responder struct {
	cfg      Config
	logger   *slog.Logger
	httpPort int
	natsPort int

	server *zeroconf.Server // nil until Start succeeds; nil when disabled
}

// New validates cfg and returns a [Responder]. It parses the HTTP and NATS
// ports up front so configuration errors surface before the server boots.
// An empty [Config.InstanceID] is filled with a freshly generated UUID.
//
// When cfg.Enabled is false, port parsing is skipped and the returned
// responder's Start is a no-op.
func New(cfg Config, logger *slog.Logger) (*Responder, error) {
	r := &Responder{cfg: cfg, logger: logger}

	if !cfg.Enabled {
		return r, nil
	}

	if cfg.InstanceName == "" {
		return nil, errors.New("discovery: instance name must not be empty")
	}

	httpPort, err := portFromAddr(cfg.HTTPAddr)
	if err != nil {
		return nil, err
	}
	natsPort, err := portFromAddr(cfg.NATSAddr)
	if err != nil {
		return nil, err
	}
	r.httpPort = httpPort
	r.natsPort = natsPort

	if r.cfg.InstanceID == "" {
		r.cfg.InstanceID = uuid.NewString()
	}

	return r, nil
}

// Start publishes the mDNS service record on all multicast-capable interfaces.
// It returns nil immediately when discovery is disabled. The advertisement
// runs in background goroutines managed by the underlying responder until
// [Responder.Shutdown] is called.
//
// The ctx argument is accepted for symmetry with other components and to allow
// future cancellation support; the current zeroconf registration is
// non-blocking and does not observe it.
func (r *Responder) Start(ctx context.Context) error {
	if !r.cfg.Enabled {
		r.logger.InfoContext(ctx, "discovery: mDNS disabled by configuration")
		return nil
	}

	txt := buildTXTRecords(r.cfg.InstanceID, r.httpPort, r.natsPort, hostname(), version.Version, r.cfg.HTTPTLS, r.cfg.NATSTLS)

	server, err := zeroconf.Register(
		r.cfg.InstanceName,
		serviceType,
		domain,
		r.httpPort,
		txt,
		nil, // nil interfaces = advertise on all multicast-capable interfaces
	)
	if err != nil {
		return fmt.Errorf("discovery: register mDNS service: %w", err)
	}
	r.server = server

	r.logger.InfoContext(
		ctx, "discovery: mDNS service advertised",
		slog.String("instance", r.cfg.InstanceName),
		slog.String("service", serviceType),
		slog.Int("http_port", r.httpPort),
		slog.Int("nats_port", r.natsPort),
		slog.String("id", r.cfg.InstanceID),
	)
	return nil
}

// Shutdown stops the mDNS advertisement, sending goodbye packets so browsers
// remove the service promptly. It is safe to call when Start was never called
// or discovery is disabled.
//
// Start and Shutdown are not safe for concurrent use; the server lifecycle
// calls them sequentially (Start during boot, Shutdown during graceful stop).
func (r *Responder) Shutdown() {
	if r.server == nil {
		return
	}
	r.server.Shutdown()
	r.server = nil
	r.logger.InfoContext(context.Background(), "discovery: mDNS advertisement stopped")
}

// portFromAddr extracts the numeric port from a "host:port" address.
func portFromAddr(addr string) (int, error) {
	if addr == "" {
		return 0, errors.New("discovery: address must not be empty")
	}
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return 0, fmt.Errorf("discovery: parse address %q: %w", addr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return 0, fmt.Errorf("discovery: invalid port in %q: %w", addr, err)
	}
	if port < 1 || port > 65535 {
		return 0, fmt.Errorf("discovery: port %q out of range [1,65535]", addr)
	}
	return port, nil
}

// buildTXTRecords assembles the DNS-SD TXT key=value records advertised
// alongside the service. Order is stable for deterministic tests.
func buildTXTRecords(instanceID string, httpPort, natsPort int, host, ver string, httpTLS, natsTLS bool) []string {
	txt := []string{
		"id=" + instanceID,
		"http=" + strconv.Itoa(httpPort),
		"nats=" + strconv.Itoa(natsPort),
		"host=" + host,
		"version=" + ver,
	}
	// The TLS keys are OMITTED rather than emitted as "tls=0" when off, so a
	// plaintext farm advertises byte-for-byte what it always has. Clients that
	// predate these keys ignore unknown keys, so adding them is compatible in
	// both directions.
	if httpTLS {
		txt = append(txt, "tls=1")
	}
	if natsTLS {
		txt = append(txt, "nats_tls=1")
	}
	return txt
}

// hostname returns the host's reported name, or "unknown" if it cannot be
// determined. A missing hostname is not fatal to advertisement.
func hostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "unknown"
	}
	return h
}
