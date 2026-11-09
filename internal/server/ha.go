// SPDX-License-Identifier: AGPL-3.0-or-later

package server

// A server in a high-availability group shares one PostgreSQL database and
// one external NATS account with the group's other servers, and leads only
// while it holds the leader lease (internal/ha).
//
// It starts as a warm standby: store open under the shared lock, membership
// row live, broker connected and checked, auth-callout responder and
// worker-diagnostic subscription running, HTTP listener serving its probes
// and metrics and answering everything else 503. It then campaigns for the
// lease. On winning it promotes itself (ha_promote.go) and holds the lease;
// on any loss of authority it exits with [ExitDemoted], for its supervisor to
// restart it as a standby. A fresh process has no stale state, which making
// the scheduler, the consumers and the hub stoppable in place could not
// promise.
//
// Nothing here runs without HA: [Server.Run] branches to runHA once.

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/uberware/sqi/internal/api"
	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/ha"
	"github.com/uberware/sqi/internal/health"
	"github.com/uberware/sqi/internal/middleware"
	"github.com/uberware/sqi/internal/scheduler"
	"github.com/uberware/sqi/internal/store"
)

// ErrDemoted is what [Server.Run] returns, wrapping the [*ha.LostError], when
// this server lost its leadership and stopped so that its supervisor restarts
// it as a standby. It names its own exit code, [ExitDemoted], through an
// ExitCode method, which is how the Windows service reports it to the SCM
// without internal/winsvc importing this package.
var ErrDemoted error = demotedError{}

// demotedError is [ErrDemoted]'s type.
type demotedError struct{}

func (demotedError) Error() string { return "server: demoted" }

// ExitCode is the process exit code of a demoted server.
func (demotedError) ExitCode() int { return ExitDemoted }

// ExitDemoted is the exit code of a demoted server. It is documented, so a
// supervisor and its alerting can tell a demotion, after which a restart
// brings the server back as a standby, from a crash.
const ExitDemoted = 75

// haBroker is the external broker as an HA server drives it ([bus.Remote]).
type haBroker interface {
	bus.Broker
	StartStandby(ctx context.Context, opts bus.GuardOptions) error
	StandbyReady() error
	StandbyChecked() <-chan struct{}
	TakeLeadership(ctx context.Context, opts bus.GuardOptions) error
	Connected() (up bool, since time.Time)
	SetCalloutActive(active bool)
}

// haState is what an HA server keeps beyond a single server's.
type haState struct {
	self    store.ServerMember // this server's membership row
	role    *roleState
	elector *ha.Elector
	broker  haBroker
	handler *handlerSwap // the HTTP handler, swapped once the API is built

	// cancel ends the HA loops (membership, group observer, leader sweeper),
	// and loops is how many are running.
	cancel context.CancelFunc
	loops  sync.WaitGroup

	// warned holds the version and grants pairs already warned about. Only
	// the group observer's goroutine touches it.
	warned map[string]bool
}

func newHAState(self store.ServerMember) *haState {
	return &haState{self: self, role: newRoleState(), handler: &handlerSwap{}, warned: map[string]bool{}}
}

// stopLoops ends the HA loops and waits for them. It does nothing on a
// server without HA (nil h).
func (h *haState) stopLoops() {
	if h == nil {
		return
	}
	if h.cancel != nil {
		h.cancel()
	}
	h.loops.Wait()
}

// handlerSwap is an HTTP handler that can be replaced while it serves, so a
// standby answers its probes before its API exists.
type handlerSwap struct{ h atomic.Pointer[http.Handler] }

func (s *handlerSwap) set(h http.Handler) { s.h.Store(&h) }

func (s *handlerSwap) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	(*s.h.Load()).ServeHTTP(w, r)
}

// haSelf is this process's membership row: its [Server.identity] and the
// broker permissions it grants workers.
func (s *Server) haSelf() store.ServerMember {
	self := s.identity()
	self.GrantsFingerprint = bus.GrantsFingerprint()
	return self
}

// runHA is [Server.Run] for a server in an HA group: boot as a standby,
// campaign, promote, hold. It returns nil after a graceful stop, an error
// wrapping [ErrDemoted] after a loss of leadership, and any other error when
// the server could not start.
func (s *Server) runHA(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.ha = newHAState(s.haSelf())
	s.ha.cancel = cancel
	s.metrics.RegisterHA()
	warnHASupervisor(ctx, s.logger)

	if err := s.startStandby(ctx); err != nil {
		s.abortStart()
		return fmt.Errorf("startup failed: %w", err)
	}
	s.logger.InfoContext(ctx, "sqi-server started as a standby",
		slog.String("instance_name", s.ha.self.Name), slog.String("instance_id", s.ha.self.InstanceID))

	if err := s.warmStandby(ctx); err != nil {
		if ctx.Err() != nil {
			return s.shutdown()
		}
		s.abortStart()
		return fmt.Errorf("startup failed: %w", err)
	}
	s.logger.InfoContext(ctx, "sqi-server ready as a standby — campaigning for leadership")

	epoch, err := s.ha.elector.Campaign(ctx)
	if err != nil {
		s.logger.InfoContext(ctx, "shutdown signal received, stopping components")
		return s.shutdown()
	}
	if lost := s.lead(ctx, epoch); lost != nil {
		return s.demote(lost)
	}
	s.logger.InfoContext(ctx, "shutdown signal received, stopping components")
	return s.shutdown()
}

// lead promotes this server at epoch and holds the lease until ctx ends,
// returning nil, or authority is lost, returning the loss. The lease is held
// from the start, since promotion can outlast it: taking the broker's guard
// may watch another holder's key for a guard TTL. Everything the promotion
// starts runs under a context that ends with the hold, so a leader that lost
// its authority mid-promotion goes no further.
func (s *Server) lead(ctx context.Context, epoch int64) error {
	pctx, cancel := context.WithCancel(ctx)
	defer cancel()
	held := make(chan error, 1)
	go func() {
		defer cancel()
		held <- s.ha.elector.Hold(ctx, epoch)
	}()
	if err := s.promote(pctx, epoch); err != nil && pctx.Err() == nil {
		s.ha.elector.Lose(promotionReason(err), fmt.Errorf("promotion: %w", err))
	}
	return <-held
}

// warnHASupervisor says, once at start, what a demotion relies on.
func warnHASupervisor(ctx context.Context, logger *slog.Logger) {
	logger.WarnContext(ctx, "ha: a leader that loses its leadership exits with code 75, and stays stopped unless a supervisor restarts it",
		slog.Int("exit_code", ExitDemoted),
		slog.String("remediation", "run every sqi-server of the group under a supervisor that restarts it when it exits: "+
			"systemd Restart=on-failure, Compose restart: unless-stopped, or the Windows service's recovery actions"))
}

// startStandby brings up what a standby has whether or not its broker
// answers: the store, its membership row, the broker connections, the group
// observer and the HTTP listener. It runs nothing that only the leader may:
// no seeding, no bootstrap, no broker-layout reconcile, no scheduler.
func (s *Server) startStandby(ctx context.Context) error {
	if err := s.openStore(ctx); err != nil {
		return err
	}

	b, err := s.buildBroker(ctx)
	if err != nil {
		return fmt.Errorf("start bus: %w", err)
	}
	hb, ok := b.(haBroker)
	if !ok {
		return fmt.Errorf("start bus: high availability needs an external NATS broker (nats.mode: external); this one is %T", b)
	}
	s.broker, s.ha.broker = hb, hb

	s.ha.elector = ha.New(ha.Config{
		Self:     s.ha.self,
		LeaseTTL: s.cfg.HA.LeaseTTL,
		BrokerUp: hb.Connected,
		Logger:   s.logger,
	}, s.store)
	s.ha.role.setAuthority(func() bool { return s.ha.elector.Authorized() })
	s.ha.loops.Go(func() { s.ha.elector.RunMembership(ctx) })

	if err := hb.StartStandby(ctx, s.guardOptions()); err != nil {
		return fmt.Errorf("start bus: %w", err)
	}
	s.health.Register("nats", health.CheckerFunc(hb.Check))
	s.health.SetRoleProvider(s.ha.role.info)
	s.ha.loops.Go(func() { s.watchGroup(ctx) })

	s.ha.handler.set(s.probeHandler())
	s.httpServer = &http.Server{
		Addr:              s.cfg.HTTPAddr,
		Handler:           s.ha.handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	return s.serveHTTP(ctx)
}

// probeHandler is what a standby serves until its API is built: the probes
// and its metrics, everything else gated.
func (s *Server) probeHandler() http.Handler {
	mux := probeMux{http.NewServeMux()}
	api.MountProbes(mux, true, s.health, s.metrics)
	return middleware.Standby(s.ha.role.isLeader, s.ha.role.leaderName, api.ProbePaths)(mux)
}

// probeMux is a [http.ServeMux] as an [api.ProbeRouter].
type probeMux struct{ *http.ServeMux }

func (m probeMux) Get(pattern string, h http.HandlerFunc) { m.Handle("GET "+pattern, h) }

// guardOptions are the broker guard's view of this database's membership,
// and what losing the guard means: another server leads.
func (s *Server) guardOptions() bus.GuardOptions {
	return bus.GuardOptions{
		IsMember: s.isMember,
		OnLost:   func(err error) { s.ha.elector.Lose(ha.ReasonGuardLost, err) },
	}
}

// memberLookupTimeout bounds the guard's membership lookup.
const memberLookupTimeout = 5 * time.Second

// isMember reports whether instanceID is a server of this database. A failed
// lookup answers no, so the guard watches the key before taking it rather
// than overwriting it at once.
func (s *Server) isMember(instanceID string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), memberLookupTimeout)
	defer cancel()
	members, _, err := s.store.ListServerMembers(ctx)
	if err != nil {
		s.logger.WarnContext(ctx, "ha: list server members failed", slog.Any("error", err))
		return false
	}
	for _, m := range members {
		if m.InstanceID == instanceID {
			return true
		}
	}
	return false
}

// brokerPoll is how often a standby whose broker is down checks whether it
// has connected.
const brokerPoll = 100 * time.Millisecond

// warmStandby finishes a standby once its broker connects: the bus client,
// the hub, the scheduler (built, not run) with its worker-diagnostic
// subscription, and the API behind the standby gate. It returns once the
// broker's standby checks have passed, and their refusal if they do not.
func (s *Server) warmStandby(ctx context.Context) error {
	if err := s.awaitBroker(ctx); err != nil {
		return err
	}
	if err := s.buildStandbyAPI(ctx); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.ha.broker.StandbyChecked():
	}
	return s.ha.broker.StandbyReady()
}

// awaitBroker waits until the broker's data connection is up, which the bus
// client needs to be created, or until the standby checks refuse it.
func (s *Server) awaitBroker(ctx context.Context) error {
	t := time.NewTicker(brokerPoll)
	defer t.Stop()
	checked := s.ha.broker.StandbyChecked()
	for {
		if up, _ := s.ha.broker.Connected(); up {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-checked:
			if err := s.ha.broker.StandbyReady(); err != nil {
				return err
			}
			checked = nil // passed; wait for the connection itself
		case <-t.C:
		}
	}
}

// buildStandbyAPI creates what needs the broker: the bus client, the
// WebSocket hub, the scheduler and the API, which replaces the probe handler.
// The scheduler only subscribes to worker diagnostics, so the Server Log
// keeps worker history across a failover; it runs on promotion.
func (s *Server) buildStandbyAPI(ctx context.Context) error {
	busClient, err := s.broker.NewClient()
	if err != nil {
		return fmt.Errorf("start bus client: %w", err)
	}
	s.busClient = busClient
	s.logger.InfoContext(ctx, "bus: typed client connected")

	s.wsHub = newWSHub(s.logger, s.store, s.cfg.AuthEnabled)
	s.notifyDiagToHub()

	schedCfg := s.schedulerConfigFor(busClient)
	schedCfg.OnFenced = func(err error) { s.ha.elector.Lose(ha.ReasonFenced, err) }
	s.sched = scheduler.New(schedCfg, s.store, s.busClient, s.metrics, s.logger, s.wsHub, s.diagBuf)
	if err := s.sched.StartDiag(); err != nil {
		// Run tries again on promotion; until then worker diagnostics are not
		// collected here.
		s.logger.WarnContext(ctx, "scheduler: worker diagnostics not collected on this standby", slog.Any("error", err))
	}

	deps := s.apiDeps()
	// The authenticator only: the first admin is seeded at promotion.
	deps.Auth = s.authenticator()
	if err := s.wireDirectoryDeps(ctx, &deps); err != nil {
		return err
	}
	rc := routerConfig(s.cfg)
	rc.HA = &api.HAGate{IsLeader: s.ha.role.isLeader, LeaderName: s.ha.role.leaderName}
	s.ha.handler.set(api.NewRouter(rc, deps, s.logger, s.metrics, s.health))
	return nil
}
