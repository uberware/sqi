// SPDX-License-Identifier: AGPL-3.0-or-later

package bus

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	nats "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/uberware/sqi/internal/redact"
)

// haGuardWatch is what checking or taking the guard may add to the connect
// timeout in an HA group: a key held by a server outside this database is
// watched for one TTL, polled a fraction of the refresh period past it,
// before it is refused or taken. One refresh period covers that poll and the
// requests around the watch.
func haGuardWatch() time.Duration { return guardTTL + guardRefresh }

// ErrStandbyUnchecked is what [Remote.StandbyReady] returns until a standby
// has checked its broker: no broker has answered yet, or the checks are under
// way.
var ErrStandbyUnchecked = errors.New("bus: this standby has not checked the external broker yet")

// StartStandby starts the broker for a standby server of an HA group: it
// opens the data connection, and with worker authentication on the callout
// and system connections. It neither takes the account's guard nor creates
// any stream; [Remote.TakeLeadership] does both on promotion.
//
// Before a standby does anything on the broker it checks it, as Start does
// (version, JetStream on the account), and checks the guard with
// [CheckGuard] under opts, so a standby pointed at an account a server of
// another database holds is refused, after watching that server's key for up
// to one TTL. The connect timeout bounds the broker checks, and the guard
// check may take [haGuardWatch] past it. Only once the checks pass does the
// standby answer the cluster's authorization requests, so that workers can
// connect while no server leads; until then a callout reaching the account
// may belong to that other farm, whose credentials this server does not have.
//
// A standby does not need its broker to start. When a broker answers the
// first attempt, StartStandby runs the checks and returns their failure, if
// any, having shut the broker down, as Start does. When none does,
// StartStandby logs it and returns nil; the connections keep trying in the
// background, and the checks run once the data connection connects. Then a
// failure a retry may clear is logged and the checks run again; a refusal
// (version floor, no JetStream on the account, the guard held by another
// database's server) is final, logged and reported by [Remote.StandbyReady],
// and the standby stays not ready and answers no callout. [Remote.StandbyChecked] is closed when
// the outcome is known, and [Remote.Connected] says whether the data
// connection is up.
//
// [RemoteConfig.InstanceID] must be set. StartStandby must be called instead
// of [Remote.Start], once.
func (r *Remote) StartStandby(ctx context.Context, opts GuardOptions) error {
	if err := r.markStarted(); err != nil {
		return err
	}
	if r.cfg.InstanceID == "" {
		return errors.New("bus: external broker: a standby needs the server's member instance id")
	}
	if err := r.cfg.validate(); err != nil {
		return err
	}
	r.warnLooseSeedFiles(ctx)
	dialOpts, err := r.cfg.dialOptions()
	if err != nil {
		return err
	}
	responder, err := r.newResponder()
	if err != nil {
		return err
	}

	data, err := r.open(roleData, dialOpts, true)
	if err != nil {
		return err
	}
	auth, err := r.openAuth(responder)
	if err != nil {
		data.nc.Close()
		return err
	}
	r.mu.Lock()
	r.data, r.dialOpts = data.nc, dialOpts
	r.callout, r.system, r.responder = auth.callout, auth.system, responder
	r.standby = true
	r.mu.Unlock()

	if !data.nc.IsConnected() {
		r.logger.WarnContext(ctx, "bus: no external nats broker answered; this standby keeps trying in the background",
			slog.String("urls", r.cfg.redactedURLs()),
			slog.Any("error", data.lastErr.get()),
			slog.String("impact", "this sqi-server is not ready, answers no worker connects and cannot become the leader until it connects"))
		r.checkWhenConnected(data, auth.system, opts)
		return nil
	}
	if err := r.checkStandby(ctx, data.nc, auth.system, opts); err != nil {
		r.finishStandbyCheck(err)
		r.Shutdown()
		return err
	}
	r.passStandbyCheck(ctx, data.nc)
	return nil
}

// standbyRecheckWait is the pause before a standby that connected late checks
// its broker again after a check that may pass on a retry failed.
const standbyRecheckWait = 2 * time.Second

// checkWhenConnected runs the standby checks on a background goroutine once
// the data connection connects. A check that fails for a reason a retry may
// clear (a cluster still electing its JetStream leaders after a restart, a
// request lost to a reconnect) is logged and tried again; a refusal
// ([refusal]) is final. [Remote.Shutdown] stops the goroutine and waits for
// it.
func (r *Remote) checkWhenConnected(data roleConn, system *nats.Conn, opts GuardOptions) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	r.mu.Lock()
	r.checkCancel, r.checkDone = cancel, done
	r.mu.Unlock()
	go func() {
		defer close(done)
		for awaitConnected(ctx, data) {
			err := r.checkStandby(ctx, data.nc, system, opts)
			switch {
			case ctx.Err() != nil:
				return // shut down while checking; there is no outcome to report
			case err == nil:
				r.passStandbyCheck(ctx, data.nc)
				return
			case isRefusal(err):
				r.finishStandbyCheck(err)
				r.logger.ErrorContext(ctx, "bus: the external broker this standby connected to refuses it",
					slog.Any("error", err),
					slog.String("impact", "this sqi-server stays not ready, answers no worker connects and cannot become the leader"),
					slog.String("remediation", "correct the cause and restart this sqi-server"))
				return
			}
			r.logger.WarnContext(ctx, "bus: could not check the external broker this standby connected to; trying again",
				slog.Any("error", err),
				slog.Duration("retry_in", standbyRecheckWait),
				slog.String("impact", "this sqi-server is not ready and cannot become the leader until a check passes"))
			timer := time.NewTimer(standbyRecheckWait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
}

// awaitConnected waits until data is connected, and reports false when it
// closed or ctx ended first.
func awaitConnected(ctx context.Context, data roleConn) bool {
	for !data.nc.IsConnected() {
		select {
		case <-data.connected:
		case <-data.closed:
			return false
		case <-ctx.Done():
			return false
		}
	}
	return ctx.Err() == nil
}

// refusal marks a standby check failure no retry can clear: the broker is
// below the version floor, the account has no JetStream, or a server of
// another database holds the account.
type refusal struct{ err error }

func (e refusal) Error() string { return e.err.Error() }

func (e refusal) Unwrap() error { return e.err }

func isRefusal(err error) bool {
	var r refusal
	return errors.As(err, &r)
}

// passStandbyCheck starts answering callouts, unless [Remote.SetCalloutActive]
// has asked for them off, then records that the standby checks passed.
func (r *Remote) passStandbyCheck(ctx context.Context, data *nats.Conn) {
	r.openCallout(ctx)
	r.finishStandbyCheck(nil)
	r.logger.InfoContext(
		ctx, "bus: connected to external nats as a standby",
		slog.String("url", redact.URL(data.ConnectedUrl())),
		slog.String("server_version", brokerVersion(data)),
		slog.String("cluster", data.ConnectedClusterName()),
		slog.String("account", r.cfg.Account),
		slog.Bool("worker_auth", r.cfg.Auth.Enabled),
	)
}

// finishStandbyCheck records the outcome of the standby checks. The first
// outcome is final.
func (r *Remote) finishStandbyCheck(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !errors.Is(r.standbyErr, ErrStandbyUnchecked) {
		return
	}
	r.standbyErr = err
	close(r.standbyChecked)
}

// openCallout lets the responder answer callouts on the callout connection:
// it joins the queue group, or, when [Remote.SetCalloutActive] has asked for
// it off, it is given the connection paused, for SetCalloutActive to resume.
func (r *Remote) openCallout(ctx context.Context) {
	r.calloutMu.Lock()
	defer r.calloutMu.Unlock()
	r.mu.Lock()
	responder, callout, wanted := r.responder, r.callout, r.calloutWanted
	r.calloutOpen = true
	r.mu.Unlock()
	if responder == nil || callout == nil {
		return
	}
	if !wanted {
		if err := responder.attach(callout); err != nil {
			r.logger.ErrorContext(ctx, "bus: could not start the auth callout responder", slog.Any("error", err))
		}
		return
	}
	if err := responder.Listen(callout); err != nil {
		r.logger.ErrorContext(ctx, "bus: could not start the auth callout responder",
			slog.Any("error", err),
			slog.String("impact", "this sqi-server answers no worker connects; the other servers in the group still do"))
		return
	}
	if err := flushIfConnected(callout); err != nil {
		r.logger.WarnContext(ctx, "bus: the broker has not confirmed the auth callout subscription; it is sent again when the connection recovers",
			slog.Any("error", err))
	}
}

// StandbyReady reports the outcome of a standby's broker checks (see
// [Remote.StartStandby]): [ErrStandbyUnchecked] until they have run, nil once
// they passed, and why they failed otherwise, such as an error wrapping
// [ErrGuardHeld] when a server of another database holds the account. A
// failure is final: the server cannot become ready or the leader, and should
// exit. For a broker started with Start it is nil once Start succeeds.
func (r *Remote) StandbyReady() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.standbyErr
}

// StandbyChecked is closed once [Remote.StandbyReady] has its final outcome.
func (r *Remote) StandbyChecked() <-chan struct{} {
	return r.standbyChecked
}

// openAuth opens a standby's callout connection, and its system connection
// when one is configured, without waiting for either to connect. The
// responder is started on the callout connection only once the standby checks
// pass.
func (r *Remote) openAuth(responder *Responder) (remoteAuth, error) {
	var a remoteAuth
	if responder == nil {
		return a, nil
	}
	opts, err := r.cfg.seedDialOptions("auth callout", r.cfg.Auth.CalloutNkeySeedFile)
	if err != nil {
		return a, err
	}
	callout, err := r.open(roleCallout, opts, true)
	if err != nil {
		return a, err
	}
	a.callout = callout.nc

	if r.cfg.Auth.SystemCredsFile == "" {
		return a, nil
	}
	if opts, err = r.cfg.seedDialOptions("system", r.cfg.Auth.SystemCredsFile); err != nil {
		a.close()
		return remoteAuth{}, err
	}
	system, err := r.open(roleSystem, opts, true)
	if err != nil {
		a.close()
		return remoteAuth{}, err
	}
	a.system = system.nc
	return a, nil
}

// checkStandby checks the broker a standby reached, and the account's guard,
// without writing either.
func (r *Remote) checkStandby(ctx context.Context, data, system *nats.Conn, opts GuardOptions) error {
	deadline := time.Now().Add(r.cfg.ConnectTimeout)
	js, err := r.checkBroker(ctx, data, deadline)
	if err != nil {
		if CheckBrokerVersion(brokerVersion(data)) != nil || errors.Is(err, jetstream.ErrJetStreamNotEnabledForAccount) {
			return refusal{err}
		}
		return err
	}
	guardCtx, cancel := context.WithDeadline(ctx, deadline.Add(haGuardWatch()))
	defer cancel()
	if err := CheckGuard(guardCtx, js, opts, r.logger); err != nil {
		if errors.Is(err, ErrGuardHeld) {
			return refusal{err}
		}
		return err
	}
	var cluster clusterView
	if system != nil && system.IsConnected() {
		if cluster, err = askCluster(ctx, system, varzPingSubject, clusterView{}); err != nil {
			return err
		}
	}
	identity := remoteIdentity(r.cfg.Account, data.ConnectedClusterName(), data.ConnectedServerName(), data.ConnectedServerId())
	r.mu.Lock()
	r.identity = identity
	r.cluster = r.cluster.merge(cluster)
	r.mu.Unlock()
	return nil
}

// TakeLeadership promotes a standby started with [Remote.StartStandby]: it
// checks the broker as Start does, takes the account's guard under opts, and
// creates or updates the streams with the configured replica count. It is
// Start's provisioning, under the HA guard. The guard is held under
// [RemoteConfig.InstanceID], so the next leader recognizes it as a member's,
// and opts.OnLost hears when another server takes it.
//
// It fails at once when the data connection is down: only a standby with a
// broker may lead. The connect timeout plus [haGuardWatch] bounds it, the
// watch of a key held by a server outside this database included. On failure
// nothing is held, and it may be called again; on success, a second call
// fails. [Remote.Shutdown] releases the guard.
func (r *Remote) TakeLeadership(ctx context.Context, opts GuardOptions) error {
	data, system, err := r.beginLeadership()
	if err != nil {
		return err
	}
	guard, identity, cluster, err := r.lead(ctx, data, system, opts)

	r.mu.Lock()
	if err == nil && r.data == nil {
		err = errors.New("bus: the broker was shut down while this server was taking leadership")
	}
	if err != nil {
		r.leading = false
		r.mu.Unlock()
		r.releaseGuard(guard)
		return err
	}
	r.guard, r.identity = guard, identity
	r.cluster = r.cluster.merge(cluster)
	r.mu.Unlock()

	r.logger.InfoContext(
		ctx, "bus: this sqi-server holds the external nats account as the leader",
		slog.String("url", redact.URL(data.ConnectedUrl())),
		slog.String("cluster", data.ConnectedClusterName()),
		slog.String("account", r.cfg.Account),
		slog.Int("stream_replicas", r.cfg.StreamReplicas),
		slog.String("identity", identity),
		slog.String("instance_id", r.cfg.InstanceID),
	)
	return nil
}

// beginLeadership checks that a promotion may start and marks one under way.
func (r *Remote) beginLeadership() (data, system *nats.Conn, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case !r.standby:
		return nil, nil, errors.New("bus: TakeLeadership needs a broker started with StartStandby")
	case r.data == nil:
		return nil, nil, errors.New("bus: broker shut down")
	case r.leading:
		return nil, nil, errors.New("bus: this server already leads, or is taking leadership")
	case !r.data.IsConnected():
		return nil, nil, fmt.Errorf("bus: the external broker is not connected (%s); a standby takes leadership only once it is",
			r.cfg.redactedURLs())
	case r.standbyErr != nil:
		return nil, nil, r.standbyErr
	}
	r.leading = true
	return r.data, r.system, nil
}

// lead takes the guard, provisions the streams and, when the system
// connection has not counted the cluster's nodes yet, counts them, as Start
// does. On failure it holds nothing.
func (r *Remote) lead(ctx context.Context, data, system *nats.Conn, opts GuardOptions) (*Guard, string, clusterView, error) {
	deadline := time.Now().Add(r.cfg.ConnectTimeout + haGuardWatch())
	identity, guard, err := r.provisionWith(ctx, data, deadline, opts)
	if err != nil {
		return nil, "", clusterView{}, err
	}
	r.mu.Lock()
	known := r.cluster
	r.mu.Unlock()
	if system == nil || known.nodes > 0 {
		return guard, identity, clusterView{}, nil
	}
	cluster, err := askCluster(ctx, system, varzPingSubject, clusterView{})
	if err != nil {
		r.releaseGuard(guard)
		return nil, "", clusterView{}, err
	}
	return guard, identity, cluster, nil
}

// Connected reports whether the data connection is up and, when it is, since
// when: the time it last connected or reconnected, in UTC. While it is down,
// and before Start or StartStandby and after Shutdown, it returns false and
// the zero time. Both are read from nats.go's connection handlers, which run
// just after the event, so a connection that has only just come up may be
// reported down for that moment. Safe for concurrent use.
func (r *Remote) Connected() (up bool, since time.Time) {
	r.mu.Lock()
	data := r.data
	r.mu.Unlock()
	since = r.dataUp.get()
	if data == nil || !data.IsConnected() || since.IsZero() {
		return false, time.Time{}
	}
	return true, since
}

// SetCalloutActive takes this server's auth-callout responder out of the
// sqi-auth queue group (false), so that every authorization request goes to
// the other servers in it, or puts it back (true). An HA standby whose
// worker grants differ from the leader's ([GrantsFingerprint]) leaves the
// group, so that a worker is never authorized differently depending on which
// server answers. Setting the state the responder is already in does
// nothing. It is safe to call concurrently with requests being answered,
// which complete; it does nothing with worker authentication off, and after
// Shutdown. On a standby whose checks have not passed it only records the
// state, which applies when they do; a standby refused by its checks never
// answers.
func (r *Remote) SetCalloutActive(active bool) {
	r.calloutMu.Lock()
	defer r.calloutMu.Unlock()
	r.mu.Lock()
	r.calloutWanted = active
	responder, open := r.responder, r.calloutOpen
	r.mu.Unlock()
	if responder == nil || !open {
		return
	}
	ctx := context.Background()
	if active {
		changed, err := responder.Resume()
		switch {
		case err != nil:
			r.logger.ErrorContext(ctx, "bus: the auth callout responder could not rejoin the sqi-auth queue group",
				slog.Any("error", err),
				slog.String("impact", "this sqi-server answers no worker connects; the other servers in the group still do"))
		case changed:
			r.logger.InfoContext(ctx, "bus: the auth callout responder rejoined the sqi-auth queue group")
		}
		return
	}
	changed, err := responder.Pause()
	switch {
	case err != nil:
		r.logger.WarnContext(ctx, "bus: the auth callout responder left the sqi-auth queue group, but the broker has not confirmed it",
			slog.Any("error", err),
			slog.String("impact", "a worker connect sent to this server meanwhile goes unanswered and is retried"))
	case changed:
		r.logger.InfoContext(ctx, "bus: the auth callout responder left the sqi-auth queue group",
			slog.String("impact", "the other sqi-servers in the group answer every worker connect"))
	}
}
