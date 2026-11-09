// SPDX-License-Identifier: AGPL-3.0-or-later

// Package power lets sqi-server put the machine a worker runs on to sleep or
// power it off, and asks a worker to wake a peer, on behalf of a capacity
// group.
//
// The server sends a [protocol.ControlMsg] as a core-NATS request to
// worker.control.<workerID> and the worker answers with a
// [protocol.ControlReplyMsg]. Sleep and shutdown refuse unless the operator set
// worker.power.allow_remote_sleep, and always refuse while a task is running.
// Wake needs no opt-in: the worker only sends UDP broadcasts for a peer that is
// asleep and so cannot receive a command itself.
//
// # Leaving and coming back
//
// A worker that accepts a sleep or shutdown deregisters as
// [protocol.DeregisterRetired] and then runs the command. If the command fails
// the worker re-registers at once. After a sleep that succeeded it
// re-registers when [WatchResume] sees the machine wake, or 60 seconds after
// the command returned, whichever is first, so a sleep that never took effect
// cannot leave the worker out of the farm. A shutdown that succeeds ends the
// process; one whose command returned success but left the machine running
// re-registers 60 seconds after the command returned, as a sleep does.
package power

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	nats "github.com/nats-io/nats.go"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/wol"
	"github.com/uberware/sqi/internal/worker/protocol"
)

const (
	// resumeFallback is how long after a successful sleep or shutdown command
	// returned the worker re-registers when it has seen no resume, or is
	// still running.
	resumeFallback = 60 * time.Second
	// resumeSampleInterval is how often the resume watcher samples the clocks.
	resumeSampleInterval = 2 * time.Second
	// fallbackPoll is how often the fallback wait reads the clock.
	fallbackPoll = time.Second
	// maxCommandOutput bounds the command output kept for an error message.
	maxCommandOutput = 2048
)

// Deps are the worker's parts the controller drives. Busy, Deregister and
// Reregister are required; the rest default to the real thing.
type Deps struct {
	WorkerID string
	// Busy returns the number of tasks the worker is running.
	Busy func() int
	// Deregister tells the server the worker is leaving.
	Deregister func(reason string, kind protocol.DeregisterKind)
	// Reregister registers the worker again.
	Reregister func(ctx context.Context) error
	// Run runs a command to completion. The default runs it with os/exec.
	Run func(ctx context.Context, argv []string) error
	// SendWake sends magic packets for macs to targets. The default is
	// [wol.Send] with its default burst and gap.
	SendWake func(ctx context.Context, macs []net.HardwareAddr, targets []string) error
	// Now returns the time. The default is [time.Now].
	Now    func() time.Time
	Logger *slog.Logger
}

// Config is the operator's power policy.
type Config struct {
	// AllowRemoteSleep permits the server to sleep or shut down the machine.
	AllowRemoteSleep bool
	// SleepCommand and ShutdownCommand are argv lists. Empty means the
	// platform default, if there is one.
	SleepCommand, ShutdownCommand []string
}

// natsConn is the subset of [*nats.Conn] Subscribe uses.
type natsConn interface {
	Subscribe(subj string, cb nats.MsgHandler) (*nats.Subscription, error)
	Flush() error
}

// Subscribe subscribes to worker.control.<WorkerID> and answers each request
// as the package documentation describes. Like cancel.Subscribe it returns once
// the server has the subscription, so a worker that calls it before its first
// lease request never misses a command for it. ctx ends any wait the
// controller is in; it does not unsubscribe.
func Subscribe(ctx context.Context, nc natsConn, cfg Config, d Deps) (*nats.Subscription, error) {
	if d.Busy == nil || d.Deregister == nil || d.Reregister == nil {
		return nil, errors.New("power: Busy, Deregister and Reregister are required")
	}
	c := newController(ctx, cfg, d)
	subj := bus.WorkerControlSubject(d.WorkerID)
	sub, err := nc.Subscribe(subj, func(m *nats.Msg) {
		c.handle(m.Data, func(r protocol.ControlReplyMsg) error { return respond(m, r) })
	})
	if err != nil {
		return nil, fmt.Errorf("power: subscribe %q: %w", subj, err)
	}
	if err := nc.Flush(); err != nil {
		if uerr := sub.Unsubscribe(); uerr != nil {
			c.log.WarnContext(ctx, "power: unsubscribe after failed flush", slog.Any("error", uerr))
		}
		return nil, fmt.Errorf("power: flush subscription %q: %w", subj, err)
	}
	return sub, nil
}

// respond sends r as the reply to the request m.
func respond(m *nats.Msg, r protocol.ControlReplyMsg) error {
	if m.Reply == "" {
		return errors.New("the message is not a request: it has no reply subject")
	}
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return m.Respond(data)
}

// controller handles control messages for one worker.
type controller struct {
	ctx context.Context
	cfg Config
	d   Deps
	log *slog.Logger

	// retiring is set from the moment a sleep or shutdown is accepted until the
	// worker is back (after a failure or a resume). A shutdown that succeeds
	// leaves it set: the process is about to end.
	retiring atomic.Bool
	// wg tracks the goroutines the controller starts.
	wg sync.WaitGroup

	// The fields below are what tests replace.
	defaults    func() (sleep, shutdown []string)
	watchResume func(ctx context.Context, onResume func())
	pollEvery   time.Duration
}

func newController(ctx context.Context, cfg Config, d Deps) *controller {
	if d.Run == nil {
		d.Run = runCommand
	}
	if d.SendWake == nil {
		d.SendWake = func(ctx context.Context, macs []net.HardwareAddr, targets []string) error {
			_, err := wol.Send(ctx, macs, targets, wol.DefaultBurst, wol.DefaultGap)
			return err
		}
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	logger := d.Logger
	if logger == nil {
		logger = slog.Default()
	}
	c := &controller{
		ctx:       ctx,
		cfg:       cfg,
		d:         d,
		log:       logger,
		defaults:  defaultCommands,
		pollEvery: fallbackPoll,
	}
	c.watchResume = func(ctx context.Context, onResume func()) {
		WatchResume(ctx, resumeSampleInterval, c.d.Now, onResume)
	}
	return c
}

// plan is what handling one message decided: the reply, and what to do once
// the reply has been delivered.
type plan struct {
	reply protocol.ControlReplyMsg
	// act runs on its own goroutine after the reply was delivered.
	act func()
	// abandon undoes the decision when the reply could not be delivered.
	abandon func()
}

func refuse(format string, args ...any) plan {
	return plan{reply: protocol.ControlReplyMsg{Reason: fmt.Sprintf(format, args...)}}
}

// handle decides what to do with one message, replies, and only then acts. A
// command whose reply could not be delivered is not carried out: the server
// would not know the worker had accepted it.
func (c *controller) handle(data []byte, reply func(protocol.ControlReplyMsg) error) {
	p := c.decide(data)
	if p.reply.Accepted {
		c.log.InfoContext(c.ctx, "power: control command accepted", slog.String("worker_id", c.d.WorkerID))
	} else {
		c.log.WarnContext(c.ctx, "power: control command refused",
			slog.String("worker_id", c.d.WorkerID), slog.String("reason", p.reply.Reason))
	}
	if err := reply(p.reply); err != nil {
		c.log.ErrorContext(c.ctx, "power: could not send the reply; not acting on the command",
			slog.String("worker_id", c.d.WorkerID), slog.Any("error", err))
		if p.abandon != nil {
			p.abandon()
		}
		return
	}
	if p.act != nil {
		c.wg.Go(p.act)
	}
}

func (c *controller) decide(data []byte) plan {
	var msg protocol.ControlMsg
	if err := json.Unmarshal(data, &msg); err != nil {
		return refuse("malformed control message: %v", err)
	}
	if msg.Version != protocol.ProtocolVersion {
		return refuse("unsupported protocol version %q, want %q", msg.Version, protocol.ProtocolVersion)
	}
	if msg.Type != protocol.TypeControl {
		return refuse("unexpected message type %q", msg.Type)
	}
	switch msg.Command {
	case protocol.ControlSleep, protocol.ControlShutdown:
		return c.decidePower(msg.Command)
	case protocol.ControlWake:
		return c.decideWake(msg)
	default:
		return refuse("unknown command %q", msg.Command)
	}
}

// decidePower decides a sleep or shutdown.
func (c *controller) decidePower(cmd protocol.ControlCommand) plan {
	// Busy comes first and is not an opt-in matter: taking the machine down
	// kills whatever it is running.
	if c.d.Busy() > 0 {
		return refuse(protocol.ControlRefusalBusy)
	}
	if !c.cfg.AllowRemoteSleep {
		return refuse(protocol.ControlRefusalNotAllowed)
	}
	argv := c.command(cmd)
	if len(argv) == 0 {
		return refuse("no default %s command on %s", cmd, runtime.GOOS)
	}
	if !c.retiring.CompareAndSwap(false, true) {
		return refuse(protocol.ControlRefusalAlreadyRetiring)
	}
	return plan{
		reply:   protocol.ControlReplyMsg{Accepted: true},
		act:     func() { c.retire(cmd, argv) },
		abandon: func() { c.retiring.Store(false) },
	}
}

// command returns the argv for cmd: the configured one, else the platform's.
func (c *controller) command(cmd protocol.ControlCommand) []string {
	sleepDefault, shutdownDefault := c.defaults()
	if cmd == protocol.ControlSleep {
		if len(c.cfg.SleepCommand) > 0 {
			return c.cfg.SleepCommand
		}
		return sleepDefault
	}
	if len(c.cfg.ShutdownCommand) > 0 {
		return c.cfg.ShutdownCommand
	}
	return shutdownDefault
}

// retire is the work after an accepted sleep or shutdown: leave the farm, run
// the command, and come back once the machine resumed, or when it is still
// running after resumeFallback.
func (c *controller) retire(cmd protocol.ControlCommand, argv []string) {
	ctx := c.ctx

	// Watch for the resume before the command runs: "systemctl suspend" blocks
	// across the suspend, so the machine may be awake again before it returns.
	var resumed atomic.Bool
	resumeCh := make(chan struct{})
	watchCtx, stopWatch := context.WithCancel(ctx)
	defer stopWatch()
	if cmd == protocol.ControlSleep {
		c.wg.Go(func() {
			c.watchResume(watchCtx, func() {
				if resumed.CompareAndSwap(false, true) {
					close(resumeCh)
				}
			})
		})
	}

	c.d.Deregister("capacity group retirement: "+string(cmd), protocol.DeregisterRetired)

	// A lease may have landed between accepting and now. A task started now
	// would be killed by the command, so stay.
	if n := c.d.Busy(); n > 0 {
		c.log.WarnContext(ctx, "power: a task arrived after the command was accepted; not running it",
			slog.String("command", string(cmd)), slog.Int("active_tasks", n))
		c.comeBack(ctx)
		return
	}

	c.log.InfoContext(ctx, "power: running the retirement command",
		slog.String("command", string(cmd)), slog.String("program", argv[0]))
	if err := c.d.Run(ctx, argv); err != nil {
		c.log.ErrorContext(ctx, "power: the retirement command failed; re-registering",
			slog.String("command", string(cmd)), slog.String("program", argv[0]), slog.Any("error", err))
		c.comeBack(ctx)
		return
	}
	// After a shutdown the operating system stops the process during the
	// wait, which never sees a resume: only a shutdown that did not take
	// effect reaches its end.
	if c.awaitResume(ctx, cmd, resumeCh) {
		c.comeBack(ctx)
	}
}

// awaitResume waits, after cmd returned success, until the machine resumed or
// resumeFallback has passed on the wall clock since now. It reports false when
// ctx ended first, in which case the worker is shutting down and registering
// again would be wrong.
func (c *controller) awaitResume(ctx context.Context, cmd protocol.ControlCommand, resumed <-chan struct{}) bool {
	// Round(0) compares wall clocks: the monotonic reading stops during a
	// suspend, and the fallback must not stop with it.
	deadline := c.d.Now().Round(0).Add(resumeFallback)
	for {
		select {
		case <-resumed:
			c.log.InfoContext(ctx, "power: the machine resumed; re-registering")
			return true
		case <-ctx.Done():
			return false
		case <-time.After(c.pollEvery):
			if !c.d.Now().Round(0).Before(deadline) {
				msg := "power: no resume seen after the sleep command; re-registering"
				if cmd == protocol.ControlShutdown {
					msg = "power: the machine is still running after the shutdown command; re-registering"
				}
				c.log.WarnContext(ctx, msg, slog.Duration("after", resumeFallback))
				return true
			}
		}
	}
}

// comeBack registers the worker again and lets it be retired again.
func (c *controller) comeBack(ctx context.Context) {
	defer c.retiring.Store(false)
	if ctx.Err() != nil {
		return
	}
	if err := c.d.Reregister(ctx); err != nil {
		c.log.ErrorContext(ctx, "power: re-registration failed", slog.Any("error", err))
	}
}

// decideWake decides a wake request.
func (c *controller) decideWake(msg protocol.ControlMsg) plan {
	var macs []net.HardwareAddr
	for _, s := range msg.MACs {
		mac, err := net.ParseMAC(s)
		if err != nil || len(mac) != 6 {
			c.log.WarnContext(c.ctx, "power: ignoring a wake address that is not a 6-byte MAC",
				slog.String("mac", s))
			continue
		}
		macs = append(macs, mac)
	}
	if len(macs) == 0 {
		return refuse("no valid 6-byte MAC address to wake")
	}
	if len(msg.Targets) == 0 {
		return refuse("no target to send the wake packets to")
	}
	targets := append([]string(nil), msg.Targets...)
	return plan{
		reply: protocol.ControlReplyMsg{Accepted: true},
		act: func() {
			if err := c.d.SendWake(c.ctx, macs, targets); err != nil {
				c.log.WarnContext(c.ctx, "power: sending wake packets", slog.Any("error", err))
			}
		},
	}
}

// runCommand runs argv to completion, with no shell. A failure carries the
// start of the command's output.
func runCommand(ctx context.Context, argv []string) error {
	out, err := exec.CommandContext(ctx, argv[0], argv[1:]...).CombinedOutput() //nolint:gosec // argv is operator configuration or a per-OS default
	if err == nil {
		return nil
	}
	text := strings.TrimSpace(string(out))
	if len(text) > maxCommandOutput {
		text = text[:maxCommandOutput]
		for !utf8.ValidString(text) {
			text = text[:len(text)-1]
		}
		text += "..."
	}
	if text == "" {
		return err
	}
	return fmt.Errorf("%w: %s", err, text)
}
