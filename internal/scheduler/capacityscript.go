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
// timeout, the group degrades: the record falls back to the member rows that
// are not retiring, further growth waits two boot timeouts, and the instances
// that booted but never registered (they still bill) are retired by instance
// ID when the provider can list them. sqi never shrinks a sized group by
// lowering its size: it retires named members, and the script's retire lowers
// the cloud group's desired size as it removes them.

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
	_, r := sp.call(ctx, g, script.Request{Action: actionWake, Members: scriptMembers(members)})
	return sameResult(members, r)
}

// grow asks for desired machines in all; current is the size last asked for.
func (sp *scriptProvider) grow(ctx context.Context, g store.CapacityGroup, desired, current int) actionResult {
	_, r := sp.call(ctx, g, script.Request{Action: actionGrow, Desired: desired, Current: current})
	return r
}

// retire sends one retire request naming every member; its answer is each
// one's.
func (sp *scriptProvider) retire(ctx context.Context, g store.CapacityGroup, members []store.Worker) map[string]actionResult {
	_, r := sp.call(ctx, g, script.Request{Action: actionRetire, Members: scriptMembers(members)})
	return sameResult(members, r)
}

// retireInstances sends one retire request naming each instance by its ID
// alone.
func (sp *scriptProvider) retireInstances(ctx context.Context, g store.CapacityGroup, instanceIDs []string) actionResult {
	members := make([]script.Member, len(instanceIDs))
	for i, id := range instanceIDs {
		members[i] = script.Member{InstanceID: id}
	}
	_, r := sp.call(ctx, g, script.Request{Action: actionRetire, Members: members})
	return r
}

// list returns the instance IDs the script reports that sqi may use (see
// [usableInstanceIDs]). The script's own message is replaced by counts, so no
// ID it returned reaches a log through it.
func (sp *scriptProvider) list(ctx context.Context, g store.CapacityGroup) ([]string, actionResult) {
	resp, r := sp.call(ctx, g, script.Request{Action: actionList})
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
	_, ok := s.configuredScript(g)
	return ok && g.Kind == store.CapacityGroupSized
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
// evaluation st (see [Scheduler.settleLaunch]), records a grow when the
// policy wants more machines and no backoff holds, and plans the orphan
// cleanup a degradation calls for. It fills st.Requested and st.Degraded, and
// returns the group-level part of g's next call, empty while a call is
// running. When the records could not be read, st carries the last record
// this process saw, so the degraded gauge does not flap. It returns true when
// a write was fenced.
func (s *Scheduler) planLaunches(
	ctx context.Context, now time.Time, g store.CapacityGroup, st *capacity.Status, records launchRecords,
) (groupCall, bool) {
	act := &s.capState.act
	if !records.read {
		if prev, ok := act.launches[g.ID]; ok {
			fillLaunchStatus(st, prev)
		}
		return groupCall{}, false
	}
	rec, ok := records.byGroup[g.ID]
	if !ok {
		rec = store.CapacityGroupState{GroupID: g.ID}
	}
	present, rows := launchCounts(*st)
	var call groupCall
	if s.settleLaunch(ctx, now, g, &rec, present, rows) {
		return groupCall{}, true
	}
	if !act.isInFlight(g.ID) {
		s.planOrphanCleanup(now, g, rec, &call)
		if s.planGrow(ctx, now, g, &rec, st.DesiredMachines, rows, &call) {
			return groupCall{}, true
		}
	}
	if act.launches == nil {
		act.launches = map[string]store.CapacityGroupState{}
	}
	act.launches[g.ID] = rec
	fillLaunchStatus(st, rec)
	return call, false
}

// launchCounts returns, from a sized group's evaluation, the machines its
// launch record counts as present (every member row, online or offline, that
// is not retiring) and all its member rows.
//
// The record follows rows, not members online: a member that restarts or
// misses heartbeats keeps its row and its machine, and must neither lower the
// record nor make a grow's current fall below the cloud group's real size
// (an absolute grow below it would terminate working instances). A machine
// that is gone loses its row: a retired or preempted member is deleted
// when it deregisters, and one that vanished is deleted after a boot timeout
// (see [Scheduler.removeVanishedEphemerals]). A retiring member is not
// present: its retirement lowers the record before its row is deleted
// ([Scheduler.lowerRequested]), so counting it would count it twice.
//
// Counting rows masks two things, both in the direction that never shrinks
// the group. A row whose machine vanished silently is still counted until it
// is deleted, up to a boot timeout later, and meanwhile can stand in for one
// launched machine that never registered: the launch reads as met, so that
// machine is neither reported nor cleaned up as an orphan, and with a script
// that reads a grow as a delta from current it keeps running (and billing)
// for good. And a member that is not ephemeral keeps its row while offline,
// so in a sized group it overstates a grow's current by one.
func launchCounts(st capacity.Status) (present, rows int) {
	rows = st.OnlineMembers + st.OfflineMembers
	return max(rows-st.Retiring, 0), rows
}

// settleLaunch brings g's launch record up to date with the machines present
// and the member rows (see [launchCounts]).
//
// A launch is unmet while RequestedAt is set. Once as many machines are
// present as were asked for, it is met: RequestedAt is cleared, and a
// degradation that an earlier launch left is cleared with it (its backoff is
// left to run out). Only an unmet launch degrades, when its machines have not
// all registered a boot timeout after it was asked for: the record falls back
// to the machines present, the launch is resolved, and growth backs off for
// two boot timeouts.
//
// With no launch unmet, Requested follows the machines present, so a machine
// lost later (retired, preempted, vanished) is replaced by a normal grow and
// never reads as a launch that failed. While the group is degraded it follows
// only the rows down instead, so it keeps what was present at the
// degradation, less the machines since gone: a member retiring is still
// counted, and one whose retirement aborts does not come back as a machine
// that registered late. Machines present beyond the record (that did register
// late) clear the degradation at once. It returns true when the write was
// fenced.
func (s *Scheduler) settleLaunch(
	ctx context.Context, now time.Time, g store.CapacityGroup, rec *store.CapacityGroupState, present, rows int,
) bool {
	next := *rec
	var warn, recovered bool
	switch {
	case rec.RequestedAt != nil && present >= rec.Requested:
		next.Requested, next.RequestedAt = present, nil
		recovered = rec.DegradedReason != ""
	case rec.RequestedAt != nil && now.After(rec.RequestedAt.Add(g.BootTimeout)):
		next.Requested, next.RequestedAt = present, nil
		next.DegradedReason = fmt.Sprintf("%d of %d machines did not register within boot_timeout (%s)",
			rec.Requested-present, rec.Requested, g.BootTimeout)
		next.DegradedAt = new(now)
		next.BackoffUntil = new(now.Add(2 * g.BootTimeout))
		warn = true
	case rec.RequestedAt == nil && rec.DegradedReason != "":
		// Requested holds what was present at the degradation. Only a row that
		// is gone lowers it, not a member retiring: a retirement that then
		// aborts would bring that member back as present and read as a late
		// registration.
		recovered = present > rec.Requested
		next.Requested = min(rec.Requested, rows)
		if recovered {
			next.Requested = present
		}
	case rec.RequestedAt == nil:
		next.Requested = present
	}
	if recovered {
		next.DegradedReason, next.DegradedAt = "", nil
	}
	if sameLaunchRecord(next, *rec) {
		return false
	}
	next.UpdatedAt = now
	err := s.store.PutCapacityGroupState(ctx, next)
	if s.stopIfFenced(ctx, err) {
		return true
	}
	if err != nil {
		s.warnUnlessStopped(ctx, err, "scheduler: capacity sweep: update launch record failed", slog.String("group", g.Name))
		return false
	}
	switch {
	case warn:
		s.logger.WarnContext(ctx, "scheduler: capacity group degraded: machines it launched did not register",
			slog.String("group", g.Name), slog.String("reason", next.DegradedReason), slog.Int("requested", rec.Requested),
			slog.Int("present", present), slog.Time("backoff_until", *next.BackoffUntil))
	case recovered:
		s.logger.InfoContext(ctx, "scheduler: capacity group is no longer degraded: machines registered",
			slog.String("group", g.Name), slog.Int("present", present))
	}
	*rec = next
	return false
}

// sameLaunchRecord reports whether a and b record the same launch.
func sameLaunchRecord(a, b store.CapacityGroupState) bool {
	sameTime := func(x, y *time.Time) bool { return (x == nil) == (y == nil) && (x == nil || x.Equal(*y)) }
	return a.Requested == b.Requested && a.DegradedReason == b.DegradedReason && sameTime(a.RequestedAt, b.RequestedAt) &&
		sameTime(a.DegradedAt, b.DegradedAt) && sameTime(a.BackoffUntil, b.BackoffUntil)
}

// planOrphanCleanup adds the orphan cleanup of g's current degradation to
// call when this process has not run it, no launch is unmet (its instances
// may still be booting, and must not look orphaned) and the degradation is
// younger than a boot timeout: later, the backend may be booting replacements
// of its own.
//
// One case this cannot tell apart: a member lost (its row deleted) while a
// launch is unmet, whose replacement the backend starts on its own and which
// has not registered by the launch's deadline. That launch degrades, and the
// replacement is listed as an orphan and may be retired; the refusal of more
// orphans than max_machines less the members bounds it. A degradation cleared
// by late registrations before its cleanup runs (see [Scheduler.settleLaunch])
// is not cleaned up at all, and nothing records that it was not.
func (s *Scheduler) planOrphanCleanup(now time.Time, g store.CapacityGroup, rec store.CapacityGroupState, call *groupCall) {
	if rec.DegradedAt == nil || rec.RequestedAt != nil || !now.Before(rec.DegradedAt.Add(g.BootTimeout)) {
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
// asked for and the group's member rows (see [launchCounts]) and no backoff
// holds, and adds it to call: the record is written before the call, so a
// failure between the two leaves a request the next leader repeats. An unmet
// launch younger than the boot timeout that this process has not sent (the
// previous leader recorded it) is repeated, re-stamped first so the machines
// it starts get a whole boot timeout. A grow is absolute, so one below the
// rows or below its own current would shrink the cloud group, which only a
// retire of named members may do: none is ever sent. It returns true when
// the write was fenced.
func (s *Scheduler) planGrow(
	ctx context.Context, now time.Time, g store.CapacityGroup, rec *store.CapacityGroupState, desired, rows int, call *groupCall,
) bool {
	backingOff := rec.BackoffUntil != nil && now.Before(*rec.BackoffUntil)
	current := max(rec.Requested, rows)
	var req growRequest
	switch {
	case desired > current && !backingOff:
		req = growRequest{desired: desired, current: current}
	case rec.RequestedAt != nil && now.Before(rec.RequestedAt.Add(g.BootTimeout)):
		if _, sent := s.capState.act.dispatched[groupIntent(g.ID, intentGrow, *rec.RequestedAt)]; sent {
			return false
		}
		// The size it was last asked for is this request's own: a script
		// that reads a delta from current does nothing twice.
		req = growRequest{desired: current, current: current}
	default:
		return false
	}
	if req.desired < req.current || req.desired < rows {
		return false
	}
	next := *rec
	next.Requested, next.RequestedAt, next.UpdatedAt = req.desired, new(now), now
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
	call.grow = &req
	call.keys = append(call.keys, groupIntent(g.ID, intentGrow, now))
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
// registers meanwhile is never taken for an orphan. Any member, online or
// offline, that reports no instance ID would make its own instance look
// orphaned, so then nothing is retired and the members are named in one
// warning: this runs once per degradation, so the warning is too (a stale
// offline ephemeral row is removed after a boot timeout, so the skip does not
// last). More unreported instances than max_machines less the members cannot
// be machines sqi asked for, so those are refused too. A provider that cannot
// list leaves the gap in the degraded reason.
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
	// The list can take minutes, so shutdown may have abandoned the call by
	// now: the read goes through the gate like the writes.
	var members []store.Worker
	var err error
	if !s.capState.act.gate.do(func() { members, err = s.store.ListCapacityGroupMembers(ctx, g.ID) }) {
		return
	}
	if err != nil {
		s.warnUnlessStopped(ctx, err, "scheduler: capacity provider: read members for orphan cleanup failed; nothing retired",
			slog.String("group", g.Name))
		return
	}
	if missing := withoutInstanceID(members); len(missing) > 0 {
		s.logger.WarnContext(ctx, "scheduler: capacity group orphan cleanup skipped: members report no cloud instance ID, "+
			"so their own instances would look orphaned",
			slog.String("group", g.Name), slog.Any("worker_ids", missing),
			slog.String("remediation", "set worker.cloud_instance_id.source on the group's machines"))
		return
	}
	orphans := unreported(ids, members)
	if room := g.MaxMachines - len(members); len(orphans) > room {
		s.logger.WarnContext(ctx, "scheduler: capacity group orphan cleanup refused: the provider lists more unreported "+
			"instances than max_machines less the members, which sqi never asked for",
			slog.String("group", g.Name), slog.Int("unreported", len(orphans)), slog.Int("max_machines", g.MaxMachines),
			slog.Int("members", len(members)))
		return
	}
	if len(orphans) == 0 || !s.mayActOn() {
		return
	}
	s.logger.InfoContext(ctx, "scheduler: capacity provider retiring instances that never registered",
		slog.String("group", g.Name), slog.Int("count", len(orphans)),
		slog.Any("instance_ids", orphans[:min(len(orphans), orphansLogged)]))
	s.recordGroupAction(ctx, now, g, actionRetire, p.retireInstances(ctx, g, orphans))
}

// withoutInstanceID returns the IDs of the members of members, online or
// offline and in any power state, that report no cloud instance ID.
func withoutInstanceID(members []store.Worker) []string {
	var out []string
	for _, w := range members {
		if w.CloudInstanceID == "" {
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

// lowerRequested lowers g's launch record for retired, the members the
// provider reports it retired, before their retirements are finished. The
// script's retire lowers the cloud group's desired size as it removes them,
// so the record keeps matching it:
//
//   - with a launch unmet, by one per member retired, to no less than zero
//     (the machines still booting are counted in no member);
//   - otherwise to at most the member rows that are neither retired nor
//     retiring, the machines present as [launchCounts] counts them. That is
//     what the record would follow to anyway, and it cannot subtract twice: a
//     retired member that already left, and a sweep that already lowered the
//     record for it, leave it unchanged.
func (s *Scheduler) lowerRequested(ctx context.Context, now time.Time, g store.CapacityGroup, retired []string) {
	s.updateLaunchRecord(ctx, now, g, func(rec *store.CapacityGroupState) bool {
		if rec.RequestedAt != nil {
			if rec.Requested == 0 {
				return false
			}
			rec.Requested = max(rec.Requested-len(retired), 0)
			return true
		}
		members, err := s.store.ListCapacityGroupMembers(ctx, g.ID)
		if err != nil {
			s.warnUnlessStopped(ctx, err, "scheduler: capacity provider: read members for the launch record failed",
				slog.String("group", g.Name))
			return false
		}
		staying := 0
		for _, w := range members {
			if w.PowerState != store.PowerRetiring && !slices.Contains(retired, w.ID) {
				staying++
			}
		}
		if staying >= rec.Requested {
			return false
		}
		rec.Requested = staying
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
// reports it has carried out ([Scheduler.finishRetirement]), and reports
// whether it did. A member no longer retiring (its deregistration finished it
// first) is left as it is.
func (s *Scheduler) finishProviderRetirement(ctx context.Context, now time.Time, g store.CapacityGroup, w store.Worker) (bool, error) {
	done, deleted, ok, err := s.finishRetirement(ctx, w, now, "scheduler: capacity group member retired by its provider",
		slog.String("group", g.Name))
	if ok && !deleted {
		s.notifyWorkerStatus(done)
	}
	return ok, err
}

// finishRetirement finishes the retirement of w in the store: a named member
// becomes asleep and offline, and an ephemeral one is deleted. When it
// finished, it logs msg with attrs, and a deleted worker is announced as
// removed and has its credential and broker connection revoked.
func (s *Scheduler) finishRetirement(
	ctx context.Context, w store.Worker, now time.Time, msg string, attrs ...slog.Attr,
) (done store.Worker, deleted, ok bool, err error) {
	done, deleted, ok, err = s.store.FinishRetirement(ctx, w.ID, now)
	if err != nil || !ok {
		return done, deleted, ok, err
	}
	s.logger.LogAttrs(ctx, slog.LevelInfo, msg, append(attrs, slog.String("worker_id", w.ID), slog.Bool("removed", deleted))...)
	if deleted {
		s.announceRemovedWorker(done)
		s.revokeRemovedWorker(ctx, w.ID)
	}
	return done, deleted, ok, nil
}

// notifyWorkerStatus fans out w's stored status to WebSocket clients.
func (s *Scheduler) notifyWorkerStatus(w store.Worker) {
	s.notifier.NotifyWorker(ws.WorkerEvent{
		WorkerID: w.ID, Name: w.Name, Hostname: w.Hostname, FarmID: w.FarmID,
		Status: string(w.EffectiveStatus()),
	})
}
