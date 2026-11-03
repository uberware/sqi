// SPDX-License-Identifier: AGPL-3.0-or-later

// Package discovery advertises the running sqi-server on the local network via
// multicast DNS (mDNS / DNS-SD) so that workers and the sqi CLI can find it
// without manual address configuration.
//
// The server publishes a single "_sqi._tcp" service instance whose SRV record
// points at the HTTP API port — the canonical, externally-bound sqi endpoint.
// Additional connection details, including the broker URL, are carried in TXT
// records:
//
//	id=<instance-id>   unique per process; lets clients dedupe re-announcements
//	http=<http-port>   REST + WebSocket API port (also the SRV port)
//	nats_url=<url>     broker URL for worker connections; the nats:// or tls://
//	                   scheme says whether the broker requires TLS
//	host=<hostname>    advertising host's reported name
//	version=<version>  server build version
//	tls=1              present only when the REST API is TLS-terminated
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
	"strings"

	"github.com/google/uuid"
	"github.com/grandcat/zeroconf"

	"github.com/uberware/sqi/internal/redact"
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

	// NATSURL is the broker URL workers should connect to, advertised as the
	// "nats_url" TXT record. Its nats:// or tls:// scheme is how a discovering
	// worker learns whether the broker requires TLS. Must not be empty when
	// Enabled is true.
	NATSURL string

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

	// Interfaces restricts which network interfaces the service is advertised
	// on. Nil — the production value — advertises on every multicast-capable
	// interface, which is the point of mDNS discovery.
	//
	// It is set by tests, to loopback, so a test run never transmits an
	// advertisement onto the network it happens to be running on. It has no
	// config key: an operator who wants to limit advertisement should say so
	// through a real setting, and none exists yet.
	Interfaces []net.Interface
}

// Responder owns the lifecycle of the mDNS service advertisement. Create one
// with [New], then call [Responder.Start] and [Responder.Shutdown].
type Responder struct {
	cfg      Config
	logger   *slog.Logger
	httpPort int

	server *zeroconf.Server // nil until Start succeeds; nil when disabled
}

// New validates cfg and returns a [Responder]. It parses the HTTP port and
// checks the broker URL up front so configuration errors surface before the
// server boots.
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
	if err := validateNATSURL(cfg.NATSURL); err != nil {
		return nil, err
	}
	r.httpPort = httpPort

	if r.cfg.InstanceID == "" {
		r.cfg.InstanceID = uuid.NewString()
	}

	return r, nil
}

// validateNATSURL applies [CheckNATSURL] as a backstop; configuration
// validation reports the same problems to the operator at load.
func validateNATSURL(raw string) error {
	if raw == "" {
		return errors.New("discovery: broker URL must not be empty")
	}
	if _, err := CheckNATSURL(raw); err != nil {
		if errors.Is(err, ErrNATSURLTooLong) {
			return fmt.Errorf("discovery: broker URL makes a %d-byte %s record, over the %d-byte DNS TXT limit: %w",
				NATSURLRecordLen(raw), NATSURLKey, MaxTXTString, err)
		}
		return fmt.Errorf("discovery: broker URL %q %w", redact.URL(raw), err)
	}
	return nil
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

	txt := buildTXTRecords(r.cfg.InstanceID, r.httpPort, r.cfg.NATSURL, hostname(), version.Version, r.cfg.HTTPTLS)

	server, err := zeroconf.Register(
		r.cfg.InstanceName,
		serviceType,
		domain,
		r.httpPort,
		txt,
		r.cfg.Interfaces, // nil = every multicast-capable interface
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
		slog.String("nats_url", redact.URL(r.cfg.NATSURL)),
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
func buildTXTRecords(instanceID string, httpPort int, natsURL, host, ver string, httpTLS bool) []string {
	txt := []string{
		"id=" + instanceID,
		"http=" + strconv.Itoa(httpPort),
		NATSURLKey + "=" + natsURL,
		"host=" + host,
		"version=" + ver,
	}
	// The tls key is omitted rather than emitted as "tls=0" when off, so a
	// plaintext farm advertises nothing extra. The broker's TLS needs no key:
	// the nats_url scheme carries it.
	if httpTLS {
		txt = append(txt, "tls=1")
	}
	return txt
}

// SRVHost returns the host the service's SRV record names: the machine's
// hostname in the ".local" domain, which is how the underlying responder
// forms it. The server uses it as the host of a derived broker URL, so that
// URL names the same host as the SRV record and a discovering worker can fall
// back to the advertised address when that name does not resolve.
func SRVHost() string {
	return hostname() + "." + strings.TrimSuffix(domain, ".")
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
