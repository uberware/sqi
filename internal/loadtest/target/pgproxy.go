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
	if m.settings.Spec.Store != "postgres" || !hasDatabaseFault(m.cfg.Scenario) {
		return nil
	}
	cfg, err := pgconn.ParseConfig(m.pgDSN)
	if err != nil {
		return fmt.Errorf("target: PostgreSQL DSN %s: %w", pgdsn.Redact(m.pgDSN), err)
	}
	if strings.HasPrefix(cfg.Host, "/") {
		return errors.New("target: a database fault needs a TCP PostgreSQL connection, and the DSN names a unix socket")
	}
	p, err := netfault.Listen(net.JoinHostPort(cfg.Host, strconv.Itoa(int(cfg.Port))), laneSeed(m.cfg.Seed, dbProxyLane))
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
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("target: proxy address %q: %w", addr, err)
	}
	out := dsn + " host=" + host + " port=" + port
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return "", fmt.Errorf("target: PostgreSQL DSN %s: %w", pgdsn.Redact(dsn), err)
		}
		u.Host = addr
		q := u.Query()
		if q.Has("host") || q.Has("port") {
			q.Del("host")
			q.Del("port")
			u.RawQuery = q.Encode()
		}
		out = u.String()
	}
	got, err := pgconn.ParseConfig(out)
	if err != nil || got.Host != host || strconv.Itoa(int(got.Port)) != port {
		return "", fmt.Errorf("target: cannot point PostgreSQL DSN %s at %s", pgdsn.Redact(dsn), addr)
	}
	return out, nil
}
