// SPDX-License-Identifier: AGPL-3.0-or-later

package discovery

import (
	"context"
	"log/slog"
	"strings"

	workerconfig "github.com/uberware/sqi/internal/worker/config"
)

// ApplyTLS folds what mDNS learned about the server into the worker's NATS
// configuration.
//
// This is what makes nats.tls_enabled's "auto" mode able to see a TLS-required
// broker. Without it a worker that discovers its server has no way to know the
// broker needs TLS, attempts plaintext, and fails with nats.go's
// "secure connection not available" — which names neither the cause nor the
// setting that fixes it.
//
// An explicit nats.url discovers nothing, so found's TLS fields are false and
// this is a no-op: an operator who configures the URL by hand also configures
// the transport by hand.
func ApplyTLS(ctx context.Context, cfg *workerconfig.NATSConfig, found Result, logger *slog.Logger) {
	switch {
	case found.NATSTLS && cfg.TLSEnabled == workerconfig.TLSOff:
		// The operator forced plaintext against a broker that requires TLS.
		// Honor it — "false" means false — but say plainly why the connection
		// is about to fail, since the broker's own error will not mention this
		// setting.
		logger.WarnContext(ctx,
			"discovery: server advertises a TLS-required broker but nats.tls_enabled is \"false\"; "+
				"the connection will be refused — set it to \"auto\" or \"true\"",
			slog.String("url", cfg.URL))

	case found.NATSTLS && !cfg.UseTLS(false):
		// "auto" with nothing configured locally. The advertisement is the
		// reason to use TLS, so use it. With no CA configured this verifies
		// against the system roots, which fails on a farm CA with a readable
		// certificate error naming the issuer — a far better diagnostic than
		// a plaintext attempt produces.
		logger.InfoContext(ctx,
			"discovery: server advertises a TLS-required broker; enabling TLS for this connection",
			slog.String("url", cfg.URL))
		cfg.TLSEnabled = workerconfig.TLSOn
		if cfg.TLSCAFile == "" && !cfg.InsecureSkipVerify {
			logger.WarnContext(ctx,
				"discovery: no nats.tls_ca_file is configured, so the broker certificate is verified "+
					"against the system roots; a farm CA will not verify — copy the server's ca.crt "+
					"and set nats.tls_ca_file")
		}
	}

	// Enrollment never uses a discovered URL, so this cannot be corrected
	// automatically — but an http:// server_url against a server advertising
	// HTTPS is a misconfiguration worth naming before the request fails.
	if found.HTTPTLS && strings.HasPrefix(cfg.ServerURL, "http://") {
		logger.WarnContext(ctx,
			"discovery: server advertises HTTPS but nats.server_url is http://; "+
				"enrollment will fail — use https:// and set nats.server_tls_ca_file",
			slog.String("server_url", cfg.ServerURL))
	}
}
