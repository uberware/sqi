// SPDX-License-Identifier: AGPL-3.0-or-later

package simworker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/nats-io/nats.go"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// refusalSimulated is the reason a simulated worker refuses every control
// command but reregister with: it has no machine to sleep, shut down or send
// wake packets from.
const refusalSimulated = "simulated worker: power commands are not supported"

// refusalStopping is the reason a reregister is refused once the responder is
// being stopped.
const refusalStopping = "simulated worker: stopping"

// controlResponder answers the control requests on worker.control.<workerID>
// for one simulated worker process, as a real worker's power controller does
// for the one command a simulated worker can carry out: reregister. It accepts
// that, replies, and then registers through the real registrar; every other
// command is refused.
//
// It does not use internal/worker/power, whose controller accepts a wake and
// then sends real Wake-on-LAN broadcasts; a simulated worker must refuse one,
// or the server would count its own packets as sent through a relay that sent
// nothing.
type controlResponder struct {
	ctx      context.Context
	register func(context.Context) error
	logger   *slog.Logger

	// registering is set while a re-registration runs, so a second request
	// arriving meanwhile is accepted but starts no second one: the server asks
	// a worker at most once per heartbeat timeout, and one registration
	// answers every request made before it lands.
	registering atomic.Bool
	wg          sync.WaitGroup

	// mu orders accepting a reregister (which counts a registration in wg)
	// against stop. stopped is set under it, and once set no registration is
	// counted, so wg.Wait never races an Add.
	mu      sync.Mutex
	stopped bool

	// sub is the control subscription, set by subscribeControl.
	sub *nats.Subscription
}

// subscribeControl subscribes r to workerID's control subject on nc and
// flushes, so the server has the subscription before the worker first asks for
// work, as power.Subscribe does for a real worker. r keeps the subscription
// for [controlResponder.stop].
func subscribeControl(nc *nats.Conn, workerID string, r *controlResponder) error {
	subj := bus.WorkerControlSubject(workerID)
	sub, err := nc.Subscribe(subj, r.handle)
	if err != nil {
		return fmt.Errorf("simworker: subscribe %q: %w", subj, err)
	}
	if err := nc.Flush(); err != nil {
		if uerr := sub.Unsubscribe(); uerr != nil {
			r.logger.WarnContext(r.ctx, "simworker: unsubscribe after failed flush", slog.Any("error", uerr))
		}
		return fmt.Errorf("simworker: flush subscription %q: %w", subj, err)
	}
	r.sub = sub
	return nil
}

// handle answers one control request on the subscription's goroutine. An
// accepted reregister is counted in wg before the reply goes out, so a caller
// that stops the responder on seeing the reply waits for the registration, and
// the registration starts whether or not the reply could be sent. It runs on a
// goroutine of its own, so a slow broker holds up neither the reply nor the
// next request. Once the responder is stopped a reregister is refused.
func (r *controlResponder) handle(m *nats.Msg) {
	reply, accepted := r.decide(m.Data)
	launch := false
	if accepted {
		r.mu.Lock()
		switch {
		case r.stopped:
			reply = protocol.ControlReplyMsg{Reason: refusalStopping}
		case r.registering.CompareAndSwap(false, true):
			r.wg.Add(1)
			launch = true
		}
		r.mu.Unlock()
	}
	data, err := json.Marshal(reply)
	if err == nil {
		err = m.Respond(data)
	}
	if err != nil {
		r.logger.WarnContext(r.ctx, "simworker: could not answer a control request", slog.Any("error", err))
	}
	if !launch {
		return
	}
	go func() {
		defer r.wg.Done()
		defer r.registering.Store(false)
		if err := r.register(r.ctx); err != nil {
			r.logger.DebugContext(r.ctx, "simworker: re-registration the server asked for failed", slog.Any("error", err))
		}
	}()
}

// decide returns the reply to data and whether it accepts a reregister.
func (*controlResponder) decide(data []byte) (protocol.ControlReplyMsg, bool) {
	var msg protocol.ControlMsg
	if err := json.Unmarshal(data, &msg); err != nil {
		return protocol.ControlReplyMsg{Reason: fmt.Sprintf("malformed control message: %v", err)}, false
	}
	switch {
	case msg.Version != protocol.ProtocolVersion:
		return protocol.ControlReplyMsg{
			Reason: fmt.Sprintf("unsupported protocol version %q, want %q", msg.Version, protocol.ProtocolVersion),
		}, false
	case msg.Type != protocol.TypeControl:
		return protocol.ControlReplyMsg{Reason: fmt.Sprintf("unexpected message type %q", msg.Type)}, false
	case msg.Command == protocol.ControlReregister:
		return protocol.ControlReplyMsg{Accepted: true}, true
	default:
		return protocol.ControlReplyMsg{Reason: refusalSimulated}, false
	}
}

// stop refuses further reregisters, unsubscribes from the control subject,
// then waits for every registration already accepted. A request handler still
// running when the unsubscribe returns either counted its registration before
// stop took the lock, and is waited for, or finds the responder stopped.
func (r *controlResponder) stop() {
	r.mu.Lock()
	r.stopped = true
	r.mu.Unlock()
	if r.sub != nil {
		if err := r.sub.Unsubscribe(); err != nil && !errors.Is(err, nats.ErrConnectionClosed) {
			r.logger.WarnContext(r.ctx, "simworker: unsubscribe from the control subject", slog.Any("error", err))
		}
	}
	r.wg.Wait()
}
