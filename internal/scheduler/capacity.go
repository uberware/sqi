// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

// capacity.go is the capacity sweep step. Once per heartbeat-sweep tick, and
// so on the leader only, it removes the ephemeral members of every group that
// vanished (see [Scheduler.removeVanishedEphemerals]), then reads what
// internal/capacity's policy needs, evaluates every enabled capacity group, and
// publishes the result as each group's status (read by the API) and as the
// sqi_capacity_* gauges (read by external autoscalers). Removing a vanished
// member is its only write.
//
// With no group at all it reads the group list and nothing else: no member or
// demand query, no log line and no series, so a farm that has not set up
// capacity groups is unaffected. With only disabled groups it also
// reads their members, whose vanished ephemeral machines still need removing,
// but evaluates and exports nothing.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/uberware/sqi/internal/capacity"
	"github.com/uberware/sqi/internal/store"
)

// capacityState is what the capacity sweep keeps between ticks. It lives in
// memory only: after a restart or failover the new leader starts it afresh,
// which is conservative, because idle times restart and so no member is
// retired sooner than its idle timeout.
type capacityState struct {
	// sweepMu serializes sweeps and guards every field up to mu.
	sweepMu sync.Mutex
	// idleSince is, per online member holding no task, the first tick it was
	// seen so. A member seen holding a task, or offline, has no entry.
	idleSince map[string]time.Time
	// lastStepUp and lastDesired are carried between evaluations for
	// [capacity.Snapshot], by group ID.
	lastStepUp  map[string]time.Time
	lastDesired map[string]int
	// warned is, per mismatched member, the reason last logged, so each
	// mismatch is logged once.
	warned map[string]string

	// mu guards statuses, which the API reads concurrently with the sweep.
	// Only the sweep writes statuses, with sweepMu held, so the sweep may
	// read them under sweepMu alone.
	mu sync.Mutex
	// statuses are the last evaluation's, in group order. Their group names
	// are the ones the gauges hold series for.
	statuses []capacity.Status
}

// CapacityStatus returns the last evaluation of the enabled group groupID,
// and false when there is none: before the first sweep, or when the group is
// disabled, deleted or in another farm. Safe for concurrent use.
func (s *Scheduler) CapacityStatus(groupID string) (capacity.Status, bool) {
	cs := &s.capState
	cs.mu.Lock()
	defer cs.mu.Unlock()
	for _, st := range cs.statuses {
		if st.GroupID == groupID {
			return st, true
		}
	}
	return capacity.Status{}, false
}

// capacityStatuses returns the last evaluation of every enabled group,
// priority descending then name, or nothing before the first sweep. Safe for
// concurrent use.
func (s *Scheduler) capacityStatuses() []capacity.Status {
	cs := &s.capState
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return slices.Clone(cs.statuses)
}

// sweepCapacity is the capacity step of a sweep tick.
func (s *Scheduler) sweepCapacity(ctx context.Context) {
	s.sweepCapacityAt(ctx, time.Now().UTC())
}

// sweepCapacityAt is [Scheduler.sweepCapacity] at now. Each phase is a method
// of its own, called with sweepMu held.
func (s *Scheduler) sweepCapacityAt(ctx context.Context, now time.Time) {
	cs := &s.capState
	cs.sweepMu.Lock()
	defer cs.sweepMu.Unlock()

	all, err := s.store.ListCapacityGroups(ctx)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			s.logger.WarnContext(ctx, "scheduler: capacity sweep: list capacity groups failed", slog.Any("error", err))
		}
		return
	}
	farmGroups := s.farmCapacityGroups(all)
	if len(farmGroups) == 0 {
		s.clearCapacity()
		return
	}
	members, err := s.listCapacityMembers(ctx, farmGroups)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			s.logger.WarnContext(ctx, "scheduler: capacity sweep: read failed; keeping the last evaluation",
				slog.Any("error", err))
		}
		return
	}
	if s.removeVanishedEphemerals(ctx, now, farmGroups, members) {
		return // fenced
	}
	groups := enabledCapacityGroups(farmGroups)
	if len(groups) == 0 {
		s.clearCapacity()
		return
	}
	s.evaluateCapacity(ctx, now, groups, members)
}

// farmCapacityGroups keeps the groups of the scheduler's farm (of every farm
// when it manages all), enabled or not, in the store's order: priority
// descending, then name.
func (s *Scheduler) farmCapacityGroups(all []store.CapacityGroup) []store.CapacityGroup {
	var out []store.CapacityGroup
	for _, g := range all {
		if s.cfg.FarmID == "" || g.FarmID == s.cfg.FarmID {
			out = append(out, g)
		}
	}
	return out
}

// enabledCapacityGroups keeps the enabled groups of groups, in order.
func enabledCapacityGroups(groups []store.CapacityGroup) []store.CapacityGroup {
	var out []store.CapacityGroup
	for _, g := range groups {
		if g.Enabled {
			out = append(out, g)
		}
	}
	return out
}

// listCapacityMembers reads each group's members, online and offline, by
// group ID.
func (s *Scheduler) listCapacityMembers(ctx context.Context, groups []store.CapacityGroup) (map[string][]store.Worker, error) {
	out := make(map[string][]store.Worker, len(groups))
	for _, g := range groups {
		workers, err := s.store.ListCapacityGroupMembers(ctx, g.ID)
		if err != nil {
			return nil, fmt.Errorf("list members of capacity group %s: %w", g.Name, err)
		}
		out[g.ID] = workers
	}
	return out, nil
}

// removeVanishedEphemerals deletes every ephemeral member of groups that has
// been offline for longer than its group's BootTimeout, judged by its last
// heartbeat, revokes its credential and drops it from members. That is a cloud
// machine that went away without deregistering (terminated, or preempted with
// no notice): the 24 h retention sweep would delete its row but revoke
// nothing. It returns true when a write was fenced, and the sweep stops.
//
// A member that came back is online and is kept, as is one the operator
// disabled (the disable flag is the maintenance hold, which the retention
// sweep honors too). The store re-checks all of that, and the heartbeat
// cutoff, in the delete itself. Nothing is removed while the liveness sweeps
// are deferred ([Scheduler.livenessDeferred]): during a broker outage no
// machine can come back, so its time offline proves nothing.
func (s *Scheduler) removeVanishedEphemerals(
	ctx context.Context, now time.Time, groups []store.CapacityGroup, members map[string][]store.Worker,
) bool {
	if deferred, _ := s.livenessDeferred(now); deferred {
		return false
	}
	removed := 0
	for _, g := range groups {
		kept := make([]store.Worker, 0, len(members[g.ID]))
		for _, w := range members[g.ID] {
			if !vanishedEphemeral(w, g, now) {
				kept = append(kept, w)
				continue
			}
			gone, deleted, err := s.store.DeleteVanishedEphemeralWorker(ctx, w.ID, now.Add(-g.BootTimeout), now)
			if s.stopIfFenced(ctx, err) {
				return true
			}
			if err != nil || !deleted {
				// A failed write is retried next tick; a refused one means the
				// member came back, heartbeated, was disabled or took work
				// since it was read.
				s.warnUnlessStopped(ctx, err, "scheduler: capacity sweep: remove vanished ephemeral worker failed",
					slog.String("group", g.Name), slog.String("worker_id", w.ID))
				kept = append(kept, w)
				continue
			}
			s.logger.InfoContext(ctx, "scheduler: capacity sweep removed an ephemeral worker that vanished, and revoked its credential",
				slog.String("group", g.Name), slog.String("worker_id", w.ID),
				slog.Time("last_heartbeat_at", *w.LastHeartbeatAt), slog.Duration("boot_timeout", g.BootTimeout))
			s.announceRemovedWorker(gone)
			s.revokeRemovedWorker(ctx, w.ID)
			removed++
		}
		members[g.ID] = kept
	}
	if removed > 0 {
		s.refreshWorkerGauge(ctx)
	}
	return false
}

// vanishedEphemeral reports whether w, a member of g, is an ephemeral member
// offline for longer than g's BootTimeout at now. A member with no recorded
// heartbeat is never judged vanished.
func vanishedEphemeral(w store.Worker, g store.CapacityGroup, now time.Time) bool {
	return w.Ephemeral && !w.Disabled && w.Status == store.WorkerStatusOffline &&
		w.LastHeartbeatAt != nil && now.Sub(*w.LastHeartbeatAt) > g.BootTimeout
}

// clearCapacity forgets every status, series and remembered time. It touches
// no metric unless an earlier sweep exported one.
func (s *Scheduler) clearCapacity() {
	cs := &s.capState
	for _, st := range cs.statuses {
		s.deleteCapacitySeries(st.GroupName)
	}
	cs.idleSince, cs.lastStepUp, cs.lastDesired, cs.warned = nil, nil, nil, nil
	cs.mu.Lock()
	cs.statuses = nil
	cs.mu.Unlock()
}

// evaluateCapacity evaluates groups, whose members workers holds by group ID,
// and publishes the result. A failed read keeps the last evaluation.
func (s *Scheduler) evaluateCapacity(
	ctx context.Context, now time.Time, groups []store.CapacityGroup, workers map[string][]store.Worker,
) {
	in, err := s.capacityInputs(ctx, now, groups, workers)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			s.logger.WarnContext(ctx, "scheduler: capacity sweep: read failed; keeping the last evaluation",
				slog.Any("error", err))
		}
		return
	}
	cs := &s.capState
	cs.idleSince = in.idleSince
	cs.lastStepUp = keepGroups(cs.lastStepUp, groups)
	cs.lastDesired = keepGroups(cs.lastDesired, groups)

	statuses := capacity.Evaluate(capacity.Snapshot{
		Now:     now,
		Groups:  groups,
		Members: in.members,
		Others:  in.others,
		Demand:  in.demand,
		CanRun: func(w store.Worker, item capacity.DemandItem) bool {
			return s.couldRun(w, item.Job, item.Step, item.Demand.RequiredCores, in.pools)
		},
		LastStepUp:  cs.lastStepUp,
		LastDesired: cs.lastDesired,
	})
	s.recordMismatches(ctx, groups, in.members, statuses)
	s.publishCapacity(statuses)
}

// keepGroups returns m, made if nil, without the entries of groups not in
// groups.
func keepGroups[V any](m map[string]V, groups []store.CapacityGroup) map[string]V {
	if m == nil {
		return map[string]V{}
	}
	maps.DeleteFunc(m, func(id string, _ V) bool {
		return !slices.ContainsFunc(groups, func(g store.CapacityGroup) bool { return g.ID == id })
	})
	return m
}

// capacityInputs is what one evaluation reads from the store.
type capacityInputs struct {
	members map[string][]capacity.Member
	others  []capacity.Member
	demand  []capacity.DemandItem
	// pools are the usage pools by lowercased name, read only when a demand
	// step needs one.
	pools map[string]store.UsagePool
	// idleSince replaces capacityState.idleSince once the reads succeed.
	idleSince map[string]time.Time
}

// capacityInputs reads the other online workers, what every worker holds and
// the ready work, and builds the members of groups from workers.
func (s *Scheduler) capacityInputs(
	ctx context.Context, now time.Time, groups []store.CapacityGroup, workers map[string][]store.Worker,
) (capacityInputs, error) {
	// Every farm, as the lease path's CommittedCores sums a worker's tasks.
	commitments, err := s.store.ListWorkerCommitments(ctx, "")
	if err != nil {
		return capacityInputs{}, fmt.Errorf("list worker commitments: %w", err)
	}
	in := capacityInputs{idleSince: map[string]time.Time{}}
	in.members = s.capacityMembers(now, groups, workers, commitments, in.idleSince)
	if in.others, err = s.capacityOthers(ctx, in.members, commitments); err != nil {
		return capacityInputs{}, err
	}
	if in.demand, in.pools, err = s.capacityDemand(ctx, now); err != nil {
		return capacityInputs{}, err
	}
	return in, nil
}

// capacityMembers builds each group's members, online and offline, from
// workers (by group ID), and records in idle when each online member holding
// nothing was first seen so.
func (s *Scheduler) capacityMembers(
	now time.Time, groups []store.CapacityGroup, workers map[string][]store.Worker,
	commitments map[string]store.WorkerCommitment, idle map[string]time.Time,
) map[string][]capacity.Member {
	out := make(map[string][]capacity.Member, len(groups))
	for _, g := range groups {
		members := make([]capacity.Member, 0, len(workers[g.ID]))
		for _, w := range workers[g.ID] {
			m := capacity.Member{Worker: w}
			if c, busy := commitments[w.ID]; busy {
				m.CommittedCores = committedCores(c, w)
			} else if w.Status == store.WorkerStatusOnline {
				since, seen := s.capState.idleSince[w.ID]
				if !seen {
					since = now
				}
				idle[w.ID] = since
				m.IdleSince = &since
			}
			members = append(members, m)
		}
		out[g.ID] = members
	}
	return out
}

// capacityOthers reads the online, enabled workers of the scheduler's farm
// that are in no enabled group. A member of a disabled group is an ordinary
// worker here: its free cores absorb demand like any other's.
func (s *Scheduler) capacityOthers(
	ctx context.Context, members map[string][]capacity.Member, commitments map[string]store.WorkerCommitment,
) ([]capacity.Member, error) {
	workers, err := s.onlineWorkers(ctx)
	if err != nil {
		return nil, fmt.Errorf("list online workers: %w", err)
	}
	var out []capacity.Member
	for _, w := range workers {
		if _, grouped := members[w.CapacityGroupID]; grouped {
			continue
		}
		out = append(out, capacity.Member{Worker: w, CommittedCores: committedCores(commitments[w.ID], w)})
	}
	return out, nil
}

// committedCores is the cores c holds on w, a task declaring none costing the
// whole machine as the lease path counts it.
func committedCores(c store.WorkerCommitment, w store.Worker) int {
	return c.DeclaredCores + c.UndeclaredTasks*max(w.CPUCount, 1)
}

// capacityDemand reads the leasable ready work per step, with the job and
// step row of each and, when any step needs one, the usage pools. Each job and
// step is read once, so the reads scale with the steps holding ready work,
// never with their tasks. Work whose job or step has gone since is dropped.
func (s *Scheduler) capacityDemand(ctx context.Context, now time.Time) ([]capacity.DemandItem, map[string]store.UsagePool, error) {
	rows, err := s.store.ListReadyStepDemand(ctx, s.cfg.FarmID, now)
	if err != nil {
		return nil, nil, fmt.Errorf("list ready step demand: %w", err)
	}
	jobs := map[string]*store.Job{}
	steps := map[string]*store.Step{}
	var items []capacity.DemandItem
	needPools := false
	for _, r := range rows {
		if err := readOnce(ctx, jobs, r.JobID, s.store.GetJob); err != nil {
			return nil, nil, fmt.Errorf("get job %s: %w", r.JobID, err)
		}
		if err := readOnce(ctx, steps, r.StepID, s.store.GetStep); err != nil {
			return nil, nil, fmt.Errorf("get step %s: %w", r.StepID, err)
		}
		job, step := jobs[r.JobID], steps[r.StepID]
		if job == nil || step == nil {
			continue
		}
		items = append(items, capacity.DemandItem{Demand: r, Job: *job, Step: *step})
		needPools = needPools || (step.HostRequirements != nil && len(step.HostRequirements.UsagePools) > 0)
	}
	if !needPools {
		return items, nil, nil
	}
	all, err := s.store.ListUsagePools(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("list usage pools: %w", err)
	}
	// Keyed as buildUsageContext keys them, for checkUsagePools.
	pools := make(map[string]store.UsagePool, len(all))
	for _, p := range all {
		pools[strings.ToLower(p.Name)] = p
	}
	return items, pools, nil
}

// readOnce reads the row id into cache unless it is already there, storing
// nil for a row that does not exist.
func readOnce[T any](ctx context.Context, cache map[string]*T, id string, get func(context.Context, string) (T, error)) error {
	if _, seen := cache[id]; seen {
		return nil
	}
	v, err := get(ctx, id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		cache[id] = nil
	case err != nil:
		return err
	default:
		cache[id] = &v
	}
	return nil
}

// recordMismatches fills each sized group's Status.Mismatches with its online
// members whose registration falls short of the profile, and logs each
// mismatch at Warn the first time it is seen. statuses are in groups order.
func (s *Scheduler) recordMismatches(
	ctx context.Context, groups []store.CapacityGroup, members map[string][]capacity.Member, statuses []capacity.Status,
) {
	cs := &s.capState
	warned := map[string]string{}
	for i, g := range groups {
		if g.Kind != store.CapacityGroupSized {
			continue
		}
		for _, m := range members[g.ID] {
			w := m.Worker
			if w.Status != store.WorkerStatusOnline {
				continue
			}
			reason := profileMismatch(g, w)
			if reason == "" {
				continue
			}
			statuses[i].Mismatches = append(statuses[i].Mismatches, capacity.Mismatch{WorkerID: w.ID, Reason: reason})
			if cs.warned[w.ID] != reason {
				s.logger.WarnContext(ctx, "scheduler: capacity group member does not match the group's machine profile",
					slog.String("group", g.Name), slog.String("worker_id", w.ID), slog.String("reason", reason))
			}
			warned[w.ID] = reason
		}
	}
	cs.warned = warned
}

// publishCapacity stores statuses for the API and sets the gauges, deleting
// the series of groups that are gone or renamed.
func (s *Scheduler) publishCapacity(statuses []capacity.Status) {
	cs := &s.capState
	current := make(map[string]struct{}, len(statuses))
	for _, st := range statuses {
		s.metrics.CapacityDemandCores.WithLabelValues(st.GroupName).Set(float64(st.DemandCores))
		s.metrics.CapacityDesiredMachines.WithLabelValues(st.GroupName).Set(float64(st.DesiredMachines))
		s.metrics.CapacityMembers.WithLabelValues(st.GroupName, "online").Set(float64(st.OnlineMembers))
		s.metrics.CapacityMembers.WithLabelValues(st.GroupName, "offline").Set(float64(st.OfflineMembers))
		current[st.GroupName] = struct{}{}
	}
	for _, old := range cs.statuses {
		if _, ok := current[old.GroupName]; !ok {
			s.deleteCapacitySeries(old.GroupName)
		}
	}

	cs.mu.Lock()
	cs.statuses = statuses
	cs.mu.Unlock()
}

// deleteCapacitySeries removes every sqi_capacity_* series of the group name.
func (s *Scheduler) deleteCapacitySeries(name string) {
	s.metrics.CapacityDemandCores.DeleteLabelValues(name)
	s.metrics.CapacityDesiredMachines.DeleteLabelValues(name)
	s.metrics.CapacityMembers.DeleteLabelValues(name, "online")
	s.metrics.CapacityMembers.DeleteLabelValues(name, "offline")
}

// profileMismatch returns every way w's registration falls short of the sized
// group g's declared profile, joined by "; ", or "" when it matches. Only what
// the profile declares is compared; a member offering more is not a mismatch.
func profileMismatch(g store.CapacityGroup, w store.Worker) string {
	p := g.Profile
	var reasons []string
	reasons = append(reasons, hardwareShortfalls(p, w)...)
	reasons = append(reasons, platformMismatches(p, w)...)
	reasons = append(reasons, tagMismatches(p.Tags, w.Tags)...)
	for _, ext := range p.Extensions {
		if !slices.Contains(w.Extensions, ext) {
			reasons = append(reasons, fmt.Sprintf("extension %s is not advertised", ext))
		}
	}
	reasons = append(reasons, exprShortfalls(p.ExprLimits, w.ExprLimits)...)
	if r := queueMismatch(g.QueueIDs, w.QueueID); r != "" {
		reasons = append(reasons, r)
	}
	return strings.Join(reasons, "; ")
}

// hardwareShortfalls compares the declared amounts.
func hardwareShortfalls(p store.MachineProfile, w store.Worker) []string {
	var out []string
	below := func(name string, got, want int) {
		if want > 0 && got < want {
			out = append(out, fmt.Sprintf("%s %d is below the profile's %d", name, got, want))
		}
	}
	below("cpu_count", w.CPUCount, p.CPUCount)
	below("ram_mb", w.RAMMb, p.RAMMb)
	below("gpu_count", w.GPUInfo.Count, p.GPUCount)
	below("gpu_memory_mb", w.GPUInfo.VRAMMb, p.GPUMemoryMb)
	return out
}

// platformMismatches compares OS family and architecture as the matcher
// spells them, and compute location exactly, as the matcher compares it.
func platformMismatches(p store.MachineProfile, w store.Worker) []string {
	var out []string
	differs := func(name, got, want string, norm func(string) string) {
		if want != "" && norm(got) != norm(want) {
			out = append(out, fmt.Sprintf("%s %q differs from the profile's %q", name, got, want))
		}
	}
	differs("os", w.OS, p.OS, osFamily)
	differs("arch", w.Arch, p.Arch, cpuArch)
	differs("compute_location", w.ComputeLocation, p.ComputeLocation, func(v string) string { return v })
	return out
}

// tagMismatches reports each declared tag the worker lacks or carries with
// another value, keys and values compared as the matcher compares them.
func tagMismatches(want, have map[string]string) []string {
	var out []string
	for _, key := range slices.Sorted(maps.Keys(want)) {
		got, ok := tagValueFold(have, key)
		switch {
		case !ok:
			out = append(out, fmt.Sprintf("tag %s is missing (the profile declares %q)", key, want[key]))
		case !strings.EqualFold(got, want[key]):
			out = append(out, fmt.Sprintf("tag %s is %q, the profile declares %q", key, got, want[key]))
		}
	}
	return out
}

// exprShortfalls compares each EXPR cap the profile declares with the one the
// worker enforces, an unadvertised cap read as the scheduler reads it
// elsewhere.
func exprShortfalls(want, have store.WorkerExprLimits) []string {
	h := workerExprCapsOrLegacy(have)
	var out []string
	for _, c := range []struct {
		key       string
		got, want int64
	}{
		{"expr.operation_limit", h.OperationLimit, want.OperationLimit},
		{"expr.memory_limit", h.MemoryLimit, want.MemoryLimit},
		{"expr.assignment_positions", h.AssignmentPositions, want.AssignmentPositions},
		{"expr.assignment_retained_bytes", h.AssignmentRetainedBytes, want.AssignmentRetainedBytes},
		{"expr.let_retained_bytes", h.LetRetainedBytes, want.LetRetainedBytes},
	} {
		if c.want > 0 && c.got < c.want {
			out = append(out, fmt.Sprintf("%s %d is below the profile's %d", c.key, c.got, c.want))
		}
	}
	return out
}

// queueMismatch reports a worker queue affinity that does not cover the
// group's queues. A worker with none takes work from every queue, as the
// lease path reads it, and covers any list.
func queueMismatch(groupQueues []string, workerQueue string) string {
	switch {
	case workerQueue == "":
		return ""
	case len(groupQueues) == 1 && groupQueues[0] == workerQueue:
		return ""
	case len(groupQueues) == 0:
		return fmt.Sprintf("queue affinity %s does not cover the group's queues (every queue in the farm)", workerQueue)
	default:
		return fmt.Sprintf("queue affinity %s does not cover the group's queues %s",
			workerQueue, strings.Join(groupQueues, ", "))
	}
}
