// SPDX-License-Identifier: AGPL-3.0-or-later

package bus

import (
	"context"
	"errors"
	"fmt"
	"net"

	natsserver "github.com/nats-io/nats-server/v2/server"
)

// stageStopped is the restart stage between stopping the old server and
// starting the new one.
const stageStopped = "stopped"

// errNoServer is what an in-process dial gets while no server is current:
// during a restart, or after one that failed to start. nats.go retries it.
var errNoServer = errors.New("bus: the embedded broker is restarting")

var _ interface {
	InProcessConn() (net.Conn, error)
} = (*Embedded)(nil)

// InProcessConn dials the current server in-process. Every in-process
// connection to the embedded broker dials through it rather than through one
// server, so when the broker restarts nats.go's reconnect lands on the new
// server.
func (e *Embedded) InProcessConn() (net.Conn, error) {
	e.mu.Lock()
	ns := e.ns
	e.mu.Unlock()
	if ns == nil {
		return nil, errNoServer
	}
	return ns.InProcessConn()
}

// noteBreak hands a stream break nats-server logged to the recovery loop. It
// never blocks the logger: one pending break is enough to wake the loop.
func (e *Embedded) noteBreak(b streamBreak) {
	select {
	case e.breakCh <- b:
	default:
	}
}

// boundPortOf is the port ns listens on, or fallback when it cannot be read.
func boundPortOf(ns *natsserver.Server, fallback int) int {
	if a, ok := ns.Addr().(*net.TCPAddr); ok && a.Port > 0 {
		return a.Port
	}
	return fallback
}

// abandonStart undoes a Start that got as far as making ns current.
func (e *Embedded) abandonStart(ns *natsserver.Server, nl *natsLogger) {
	e.mu.Lock()
	e.ns, e.natsLog = nil, nil
	e.mu.Unlock()
	stopServer(ns)
	nl.close()
}

func (e *Embedded) stage(name string) {
	if e.stageHook != nil {
		e.stageHook(name)
	}
}

// restart stops the current server and starts a new one on the same data
// directory, port, account layout and keys. nats-server keeps a stream's
// write error until the stream's store is reopened, and a new server reopens
// every store from the data directory with nothing stored lost. Clients are
// disconnected and reconnect: workers to the same address, sqi's own
// connections through [Embedded.InProcessConn].
//
// The options are built afresh: a server's options hold the accounts it
// registered, which a second server cannot share. When the new server fails
// to start there is no current server; restart returns the error and a later
// restart may succeed.
func (e *Embedded) restart(ctx context.Context) error {
	e.restartMu.Lock()
	defer e.restartMu.Unlock()

	e.mu.Lock()
	if !e.started || e.stopped {
		e.mu.Unlock()
		return errors.New("bus: restart: the broker is not running")
	}
	old, oldLog, layout, port := e.ns, e.natsLog, e.layout, e.port
	e.ns, e.natsLog = nil, nil
	e.mu.Unlock()

	if old != nil {
		stopServer(old)
		oldLog.close()
	}
	e.stage(stageStopped)

	opts, err := e.serverOptions(layout)
	if err != nil {
		return fmt.Errorf("bus: restart: %w", err)
	}
	opts.Port = port
	ns, nl, err := e.startServer(ctx, opts, layout)
	if err != nil {
		return fmt.Errorf("bus: restart: %w", err)
	}
	e.mu.Lock()
	e.ns, e.natsLog = ns, nl
	e.mu.Unlock()
	return nil
}
