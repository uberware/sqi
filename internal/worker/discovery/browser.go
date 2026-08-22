// SPDX-License-Identifier: AGPL-3.0-or-later

// Package discovery provides mDNS-based sqi-server discovery for sqi-worker.
//
// At startup the worker needs the NATS URL of a running sqi-server. When an
// explicit URL is configured it is used directly. When no URL is configured
// and mDNS is enabled, [Browse] scans the local network for "_sqi._tcp"
// services — the same service type that sqi-server's internal/discovery
// package advertises — and returns the first discovered server's NATS URL.
//
// TXT record format advertised by sqi-server:
//
//	id=<instance-id>   unique per process
//	http=<http-port>   REST + WebSocket API port
//	nats=<nats-port>   NATS JetStream port for worker connections
//	host=<hostname>    advertising host's reported name
//	version=<version>  server build version
//
// The browser constructs a "nats://<host>:<nats-port>" URL from the SRV
// record's hostname and the nats TXT record value.
package discovery

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/grandcat/zeroconf"
)

const (
	// serviceType is the DNS-SD service type advertised by sqi-server.
	// Must match internal/discovery.serviceType on the server side.
	serviceType = "_sqi._tcp"

	// domain is the standard mDNS multicast domain.
	domain = "local."
)

// Result holds the information extracted from a discovered sqi-server
// mDNS announcement.
type Result struct {
	// NATSURL is a ready-to-use NATS connection URL, e.g. "nats://host:4222".
	NATSURL string

	// InstanceName is the DNS-SD service instance name of the discovered server.
	InstanceName string

	// InstanceID is the unique process ID extracted from the "id" TXT record.
	InstanceID string

	// Version is the server build version from the "version" TXT record.
	Version string

	// NATSTLS reports whether the discovered server's broker requires TLS,
	// from the "nats_tls" TXT record. A server that predates the record, or
	// one serving a plaintext broker, leaves this false.
	//
	// This is an INPUT to nats.tls_enabled's "auto" mode: it is the one signal
	// that can tell a worker the broker needs TLS without the operator having
	// configured anything locally.
	NATSTLS bool

	// HTTPTLS reports whether the discovered server's REST API is
	// TLS-terminated, from the "tls" TXT record. Enrollment does not use a
	// discovered URL (nats.server_url is always explicit), so this is used to
	// warn about an http:// server_url pointed at an https:// server rather
	// than to rewrite anything.
	HTTPTLS bool
}

// ErrDiscoveryTimeout is returned by [Browse] when the timeout expires with
// no server found and mDNS is enabled.
var ErrDiscoveryTimeout = errors.New("discovery: mDNS browse timed out with no sqi-server found")

// ErrDiscoveryDisabled is returned by [Browse] when mDNS is disabled and no
// explicit NATS URL was provided. Callers should surface a clear message.
var ErrDiscoveryDisabled = errors.New("discovery: mDNS is disabled and no explicit NATS URL is configured")

// Browse scans the local network for "_sqi._tcp" mDNS services and returns
// the first discovered server's connection details.
//
// Browse blocks until:
//   - a server is found (returns the [Result] immediately), or
//   - timeout elapses (returns [ErrDiscoveryTimeout]), or
//   - ctx is canceled (returns ctx.Err()).
//
// The timeout parameter should come from [workerconfig.DiscoveryConfig.MDNSTimeout]
// (default 5 s). Pass a zero timeout to use a 5 s default.
func Browse(ctx context.Context, timeout time.Duration, logger *slog.Logger) (Result, error) {
	return browseService(ctx, serviceType, timeout, logger)
}

// browseService is [Browse] with the DNS-SD service type as a parameter.
// Production code always browses serviceType; tests exercise the timeout path
// with a service type nothing advertises, so a sqi-server legitimately running
// on the developer's network cannot satisfy the browse and mask the timeout.
func browseService(ctx context.Context, service string, timeout time.Duration, logger *slog.Logger) (Result, error) {
	if timeout <= 0 {
		// Belt-and-suspenders: the canonical default lives in
		// workerconfig.DiscoveryConfig.MDNSTimeout (5 s). This guard protects
		// callers that invoke Browse directly with a zero timeout.
		timeout = 5 * time.Second
	}

	logger.InfoContext(
		ctx, "discovery: starting mDNS browse",
		slog.String("service", service),
		slog.Duration("timeout", timeout),
	)

	resolver, err := zeroconf.NewResolver(nil)
	if err != nil {
		return Result{}, fmt.Errorf("discovery: create mDNS resolver: %w", err)
	}

	// entries is owned by the zeroconf resolver. Do NOT close it here; the
	// resolver closes it when browseCtx is canceled (via defer cancelBrowse).
	// Closing it a second time would panic. The buffer of 8 prevents the
	// resolver's goroutines from blocking on a write after we return.
	entries := make(chan *zeroconf.ServiceEntry, 8)

	browseCtx, cancelBrowse := context.WithTimeout(ctx, timeout)
	defer cancelBrowse()

	if err := resolver.Browse(browseCtx, service, domain, entries); err != nil {
		return Result{}, fmt.Errorf("discovery: browse %s: %w", service, err)
	}

	for {
		select {
		case entry, ok := <-entries:
			if !ok {
				// Channel closed — browse ended. When the caller's context was
				// canceled, zeroconf closes the entries channel as part of its
				// own cleanup, so both this case and browseCtx.Done() become
				// ready simultaneously. Prefer the caller's cancellation error
				// so callers can detect context.Canceled / context.DeadlineExceeded
				// rather than the internal ErrDiscoveryTimeout sentinel.
				if ctx.Err() != nil {
					return Result{}, ctx.Err()
				}
				return Result{}, ErrDiscoveryTimeout
			}
			result, err := entryToResult(entry)
			if err != nil {
				logger.WarnContext(
					ctx, "discovery: skipping malformed mDNS entry",
					slog.String("instance", entry.Instance),
					slog.Any("error", err),
				)
				continue
			}
			logger.InfoContext(
				ctx, "discovery: found sqi-server via mDNS",
				slog.String("instance", result.InstanceName),
				slog.String("nats_url", result.NATSURL),
				slog.String("version", result.Version),
				slog.String("server_id", result.InstanceID),
			)
			return result, nil

		case <-browseCtx.Done():
			// browseCtx fires for both our internal timeout and an external
			// cancellation/deadline on ctx. Check ctx.Err() to distinguish:
			// non-nil means the caller canceled or hit its own deadline, so
			// propagate that error rather than returning ErrDiscoveryTimeout.
			if ctx.Err() != nil {
				return Result{}, ctx.Err()
			}
			logger.WarnContext(
				ctx, "discovery: mDNS browse timed out — no sqi-server found on local network",
				slog.Duration("timeout", timeout),
			)
			return Result{}, ErrDiscoveryTimeout
		}
	}
}

// entryToResult converts a raw zeroconf ServiceEntry into a [Result].
// It returns an error if the entry is missing required information (hostname
// or NATS port in TXT records).
func entryToResult(entry *zeroconf.ServiceEntry) (Result, error) {
	host := entry.HostName
	if host == "" {
		// Fall back to IP addresses if the SRV hostname is absent.
		switch {
		case len(entry.AddrIPv4) > 0:
			// Use To4() to get the dotted-decimal form: net.IP.String() on a
			// v4-mapped IPv6 address returns "::ffff:a.b.c.d", which is not
			// a valid host in a nats:// URL.
			ip4 := entry.AddrIPv4[0].To4()
			if ip4 == nil {
				ip4 = entry.AddrIPv4[0]
			}
			host = ip4.String()
		case len(entry.AddrIPv6) > 0:
			host = entry.AddrIPv6[0].String()
		default:
			return Result{}, errors.New("no hostname or IP address in mDNS entry")
		}
	}
	// Strip trailing dot from mDNS hostname (e.g. "myhost.local." → "myhost.local")
	host = strings.TrimSuffix(host, ".")

	txt := parseTXTRecords(entry.Text)

	natsPortStr, ok := txt["nats"]
	if !ok || natsPortStr == "" {
		return Result{}, errors.New("mDNS entry missing 'nats' TXT record")
	}
	natsPort, err := strconv.Atoi(natsPortStr)
	if err != nil || natsPort < 1 || natsPort > 65535 {
		return Result{}, fmt.Errorf("invalid nats port %q in mDNS TXT record", natsPortStr)
	}

	return Result{
		NATSURL:      "nats://" + net.JoinHostPort(host, strconv.Itoa(natsPort)),
		InstanceName: entry.Instance,
		InstanceID:   txt["id"],
		Version:      txt["version"],
		NATSTLS:      txt["nats_tls"] == "1",
		HTTPTLS:      txt["tls"] == "1",
	}, nil
}

// parseTXTRecords converts a slice of "key=value" DNS TXT strings into a map.
// Records without an "=" are stored with an empty string value.
// First occurrence wins on duplicate keys so that a well-formed record is not
// overwritten by a subsequent malformed one.
func parseTXTRecords(records []string) map[string]string {
	out := make(map[string]string, len(records))
	for _, r := range records {
		k, v, _ := strings.Cut(r, "=")
		k = strings.TrimSpace(k)
		if _, exists := out[k]; !exists {
			out[k] = strings.TrimSpace(v)
		}
	}
	return out
}

// Resolve returns the discovered server, applying the following precedence:
//
//  1. If explicitURL is non-empty, return it directly (mDNS bypassed entirely).
//  2. If mdnsEnabled is false, return an error with a clear message.
//  3. Run [Browse] and return what it found.
//
// It returns the whole [Result], not just the URL: the TLS records travel with
// it, and a caller that only took the URL would silently drop the one signal
// that can tell a worker its broker needs TLS.
//
// An explicit URL yields a Result with only NATSURL set — nothing was
// discovered, so nothing is known about the server's transport, and the TLS
// fields stay false rather than claiming otherwise.
//
// This is the single entry point that start.go calls before dialing NATS.
func Resolve(ctx context.Context, explicitURL string, mdnsEnabled bool, timeout time.Duration, logger *slog.Logger) (Result, error) {
	// Explicit URL bypasses mDNS entirely.
	if explicitURL != "" {
		logger.InfoContext(
			ctx, "discovery: using explicit NATS URL (mDNS bypassed)",
			slog.String("url", explicitURL),
		)
		return Result{NATSURL: explicitURL}, nil
	}

	// MDNS disabled — log clearly and return actionable error.
	if !mdnsEnabled {
		logger.ErrorContext(ctx, "discovery: mDNS is disabled and no explicit NATS URL is configured; "+
			"set nats.url in configuration or enable discovery.enable_mdns")
		return Result{}, ErrDiscoveryDisabled
	}

	return Browse(ctx, timeout, logger)
}
