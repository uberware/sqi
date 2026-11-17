// SPDX-License-Identifier: AGPL-3.0-or-later

package simworker

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/nats-io/nats.go"

	"github.com/uberware/sqi/internal/worker/power"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// controlResponder answers the control requests on worker.control.<workerID>
// for one simulated worker process through the real worker's power
// controller. A simulated worker carries out one command, reregister, which
// registers it through the real registrar. Sleep and shutdown are refused as
// on a worker without worker.power.allow_remote_sleep, and wake is refused
// ([power.Config.RefuseWake]): a simulated worker has no network to send wake
// packets on, and the server would otherwise count packets as sent through a
// relay that sent nothing.
type controlResponder struct {
	ctx  context.Context
	gate reregisterGate
	sub  *nats.Subscription
}

// controlDeps are the parts of a simulated worker process its power
// controller drives.
type controlDeps struct {
	workerID   string
	busy       func() int
	deregister func(reason string, kind protocol.DeregisterKind)
	register   func(context.Context) error
	logger     *slog.Logger
}

// subscribeControl subscribes a responder for d.workerID's control subject on
// nc. It returns once the server has the subscription, as power.Subscribe
// does for a real worker. ctx ends a registration under way.
func subscribeControl(ctx context.Context, nc *nats.Conn, d controlDeps) (*controlResponder, error) {
	r := &controlResponder{ctx: ctx, gate: reregisterGate{register: d.register, logger: d.logger}}
	sub, err := power.Subscribe(ctx, nc, power.Config{RefuseWake: true}, power.Deps{
		WorkerID:   d.workerID,
		Busy:       d.busy,
		Deregister: d.deregister,
		Reregister: r.gate.reregister,
		Logger:     d.logger,
	})
	if err != nil {
		return nil, err
	}
	r.sub = sub
	return r, nil
}

// stop unsubscribes from the control subject, then waits for a registration
// still running. No registration starts once it has begun: a request the
// controller accepted before the unsubscribe and has not yet acted on finds
// the gate stopped.
func (r *controlResponder) stop() {
	if err := r.sub.Unsubscribe(); err != nil && !errors.Is(err, nats.ErrConnectionClosed) {
		r.gate.logger.WarnContext(r.ctx, "simworker: unsubscribe from the control subject", slog.Any("error", err))
	}
	r.gate.stop()
}

// reregisterGate runs the registrations a simulated worker's power controller
// asks for: at most one at a time, since the server asks a worker at most once
// per heartbeat timeout and one registration answers every request made before
// it lands, and none once stopped. A failed registration is logged at debug
// level, not reported to the controller, which would log it as an error: a
// fleet of simulated workers torn down mid-registration would flood the log.
type reregisterGate struct {
	register func(context.Context) error
	logger   *slog.Logger

	// mu orders starting a registration (counted in wg) against stop.
	mu      sync.Mutex
	running bool
	stopped bool
	wg      sync.WaitGroup
}

// reregister registers through g.register, unless a registration is already
// running or g is stopped, in which case it returns at once. It always
// returns nil.
func (g *reregisterGate) reregister(ctx context.Context) error {
	g.mu.Lock()
	if g.running || g.stopped {
		g.mu.Unlock()
		return nil
	}
	g.running = true
	g.wg.Add(1)
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		g.running = false
		g.mu.Unlock()
		g.wg.Done()
	}()
	if err := g.register(ctx); err != nil {
		g.logger.DebugContext(ctx, "simworker: re-registration the server asked for failed", slog.Any("error", err))
	}
	return nil
}

// stop makes every later reregister return at once, then waits for one still
// running. A registration is counted in wg only while g is not stopped, under
// mu, so the wait never races a count.
func (g *reregisterGate) stop() {
	g.mu.Lock()
	g.stopped = true
	g.mu.Unlock()
	g.wg.Wait()
}
