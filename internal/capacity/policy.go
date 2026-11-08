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
//  3. Retire. Online members idle for at least the idle timeout that absorbed
//     none of the demand are retire candidates, longest idle first, as many as
//     can go without leaving fewer online than min_machines or than the
//     members kept (busy, idle too briefly, disabled, or absorbing demand).
//  4. Size. The desired size counts the machines present: with demand to
//     add machines for, the online members (retire candidates included) plus
//     those machines; with none, the members kept. Growth is limited per
//     evaluation by max_step_up and the scale-up cooldown, and the result is
//     clamped to [min, max]. Machines a step-up asked for that have not
//     registered yet are held for one boot timeout, so the size does not fall
//     back while they boot, but new growth is measured from registered members
//     only, so machines that never register cost at most one step.
//
// All demand arithmetic is on task counts per step, never per task, so a step
// with a million ready tasks costs the same as one with a single task.
package capacity

import (
	"cmp"
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
	// cooldown applies and no machine counts as booting.
	LastStepUp map[string]time.Time
	// LastDesired is each group's DesiredMachines from the previous
	// evaluation, by group ID. The caller keeps the map between evaluations;
	// Evaluate writes every group's entry (see [Evaluate]). A nil map records
	// nothing, so no machine counts as booting.
	LastDesired map[string]int
}

// Status is one group's evaluation.
type Status struct {
	GroupID, GroupName string
	// DemandCores is the cores of ready work assigned to the group: work it
	// could run that no idle worker absorbed and no higher-priority group
	// took. It never exceeds what the group's free machine slots could hold.
	DemandCores int
	// DesiredMachines is the total the group should have, counting the
	// machines present. While its demand calls for machines it is the online
	// members, retire candidates included, plus those machines; otherwise it
	// is the members it keeps. Either way at least min_machines, at most
	// max_machines. See [Evaluate] for how an actuator reads it.
	DesiredMachines int
	// OnlineMembers and OfflineMembers count the group's members by liveness;
	// a disabled member counts in whichever it is.
	OnlineMembers, OfflineMembers int
	// RetireCandidates are worker IDs, longest idle first. Retiring all of
	// them never leaves fewer online members than min_machines, and never
	// retires a member that absorbed demand.
	RetireCandidates []string
	// Mismatches are a sized group's online members whose registration falls
	// short of the group's profile, by worker ID. Evaluate leaves it empty;
	// the caller, which can see each member's registration, fills it. A
	// mismatched member is still a member.
	Mismatches  []Mismatch
	EvaluatedAt time.Time
}

// Mismatch is a member whose registration does not match its sized group's
// declared profile.
type Mismatch struct {
	WorkerID string
	// Reason names every way the member falls short, joined by "; ".
	Reason string
}

// Evaluate returns one Status per group, in s.Groups order.
//
// DesiredMachines is a total target: the machines the group should have,
// counting those already present. An incremental actuator reads a Status as
// two independent instructions:
//
//   - Grow: add (launch or wake) DesiredMachines − OnlineMembers machines,
//     less the machines it has already launched that have not registered
//     yet, when that is positive.
//   - Shrink: retire the RetireCandidates, by name.
//
// It never subtracts the candidates as well: they are already counted in
// DesiredMachines while the group grows (they are present until retired), and
// already excluded from it while the group shrinks (DesiredMachines is then
// the members kept, so retiring the candidates brings the group to it). A
// count-driven autoscaler that cannot retire by name sets its capacity to
// DesiredMachines and may then remove the wrong machine when shrinking.
//
// Machines that arrive unable to run the demand that called for them (an
// image missing what the profile promises) stay idle, so the demand stays and
// the group keeps growing. That growth is bounded by max_machines and paced by
// registration (below), and the sweep reports such members as profile
// mismatches; backing off is left to the providers that act on the signal.
//
// Before any limit, the desired size is online + need when the group's
// demand calls for need > 0 more machines (need = DemandCores over one
// machine's cores, rounded up), and kept, the online members that are not
// retire candidates, when it calls for none. min_machines then raises it, so
// on a cold start the machines min asks for and the machines demand asks for
// are the same machines.
//
// The limits compare two numbers. online is the group's online members.
// current adds the machines asked for but not yet registered: it is
// max(online, LastDesired[g]) while the last step-up is within g's
// BootTimeout, and online after that. The desired size may then be:
//
//   - outside the scale-up cooldown, at most max(current, online + MaxStepUp):
//     what was asked for is held, and new growth is paced by machines that
//     actually registered, so a step-up whose machines never register costs
//     one step and does not compound;
//   - inside the cooldown, at most current.
//
// Falling demand lowers the desired size below both. min_machines and
// max_machines apply last. A recorded time up to one window (cooldown or boot
// timeout) in the future, as after the wall clock steps back, counts as
// inside that window.
//
// It writes two entries per group g and nothing else:
//
//   - s.LastDesired[g.ID] = the group's DesiredMachines, on every evaluation.
//   - s.LastStepUp[g.ID] = s.Now, when the desired size exceeds current and
//     g was not inside its scale-up cooldown. Holding machines that are still
//     booting is not a step-up, and a step-up forced by min_machines during a
//     cooldown does not restart the cooldown.
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
		cores = satAdd(cores, int64(max(m.Worker.CPUCount, 0)))
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
		p.profile = g.ProfileWorker()
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

// status picks the group's retire candidates and sizes it.
func (p *groupPlan) status(s Snapshot, absorbed map[string]bool) Status {
	idle := p.retirable(s.Now, absorbed)
	kept := p.online - len(idle)
	// Invariant: retiring every candidate leaves at least max(min, kept)
	// members online, so never fewer than min_machines, and every candidate
	// is idle past the idle timeout and absorbed none of this evaluation's
	// demand; the members that did are among the kept.
	retiring := idle[:max(p.online-max(p.g.MinMachines, kept), 0)]
	st := Status{
		GroupID:         p.g.ID,
		GroupName:       p.g.Name,
		DemandCores:     toInt(p.demand),
		DesiredMachines: p.desired(s, kept),
		OnlineMembers:   p.online,
		OfflineMembers:  p.offline,
		EvaluatedAt:     s.Now,
	}
	for _, m := range retiring {
		st.RetireCandidates = append(st.RetireCandidates, m.Worker.ID)
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

// desired is the group's desired size (see [Evaluate]): the online members
// plus the machines its demand calls for, or the members it keeps when it
// calls for none, limited and clamped. It records the result in s.LastDesired
// and a step-up in s.LastStepUp.
func (p *groupPlan) desired(s Snapshot, kept int) int {
	g := p.g
	last, stepped := s.LastStepUp[g.ID]
	cooling := stepped && within(s.Now.Sub(last), g.ScaleUpCooldown)
	current := p.current(s)

	limit := int64(current)
	if !cooling {
		limit = max(limit, satAdd(int64(p.online), int64(max(g.MaxStepUp, 0))))
	}
	computed := int64(kept)
	if need := ceilDiv(p.demand, p.perMachine); need > 0 {
		// Retire candidates are present until retired, so growth counts them.
		computed = satAdd(int64(p.online), need)
	}
	desired := toInt(min(computed, limit, int64(g.MaxMachines)))
	desired = max(desired, g.MinMachines)

	if desired > current && !cooling && s.LastStepUp != nil {
		s.LastStepUp[g.ID] = s.Now
	}
	if s.LastDesired != nil {
		s.LastDesired[g.ID] = desired
	}
	return desired
}

// current is the group's size for limiting a step-up: its online members, or
// the size it last asked for while that request is younger than the boot
// timeout and its machines may still be booting.
func (p *groupPlan) current(s Snapshot) int {
	last, stepped := s.LastStepUp[p.g.ID]
	if !stepped || !within(s.Now.Sub(last), p.g.BootTimeout) {
		return p.online
	}
	return max(s.LastDesired[p.g.ID], p.online)
}

// within reports whether elapsed falls inside a window of the given length.
// A negative elapsed (the wall clock stepped back past the recorded time)
// counts as inside, but for at most one window, so a large jump cannot hold
// forever. A zero window holds nothing.
func within(elapsed, window time.Duration) bool {
	return window > 0 && elapsed < window && elapsed > -window
}

// sortedMembers returns members sorted by worker ID.
func sortedMembers(members []Member) []Member {
	out := slices.Clone(members)
	slices.SortStableFunc(out, func(a, b Member) int { return cmp.Compare(a.Worker.ID, b.Worker.ID) })
	return out
}

// ceilDiv is a/b rounded up, for a >= 0 and b >= 1, without overflow.
func ceilDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 {
		q++
	}
	return q
}

// satAdd is a+b for non-negative operands, saturating at math.MaxInt64.
func satAdd(a, b int64) int64 {
	if a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
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
