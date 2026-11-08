// SPDX-License-Identifier: AGPL-3.0-or-later

// Package capacity decides, for each capacity group, how much waiting work it
// could serve and how many machines it should have. It is a pure function of
// a [Snapshot]: no I/O, no goroutines and no clock reads, and the same snapshot
// always gives the same answer whatever order its slices and maps arrive in.
//
// The policy, in order:
//
//  1. Absorb. Ready work that online, enabled workers already have free cores
//     for is taken off the demand first: workers in no group, then each
//     group's online members, in group order. Each worker's free cores are
//     spent once across all demand.
//  2. Assign. What is left goes to the groups in priority order, each taking
//     only work it could run, from queues it serves, up to the machines it
//     could still add. What one group cannot take passes to the next; work no
//     group can run drives nothing.
//  3. Size. A group's desired size is the members it still needs plus the
//     machines its assigned demand calls for, limited per evaluation by
//     max_step_up and the scale-up cooldown, then clamped to [min, max].
//  4. Retire. Online members idle for at least the idle timeout that absorbed
//     no demand are retire candidates, longest idle first, never taking the
//     group below its desired size.
//
// All demand arithmetic is on task counts per step, never per task, so a step
// with a million ready tasks costs the same as one with a single task.
package capacity

import (
	"cmp"
	"maps"
	"math"
	"slices"
	"time"

	"github.com/uberware/sqi/internal/store"
)

// Member is a worker as the policy sees it: one of a group's members, or a
// worker in no group.
type Member struct {
	Worker store.Worker
	// CommittedCores is the CPU the worker holds across its assigned and
	// running tasks, an undeclared task counting as the whole machine.
	CommittedCores int
	// IdleSince is when the worker was first seen holding no task; nil while
	// it holds any.
	IdleSince *time.Time
}

// DemandItem is one step's leasable ready tasks, with the job and step rows
// [Snapshot.CanRun] needs.
type DemandItem struct {
	Demand store.StepDemand
	Job    store.Job
	Step   store.Step
}

// Snapshot is everything one evaluation reads.
type Snapshot struct {
	Now time.Time
	// Groups are the enabled groups, ordered priority descending, then name.
	Groups []store.CapacityGroup
	// Members are each group's members, online and offline, by group ID.
	Members map[string][]Member
	// Others are the online, enabled workers in no group.
	Others []Member
	Demand []DemandItem
	// CanRun reports whether w could ever run item's tasks, whatever its
	// current load. A nil CanRun runs nothing, so no demand counts.
	CanRun func(w store.Worker, item DemandItem) bool
	// LastStepUp is when each group, by ID, last stepped up. The caller keeps
	// the map between evaluations; Evaluate writes a group's entry when it
	// steps that group up (see [Evaluate]). A nil map records nothing, so no
	// cooldown ever applies.
	LastStepUp map[string]time.Time
}

// Status is one group's evaluation.
type Status struct {
	GroupID, GroupName string
	// DemandCores is the cores of ready work assigned to the group: work it
	// could run that no idle worker absorbed and no higher-priority group
	// took. It never exceeds what the group's free machine slots could hold.
	DemandCores int
	// DesiredMachines is how many members the group should have online.
	DesiredMachines int
	// OnlineMembers and OfflineMembers count the group's members by liveness;
	// a disabled member counts in whichever it is.
	OnlineMembers, OfflineMembers int
	// RetireCandidates are worker IDs, longest idle first.
	RetireCandidates []string
	EvaluatedAt      time.Time
}

// Evaluate returns one Status per group, in s.Groups order.
//
// It writes s.LastStepUp[g.ID] = s.Now when group g's desired size exceeds its
// online members and g was not inside its scale-up cooldown, and writes nothing
// else. A step-up forced by min_machines during a cooldown does not restart the
// cooldown.
func Evaluate(s Snapshot) []Status {
	items := sortedItems(s.Demand)
	plans := make([]*groupPlan, len(s.Groups))
	for i, g := range s.Groups {
		plans[i] = newGroupPlan(g, s.Members[g.ID])
	}
	remaining, absorbed := absorb(s, items, plans)
	assign(s, items, remaining, plans)

	out := make([]Status, len(plans))
	for i, p := range plans {
		out[i] = p.status(s, absorbed)
	}
	return out
}

// sortedItems orders demand as the lease path offers it, so the work that
// would be leased first is the work idle workers absorb first: job priority
// descending, job creation ascending, step order ascending. The remaining keys
// (job, step, queue and cores) make the order total.
func sortedItems(demand []DemandItem) []DemandItem {
	items := slices.Clone(demand)
	slices.SortStableFunc(items, func(a, b DemandItem) int {
		return cmp.Or(
			cmp.Compare(b.Job.Priority, a.Job.Priority),
			a.Job.CreatedAt.Compare(b.Job.CreatedAt),
			cmp.Compare(a.Step.StepOrder, b.Step.StepOrder),
			cmp.Compare(a.Demand.JobID, b.Demand.JobID),
			cmp.Compare(a.Demand.StepID, b.Demand.StepID),
			cmp.Compare(a.Demand.QueueID, b.Demand.QueueID),
			cmp.Compare(a.Demand.RequiredCores, b.Demand.RequiredCores),
			cmp.Compare(a.Demand.Ready, b.Demand.Ready),
		)
	})
	return items
}

// runs is s.CanRun, with a nil function running nothing.
func runs(s Snapshot, w store.Worker, item DemandItem) bool {
	return s.CanRun != nil && s.CanRun(w, item)
}

// online reports whether w is online by liveness.
func online(w store.Worker) bool { return w.Status == store.WorkerStatusOnline }

// machineCores is what one task declaring no cores costs on w, as the lease
// path's fullMachineCost: the whole machine, or 1 for a worker that reports
// no cores.
func machineCores(w store.Worker) int64 { return max(int64(w.CPUCount), 1) }

// taskCost is the cores one task of item costs on a machine of the given size.
func taskCost(item DemandItem, machine int64) int64 {
	if item.Demand.RequiredCores > 0 {
		return int64(item.Demand.RequiredCores)
	}
	return machine
}

// absorber is a worker whose free cores can take demand.
type absorber struct {
	w    store.Worker
	free int64
}

// absorbers lists the online, enabled workers that can take demand, in
// absorption order: s.Others by ID, then each group's online members by ID in
// group order. A worker listed more than once appears once, where first seen.
func absorbers(s Snapshot, plans []*groupPlan) []*absorber {
	var out []*absorber
	seen := map[string]bool{}
	add := func(m Member) {
		if !online(m.Worker) || m.Worker.Disabled || seen[m.Worker.ID] {
			return
		}
		seen[m.Worker.ID] = true
		if free := machineCores(m.Worker) - int64(m.CommittedCores); free > 0 {
			out = append(out, &absorber{w: m.Worker, free: free})
		}
	}
	for _, m := range sortedMembers(s.Others) {
		add(m)
	}
	for _, p := range plans {
		for _, m := range p.members {
			add(m)
		}
	}
	return out
}

// absorb takes off each item the tasks idle workers have free cores for, and
// returns the tasks left per item and the IDs of the workers that took any.
func absorb(s Snapshot, items []DemandItem, plans []*groupPlan) (remaining []int64, absorbed map[string]bool) {
	pool := absorbers(s, plans)
	remaining = make([]int64, len(items))
	absorbed = map[string]bool{}
	for i, item := range items {
		left := max(int64(item.Demand.Ready), 0)
		for _, a := range pool {
			if left == 0 {
				break
			}
			cost := taskCost(item, machineCores(a.w))
			if a.free < cost || !runs(s, a.w, item) {
				continue
			}
			took := min(left, a.free/cost)
			a.free -= took * cost
			left -= took
			absorbed[a.w.ID] = true
		}
		remaining[i] = left
	}
	return remaining, absorbed
}

// assign hands each item's remaining tasks to the groups in order, each taking
// what it can hold.
func assign(s Snapshot, items []DemandItem, remaining []int64, plans []*groupPlan) {
	for i, item := range items {
		for _, p := range plans {
			if remaining[i] == 0 {
				break
			}
			remaining[i] -= p.take(s, item, remaining[i])
		}
	}
}

// groupPlan is one group's evaluation in progress.
type groupPlan struct {
	g store.CapacityGroup
	// members are the group's members sorted by worker ID.
	members []Member
	online  int
	offline int
	// wakeable are a named group's offline members that are not disabled:
	// the machines it could add.
	wakeable []store.Worker
	// profile stands for one machine of a sized group.
	profile store.Worker
	// perMachine is the cores one new machine brings: a sized group's
	// profile, a named group's mean member, never below 1.
	perMachine int64
	// room is the cores the group's free machine slots can still hold.
	room   int64
	demand int64
}

func newGroupPlan(g store.CapacityGroup, members []Member) *groupPlan {
	p := &groupPlan{g: g, members: sortedMembers(members)}
	var cores int64
	for _, m := range p.members {
		cores += int64(m.Worker.CPUCount)
		switch {
		case online(m.Worker):
			p.online++
		default:
			p.offline++
			if !m.Worker.Disabled {
				p.wakeable = append(p.wakeable, m.Worker)
			}
		}
	}
	slots := int64(max(g.MaxMachines-p.online, 0))
	if g.Kind == store.CapacityGroupNamed {
		p.perMachine = 1
		if n := int64(len(p.members)); n > 0 {
			p.perMachine = max(cores/n, 1)
		}
		slots = min(slots, int64(len(p.wakeable)))
	} else {
		p.profile = profileWorker(g)
		p.perMachine = max(int64(g.Profile.CPUCount), 1)
	}
	p.room = satMul(slots, p.perMachine)
	return p
}

// take assigns up to want of item's tasks to the group, as many as fit the
// cores it can still hold, and returns how many it took. A group takes none
// of a queue it does not serve, nor of work no machine it could add can run.
func (p *groupPlan) take(s Snapshot, item DemandItem, want int64) int64 {
	if !p.serves(item.Demand.QueueID) {
		return 0
	}
	room := p.room
	if p.g.Kind == store.CapacityGroupNamed {
		// Only the members that could run it can be woken for it.
		capable := int64(0)
		for _, w := range p.wakeable {
			if runs(s, w, item) {
				capable++
			}
		}
		room = min(room, satMul(capable, p.perMachine))
	} else if !runs(s, p.profile, item) {
		return 0
	}
	cost := taskCost(item, p.perMachine)
	took := min(want, room/cost)
	p.room -= took * cost
	p.demand += took * cost
	return took
}

// serves reports whether the group's queue allow-list admits queueID.
func (p *groupPlan) serves(queueID string) bool {
	return len(p.g.QueueIDs) == 0 || slices.Contains(p.g.QueueIDs, queueID)
}

// status sizes the group and picks its retire candidates.
func (p *groupPlan) status(s Snapshot, absorbed map[string]bool) Status {
	idle := p.retirable(s.Now, absorbed)
	desired := p.desired(s, p.online-len(idle))
	st := Status{
		GroupID:         p.g.ID,
		GroupName:       p.g.Name,
		DemandCores:     toInt(p.demand),
		DesiredMachines: desired,
		OnlineMembers:   p.online,
		OfflineMembers:  p.offline,
		EvaluatedAt:     s.Now,
	}
	if n := p.online - desired; n > 0 {
		for _, m := range idle[:min(n, len(idle))] {
			st.RetireCandidates = append(st.RetireCandidates, m.Worker.ID)
		}
	}
	return st
}

// retirable returns the online, enabled members idle for at least the idle
// timeout that absorbed no demand, longest idle first, then by worker ID.
func (p *groupPlan) retirable(now time.Time, absorbed map[string]bool) []Member {
	var out []Member
	for _, m := range p.members {
		w := m.Worker
		if !online(w) || w.Disabled || m.IdleSince == nil || absorbed[w.ID] {
			continue
		}
		if now.Sub(*m.IdleSince) >= p.g.IdleTimeout {
			out = append(out, m)
		}
	}
	slices.SortStableFunc(out, func(a, b Member) int {
		return cmp.Or(a.IdleSince.Compare(*b.IdleSince), cmp.Compare(a.Worker.ID, b.Worker.ID))
	})
	return out
}

// desired is the group's desired size: the online members it keeps (kept),
// plus the machines its demand calls for, at most max_step_up of them and none
// inside the scale-up cooldown, clamped to [min, max]. It records the step-up
// in s.LastStepUp.
func (p *groupPlan) desired(s Snapshot, kept int) int {
	g := p.g
	last, stepped := s.LastStepUp[g.ID]
	// A step-up recorded in the future (the wall clock stepped back) holds
	// too, but for at most one cooldown, so a large jump cannot hold forever.
	elapsed := s.Now.Sub(last)
	cooling := stepped && g.ScaleUpCooldown > 0 && elapsed < g.ScaleUpCooldown && elapsed > -g.ScaleUpCooldown

	add := 0
	if !cooling {
		need := ceilDiv(p.demand, p.perMachine)
		add = toInt(min(need, int64(max(g.MaxStepUp, 0))))
	}
	desired := min(kept+add, g.MaxMachines)
	desired = max(desired, g.MinMachines)

	if desired > p.online && !cooling && s.LastStepUp != nil {
		s.LastStepUp[g.ID] = s.Now
	}
	return desired
}

// sortedMembers returns members sorted by worker ID.
func sortedMembers(members []Member) []Member {
	out := slices.Clone(members)
	slices.SortStableFunc(out, func(a, b Member) int { return cmp.Compare(a.Worker.ID, b.Worker.ID) })
	return out
}

// profileWorker returns a synthetic worker standing for one machine of g, as
// the scheduler's profileWorker builds it: the group's farm and the hardware,
// platform, tags, extensions and EXPR limits its profile declares, with the
// group's sole queue as its queue affinity when it serves exactly one.
func profileWorker(g store.CapacityGroup) store.Worker {
	pr := g.Profile
	w := store.Worker{
		FarmID:          g.FarmID,
		ComputeLocation: pr.ComputeLocation,
		OS:              pr.OS,
		Arch:            pr.Arch,
		CPUCount:        pr.CPUCount,
		RAMMb:           pr.RAMMb,
		GPUInfo:         store.GPUInfo{Count: pr.GPUCount, VRAMMb: pr.GPUMemoryMb},
		Tags:            maps.Clone(pr.Tags),
		Extensions:      slices.Clone(pr.Extensions),
		ExprLimits:      pr.ExprLimits,
	}
	if len(g.QueueIDs) == 1 {
		w.QueueID = g.QueueIDs[0]
	}
	return w
}

// ceilDiv is a/b rounded up, for a >= 0 and b >= 1, without overflow.
func ceilDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 {
		q++
	}
	return q
}

// satMul is a*b for non-negative operands, saturating at math.MaxInt64.
func satMul(a, b int64) int64 {
	if a == 0 || b == 0 {
		return 0
	}
	if a > math.MaxInt64/b {
		return math.MaxInt64
	}
	return a * b
}

// toInt converts a non-negative int64, saturating at math.MaxInt.
func toInt(v int64) int {
	if v > math.MaxInt {
		return math.MaxInt
	}
	return int(v)
}
