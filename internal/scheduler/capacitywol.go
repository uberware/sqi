// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

// capacitywol.go is the wake_on_lan capacity provider. It wakes a named
// group's members with magic packets and retires them with a sleep or
// shutdown command on their control subject.
//
// Packets do not cross subnets, so a wake goes first through a relay: an
// awake worker of the same farm with an interface on the target's network,
// asked to broadcast the packets there. When there is none, or it does not
// accept, the server sends them itself to the limited broadcast, the target's
// directed broadcast and every configured send_to endpoint.

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"sync"
	"time"

	nats "github.com/nats-io/nats.go"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/wol"
	"github.com/uberware/sqi/internal/worker/protocol"
)

const (
	// controlRequestTimeout bounds a sleep or shutdown request.
	controlRequestTimeout = 10 * time.Second
	// relayRequestTimeout bounds a relay's answer to a wake request.
	relayRequestTimeout = 5 * time.Second
	// limitedBroadcast is the IPv4 limited broadcast address.
	limitedBroadcast = "255.255.255.255"
)

// noWakeAddress is the result message for a member with nothing to address a
// magic packet to.
const noWakeAddress = "no wake-on-LAN address recorded; the worker must register once while awake"

// wolProvider is the wake_on_lan provider.
type wolProvider struct {
	port   int
	sendTo []string
	// send sends magic packets from the server and reports how many targets
	// it reached.
	send func(ctx context.Context, macs []net.HardwareAddr, targets []string) (int, error)
	// request sends a control request to a worker and returns its reply.
	request func(ctx context.Context, workerID string, data []byte) ([]byte, error)
	// relays returns the workers that may relay a wake: the online workers
	// the scheduler sees. pickRelay applies the rest of the rule.
	relays func(ctx context.Context) ([]store.Worker, error)
	// logger receives the targets a send from the server could not reach.
	logger *slog.Logger
}

// newWOLProvider returns the wake_on_lan provider over the scheduler's
// configuration and bus.
func (s *Scheduler) newWOLProvider() *wolProvider {
	return &wolProvider{
		port:    s.cfg.WakeOnLANPort,
		sendTo:  s.cfg.WakeOnLANSendTo,
		send:    s.cfg.SendWakePackets,
		request: s.bus.RequestWorkerControl,
		relays:  s.onlineWorkers,
		logger:  s.logger,
	}
}

// wake wakes each of members, concurrently, and returns each one's result.
// A relay that cannot be listed leaves the server to send the packets itself.
func (p *wolProvider) wake(ctx context.Context, _ store.CapacityGroup, members []store.Worker) map[string]actionResult {
	relays, err := p.relays(ctx)
	if err != nil {
		relays = nil
	}
	return eachMember(members, func(w store.Worker) actionResult { return p.wakeOne(ctx, w, relays) })
}

// wakeOne wakes w, through a relay when one accepts and from the server
// otherwise.
func (p *wolProvider) wakeOne(ctx context.Context, w store.Worker, relays []store.Worker) actionResult {
	macs, target, ok := wakeAddresses(w)
	if len(macs) == 0 || !ok {
		return actionResult{Result: resultFailed, Message: noWakeAddress}
	}
	limited := net.JoinHostPort(limitedBroadcast, strconv.Itoa(p.port))
	var directed string
	if ip, ok := wol.DirectedBroadcast(target.String()); ok {
		directed = net.JoinHostPort(ip.String(), strconv.Itoa(p.port))
	}

	if relay, ok := pickRelay(w, target.Addr(), relays); ok {
		targets := nonEmpty(directed, limited)
		if p.relayWake(ctx, relay.ID, macs, targets) {
			return actionResult{Result: resultOK, Message: "magic packets sent through relay " + relay.ID}
		}
	}

	targets := append(nonEmpty(limited, directed), p.sendTo...)
	reached, err := p.send(ctx, macs, targets)
	if err != nil && p.logger != nil {
		p.logger.WarnContext(ctx, "scheduler: wake-on-LAN: magic packets could not be sent to some targets",
			slog.String("worker_id", w.ID), slog.Int("reached", reached), slog.Int("targets", len(targets)),
			slog.Any("error", err))
	}
	// A sleeping NIC needs one packet to arrive, so one target reached is a
	// wake sent: a host with no route for the limited broadcast, say, still
	// reaches the directed broadcast or a send_to relay.
	switch {
	case reached == 0:
		return actionResult{
			Result:  resultFailed,
			Message: fmt.Sprintf("send magic packets: none of the %d targets could be reached", len(targets)),
		}
	case reached < len(targets):
		return actionResult{
			Result:  resultOK,
			Message: fmt.Sprintf("magic packets sent from the server to %d of %d targets", reached, len(targets)),
		}
	default:
		return actionResult{Result: resultOK, Message: "magic packets sent from the server"}
	}
}

// relayWake asks relayID to send magic packets for macs to targets and
// reports whether it accepted within relayRequestTimeout.
func (p *wolProvider) relayWake(ctx context.Context, relayID string, macs []net.HardwareAddr, targets []string) bool {
	msg := protocol.ControlMsg{
		Version: protocol.ProtocolVersion, Type: protocol.TypeControl, Command: protocol.ControlWake,
		Targets: targets,
	}
	for _, m := range macs {
		msg.MACs = append(msg.MACs, m.String())
	}
	reply, err := p.control(ctx, relayID, msg, relayRequestTimeout)
	return err == nil && reply.Accepted
}

// grow is unsupported: a wake_on_lan group is named.
func (*wolProvider) grow(context.Context, store.CapacityGroup, int, int) actionResult {
	return actionResult{Result: resultUnsupported, Message: "wake_on_lan wakes named members; it has no grow"}
}

// retireInstances is unsupported: a wake_on_lan group has no instances that
// never registered.
func (*wolProvider) retireInstances(context.Context, store.CapacityGroup, []string) actionResult {
	return actionResult{Result: resultUnsupported, Message: "wake_on_lan retires named members only"}
}

// list is unsupported: a wake_on_lan group's machines are its members.
func (*wolProvider) list(context.Context, store.CapacityGroup) ([]string, actionResult) {
	return nil, actionResult{Result: resultUnsupported, Message: "wake_on_lan has no list"}
}

// retire sends each of members the group's retire action, concurrently, and
// returns what each one answered.
func (p *wolProvider) retire(ctx context.Context, g store.CapacityGroup, members []store.Worker) map[string]actionResult {
	cmd := protocol.ControlCommand(g.RetireAction)
	return eachMember(members, func(w store.Worker) actionResult {
		msg := protocol.ControlMsg{Version: protocol.ProtocolVersion, Type: protocol.TypeControl, Command: cmd}
		reply, err := p.control(ctx, w.ID, msg, controlRequestTimeout)
		return retireResult(cmd, reply, err)
	})
}

// retireResult is the result of a sleep or shutdown request that returned
// reply and err.
func retireResult(cmd protocol.ControlCommand, reply protocol.ControlReplyMsg, err error) actionResult {
	var garbled *unreadableReplyError
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, nats.ErrTimeout):
		return actionResult{Result: resultTimeout, Message: fmt.Sprintf("no reply to the %s request within %s", cmd, controlRequestTimeout)}
	case errors.As(err, &garbled):
		return actionResult{Result: resultFailed, Message: err.Error()}
	case errors.Is(err, nats.ErrNoResponders):
		// No worker is subscribed to the control subject, so nothing was
		// delivered. Whether that ends the retirement depends on whether it
		// was the first request (see Scheduler.applyRetireResults).
		return actionResult{
			Result: resultFailed, Message: fmt.Sprintf("%s request failed: %v", cmd, err), undelivered: true,
		}
	case err != nil:
		return actionResult{Result: resultFailed, Message: fmt.Sprintf("%s request failed: %v", cmd, err)}
	case reply.Accepted:
		return actionResult{Result: resultOK, Message: string(cmd) + " accepted"}
	case reply.Reason == protocol.ControlRefusalAlreadyRetiring:
		// The request is absolute, so this refusal means the command is under
		// way: a repeat after a failover is answered so.
		return actionResult{Result: resultOK, Message: string(cmd) + " already under way"}
	default:
		return actionResult{Result: resultRefused, Message: "worker refused: " + reply.Reason}
	}
}

// unreadableReplyError is a control reply that does not decode.
type unreadableReplyError struct{ err error }

func (e *unreadableReplyError) Error() string { return "unreadable reply: " + e.err.Error() }
func (e *unreadableReplyError) Unwrap() error { return e.err }

// control sends msg to workerID and decodes its reply, waiting at most
// timeout.
func (p *wolProvider) control(
	ctx context.Context, workerID string, msg protocol.ControlMsg, timeout time.Duration,
) (protocol.ControlReplyMsg, error) {
	data, err := json.Marshal(msg)
	if err != nil {
		return protocol.ControlReplyMsg{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	raw, err := p.request(ctx, workerID, data)
	if err != nil {
		return protocol.ControlReplyMsg{}, err
	}
	var reply protocol.ControlReplyMsg
	if err := json.Unmarshal(raw, &reply); err != nil {
		return protocol.ControlReplyMsg{}, &unreadableReplyError{err: err}
	}
	return reply, nil
}

// eachMember runs f for each of members concurrently and returns the results
// by worker ID.
func eachMember(members []store.Worker, f func(store.Worker) actionResult) map[string]actionResult {
	out := make(map[string]actionResult, len(members))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, w := range members {
		wg.Go(func() {
			r := f(w)
			mu.Lock()
			out[w.ID] = r
			mu.Unlock()
		})
	}
	wg.Wait()
	return out
}

// wakeAddresses returns what w's recorded interfaces give a wake: every
// distinct 6-byte MAC of an interface with an IPv4 address, and the target,
// the IPv4 address (with its prefix) on the interface whose address is w's
// registered IP, or else the first IPv4 address recorded. ok is false when no
// IPv4 address is recorded.
func wakeAddresses(w store.Worker) (macs []net.HardwareAddr, target netip.Prefix, ok bool) {
	// An unparsable registered IP is the invalid address, which matches no
	// interface.
	registered, err := netip.ParseAddr(w.IPAddress)
	if err != nil {
		registered = netip.Addr{}
	}
	var first netip.Prefix
	for _, iface := range w.Interfaces {
		v4 := ipv4Prefixes(iface.Addrs)
		if len(v4) == 0 {
			continue
		}
		if !first.IsValid() {
			first = v4[0]
		}
		for _, p := range v4 {
			if registered.IsValid() && p.Addr() == registered && !target.IsValid() {
				target = p
			}
		}
		mac, err := net.ParseMAC(iface.MAC)
		if err != nil || len(mac) != 6 {
			continue
		}
		if !slices.ContainsFunc(macs, func(m net.HardwareAddr) bool { return m.String() == mac.String() }) {
			macs = append(macs, mac)
		}
	}
	if !target.IsValid() {
		target = first
	}
	return macs, target, target.IsValid()
}

// ipv4Prefixes parses the IPv4 addresses among addrs, in CIDR form.
func ipv4Prefixes(addrs []string) []netip.Prefix {
	var out []netip.Prefix
	for _, a := range addrs {
		p, err := netip.ParsePrefix(a)
		if err == nil && p.Addr().Is4() {
			out = append(out, p)
		}
	}
	return out
}

// pickRelay returns the worker of candidates, lowest ID first, that can relay
// a wake for target at addr: online, enabled, awake, of target's farm, not
// target itself, with an interface whose network holds addr.
func pickRelay(target store.Worker, addr netip.Addr, candidates []store.Worker) (store.Worker, bool) {
	sorted := slices.SortedFunc(slices.Values(candidates), func(a, b store.Worker) int { return cmp.Compare(a.ID, b.ID) })
	for _, w := range sorted {
		if w.ID == target.ID || w.FarmID != target.FarmID || w.Status != store.WorkerStatusOnline || w.Disabled ||
			w.EffectivePowerState() != store.PowerAwake {
			continue
		}
		for _, iface := range w.Interfaces {
			if slices.ContainsFunc(ipv4Prefixes(iface.Addrs), func(p netip.Prefix) bool { return p.Masked().Contains(addr) }) {
				return w, true
			}
		}
	}
	return store.Worker{}, false
}

// nonEmpty returns the non-empty strings of s, in order.
func nonEmpty(s ...string) []string {
	out := make([]string, 0, len(s))
	for _, v := range s {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}
