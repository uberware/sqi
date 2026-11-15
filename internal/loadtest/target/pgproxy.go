// SPDX-License-Identifier: AGPL-3.0-or-later

package target

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/uberware/sqi/internal/loadtest/netfault"
	"github.com/uberware/sqi/internal/loadtest/scenario"
	"github.com/uberware/sqi/internal/pgdsn"
	"github.com/uberware/sqi/internal/store/backend"
)

// dbProxyLane is the lane index whose seed the database proxy's jitter takes,
// out of the range of the worker groups' indexes.
const dbProxyLane = 1000

// hasDatabaseFault reports whether the scenario holds a database fault.
func hasDatabaseFault(sc *scenario.Scenario) bool {
	return slices.ContainsFunc(sc.Faults, func(f scenario.Fault) bool { return f.Database != nil })
}

// startDatabaseProxy puts a fault proxy between sqi-server and PostgreSQL when
// the scenario has a database fault. Only the server's DSN goes through it:
// the harness's own connections (store checks, creating and dropping the run's
// database) keep the direct one, so a fault never cuts the harness off from
// the database it is measuring.
func (m *managed) startDatabaseProxy(ctx context.Context) error {
	if m.settings.Spec.Store != backend.DriverPostgres || !hasDatabaseFault(m.cfg.Scenario) {
		return nil
	}
	upstream, err := dsnUpstream(m.pgDSN)
	if err != nil {
		return err
	}
	p, err := netfault.Listen(upstream, laneSeed(m.cfg.Seed, dbProxyLane))
	if err != nil {
		return fmt.Errorf("target: start the database proxy: %w", err)
	}
	via, err := dsnVia(m.pgDSN, p.Addr())
	if err != nil {
		return errors.Join(err, p.Close())
	}
	m.dbProxy, m.serverDSN = p, via
	m.logger.InfoContext(ctx, "started the database proxy", slog.String("addr", p.Addr()))
	return nil
}

// dsnVia returns dsn with its host and port replaced by addr ("127.0.0.1:N").
// A URL DSN (postgres://, postgresql://) has its authority rewritten, and a
// host or port in its query dropped; a keyword/value DSN gets host= and port=
// appended, which pgx reads over an earlier one. A DSN naming several hosts or
// a unix socket is an error. TLS passes through the proxy untouched, so a
// sslmode that checks the server's name would see 127.0.0.1.
func dsnVia(dsn, addr string) (string, error) {
	if _, err := dsnUpstream(dsn); err != nil {
		return "", err
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("target: proxy address %q: %w", addr, err)
	}
	out, got, err := rewriteDSN(dsn, "host="+host+" port="+port, func(u *url.URL) {
		u.Host = addr
		q := u.Query()
		if q.Has("host") || q.Has("port") {
			q.Del("host")
			q.Del("port")
			u.RawQuery = q.Encode()
		}
	})
	if err != nil {
		return "", err
	}
	if got == nil || got.Host != host || strconv.Itoa(int(got.Port)) != port {
		return "", fmt.Errorf("target: cannot point PostgreSQL DSN %s at %s", pgdsn.Redact(dsn), addr)
	}
	return out, nil
}

// dsnUpstream returns the one TCP host:port dsn names, which the database
// proxy fronts. A DSN naming several hosts or a unix socket is an error.
func dsnUpstream(dsn string) (string, error) {
	cfg, err := pgconn.ParseConfig(dsn)
	if err != nil {
		return "", fmt.Errorf("target: PostgreSQL DSN %s: %w", pgdsn.Redact(dsn), err)
	}
	// Fallbacks also hold sslmode=prefer's plain-text retry of the same host.
	for _, fb := range cfg.Fallbacks {
		if fb.Host != cfg.Host || fb.Port != cfg.Port {
			return "", fmt.Errorf("target: PostgreSQL DSN %s names several hosts, which one proxy cannot front", pgdsn.Redact(dsn))
		}
	}
	if strings.HasPrefix(cfg.Host, "/") {
		return "", fmt.Errorf("target: PostgreSQL DSN %s names a unix socket; the database proxy needs TCP", pgdsn.Redact(dsn))
	}
	return net.JoinHostPort(cfg.Host, strconv.Itoa(int(cfg.Port))), nil
}
