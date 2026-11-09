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
// It never acts on a count: only on the named candidates the policy returned.

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/uberware/sqi/internal/capacity"
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
	actionRetire    = "retire"
	actionConfigure = "configure"
)

// actionResult is what a provider reports for one action.
type actionResult struct{ Result, Message string }

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

// intentKey identifies one recorded member intent: a member in a power state
// recorded at a time. A new transition makes a new key.
type intentKey struct {
	workerID string
	state    store.PowerState
	stateAt  int64 // PowerStateAt as Unix nanoseconds; 0 when unset
}

// actuatorState is what the actuator keeps between ticks. It lives in memory
// only (see the file comment).
type actuatorState struct {
	// dispatched and unconfigured are touched only with the capacity
	// sweep's sweepMu held.
	//
	// dispatched holds every intent this process has sent; it is never
	// pruned, so each intent is sent at most once per process. Each entry
	// is a few words, one per transition the server has made.
	dispatched map[intentKey]struct{}
	// unconfigured is, per group ID, the script provider whose absence from
	// the configuration was last logged.
	unconfigured map[string]store.CapacityProvider

	// mu guards inFlight and lastAction, which provider calls update from
	// their own goroutines. It is a leaf: nothing is locked while it is
	// held. The publishing sweep takes it inside capacityState.mu, so the
	// order is sweepMu, then capacityState.mu, then mu; a provider call takes
	// mu and capacityState.mu one at a time, never both.
	mu sync.Mutex
	// inFlight holds the groups, by ID, with a provider call running.
	inFlight map[string]bool
	// lastAction is each group's latest provider action, by group ID.
	lastAction map[string]capacity.ActionRecord

	// calls runs the provider calls. [Scheduler.Run] closes it at shutdown
	// and waits, briefly, for the calls still running.
	calls backgroundCalls
}

// capacityCallDrainTimeout bounds how long [Scheduler.Run] waits at shutdown
// for provider calls still running: the longest a wake_on_lan call takes,
// plus a little.
const capacityCallDrainTimeout = controlRequestTimeout + time.Second

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
			if w.PowerState != store.PowerAwake && w.PowerState != "" {
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
		if overdue(members[g.ID], store.PowerWaking, cutoff) {
			reason := fmt.Sprintf("did not register within boot_timeout (%s) after a wake request", g.BootTimeout)
			failed, err := s.store.FailWakes(ctx, g.ID, cutoff, now, reason)
			if s.stopIfFenced(ctx, err) {
				return true
			}
			s.warnUnlessStopped(ctx, err, "scheduler: capacity sweep: fail overdue wakes failed", slog.String("group", g.Name))
			for _, w := range failed {
				s.logger.WarnContext(ctx, "scheduler: capacity group member did not wake",
					slog.String("group", g.Name), slog.String("worker_id", w.ID), slog.String("reason", reason))
				replaceMember(members, g.ID, w)
			}
			if len(failed) > 0 {
				s.metrics.CapacityBootTimeoutsTotal.WithLabelValues(g.Name).Add(float64(len(failed)))
				s.recordLastAction(g.ID, capacity.ActionRecord{
					Action: actionWake, Result: resultExpired, Message: memberMessage(failed[len(failed)-1].ID, reason), At: now,
				})
			}
		}
		if overdue(members[g.ID], store.PowerRetiring, cutoff) {
			reason := fmt.Sprintf("retirement did not complete within boot_timeout (%s)", g.BootTimeout)
			expired, err := s.store.ExpireRetirements(ctx, g.ID, cutoff, now, reason)
			if s.stopIfFenced(ctx, err) {
				return true
			}
			s.warnUnlessStopped(ctx, err, "scheduler: capacity sweep: expire overdue retirements failed", slog.String("group", g.Name))
			for _, id := range expired {
				s.logger.WarnContext(ctx, "scheduler: capacity group member did not retire; it is awake again",
					slog.String("group", g.Name), slog.String("worker_id", id), slog.String("reason", reason))
				setMemberPower(members, g.ID, id, store.PowerAwake, now, reason)
				s.metrics.CapacityActionsTotal.WithLabelValues(g.Name, actionRetire, resultExpired).Inc()
			}
			if len(expired) > 0 {
				s.recordLastAction(g.ID, capacity.ActionRecord{
					Action: actionRetire, Result: resultExpired, Message: memberMessage(expired[len(expired)-1], reason), At: now,
				})
			}
		}
	}
	return false
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

// replaceMember replaces the member of groupID with w's ID by w.
func replaceMember(members map[string][]store.Worker, groupID string, w store.Worker) {
	for i := range members[groupID] {
		if members[groupID][i].ID == w.ID {
			members[groupID][i] = w
		}
	}
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
// group ID. Each status's LastAction is filled when it is published
// ([Scheduler.publishCapacity]), so a call that finishes meanwhile is not
// lost. It returns true when a write was fenced, and the sweep stops.
func (s *Scheduler) actuateCapacity(
	ctx context.Context, now time.Time, groups []store.CapacityGroup, members map[string][]store.Worker,
	statuses []capacity.Status,
) bool {
	act := &s.capState.act
	act.mu.Lock()
	act.lastAction = keepGroups(act.lastAction, groups)
	act.mu.Unlock()
	act.unconfigured = keepGroups(act.unconfigured, groups)
	for i, g := range groups {
		if p, ok := s.groupProvider(ctx, now, g); ok {
			if s.actOnGroup(ctx, now, g, p, members[g.ID], statuses[i]) {
				return true
			}
		}
	}
	return false
}

// groupProvider returns g's provider, or false when g does not act: its
// provider is external, or a script the server's configuration does not
// define, which is recorded as g's last action and logged once.
func (s *Scheduler) groupProvider(ctx context.Context, now time.Time, g store.CapacityGroup) (boundProvider, bool) {
	switch name, isScript := strings.CutPrefix(string(g.Provider), "script:"); {
	case g.Provider == store.CapacityProviderWakeOnLAN:
		return boundProvider{capacityProvider: s.newWOLProvider(), kind: providerKindWakeOnLAN}, true
	case !isScript:
		return boundProvider{}, false
	case !s.scriptConfigured(name):
		s.noteUnconfiguredScript(ctx, now, g, name)
		return boundProvider{}, false
	default:
		// A configured script provider is not implemented yet: it does not
		// act.
		return boundProvider{}, false
	}
}

// scriptConfigured reports whether the server's configuration defines the
// script provider name.
func (s *Scheduler) scriptConfigured(name string) bool {
	_, ok := s.cfg.ScriptProviders[name]
	return ok
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
// then sends the provider every intent of g this process has not sent. It
// returns true when a write was fenced.
func (s *Scheduler) actOnGroup(
	ctx context.Context, now time.Time, g store.CapacityGroup, p boundProvider, members []store.Worker, st capacity.Status,
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
	for _, id := range st.RetireCandidates {
		_, ok, err := s.store.BeginRetirement(ctx, g.ID, id, now)
		if s.stopIfFenced(ctx, err) {
			return true
		}
		s.warnUnlessStopped(ctx, err, "scheduler: capacity sweep: begin retirement failed",
			slog.String("group", g.Name), slog.String("worker_id", id))
		if ok {
			moved = true
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
	s.dispatchIntents(ctx, now, g, p, members)
	return false
}

// dispatchIntents starts one provider call for g, unless one is running, with
// every waking or retiring member whose intent this process has not sent. A
// disabled member is never sent anything, and a retiring one only while it
// is online to answer. Each intent is recorded before the call starts.
func (s *Scheduler) dispatchIntents(
	ctx context.Context, now time.Time, g store.CapacityGroup, p boundProvider, members []store.Worker,
) {
	act := &s.capState.act
	if act.isInFlight(g.ID) {
		return
	}
	var wake, retire []store.Worker
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
			wake = append(wake, w)
		case w.PowerState == store.PowerRetiring && w.Status == store.WorkerStatusOnline:
			retire = append(retire, w)
		default:
			continue
		}
		if act.dispatched == nil {
			act.dispatched = map[intentKey]struct{}{}
		}
		act.dispatched[key] = struct{}{}
	}
	if len(wake) == 0 && len(retire) == 0 {
		return
	}
	act.setInFlight(g.ID, true)
	// The call keeps ctx's values, the leader epoch among them, so its
	// writes stay fenced, but not its cancellation.
	callCtx := context.WithoutCancel(ctx)
	if !act.calls.Go(func() {
		defer act.setInFlight(g.ID, false)
		s.runCapacityCall(callCtx, now, g, p, wake, retire)
	}) {
		act.setInFlight(g.ID, false)
	}
}

// memberIntent is w's current intent key.
func memberIntent(w store.Worker) intentKey {
	k := intentKey{workerID: w.ID, state: w.PowerState}
	if w.PowerStateAt != nil {
		k.stateAt = w.PowerStateAt.UnixNano()
	}
	return k
}

// runCapacityCall calls p to wake and to retire members of g, on a goroutine
// of its own, and applies the results. now is the tick that dispatched it,
// which stamps every record and write it makes.
func (s *Scheduler) runCapacityCall(
	ctx context.Context, now time.Time, g store.CapacityGroup, p boundProvider, wake, retire []store.Worker,
) {
	if len(wake) > 0 {
		s.logger.InfoContext(ctx, "scheduler: capacity provider waking members",
			slog.String("group", g.Name), slog.Any("worker_ids", workerIDs(wake)))
		s.applyWakeResults(ctx, now, g, wake, p.wake(ctx, g, wake))
	}
	if len(retire) > 0 {
		s.logger.InfoContext(ctx, "scheduler: capacity provider retiring members",
			slog.String("group", g.Name), slog.String("action", g.RetireAction), slog.Any("worker_ids", workerIDs(retire)))
		s.applyRetireResults(ctx, now, g, p.kind, retire, p.retire(ctx, g, retire))
	}
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

// applyRetireResults records each retire result and applies it through a
// fenced write: a refusal or a timeout puts the member back to awake at once;
// success is applied as the provider's kind says ([Scheduler.retireAccepted]);
// any other failure leaves the member retiring for the boot timeout to
// settle. A fenced write stops it.
func (s *Scheduler) applyRetireResults(
	ctx context.Context, now time.Time, g store.CapacityGroup, kind providerKind, members []store.Worker,
	results map[string]actionResult,
) {
	if s.deposed() {
		return
	}
	for _, w := range members {
		r := resultFor(results, w.ID)
		s.recordAction(ctx, now, g, actionRetire, w.ID, r)
		var err error
		switch r.Result {
		case resultOK:
			err = s.retireAccepted(ctx, now, kind, g, w)
		case resultRefused, resultTimeout:
			_, err = s.store.AbortRetirement(ctx, w.ID, now, r.Message)
		}
		if s.stopIfFenced(ctx, err) {
			return
		}
		s.warnUnlessStopped(ctx, err, "scheduler: capacity provider: apply retire result failed",
			slog.String("group", g.Name), slog.String("worker_id", w.ID))
	}
}

// retireAccepted applies a provider's success at retiring w, by provider
// kind. A wake_on_lan member deregisters as it goes to sleep or shuts down,
// and that deregistration finishes its retirement
// ([Scheduler.handleWorkerDeregister]), so nothing is written for it here.
func (*Scheduler) retireAccepted(_ context.Context, _ time.Time, kind providerKind, _ store.CapacityGroup, _ store.Worker) error {
	switch kind {
	case providerKindWakeOnLAN:
		return nil
	default:
		return fmt.Errorf("no rule applies a retirement accepted by a %s provider", kind)
	}
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
	s.metrics.CapacityActionsTotal.WithLabelValues(g.Name, action, r.Result).Inc()
	s.recordLastAction(g.ID, capacity.ActionRecord{
		Action: action, Result: r.Result, Message: memberMessage(workerID, r.Message), At: now,
	})
	attrs := []any{
		slog.String("group", g.Name), slog.String("worker_id", workerID), slog.String("action", action),
		slog.String("result", r.Result), slog.String("message", r.Message),
	}
	if r.Result == resultOK {
		s.logger.InfoContext(ctx, "scheduler: capacity provider action done", attrs...)
		return
	}
	s.logger.WarnContext(ctx, "scheduler: capacity provider action did not succeed", attrs...)
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

// closeCapacityCalls refuses new provider calls and waits, briefly, for those
// running, at shutdown.
func (s *Scheduler) closeCapacityCalls(ctx context.Context) {
	if !s.capState.act.calls.Close(capacityCallDrainTimeout) {
		s.logger.WarnContext(ctx, "scheduler: a capacity provider call was still running at shutdown; "+
			"its member is settled by the next leader's boot timeout",
			slog.Duration("waited", capacityCallDrainTimeout))
	}
}
