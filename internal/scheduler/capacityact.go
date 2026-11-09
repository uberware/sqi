// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

// capacityact.go is the capacity actuator: the part of the capacity sweep that
// acts on what the policy decided, for groups whose provider acts.
//
// Every action is keyed to a power state the store recorded first. A wake
// moves its members to waking, and a retirement moves its member to retiring
// (a compare-and-set that a lease landing first refuses), before the provider
// is called. The call then runs on a goroutine of its own, at most one per
// group, and its results are applied through fenced writes, so a server that
// lost the leadership meanwhile writes nothing.
//
// Expiry settles what a call left behind: a waking member that has not
// registered within its group's boot timeout becomes wake_failed, and a
// retiring one still retiring after that long goes back to awake. It runs for
// every group of the farm, whatever its provider and whether it is enabled, so
// a group switched to external or disabled mid-transition never leaves a
// member stuck.
//
// The actuator remembers, per process, which recorded intents it has sent. A
// member found waking or retiring whose intent this process has not sent is
// sent it once: that is how a new leader repeats what the old one recorded.
// Every request is absolute (wake this machine, sleep that one), so a repeat
// is harmless.
//
// It wakes and retires only the named candidates the policy returned, never a
// count. The one count it acts on is a sized group's size, which it raises
// through the group's launch record (capacityscript.go).

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/uberware/sqi/internal/capacity"
	"github.com/uberware/sqi/internal/capacity/script"
	"github.com/uberware/sqi/internal/store"
)

// Provider action results, as actionResult.Result, ActionRecord.Result and
// the result label of sqi_capacity_actions_total carry them.
const (
	resultOK          = "ok"
	resultFailed      = "failed"
	resultRefused     = "refused"
	resultUnsupported = "unsupported"
	resultTimeout     = "timeout"
	// resultExpired is a transition the boot timeout ended: a retirement that
	// never completed. A wake that did is counted in
	// sqi_capacity_boot_timeouts_total instead.
	resultExpired = "expired"
)

// Provider actions, as ActionRecord.Action and the action label carry them.
const (
	actionWake      = "wake"
	actionGrow      = "grow"
	actionRetire    = "retire"
	actionList      = "list"
	actionConfigure = "configure"
)

// actionResult is what a provider reports for one action.
type actionResult struct {
	Result, Message string
	// undelivered marks a wake_on_lan retire request that reached no one: no
	// worker was subscribed to the member's control subject. See
	// [Scheduler.applyRetireResults] for what it means.
	undelivered bool
}

// capacityProvider is a mechanism that starts and stops a group's machines.
// Each method blocks until the provider has answered and reports the result;
// requests are absolute, so repeating one is harmless.
type capacityProvider interface {
	// wake starts each of members, and returns each one's result by worker
	// ID.
	wake(ctx context.Context, g store.CapacityGroup, members []store.Worker) map[string]actionResult
	// grow asks a sized group's provider for desired machines in all; it
	// last asked for current.
	grow(ctx context.Context, g store.CapacityGroup, desired, current int) actionResult
	// retire stops each of members, and returns each one's result by worker
	// ID.
	retire(ctx context.Context, g store.CapacityGroup, members []store.Worker) map[string]actionResult
	// retireInstances stops each of a sized group's instances that never
	// registered, named by instance ID alone.
	retireInstances(ctx context.Context, g store.CapacityGroup, instanceIDs []string) actionResult
	// list returns the provider's machine instance IDs for the group.
	list(ctx context.Context, g store.CapacityGroup) ([]string, actionResult)
}

// providerKind names a provider implementation. What a provider's results
// mean for the store is decided by kind, not by provider name.
type providerKind string

// providerKindWakeOnLAN is the built-in wake_on_lan provider.
const providerKindWakeOnLAN providerKind = "wake_on_lan"

// boundProvider is a group's provider, ready to call.
type boundProvider struct {
	capacityProvider

	kind providerKind
}

// intentKey identifies one recorded intent: a member in a power state
// recorded at a time, or a group-level request (a sized group's grow, or its
// orphan cleanup after a degradation) recorded at a time. A new transition or
// record makes a new key.
type intentKey struct {
	// subject is "worker:<id>" or "group:<id>".
	subject string
	// intent is the member's power state, or the group-level request.
	intent string
	at     int64 // the recorded time as Unix nanoseconds; 0 when unset
}

// Group-level intents.
const (
	intentGrow    = "grow"
	intentOrphans = "orphans"
)

// groupIntent is the key of groupID's intent recorded at at.
func groupIntent(groupID, intent string, at time.Time) intentKey {
	return intentKey{subject: "group:" + groupID, intent: intent, at: at.UnixNano()}
}

// actuatorState is what the actuator keeps between ticks. It lives in memory
// only (see the file comment).
type actuatorState struct {
	// dispatched, begun and unconfigured are touched only with the capacity
	// sweep's sweepMu held.
	//
	// dispatched holds every intent this process has sent; it is never
	// pruned, so each intent is sent at most once per process. Each entry
	// is a few words, one per transition the server has made.
	dispatched map[intentKey]struct{}
	// begun holds the retirements this process began (its own
	// BeginRetirement moved the member to retiring) and has not yet sent, so
	// the dispatch can tell a first request from one re-sent after a restart
	// or a failover. An entry goes when its intent is dispatched.
	begun map[intentKey]struct{}
	// unconfigured is, per group ID, the script provider whose absence from
	// the configuration was last logged.
	unconfigured map[string]store.CapacityProvider
	// launches is each sized script group's launch record as the last sweep
	// that read it left it, by group ID; a sweep whose read fails reports it
	// instead.
	launches map[string]store.CapacityGroupState

	// stateMu serializes the group states' (launch records')
	// read-modify-write: a sweep holds it from its read of every state to
	// the end of its writes, and a provider call holds it around each fresh
	// read and write of its own (capacityscript.go). It is held across store
	// calls. The order is sweepMu, then stateMu, then capacityState.mu, then
	// mu; a provider call takes gate (shared), then stateMu, and never
	// sweepMu.
	stateMu sync.Mutex

	// mu guards inFlight, lastAction, liveGroups and seriesGroups, which
	// provider calls use from their own goroutines. It is a leaf: nothing is
	// locked while it is held. The publishing sweep takes it inside
	// capacityState.mu, so the order is sweepMu, then capacityState.mu, then
	// mu; a provider call takes mu and capacityState.mu one at a time, never
	// both.
	mu sync.Mutex
	// inFlight holds the groups, by ID, with a provider call running.
	inFlight map[string]bool
	// lastAction is each group's latest provider action, by group ID.
	lastAction map[string]capacity.ActionRecord
	// liveGroups is each group of the farm, by ID, with its name, as the
	// last sweep listed them. A counter is added to only for a group here
	// under that name, so a call finishing after its group was deleted or
	// renamed makes no series that nothing would delete.
	liveGroups map[string]string
	// seriesGroups are the group names that have had a counter added to.
	// Each sweep deletes the series of those no longer in the farm.
	seriesGroups map[string]struct{}

	// gate lets provider calls write to the store until shutdown closes it.
	gate writeGate
	// calls runs the provider calls. [Scheduler.Run] closes it at shutdown
	// and waits, for at most [Scheduler.capacityDrainTimeout], for the calls
	// still running.
	calls backgroundCalls

	// newScriptProvider, when set, stands in for the script provider; tests
	// set it.
	newScriptProvider func(script.Provider) capacityProvider
	// drainTimeout, when positive, replaces [Scheduler.capacityDrainTimeout];
	// tests shorten it.
	drainTimeout time.Duration
}

// wolCallBound bounds one wake_on_lan call: a wake waiting out a relay that
// does not answer and then sending the packets itself, followed by a retire
// waiting out its reply. The second control timeout covers the direct send
// with room to spare.
const wolCallBound = relayRequestTimeout + 2*controlRequestTimeout

// capacityDrainMargin is added to the longest provider call at shutdown.
const capacityDrainMargin = time.Second

// StopCeiling is the longest a server waits for [Scheduler.Run] to return
// once it has stopped it, before it closes the bus and the store. It sits
// inside the server's 30 s shutdown budget, which the service managers'
// stop timeouts are sized for. Run's own drains are capped to finish inside
// it ([maxCapacityDrain]).
const StopCeiling = 30 * time.Second

// stopCeilingMargin is what [maxCapacityDrain] leaves of [StopCeiling], after
// the revocation drain, for Run's goroutines to exit.
const stopCeilingMargin = 5 * time.Second

// maxCapacityDrain caps [Scheduler.capacityDrainTimeout], so Run closes the
// provider calls' write gate and returns inside [StopCeiling]. A call still
// running when it ends is logged and cut off: its request was recorded before
// it was made, so the next leader repeats it.
const maxCapacityDrain = StopCeiling - revokeDrainTimeout - stopCeilingMargin

// capacityDrainTimeout bounds how long [Scheduler.Run] waits at shutdown for
// provider calls still running: the longest one provider action can take (a
// wake_on_lan call, or the longest-running configured script, killed at its
// timeout), plus a margin, and never more than [maxCapacityDrain]. A call that
// outlasts it (a long script, or one that chains actions, such as a list and
// then a retire of what it found) is cut off and its later results are not
// applied.
func (s *Scheduler) capacityDrainTimeout() time.Duration {
	if d := s.capState.act.drainTimeout; d > 0 {
		return d
	}
	longest := wolCallBound
	for _, p := range s.cfg.ScriptProviders {
		longest = max(longest, p.MaxRunTime())
	}
	return min(longest+capacityDrainMargin, maxCapacityDrain)
}

// StopTimeout bounds how long [Scheduler.Run] waits, once its context ends,
// for the calls it drains before returning: capacity provider calls (see
// [Scheduler.capacityDrainTimeout]) and then broker revocations of removed
// workers. It stays below [StopCeiling] whatever the providers' timeouts.
func (s *Scheduler) StopTimeout() time.Duration {
	return s.capacityDrainTimeout() + revokeDrainTimeout
}

// AbandonCapacityCalls closes the provider calls' write gate, waiting only for
// a write already under way. A server that stops waiting for [Scheduler.Run]
// calls it before it closes the store, so no provider call still running
// writes to a closed store. Calling it again, or after Run has returned, does
// nothing more.
func (s *Scheduler) AbandonCapacityCalls() {
	s.capState.act.gate.close()
}

// writeGate lets provider calls write to the store until it is closed.
// Closing it waits for a write already under way, so once close returns no
// provider call writes again.
type writeGate struct {
	mu     sync.RWMutex
	closed bool
}

// do runs f, which writes to the store, and reports true, or runs nothing and
// reports false once the gate is closed. f must not call do.
func (g *writeGate) do(f func()) bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.closed {
		return false
	}
	f()
	return true
}

func (g *writeGate) close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.closed = true
}

func (g *writeGate) isClosed() bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.closed
}

// powerManaged reports whether the farm's capacity groups may have moved a
// worker's power state, as the last sweep found: a group whose provider acts,
// or a member in a state other than awake. Registration reads the prior
// worker row only then, so a farm whose groups are all external, or that has
// none, makes no extra read per registration. Until the first sweep it is
// false.
func (s *Scheduler) powerManaged() bool {
	return s.capState.powerManaged.Load()
}

// notePowerManaged records, for [Scheduler.powerManaged], what this sweep
// read.
func (s *Scheduler) notePowerManaged(groups []store.CapacityGroup, members map[string][]store.Worker) {
	managed := false
	for _, g := range groups {
		if g.Provider != store.CapacityProviderExternal {
			managed = true
			break
		}
		for _, w := range members[g.ID] {
			if w.EffectivePowerState() != store.PowerAwake {
				managed = true
				break
			}
		}
	}
	s.capState.powerManaged.Store(managed)
}

// expireCapacityTransitions fails the overdue wakes and aborts the overdue
// retirements of every group of groups, enabled or not, whatever its
// provider, and updates members (by group ID) to match. A group with no
// overdue member, by members, makes no write. It returns true when a write
// was fenced, and the sweep stops.
func (s *Scheduler) expireCapacityTransitions(
	ctx context.Context, now time.Time, groups []store.CapacityGroup, members map[string][]store.Worker,
) bool {
	for _, g := range groups {
		cutoff := now.Add(-g.BootTimeout)
		for _, t := range s.overdueTransitions(g) {
			if !overdue(members[g.ID], t.from, cutoff) {
				continue
			}
			moved, err := t.move(ctx, g.ID, cutoff, now, t.reason)
			if s.stopIfFenced(ctx, err) {
				return true
			}
			s.warnUnlessStopped(ctx, err, "scheduler: capacity sweep: "+t.failMsg, slog.String("group", g.Name))
			for _, id := range moved {
				s.logger.WarnContext(ctx, t.logMsg, slog.String("group", g.Name), slog.String("worker_id", id), slog.String("reason", t.reason))
				setMemberPower(members, g.ID, id, t.to, now, t.reason)
				if t.to == store.PowerAwake {
					// A lease request it parked while retiring may take work now.
					s.WakeWorker(id)
				}
			}
			if len(moved) > 0 {
				s.countCapacity(g, func() { t.count(float64(len(moved))) })
				s.recordLastAction(g.ID, capacity.ActionRecord{
					Action: t.action, Result: resultExpired, Message: memberMessage(moved[len(moved)-1], t.reason), At: now,
				})
			}
		}
	}
	return false
}

// overdueTransition is a power transition that moves the members of a group
// left in a state for longer than its boot timeout.
type overdueTransition struct {
	from, to store.PowerState
	reason   string
	move     func(ctx context.Context, groupID string, cutoff, now time.Time, reason string) ([]string, error)
	action   string
	count    func(n float64)
	failMsg  string
	logMsg   string
}

// overdueTransitions are g's two overdue transitions: a wake that did not
// register fails, and a retirement that did not complete returns to awake.
func (s *Scheduler) overdueTransitions(g store.CapacityGroup) [2]overdueTransition {
	return [2]overdueTransition{
		{
			from: store.PowerWaking, to: store.PowerWakeFailed,
			reason: fmt.Sprintf("did not register within boot_timeout (%s) after a wake request", g.BootTimeout),
			move:   s.store.FailWakes, action: actionWake,
			count:   func(n float64) { s.metrics.CapacityBootTimeoutsTotal.WithLabelValues(g.Name).Add(n) },
			failMsg: "fail overdue wakes failed", logMsg: "scheduler: capacity group member did not wake",
		},
		{
			from: store.PowerRetiring, to: store.PowerAwake,
			reason: fmt.Sprintf("retirement did not complete within boot_timeout (%s)", g.BootTimeout),
			move:   s.store.ExpireRetirements, action: actionRetire,
			count: func(n float64) {
				s.metrics.CapacityActionsTotal.WithLabelValues(g.Name, actionRetire, resultExpired).Add(n)
			},
			failMsg: "expire overdue retirements failed",
			logMsg:  "scheduler: capacity group member did not retire; it is awake again",
		},
	}
}

// overdue reports whether any of members has been in state since before
// cutoff.
func overdue(members []store.Worker, state store.PowerState, cutoff time.Time) bool {
	for _, w := range members {
		if w.PowerState == state && w.PowerStateAt != nil && w.PowerStateAt.Before(cutoff) {
			return true
		}
	}
	return false
}

// setMemberPower records in members that workerID of groupID moved to state
// at now with reason, as the store did.
func setMemberPower(members map[string][]store.Worker, groupID, workerID string, state store.PowerState, now time.Time, reason string) {
	for i := range members[groupID] {
		if w := &members[groupID][i]; w.ID == workerID {
			at := now
			w.PowerState, w.PowerStateAt, w.PowerReason = state, &at, reason
		}
	}
}

// actuateCapacity acts on the evaluation of groups, the enabled groups, in
// statuses order, with sweepMu held. members holds every group's members by
// group ID, and records the launch records read for the sweep. A sized script
// group's status gets its Requested and Degraded here; each status's
// LastAction is filled when it is published ([Scheduler.publishCapacity]), so
// a call that finishes meanwhile is not lost. It returns true when a write was
// fenced, and the sweep stops.
func (s *Scheduler) actuateCapacity(
	ctx context.Context, now time.Time, groups []store.CapacityGroup, members map[string][]store.Worker,
	statuses []capacity.Status, records launchRecords,
) bool {
	act := &s.capState.act
	act.mu.Lock()
	act.lastAction = keepGroups(act.lastAction, groups)
	act.mu.Unlock()
	act.unconfigured = keepGroups(act.unconfigured, groups)
	act.launches = keepGroups(act.launches, groups)
	for i, g := range groups {
		if p, ok := s.groupProvider(ctx, now, g); ok {
			if s.actOnGroup(ctx, now, g, p, members[g.ID], &statuses[i], records) {
				return true
			}
		}
	}
	return false
}

// pruneCapacitySeries records groups, the farm's, as the groups a counter may
// be added to, and deletes every series of a group name that had a counter
// and is no longer among them: a group deleted or renamed, whether or not it
// was ever evaluated.
func (s *Scheduler) pruneCapacitySeries(groups []store.CapacityGroup) {
	act := &s.capState.act
	act.mu.Lock()
	defer act.mu.Unlock()
	live := make(map[string]string, len(groups))
	names := make(map[string]bool, len(groups))
	for _, g := range groups {
		live[g.ID] = g.Name
		names[g.Name] = true
	}
	act.liveGroups = live
	for name := range act.seriesGroups {
		if !names[name] {
			s.deleteCapacitySeries(name)
			delete(act.seriesGroups, name)
		}
	}
}

// countCapacity runs inc, which adds to one of g's counters, unless g is no
// longer in the farm under g.Name (see actuatorState.liveGroups).
func (s *Scheduler) countCapacity(g store.CapacityGroup, inc func()) {
	act := &s.capState.act
	act.mu.Lock()
	defer act.mu.Unlock()
	if name, ok := act.liveGroups[g.ID]; !ok || name != g.Name {
		return
	}
	if act.seriesGroups == nil {
		act.seriesGroups = map[string]struct{}{}
	}
	act.seriesGroups[g.Name] = struct{}{}
	inc()
}

// groupProvider returns g's provider, or false when g does not act: its
// provider is external, or a script the server's configuration does not
// define, which is recorded as g's last action and logged once.
func (s *Scheduler) groupProvider(ctx context.Context, now time.Time, g store.CapacityGroup) (boundProvider, bool) {
	switch name, isScript := g.Provider.ScriptName(); {
	case g.Provider == store.CapacityProviderWakeOnLAN:
		return boundProvider{capacityProvider: s.newWOLProvider(), kind: providerKindWakeOnLAN}, true
	case !isScript:
		return boundProvider{}, false
	case !s.scriptConfigured(name):
		s.noteUnconfiguredScript(ctx, now, g, name)
		return boundProvider{}, false
	default:
		return boundProvider{capacityProvider: s.newScriptProvider(name, s.cfg.ScriptProviders[name]), kind: providerKindScript}, true
	}
}

// scriptConfigured reports whether the server's configuration defines the
// script provider name.
func (s *Scheduler) scriptConfigured(name string) bool {
	_, ok := s.cfg.ScriptProviders[name]
	return ok
}

// configuredScript returns the script provider g names, and whether g names
// one this server configures.
func (s *Scheduler) configuredScript(g store.CapacityGroup) (string, bool) {
	name, ok := g.Provider.ScriptName()
	return name, ok && s.scriptConfigured(name)
}

// noteUnconfiguredScript records that g names the script provider name,
// which the server's configuration does not define, and logs it once per
// group and provider.
func (s *Scheduler) noteUnconfiguredScript(ctx context.Context, now time.Time, g store.CapacityGroup, name string) {
	act := &s.capState.act
	msg := fmt.Sprintf("provider script:%s is not configured on this server", name)
	rec := capacity.ActionRecord{Action: actionConfigure, Result: resultFailed, Message: msg, At: now}
	if prev := act.lastActionOf(g.ID); prev == nil || prev.Action != rec.Action || prev.Message != rec.Message {
		s.recordLastAction(g.ID, rec)
	}
	if act.unconfigured[g.ID] == g.Provider {
		return
	}
	act.unconfigured[g.ID] = g.Provider
	s.logger.WarnContext(ctx, "scheduler: capacity group names a provider this server does not configure; it does not act",
		slog.String("group", g.Name), slog.String("provider", string(g.Provider)),
		slog.String("remediation", "define it under capacity.providers in the server's configuration file"))
}

// actOnGroup records g's wake and retire candidates as waking and retiring,
// applies a sized script group's launch record (see [Scheduler.planLaunches]),
// then sends the provider every intent of g this process has not sent. It
// returns true when a write was fenced.
func (s *Scheduler) actOnGroup(
	ctx context.Context, now time.Time, g store.CapacityGroup, p boundProvider, members []store.Worker, st *capacity.Status,
	records launchRecords,
) bool {
	moved := false
	if len(st.WakeCandidates) > 0 {
		ids, err := s.store.MarkWaking(ctx, g.ID, st.WakeCandidates, now)
		if s.stopIfFenced(ctx, err) {
			return true
		}
		s.warnUnlessStopped(ctx, err, "scheduler: capacity sweep: mark members waking failed", slog.String("group", g.Name))
		if len(ids) > 0 {
			moved = true
			s.logger.InfoContext(ctx, "scheduler: capacity group waking members",
				slog.String("group", g.Name), slog.Any("worker_ids", ids))
		}
	}
	if p.kind == providerKindScript && g.Kind == store.CapacityGroupSized {
		st.RetireCandidates = s.retirableByInstance(ctx, g, members, st.RetireCandidates)
	}
	for _, id := range st.RetireCandidates {
		w, ok, err := s.store.BeginRetirement(ctx, g.ID, id, now)
		if s.stopIfFenced(ctx, err) {
			return true
		}
		s.warnUnlessStopped(ctx, err, "scheduler: capacity sweep: begin retirement failed",
			slog.String("group", g.Name), slog.String("worker_id", id))
		if ok {
			moved = true
			s.capState.act.noteBegun(w)
			s.logger.InfoContext(ctx, "scheduler: capacity group retiring an idle member",
				slog.String("group", g.Name), slog.String("worker_id", id), slog.String("action", g.RetireAction))
		}
	}
	if moved {
		// The rows as the store recorded them, so each intent's key is the
		// one every later read gives.
		fresh, err := s.store.ListCapacityGroupMembers(ctx, g.ID)
		if err != nil {
			s.warnUnlessStopped(ctx, err, "scheduler: capacity sweep: re-read members failed; their requests go next tick",
				slog.String("group", g.Name))
			return false
		}
		members = fresh
	}
	var gc groupCall
	if p.kind == providerKindScript && g.Kind == store.CapacityGroupSized {
		var fenced bool
		if gc, fenced = s.planLaunches(ctx, now, g, st, records); fenced {
			return true
		}
	}
	s.dispatchIntents(ctx, now, g, p, members, gc)
	return false
}

// retirableByInstance returns the candidates of g, a sized script group, that
// report a cloud instance ID among members. A script retires a sized group's
// member by its instance, so one with none could not be retired: the request
// would fail and put it back to awake, once every idle timeout, for good. Each
// one left out is warned about once per process.
func (s *Scheduler) retirableByInstance(
	ctx context.Context, g store.CapacityGroup, members []store.Worker, candidates []string,
) []string {
	var out []string
	for _, id := range candidates {
		i := slices.IndexFunc(members, func(w store.Worker) bool { return w.ID == id })
		if i < 0 || members[i].CloudInstanceID != "" {
			out = append(out, id)
			continue
		}
		s.warnCapacityOnce(ctx, id, "retire", g.ID,
			"scheduler: capacity group member not retired: the member reports no cloud instance ID, "+
				"so its provider cannot name its instance",
			slog.String("group", g.Name),
			slog.String("remediation", "set worker.cloud_instance_id.source on the group's machines"))
	}
	return out
}

// capacityCall is everything one provider call for a group does, in order.
type capacityCall struct {
	wake, retire []store.Worker
	// firstRetire holds the IDs of the retire members whose retirement this
	// process began, as opposed to one it found recorded and re-sends.
	firstRetire map[string]bool
	group       groupCall
}

// noteBegun records that this process moved w to retiring (see
// actuatorState.begun). It is called with the capacity sweep's sweepMu held.
func (a *actuatorState) noteBegun(w store.Worker) {
	if a.begun == nil {
		a.begun = map[intentKey]struct{}{}
	}
	a.begun[memberIntent(w)] = struct{}{}
}

// dispatchIntents starts one provider call for g, unless one is running, with
// every waking or retiring member whose intent this process has not sent and
// the group-level requests gc carries. A disabled member is never sent
// anything, and a retiring one only while it is online to answer. Each intent
// is recorded before the call starts.
func (s *Scheduler) dispatchIntents(
	ctx context.Context, now time.Time, g store.CapacityGroup, p boundProvider, members []store.Worker, gc groupCall,
) {
	act := &s.capState.act
	if act.isInFlight(g.ID) {
		return
	}
	c := capacityCall{group: gc}
	keys := slices.Clone(gc.keys)
	for _, w := range members {
		if w.Disabled {
			continue
		}
		key := memberIntent(w)
		if _, sent := act.dispatched[key]; sent {
			continue
		}
		switch {
		case w.PowerState == store.PowerWaking:
			c.wake = append(c.wake, w)
		case w.PowerState == store.PowerRetiring && w.Status == store.WorkerStatusOnline:
			c.retire = append(c.retire, w)
			if _, first := act.begun[key]; first {
				if c.firstRetire == nil {
					c.firstRetire = map[string]bool{}
				}
				c.firstRetire[w.ID] = true
			}
		default:
			continue
		}
		keys = append(keys, key)
	}
	if len(keys) == 0 {
		return
	}
	if act.dispatched == nil {
		act.dispatched = map[intentKey]struct{}{}
	}
	for _, k := range keys {
		act.dispatched[k] = struct{}{}
		delete(act.begun, k)
	}
	act.setInFlight(g.ID, true)
	// The call keeps ctx's values, the leader epoch among them, so its
	// writes stay fenced, but not its cancellation.
	callCtx := context.WithoutCancel(ctx)
	if !act.calls.Go(func() {
		defer act.setInFlight(g.ID, false)
		s.runCapacityCall(callCtx, now, g, p, c)
	}) {
		act.setInFlight(g.ID, false)
	}
}

// memberIntent is w's current intent key.
func memberIntent(w store.Worker) intentKey {
	k := intentKey{subject: "worker:" + w.ID, intent: string(w.PowerState)}
	if w.PowerStateAt != nil {
		k.at = w.PowerStateAt.UnixNano()
	}
	return k
}

// runCapacityCall calls p for c, on a goroutine of its own, and applies the
// results: the wakes, the retirements, then the group-level requests. A call
// stops between steps once this server is deposed or shutdown has cut it off.
// now is the tick that dispatched it, which stamps every record and write it
// makes.
func (s *Scheduler) runCapacityCall(ctx context.Context, now time.Time, g store.CapacityGroup, p boundProvider, c capacityCall) {
	if len(c.wake) > 0 {
		s.logger.InfoContext(ctx, "scheduler: capacity provider waking members",
			slog.String("group", g.Name), slog.Any("worker_ids", workerIDs(c.wake)))
		s.applyWakeResults(ctx, now, g, c.wake, p.wake(ctx, g, c.wake))
	}
	if len(c.retire) > 0 && s.mayActOn() {
		s.logger.InfoContext(ctx, "scheduler: capacity provider retiring members",
			slog.String("group", g.Name), slog.String("action", g.RetireAction), slog.Any("worker_ids", workerIDs(c.retire)))
		s.applyRetireResults(ctx, now, g, p.kind, c, p.retire(ctx, g, c.retire))
	}
	s.runGroupCall(ctx, now, g, p, c.group)
}

// applyWakeResults records each wake result. A woken member is already
// waking, and its registration makes it awake, so success writes nothing; a
// failure leaves it waking for the boot timeout to settle.
func (s *Scheduler) applyWakeResults(
	ctx context.Context, now time.Time, g store.CapacityGroup, members []store.Worker, results map[string]actionResult,
) {
	if s.deposed() {
		return
	}
	for _, w := range members {
		r := resultFor(results, w.ID)
		s.recordAction(ctx, now, g, actionWake, w.ID, r)
	}
}

// applyRetireResults records each retire result of c's members and applies it
// through a fenced write: success as the provider's kind says
// ([Scheduler.retireAccepted]), and a failure that the kind says ends the
// retirement ([abortsRetirement]) by putting the member back to awake at once,
// which also releases a lease request it parked meanwhile; any other failure
// leaves the member retiring for the boot timeout to settle. A sized script
// group's launch record is lowered for the members the script retired first
// ([Scheduler.lowerRequested]), before any of them is deleted. A fenced write
// stops it, and so does shutdown closing the actuator's gate.
//
// A wake_on_lan request that reached no one ([actionResult.undelivered]) is
// read by who began the retirement. When this process did, the request was
// its first: the worker is online but not listening on its control subject,
// nothing was delivered, and the retirement is refused, so the member goes
// back to awake rather than being held out of work for a boot timeout on
// every idle timeout. A request re-sent for a retirement recorded before this
// process (after a restart or a failover) keeps the failure: the worker may
// have carried out the first request and left already, and its queued
// deregistration finishes the retirement, or the boot timeout settles it.
func (s *Scheduler) applyRetireResults(
	ctx context.Context, now time.Time, g store.CapacityGroup, kind providerKind, c capacityCall,
	results map[string]actionResult,
) {
	if s.deposed() {
		return
	}
	members := c.retire
	if kind == providerKindScript && g.Kind == store.CapacityGroupSized {
		s.lowerRequestedForRetired(ctx, now, g, members, results)
	}
	anyFinished := false
	for _, w := range members {
		r := resultFor(results, w.ID)
		if r.undelivered && c.firstRetire[w.ID] {
			r = actionResult{Result: resultRefused, Message: "worker is not listening on its control subject"}
		}
		s.recordAction(ctx, now, g, actionRetire, w.ID, r)
		var err error
		var aborted, finished bool
		open := s.capState.act.gate.do(func() {
			switch {
			case r.Result == resultOK:
				finished, err = s.retireAccepted(ctx, now, kind, g, w)
			case abortsRetirement(kind, r.Result):
				aborted, err = s.store.AbortRetirement(ctx, w.ID, now, r.Message)
			}
		})
		if !open || s.stopIfFenced(ctx, err) {
			return
		}
		anyFinished = anyFinished || finished
		if aborted {
			// A lease request it parked while retiring may take work now.
			s.WakeWorker(w.ID)
		}
		s.warnUnlessStopped(ctx, err, "scheduler: capacity provider: apply retire result failed",
			slog.String("group", g.Name), slog.String("worker_id", w.ID))
	}
	if anyFinished {
		s.capState.act.gate.do(func() { s.refreshWorkerGauge(ctx) })
	}
}

// lowerRequestedForRetired lowers a sized script group's requested size by
// the members of members its provider retired ([Scheduler.lowerRequested]).
func (s *Scheduler) lowerRequestedForRetired(
	ctx context.Context, now time.Time, g store.CapacityGroup, members []store.Worker, results map[string]actionResult,
) {
	var retired []string
	for _, w := range members {
		if resultFor(results, w.ID).Result == resultOK {
			retired = append(retired, w.ID)
		}
	}
	if len(retired) > 0 {
		s.lowerRequested(ctx, now, g, retired)
	}
}

// abortsRetirement reports whether a retire result of a provider of kind puts
// the member back to awake at once. A worker refusing or not answering a
// wake_on_lan sleep keeps running, but any other failure (an unreadable reply,
// or no responders to a re-sent request, see [Scheduler.applyRetireResults])
// may mean it already left, so that waits for the boot timeout, and for a
// deregistration that finishes the retirement meanwhile. A script that did not
// carry out a retire has left the machine running, whatever the reason.
func abortsRetirement(kind providerKind, result string) bool {
	if kind == providerKindScript {
		return result != resultOK
	}
	return result == resultRefused || result == resultTimeout
}

// retireAccepted applies a provider's success at retiring w, by provider
// kind. A wake_on_lan member deregisters as it goes to sleep or shuts down,
// and that deregistration finishes its retirement
// ([Scheduler.handleWorkerDeregister]), so nothing is written for it here. A
// script has stopped the machine, so its retirement finishes now
// ([Scheduler.finishProviderRetirement]). finished reports whether a
// retirement finished.
func (s *Scheduler) retireAccepted(
	ctx context.Context, now time.Time, kind providerKind, g store.CapacityGroup, w store.Worker,
) (finished bool, err error) {
	if kind != providerKindScript {
		return false, nil
	}
	return s.finishProviderRetirement(ctx, now, g, w)
}

// resultFor is results[id], or a failure when the provider gave none.
func resultFor(results map[string]actionResult, id string) actionResult {
	if r, ok := results[id]; ok {
		return r
	}
	return actionResult{Result: resultFailed, Message: "the provider reported no result for this member"}
}

// recordAction counts one member's action result, records it as g's last
// action and logs it: a failure at WARN.
func (s *Scheduler) recordAction(ctx context.Context, now time.Time, g store.CapacityGroup, action, workerID string, r actionResult) {
	s.recordResult(ctx, now, g, action, memberMessage(workerID, r.Message), r, slog.String("worker_id", workerID))
}

// recordGroupAction counts a group-level action result (a grow, a list, an
// orphan retirement), records it as g's last action and logs it: a failure
// at WARN.
func (s *Scheduler) recordGroupAction(ctx context.Context, now time.Time, g store.CapacityGroup, action string, r actionResult) {
	s.recordResult(ctx, now, g, action, r.Message, r)
}

// recordResult counts the action result r, records it as g's last action with
// the message recorded, and logs it with extra after the group: ok at INFO,
// any other result at WARN.
func (s *Scheduler) recordResult(
	ctx context.Context, now time.Time, g store.CapacityGroup, action, recorded string, r actionResult, extra ...slog.Attr,
) {
	s.countCapacity(g, func() { s.metrics.CapacityActionsTotal.WithLabelValues(g.Name, action, r.Result).Inc() })
	s.recordLastAction(g.ID, capacity.ActionRecord{Action: action, Result: r.Result, Message: recorded, At: now})
	level, msg := slog.LevelInfo, "scheduler: capacity provider action done"
	if r.Result != resultOK {
		level, msg = slog.LevelWarn, "scheduler: capacity provider action did not succeed"
	}
	attrs := append([]slog.Attr{slog.String("group", g.Name)}, extra...)
	attrs = append(attrs, slog.String("action", action), slog.String("result", r.Result), slog.String("message", r.Message))
	s.logger.LogAttrs(ctx, level, msg, attrs...)
}

// memberMessage prefixes msg with the member it concerns.
func memberMessage(workerID, msg string) string {
	return workerID + ": " + msg
}

// recordLastAction stores rec as groupID's last action, and in its published
// status, so the API sees it before the next sweep.
func (s *Scheduler) recordLastAction(groupID string, rec capacity.ActionRecord) {
	act := &s.capState.act
	act.mu.Lock()
	if act.lastAction == nil {
		act.lastAction = map[string]capacity.ActionRecord{}
	}
	act.lastAction[groupID] = rec
	act.mu.Unlock()

	cs := &s.capState
	cs.mu.Lock()
	defer cs.mu.Unlock()
	for i := range cs.statuses {
		if cs.statuses[i].GroupID == groupID {
			r := rec
			cs.statuses[i].LastAction = &r
		}
	}
}

// lastActionOf returns a copy of groupID's last action, or nil.
func (a *actuatorState) lastActionOf(groupID string) *capacity.ActionRecord {
	a.mu.Lock()
	defer a.mu.Unlock()
	rec, ok := a.lastAction[groupID]
	if !ok {
		return nil
	}
	return &rec
}

func (a *actuatorState) isInFlight(groupID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.inFlight[groupID]
}

func (a *actuatorState) setInFlight(groupID string, on bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !on {
		delete(a.inFlight, groupID)
		return
	}
	if a.inFlight == nil {
		a.inFlight = map[string]bool{}
	}
	a.inFlight[groupID] = true
}

// workerIDs returns the IDs of ws, in order.
func workerIDs(ws []store.Worker) []string {
	out := make([]string, len(ws))
	for i, w := range ws {
		out[i] = w.ID
	}
	return out
}

// closeCapacityCalls refuses new provider calls, waits for those running for
// at most [Scheduler.capacityDrainTimeout], and then closes the actuator's
// write gate, which waits for a write already under way. After it returns no
// provider call writes to the store, so none can reach a store the server
// closes next: a call still running is logged once here and goes on writing
// nothing and starting no further provider action. Its request was recorded
// before it was made, so the next leader repeats it, or the boot timeout
// settles what it left.
func (s *Scheduler) closeCapacityCalls(ctx context.Context) {
	act := &s.capState.act
	wait := s.capacityDrainTimeout()
	finished := act.calls.Close(wait)
	act.gate.close()
	if !finished {
		s.logger.WarnContext(ctx, "scheduler: a capacity provider call was still running at shutdown; "+
			"its result is not applied, and the next leader repeats the request it recorded",
			slog.Duration("waited", wait))
	}
}
