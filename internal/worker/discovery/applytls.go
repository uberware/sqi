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
// broker. A discovered tls:// URL makes nats.go use TLS on its own, but only
// with the system roots; folding it into the configuration is what applies the
// worker's own tls_* settings and lets this explain a certificate failure that
// would otherwise name neither the cause nor the setting that fixes it.
//
// An explicit nats.url discovers nothing, so found's TLS fields are false and
// this is a no-op: an operator who configures the URL by hand also configures
// the transport by hand.
//
// On trusting an advertisement at all: the signal cannot be used to weaken the
// connection. Forging a tls:// nats_url upgrades a worker to TLS; withholding it
// leaves the worker attempting plaintext against a TLS broker, which fails
// closed. Any locally configured tls_* setting still wins, so an advertisement
// can never downgrade a worker that was configured for TLS. And an attacker
// able to forge mDNS already controls NATSURL entirely, so the tls:// scheme
// adds no surface that was not already there.
func ApplyTLS(ctx context.Context, cfg *workerconfig.NATSConfig, found Result, logger *slog.Logger) {
	switch {
	case found.NATSTLS && cfg.TLSEnabled == workerconfig.TLSOff:
		// The operator set nats.tls_enabled to "false" but the advertised URL
		// is tls://, and nats.go requires TLS for a tls:// URL whatever this
		// setting says. "false" only withholds the worker's own TLS settings
		// (CA file, client certificate, insecure_skip_verify) from that
		// connection, so it dials with the system roots and no client
		// certificate. That succeeds only against a publicly trusted
		// certificate; say plainly why it otherwise will not, since the
		// resulting certificate error does not mention this setting.
		logger.WarnContext(ctx,
			"discovery: server advertises a tls:// broker URL, which requires TLS, but nats.tls_enabled is \"false\", "+
				"so nats.tls_ca_file, nats.tls_cert_file and nats.insecure_skip_verify are not applied "+
				"and the certificate is verified against the system roots; a farm CA will not verify — "+
				"set nats.tls_enabled to \"auto\" or \"true\"",
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
