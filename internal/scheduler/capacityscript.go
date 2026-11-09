// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

// capacityscript.go is the script capacity provider (an operator-configured
// program, internal/capacity/script) and what the actuator does for a sized
// group that uses one.
//
// A sized group's machines are anonymous until they register, so the server
// grows it by asking for a size and keeps a launch record of what it asked
// for: Requested machines at RequestedAt ([store.CapacityGroupState]). The
// record is written before the provider is called, so a server that fails
// between the two leaves a request the next leader repeats, and each request
// is absolute (a total, never a delta), so repeating it is harmless.
//
// When the machines asked for have not all registered within the group's boot
// timeout, the group degrades: the record falls back to the members online,
// further growth waits two boot timeouts, and the instances that booted but
// never registered (they still bill) are retired by instance ID when the
// provider can list them. sqi never shrinks a sized group by lowering its
// size: it retires named members, and the script's retire lowers the cloud
// group's desired size as it removes them.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/uberware/sqi/internal/capacity"
	"github.com/uberware/sqi/internal/capacity/script"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/ws"
)

// providerKindScript is a provider configured under capacity.providers.
const providerKindScript providerKind = "script"

// Script request actions.
const (
	scriptWake   = "wake"
	scriptGrow   = "grow"
	scriptRetire = "retire"
	scriptList   = "list"
)

// maxInstanceIDLen is the longest instance ID a list may return that sqi uses.
const maxInstanceIDLen = 256

// noListGap is added to a degraded reason when the provider cannot list the
// instances that never registered.
const noListGap = "instances that never registered may still be running; the provider has no list action"

// orphansLogged caps the instance IDs one orphan-cleanup log line names.
const orphansLogged = 20

// scriptProvider is a script capacity provider.
type scriptProvider struct {
	p script.Provider
	// logger receives the script's standard error.
	logger *slog.Logger
	// run runs the script; [script.Run] outside tests.
	run func(context.Context, script.Provider, script.Request, *slog.Logger) (script.Response, error)
}

// newScriptProvider returns the script provider name, configured as p.
func (s *Scheduler) newScriptProvider(name string, p script.Provider) capacityProvider {
	if p.Name == "" {
		p.Name = name
	}
	if f := s.capState.act.newScriptProvider; f != nil {
		return f(p)
	}
	return &scriptProvider{p: p, logger: s.logger, run: script.Run}
}

// wake sends one wake request naming every member; its answer is each one's.
func (sp *scriptProvider) wake(ctx context.Context, g store.CapacityGroup, members []store.Worker) map[string]actionResult {
	_, r := sp.call(ctx, g, script.Request{Action: scriptWake, Members: scriptMembers(members)})
	return sameResult(members, r)
}

// grow asks for desired machines in all; current is the size last asked for.
func (sp *scriptProvider) grow(ctx context.Context, g store.CapacityGroup, desired, current int) actionResult {
	_, r := sp.call(ctx, g, script.Request{Action: scriptGrow, Desired: desired, Current: current})
	return r
}

// retire sends one retire request naming every member; its answer is each
// one's.
func (sp *scriptProvider) retire(ctx context.Context, g store.CapacityGroup, members []store.Worker) map[string]actionResult {
	_, r := sp.call(ctx, g, script.Request{Action: scriptRetire, Members: scriptMembers(members)})
	return sameResult(members, r)
}

// retireInstances sends one retire request naming each instance by its ID
// alone.
func (sp *scriptProvider) retireInstances(ctx context.Context, g store.CapacityGroup, instanceIDs []string) actionResult {
	members := make([]script.Member, len(instanceIDs))
	for i, id := range instanceIDs {
		members[i] = script.Member{InstanceID: id}
	}
	_, r := sp.call(ctx, g, script.Request{Action: scriptRetire, Members: members})
	return r
}

// list returns the instance IDs the script reports that sqi may use (see
// [usableInstanceIDs]). The script's own message is replaced by counts, so no
// ID it returned reaches a log through it.
func (sp *scriptProvider) list(ctx context.Context, g store.CapacityGroup) ([]string, actionResult) {
	resp, r := sp.call(ctx, g, script.Request{Action: scriptList})
	if r.Result != resultOK {
		return nil, r
	}
	ids, dropped := usableInstanceIDs(resp.InstanceIDs)
	r.Message = fmt.Sprintf("%d instances listed", len(ids))
	if dropped > 0 {
		r.Message += fmt.Sprintf("; %d unusable instance IDs ignored", dropped)
	}
	return ids, r
}

// call runs the script for req about g and maps its answer to a result.
func (sp *scriptProvider) call(ctx context.Context, g store.CapacityGroup, req script.Request) (script.Response, actionResult) {
	req.Version = script.Version
	req.Group = script.GroupRef{ID: g.ID, Name: g.Name, Kind: string(g.Kind)}
	resp, err := sp.run(ctx, sp.p, req, sp.logger)
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return resp, actionResult{Result: resultTimeout, Message: err.Error()}
	case err != nil:
		return resp, actionResult{Result: resultFailed, Message: err.Error()}
	}
	who := "provider script:" + sp.p.Name
	r := actionResult{Message: resp.Message}
	switch resp.Result {
	case "ok":
		r.Result = resultOK
		if r.Message == "" {
			r.Message = who + " carried out the " + req.Action
		}
	case "unsupported":
		r.Result = resultUnsupported
		if r.Message == "" {
			r.Message = who + " does not implement " + req.Action
		}
	default:
		r.Result = resultFailed
		if r.Message == "" {
			r.Message = who + " reported an error"
		}
	}
	return resp, r
}

// sameResult gives each of members the result r: a provider answering one
// request for several members.
func sameResult(members []store.Worker, r actionResult) map[string]actionResult {
	out := make(map[string]actionResult, len(members))
	for _, w := range members {
		out[w.ID] = r
	}
	return out
}

// scriptMembers is what a script is told about each of workers.
func scriptMembers(workers []store.Worker) []script.Member {
	out := make([]script.Member, len(workers))
	for i, w := range workers {
		m := script.Member{WorkerID: w.ID, Hostname: w.Hostname, IPAddress: w.IPAddress, InstanceID: w.CloudInstanceID}
		for _, n := range w.Interfaces {
			m.Interfaces = append(m.Interfaces, script.NetInterface{Name: n.Name, MAC: n.MAC, Addrs: slices.Clone(n.Addrs)})
		}
		out[i] = m
	}
	return out
}

// usableInstanceIDs returns the distinct IDs of raw, in order, that sqi may
// compare, send and log: 1 to maxInstanceIDLen bytes of valid UTF-8 with no
// control, format or space character. dropped counts the others.
func usableInstanceIDs(raw []string) (ids []string, dropped int) {
	seen := make(map[string]bool, len(raw))
	for _, id := range raw {
		switch {
		case !usableInstanceID(id):
			dropped++
		case !seen[id]:
			seen[id] = true
			ids = append(ids, id)
		}
	}
	return ids, dropped
}

func usableInstanceID(id string) bool {
	if id == "" || len(id) > maxInstanceIDLen || !utf8.ValidString(id) {
		return false
	}
	return !strings.ContainsFunc(id, func(r rune) bool {
		return unicode.IsControl(r) || unicode.IsSpace(r) || unicode.Is(unicode.Cf, r)
	})
}

// ── launch records ────────────────────────────────────────────────────────────

// launchRecords are the group states read for one sweep, by group ID. read
// is false when none was needed or the read failed; a sized script group then
// neither grows nor degrades on that sweep.
type launchRecords struct {
	byGroup map[string]store.CapacityGroupState
	read    bool
}

// sizedScriptGroup reports whether g is a sized group whose provider is a
// script this server configures.
func (s *Scheduler) sizedScriptGroup(g store.CapacityGroup) bool {
	name, ok := strings.CutPrefix(string(g.Provider), "script:")
	return ok && g.Kind == store.CapacityGroupSized && s.scriptConfigured(name)
}

// readLaunchRecords reads the group states once, when any of groups is a sized
// script group, and holds the actuator's stateMu until release is called, so
// a provider call's result cannot rewrite a state between this read and the
// sweep's own writes. With none it reads nothing and holds nothing.
func (s *Scheduler) readLaunchRecords(ctx context.Context, groups []store.CapacityGroup) (records launchRecords, release func()) {
	if !slices.ContainsFunc(groups, s.sizedScriptGroup) {
		return launchRecords{}, func() {}
	}
	act := &s.capState.act
	act.stateMu.Lock()
	states, err := s.store.ListCapacityGroupStates(ctx)
	if err != nil {
		act.stateMu.Unlock()
		s.warnUnlessStopped(ctx, err, "scheduler: capacity sweep: read launch records failed; sized groups wait a tick")
		return launchRecords{}, func() {}
	}
	return launchRecords{byGroup: states, read: true}, act.stateMu.Unlock
}

// seedPacing gives the policy, for each sized script group with no step-up
// remembered and a launch younger than its boot timeout, that launch as its
// last step-up: a new leader then holds the machines already asked for, and
// keeps the scale-up cooldown, instead of pacing from zero.
func (s *Scheduler) seedPacing(now time.Time, groups []store.CapacityGroup, records launchRecords) {
	cs := &s.capState
	for _, g := range groups {
		rec, ok := records.byGroup[g.ID]
		if !ok || rec.RequestedAt == nil || !s.sizedScriptGroup(g) {
			continue
		}
		if _, stepped := cs.lastStepUp[g.ID]; stepped {
			continue
		}
		if age := now.Sub(*rec.RequestedAt); age < g.BootTimeout && age > -g.BootTimeout {
			cs.lastStepUp[g.ID] = *rec.RequestedAt
			cs.lastDesired[g.ID] = rec.Requested
		}
	}
}

// groupCall is the group-level part of a provider call: a grow, and the
// cleanup of the instances that never registered.
type groupCall struct {
	grow *growRequest
	// orphans asks for the orphan cleanup of the degradation recorded at
	// degradedAt.
	orphans    bool
	degradedAt time.Time
	// keys are the group intents the call carries, recorded when it starts.
	keys []intentKey
}

// growRequest is one absolute grow.
type growRequest struct{ desired, current int }

// planLaunches applies the launch record of g, a sized script group, to its
// evaluation st: it degrades the group when machines asked for did not
// register in time, records a grow when the policy wants more machines and
// no backoff holds, and clears the degradation once a later grow is met. It
// fills st.Requested and st.Degraded, and returns the group-level part of g's
// next call, empty while a call is running. It returns true when a write was
// fenced.
func (s *Scheduler) planLaunches(
	ctx context.Context, now time.Time, g store.CapacityGroup, st *capacity.Status, records launchRecords,
) (groupCall, bool) {
	if !records.read {
		return groupCall{}, false
	}
	rec, ok := records.byGroup[g.ID]
	if !ok {
		rec = store.CapacityGroupState{GroupID: g.ID}
	}
	online := st.OnlineMembers
	var call groupCall
	if s.degradeOverdue(ctx, now, g, &rec, online) {
		return groupCall{}, true
	}
	if !s.capState.act.isInFlight(g.ID) {
		s.planOrphanCleanup(now, g, rec, &call)
		if s.planGrow(ctx, now, g, &rec, st.DesiredMachines, online, &call) {
			return groupCall{}, true
		}
	}
	if s.clearRecovered(ctx, now, g, &rec, online) {
		return groupCall{}, true
	}
	fillLaunchStatus(st, rec)
	return call, false
}

// degradeOverdue degrades g when fewer than the machines asked for are online
// a boot timeout after they were asked for: the record falls back to online,
// and growth backs off for two boot timeouts. It returns true when the write
// was fenced.
func (s *Scheduler) degradeOverdue(ctx context.Context, now time.Time, g store.CapacityGroup, rec *store.CapacityGroupState, online int) bool {
	if rec.RequestedAt == nil || online >= rec.Requested || !now.After(rec.RequestedAt.Add(g.BootTimeout)) {
		return false
	}
	requested := rec.Requested
	reason := fmt.Sprintf("%d of %d machines did not register within boot_timeout (%s)", requested-online, requested, g.BootTimeout)
	next := *rec
	next.Requested = online
	next.DegradedReason = reason
	next.DegradedAt = new(now)
	next.BackoffUntil = new(now.Add(2 * g.BootTimeout))
	next.UpdatedAt = now
	err := s.store.PutCapacityGroupState(ctx, next)
	if s.stopIfFenced(ctx, err) {
		return true
	}
	if err != nil {
		s.warnUnlessStopped(ctx, err, "scheduler: capacity sweep: record degradation failed", slog.String("group", g.Name))
		return false
	}
	*rec = next
	s.logger.WarnContext(ctx, "scheduler: capacity group degraded: machines it launched did not register",
		slog.String("group", g.Name), slog.String("reason", reason), slog.Int("requested", requested),
		slog.Int("online", online), slog.Time("backoff_until", *next.BackoffUntil))
	return false
}

// planOrphanCleanup adds the orphan cleanup of g's current degradation to
// call when this process has not run it and no launch may still be booting:
// an instance launched within the boot timeout must not look orphaned.
func (s *Scheduler) planOrphanCleanup(now time.Time, g store.CapacityGroup, rec store.CapacityGroupState, call *groupCall) {
	if rec.DegradedAt == nil || (rec.RequestedAt != nil && !now.After(rec.RequestedAt.Add(g.BootTimeout))) {
		return
	}
	key := groupIntent(g.ID, intentOrphans, *rec.DegradedAt)
	if _, sent := s.capState.act.dispatched[key]; sent {
		return
	}
	call.orphans, call.degradedAt = true, *rec.DegradedAt
	call.keys = append(call.keys, key)
}

// planGrow records a grow to desired when it is more than both the size last
// asked for and the members online and no backoff holds, and adds it to call:
// the record is written before the call, so a failure between the two leaves
// a request the next leader repeats. A recorded launch younger than the boot
// timeout that this process has not sent (the previous leader recorded it)
// is repeated as it stands. It returns true when the write was fenced.
func (s *Scheduler) planGrow(
	ctx context.Context, now time.Time, g store.CapacityGroup, rec *store.CapacityGroupState, desired, online int, call *groupCall,
) bool {
	act := &s.capState.act
	backingOff := rec.BackoffUntil != nil && now.Before(*rec.BackoffUntil)
	if current := max(rec.Requested, online); desired > current && !backingOff {
		next := *rec
		next.Requested, next.RequestedAt, next.UpdatedAt = desired, new(now), now
		err := s.store.PutCapacityGroupState(ctx, next)
		if s.stopIfFenced(ctx, err) {
			return true
		}
		if err != nil {
			s.warnUnlessStopped(ctx, err, "scheduler: capacity sweep: record launch failed; nothing was asked for",
				slog.String("group", g.Name))
			return false
		}
		*rec = next
		call.grow = &growRequest{desired: desired, current: current}
		call.keys = append(call.keys, groupIntent(g.ID, intentGrow, now))
		return false
	}
	if rec.RequestedAt == nil || online >= rec.Requested || !now.Before(rec.RequestedAt.Add(g.BootTimeout)) {
		return false
	}
	key := groupIntent(g.ID, intentGrow, *rec.RequestedAt)
	if _, sent := act.dispatched[key]; sent {
		return false
	}
	// The size it was last asked for is this request's own: a script that
	// reads a delta from current does nothing twice.
	call.grow = &growRequest{desired: rec.Requested, current: rec.Requested}
	call.keys = append(call.keys, key)
	return false
}

// clearRecovered clears g's degradation once a grow made after it is met:
// as many members online as asked for. It returns true when the write was
// fenced.
func (s *Scheduler) clearRecovered(ctx context.Context, now time.Time, g store.CapacityGroup, rec *store.CapacityGroupState, online int) bool {
	if rec.DegradedAt == nil || rec.RequestedAt == nil || !rec.RequestedAt.After(*rec.DegradedAt) || online < rec.Requested {
		return false
	}
	next := *rec
	next.DegradedReason, next.DegradedAt, next.BackoffUntil, next.UpdatedAt = "", nil, nil, now
	err := s.store.PutCapacityGroupState(ctx, next)
	if s.stopIfFenced(ctx, err) {
		return true
	}
	if err != nil {
		s.warnUnlessStopped(ctx, err, "scheduler: capacity sweep: clear degradation failed", slog.String("group", g.Name))
		return false
	}
	*rec = next
	s.logger.InfoContext(ctx, "scheduler: capacity group is no longer degraded: the machines it asked for registered",
		slog.String("group", g.Name), slog.Int("requested", rec.Requested), slog.Int("online", online))
	return false
}

// fillLaunchStatus copies rec's requested size and degradation into st.
func fillLaunchStatus(st *capacity.Status, rec store.CapacityGroupState) {
	st.Requested = rec.Requested
	if rec.DegradedReason == "" {
		return
	}
	d := capacity.Degraded{Reason: rec.DegradedReason}
	if rec.DegradedAt != nil {
		d.Since = *rec.DegradedAt
	}
	if rec.BackoffUntil != nil {
		d.BackoffUntil = *rec.BackoffUntil
	}
	st.Degraded = &d
}

// ── group calls ───────────────────────────────────────────────────────────────

// runGroupCall runs the group-level part of a call: the orphan cleanup first,
// so the list it takes holds no instance the grow starts, then the grow.
func (s *Scheduler) runGroupCall(ctx context.Context, now time.Time, g store.CapacityGroup, p boundProvider, c groupCall) {
	if c.orphans && s.mayActOn() {
		s.cleanOrphans(ctx, now, g, p, c.degradedAt)
	}
	if c.grow != nil && s.mayActOn() {
		s.logger.InfoContext(ctx, "scheduler: capacity provider growing group",
			slog.String("group", g.Name), slog.Int("desired", c.grow.desired), slog.Int("current", c.grow.current))
		s.recordGroupAction(ctx, now, g, actionGrow, p.grow(ctx, g, c.grow.desired, c.grow.current))
	}
}

// mayActOn reports whether a running provider call may take its next step:
// this server still leads, and shutdown has not cut the call off.
func (s *Scheduler) mayActOn() bool {
	return !s.deposed() && !s.capState.act.gate.isClosed()
}

// cleanOrphans retires, by instance ID alone, every instance the provider
// lists that no member of g reports, after the degradation recorded at
// degradedAt. The members are read after the list, so a machine that
// registers meanwhile is never taken for an orphan. One online member that
// reports no instance ID would make its own instance look orphaned, so then
// nothing is retired and the members are named in one warning: this runs
// once per degradation, so the warning is too. A provider that cannot list
// leaves the gap in the degraded reason.
func (s *Scheduler) cleanOrphans(ctx context.Context, now time.Time, g store.CapacityGroup, p boundProvider, degradedAt time.Time) {
	ids, r := p.list(ctx, g)
	s.recordGroupAction(ctx, now, g, actionList, r)
	switch {
	case r.Result == resultUnsupported:
		s.updateLaunchRecord(ctx, now, g, func(rec *store.CapacityGroupState) bool {
			if rec.DegradedAt == nil || !rec.DegradedAt.Equal(degradedAt) || strings.Contains(rec.DegradedReason, noListGap) {
				return false
			}
			rec.DegradedReason += "; " + noListGap
			return true
		})
		return
	case r.Result != resultOK:
		return
	}
	// Whatever the provider, a listed ID is compared, sent and logged only
	// once it is known to be usable.
	if ids, _ = usableInstanceIDs(ids); len(ids) == 0 {
		return
	}
	members, err := s.store.ListCapacityGroupMembers(ctx, g.ID)
	if err != nil {
		s.warnUnlessStopped(ctx, err, "scheduler: capacity provider: read members for orphan cleanup failed; nothing retired",
			slog.String("group", g.Name))
		return
	}
	if missing := onlineWithoutInstanceID(members); len(missing) > 0 {
		s.logger.WarnContext(ctx, "scheduler: capacity group orphan cleanup skipped: online members report no cloud instance ID, "+
			"so their own instances would look orphaned",
			slog.String("group", g.Name), slog.Any("worker_ids", missing),
			slog.String("remediation", "set worker.cloud_instance_id.source on the group's machines"))
		return
	}
	orphans := unreported(ids, members)
	if len(orphans) == 0 || !s.mayActOn() {
		return
	}
	s.logger.InfoContext(ctx, "scheduler: capacity provider retiring instances that never registered",
		slog.String("group", g.Name), slog.Int("count", len(orphans)),
		slog.Any("instance_ids", orphans[:min(len(orphans), orphansLogged)]))
	s.recordGroupAction(ctx, now, g, actionRetire, p.retireInstances(ctx, g, orphans))
}

// onlineWithoutInstanceID returns the IDs of the online members of members
// that report no cloud instance ID.
func onlineWithoutInstanceID(members []store.Worker) []string {
	var out []string
	for _, w := range members {
		if w.Status == store.WorkerStatusOnline && w.CloudInstanceID == "" {
			out = append(out, w.ID)
		}
	}
	return out
}

// unreported returns the IDs of ids that no member of members reports, in
// order.
func unreported(ids []string, members []store.Worker) []string {
	known := make(map[string]bool, len(members))
	for _, w := range members {
		if w.CloudInstanceID != "" {
			known[w.CloudInstanceID] = true
		}
	}
	var out []string
	for _, id := range ids {
		if !known[id] {
			out = append(out, id)
		}
	}
	return out
}

// lowerRequested lowers g's launch record by n machines the provider retired,
// to no less than zero. The script's retire lowers the cloud group's desired
// size as it removes them, so the record keeps matching it.
func (s *Scheduler) lowerRequested(ctx context.Context, now time.Time, g store.CapacityGroup, n int) {
	s.updateLaunchRecord(ctx, now, g, func(rec *store.CapacityGroupState) bool {
		if rec.Requested == 0 {
			return false
		}
		rec.Requested = max(rec.Requested-n, 0)
		return true
	})
}

// updateLaunchRecord applies f to g's launch record, read fresh, and stores it
// when f reports a change, from a provider call. It holds the actuator's
// stateMu throughout, so it never interleaves with a sweep's read and writes,
// and writes nothing once shutdown has closed the actuator's gate. A group
// with no record is left alone.
func (s *Scheduler) updateLaunchRecord(ctx context.Context, now time.Time, g store.CapacityGroup, f func(*store.CapacityGroupState) bool) {
	act := &s.capState.act
	act.gate.do(func() {
		act.stateMu.Lock()
		defer act.stateMu.Unlock()
		if s.deposed() {
			return
		}
		states, err := s.store.ListCapacityGroupStates(ctx)
		if err != nil {
			s.warnUnlessStopped(ctx, err, "scheduler: capacity provider: read launch record failed", slog.String("group", g.Name))
			return
		}
		rec, ok := states[g.ID]
		if !ok || !f(&rec) {
			return
		}
		rec.UpdatedAt = now
		err = s.store.PutCapacityGroupState(ctx, rec)
		if !s.stopIfFenced(ctx, err) {
			s.warnUnlessStopped(ctx, err, "scheduler: capacity provider: update launch record failed", slog.String("group", g.Name))
		}
	})
}

// finishProviderRetirement completes the retirement of w, which the provider
// reports it has carried out: a named member becomes asleep and offline, and
// an ephemeral one is deleted with its credential and its broker connection
// dropped. A member no longer retiring (its deregistration finished it first)
// is left as it is.
func (s *Scheduler) finishProviderRetirement(ctx context.Context, now time.Time, g store.CapacityGroup, w store.Worker) error {
	done, deleted, ok, err := s.store.FinishRetirement(ctx, w.ID, now)
	if err != nil || !ok {
		return err
	}
	s.logger.InfoContext(ctx, "scheduler: capacity group member retired by its provider",
		slog.String("group", g.Name), slog.String("worker_id", w.ID), slog.Bool("removed", deleted))
	if deleted {
		s.announceRemovedWorker(done)
		s.revokeRemovedWorker(ctx, w.ID)
	} else {
		s.notifyWorkerStatus(done)
	}
	s.refreshWorkerGauge(ctx)
	return nil
}

// notifyWorkerStatus fans out w's stored status to WebSocket clients.
func (s *Scheduler) notifyWorkerStatus(w store.Worker) {
	s.notifier.NotifyWorker(ws.WorkerEvent{
		WorkerID: w.ID, Name: w.Name, Hostname: w.Hostname, FarmID: w.FarmID,
		Status: string(w.EffectiveStatus()),
	})
}
