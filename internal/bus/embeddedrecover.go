// SPDX-License-Identifier: AGPL-3.0-or-later

package bus

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	nats "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

const (
	// recoverCheckInterval is how often a broken broker is probed, and a
	// running one checked for JetStream having been shut down.
	recoverCheckInterval = 5 * time.Second
	// recoverBackoffReset is how long after a restart a new break counts as
	// unrelated, and restarts at once.
	recoverBackoffReset = 10 * time.Minute
	// recoverProbeLogEvery spaces the log lines of a probe that keeps
	// failing.
	recoverProbeLogEvery = time.Minute
	// recoverVerifyWait bounds the wait for the data connection after a
	// restart, before the streams and durables are checked.
	recoverVerifyWait = 30 * time.Second
)

// recoverBackoff is how long after the previous restart a restart waits when
// it comes within recoverBackoffReset of it: the first such, the second, and
// every one after.
var recoverBackoff = []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 5 * time.Minute}

// The recovery loop's log messages.
const (
	msgBreak         = "bus: the broker can no longer store messages on a stream"
	msgBreakAnother  = "bus: another broker stream can no longer store messages"
	msgCannotRestart = "bus: the broker cannot restart yet"
	msgRestarting    = "bus: restarting the broker"
	msgRestarted     = "bus: the broker restarted and stores messages again"
	msgRestartFailed = "bus: the broker restart failed"
	msgMissing       = "bus: consumers did not come back after the broker restarted"
)

// brokenState is a break the broker has not recovered from.
type brokenState struct {
	Stream string // "" when JetStream as a whole was shut down
	Err    string
	Since  time.Time
}

// recoverState is what the recovery loop keeps between ticks.
type recoverState struct {
	lastRestart  time.Time // the last successful restart; zero before one
	quick        int       // restarts in a row within recoverBackoffReset of the previous
	startFailed  bool      // the last attempt's new server did not start
	lastProbeLog time.Time
	// failedStarts counts attempts whose new server did not start since the
	// last failure was logged, which is at most once a minute.
	failedStarts int
	lastFailLog  time.Time
	// durables is the snapshot taken before the first attempt of a restart,
	// which the attempts after a failed start reuse: no server runs then to
	// list consumers from.
	durables map[string][]string
}

// startRecovery launches the recovery loop. Shutdown stops it.
func (e *Embedded) startRecovery() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.recoverLoop(ctx)
	}()
	e.mu.Lock()
	e.stopRecovery = func() {
		cancel()
		<-done
	}
	e.mu.Unlock()
}

// recoverLoop watches for a stream break and recovers from it: it records the
// break (see [Embedded.Check]), probes whether its cause has cleared, and then
// restarts the broker, spacing restarts that follow each other closely.
func (e *Embedded) recoverLoop(ctx context.Context) {
	ticker := time.NewTicker(e.checkEvery)
	defer ticker.Stop()
	var st recoverState
	for {
		select {
		case <-ctx.Done():
			return
		case b := <-e.breakCh:
			e.noteBroken(ctx, b)
		case <-ticker.C:
			e.checkJetStream(ctx)
		}
		e.tryRecover(ctx, &st)
	}
}

// noteBroken records b, logging the first break of an outage at ERROR and a
// further stream at WARN.
func (e *Embedded) noteBroken(ctx context.Context, b streamBreak) {
	stream := b.Stream
	if stream == "" {
		stream = "all (JetStream is disabled)"
	}
	e.mu.Lock()
	cur := e.broken
	if cur == nil {
		e.broken = &brokenState{Stream: b.Stream, Err: b.Err, Since: e.now()}
	}
	e.mu.Unlock()
	switch {
	case cur == nil:
		e.logger.ErrorContext(ctx, msgBreak,
			slog.String("stream", stream),
			slog.String("error", b.Err),
			slog.String("impact", "no task status, heartbeat or log is stored on it; workers keep retrying their reports"),
			slog.String("remediation", "free space on the volume holding nats.embedded.data_dir, or raise the file descriptor limit; "+
				"the broker restarts by itself once its data directory is writable"))
	case cur.Stream != b.Stream:
		e.logger.WarnContext(ctx, msgBreakAnother, slog.String("stream", stream), slog.String("error", b.Err))
	}
}

// checkJetStream treats a running server whose JetStream was shut down
// (nats-server does so when it runs out of resources) as broken, in case its
// log line was missed.
func (e *Embedded) checkJetStream(ctx context.Context) {
	e.mu.Lock()
	ns, broken := e.ns, e.broken
	e.mu.Unlock()
	if ns == nil || broken != nil || !ns.Running() || ns.JetStreamEnabled() {
		return
	}
	e.noteBroken(ctx, streamBreak{Err: "JetStream is disabled"})
}

// tryRecover restarts a broken broker when its cause has cleared and the
// backoff allows it.
func (e *Embedded) tryRecover(ctx context.Context, st *recoverState) {
	e.mu.Lock()
	broken := e.broken
	e.mu.Unlock()
	if broken == nil {
		return
	}
	now := e.now()
	if err := e.probe(e.cfg.DataDir); err != nil {
		if st.lastProbeLog.IsZero() || now.Sub(st.lastProbeLog) >= recoverProbeLogEvery {
			e.logger.WarnContext(ctx, msgCannotRestart, slog.Any("error", err))
			st.lastProbeLog = now
		}
		return
	}
	if now.Before(st.nextRestart()) {
		return
	}
	e.restartAndVerify(ctx, st, broken, now)
}

// nextRestart is the earliest a restart may happen: at once for the first in
// a while or after a failed attempt, else the backoff step after the last.
func (st *recoverState) nextRestart() time.Time {
	if st.startFailed || st.lastRestart.IsZero() {
		return time.Time{}
	}
	step := recoverBackoff[min(st.quick, len(recoverBackoff)-1)]
	return st.lastRestart.Add(step)
}

// restartAndVerify restarts the broker and checks what came back. Attempts
// after a failed start log nothing until one succeeds, except the failure
// itself at most once a minute.
func (e *Embedded) restartAndVerify(ctx context.Context, st *recoverState, broken *brokenState, now time.Time) {
	if !st.startFailed {
		st.durables = e.snapshotDurables(ctx)
		e.logger.InfoContext(ctx, msgRestarting, slog.String("stream", broken.Stream))
	}
	if err := e.restart(ctx); err != nil {
		if ctx.Err() != nil {
			// Shutdown ended the restart; sqi-server is stopping.
			return
		}
		st.startFailed = true
		st.failedStarts++
		if st.lastFailLog.IsZero() || now.Sub(st.lastFailLog) >= recoverProbeLogEvery {
			e.logger.ErrorContext(ctx, msgRestartFailed, slog.Any("error", err),
				slog.Int("failed_attempts", st.failedStarts),
				slog.String("impact", "no worker can connect until a restart succeeds; one is tried at every passing check"))
			st.failedStarts, st.lastFailLog = 0, now
		}
		return
	}
	durables := st.durables
	st.startFailed, st.failedStarts, st.lastFailLog, st.durables = false, 0, time.Time{}, nil
	if !st.lastRestart.IsZero() && now.Sub(st.lastRestart) < recoverBackoffReset {
		st.quick++
	} else {
		st.quick = 0
	}
	st.lastRestart = now
	// Breaks the stopped server reported before it stopped are not breaks of
	// the new one.
	select {
	case <-e.breakCh:
	default:
	}
	missing := e.verifyAfterRestart(ctx, durables)
	e.mu.Lock()
	e.broken = nil
	e.restarts++
	if len(missing) > 0 {
		e.missingConsumers = missing
	}
	e.mu.Unlock()
	e.logger.InfoContext(ctx, msgRestarted, slog.Duration("broken_for", e.now().Sub(broken.Since)))
	if len(missing) > 0 {
		e.logger.ErrorContext(ctx, msgMissing, slog.String("consumers", strings.Join(missing, ", ")),
			slog.String("impact", "the server no longer consumes those streams"),
			slog.String("remediation", "restart sqi-server"))
	}
}

// snapshotDurables lists, per stream, the consumers it has, so the check after
// a restart verifies the ones that existed. A stream it cannot read within the
// budget, as when JetStream is disabled, lists the durable consumers a
// running sqi-server keeps on it: the server creates them at startup, so they
// existed unless JetStream broke before it did, and then the check names
// them and a restart of sqi-server clears it.
func (e *Embedded) snapshotDurables(ctx context.Context) map[string][]string {
	out := make(map[string][]string)
	_, server := RestoreTargets()
	serverDurables := func(stream string) {
		for _, d := range server {
			if d.Stream == stream {
				out[stream] = append(out[stream], d.Name)
			}
		}
	}
	js, ok := e.dataJetStream()
	if !ok {
		for _, name := range StreamNames() {
			serverDurables(name)
		}
		return out
	}
	// One budget for the whole snapshot: with JetStream down every request
	// waits out its timeout.
	opCtx, cancel := context.WithTimeout(ctx, guardOpTimeout)
	defer cancel()
	for _, name := range StreamNames() {
		stream, err := js.Stream(opCtx, name)
		if err != nil {
			serverDurables(name)
			continue
		}
		names := stream.ConsumerNames(opCtx)
		for n := range names.Name() {
			out[name] = append(out[name], n)
		}
	}
	return out
}

// verifyAfterRestart waits for the data connection, creates any of sqi's
// streams that did not come back, and returns the durables of durables that
// did not, as "stream/consumer". A connection that does not come back within
// recoverVerifyWait checks nothing: Check reports it.
func (e *Embedded) verifyAfterRestart(ctx context.Context, durables map[string][]string) []string {
	if !e.waitDataConnected(ctx, recoverVerifyWait) {
		return nil
	}
	js, ok := e.dataJetStream()
	if !ok {
		return nil
	}
	opCtx, cancel := context.WithTimeout(ctx, recoverVerifyWait)
	defer cancel()
	for _, name := range StreamNames() {
		if _, err := js.Stream(opCtx, name); errors.Is(err, jetstream.ErrStreamNotFound) {
			e.logger.WarnContext(ctx, "bus: a stream did not come back after the broker restarted; creating it again",
				slog.String("stream", name))
			if err := ensureStreams(opCtx, js, embeddedStreamReplicas); err != nil {
				e.logger.ErrorContext(ctx, "bus: could not create the streams again", slog.Any("error", err))
			}
			break
		}
	}
	var missing []string
	for _, stream := range StreamNames() {
		for _, name := range durables[stream] {
			if _, err := js.Consumer(opCtx, stream, name); errors.Is(err, jetstream.ErrConsumerNotFound) {
				missing = append(missing, stream+"/"+name)
			}
		}
	}
	return missing
}

func (e *Embedded) dataJetStream() (jetstream.JetStream, bool) {
	e.mu.Lock()
	data := e.data
	e.mu.Unlock()
	if data == nil {
		return nil, false
	}
	js, err := jetstream.New(data)
	return js, err == nil
}

// waitDataConnected waits up to d for the data connection to be connected.
func (e *Embedded) waitDataConnected(ctx context.Context, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		e.mu.Lock()
		data := e.data
		e.mu.Unlock()
		if data != nil && data.Status() == nats.CONNECTED {
			return true
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// brokenError is what Check reports for a break not yet recovered from, or a
// restart after which consumers were missing; nil when there is neither.
func (e *Embedded) brokenError() error {
	e.mu.Lock()
	broken, missing := e.broken, e.missingConsumers
	e.mu.Unlock()
	switch {
	case broken != nil && broken.Stream == "":
		return fmt.Errorf("nats JetStream is disabled since %s (%s); the broker restarts once its data directory is writable",
			broken.Since.UTC().Format(time.RFC3339), broken.Err)
	case broken != nil:
		return fmt.Errorf("nats stream %s cannot store messages since %s (%s); the broker restarts once its data directory is writable",
			broken.Stream, broken.Since.UTC().Format(time.RFC3339), broken.Err)
	case len(missing) > 0:
		return fmt.Errorf("nats consumers %s did not come back after the broker restarted; restart sqi-server", strings.Join(missing, ", "))
	}
	return nil
}
