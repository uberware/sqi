// SPDX-License-Identifier: AGPL-3.0-or-later

// Package capacity decides, for each capacity group, how much waiting work it
// could serve and how many machines it should have. It is a pure function of
// a [Snapshot]: no I/O, no goroutines and no clock reads, and the same snapshot
// always gives the same answer whatever order its slices and maps arrive in.
//
// The policy, in order:
//
//  1. Absorb. Ready work that online, enabled, awake workers already have free
//     cores for is taken off the demand first: workers in no group, then each
//     group's online members, in group order. After them, each named group's
//     waking members (asked to start, not registered yet, not disabled)
//     absorb with all their cores, as pending capacity. Each worker's cores
//     are spent once across all demand, and a retiring member absorbs nothing.
//  2. Assign. What is left goes to the groups in priority order, each taking
//     only work it could run, from queues it serves, up to the machines it
//     could still add: a sized group's free slots, a named group's wakeable
//     members. What one group cannot take passes to the next; work no group
//     can run drives nothing.
//  3. Retire. Online, awake members idle for at least the idle timeout that
//     absorbed none of the demand are retire candidates, longest idle first,
//     as many as can go without leaving fewer active members than min_machines
//     or than the members kept (busy, idle too briefly, disabled, or absorbing
//     demand).
//  4. Size. The desired size counts the active members, those online and not
//     retiring, and a named group's pending members as well: with demand to
//     add machines for, the active members (retire candidates included), the
//     pending ones and those machines; with none, the members kept. Growth is
//     limited per evaluation by max_step_up and the scale-up cooldown, and the
//     result is clamped to [min, max]. max_machines caps the machines running,
//     so a retiring member still online counts toward it, and that cap wins
//     over min_machines. Machines asked for that have not registered yet are
//     held, so the size does not fall back while they boot: a step-up for one
//     boot timeout, a named group's waking members until they register or
//     their wake fails. New growth is measured from active members only, so
//     machines that never register cost at most one step.
//  5. Wake. A named group's wake candidates are its wakeable members (offline,
//     not disabled, and awake, asleep, or wake_failed for at least twice the
//     boot timeout) that could run work the group took, the smallest machine
//     first (one reporting no cores last), then the one down longest, as many
//     as the desired size has room for beside the active and pending members.
//     When min_machines asks for more than that work does, any wakeable member
//     makes up the rest.
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
	// machines present. While its demand calls for machines it is the active
	// members, retire candidates included, and a named group's pending
	// members, plus those machines; otherwise it is the members it keeps.
	// Either way at least min_machines and at most max_machines less the
	// retiring members still online, which count toward max_machines until
	// they are gone; that cap wins over min_machines. See [Evaluate] for how an
	// actuator reads it.
	DesiredMachines int
	// OnlineMembers and OfflineMembers count the group's members by liveness;
	// a disabled member counts in whichever it is. A retiring member online
	// counts as online, though sizing does not count it.
	OnlineMembers, OfflineMembers int
	// ActiveMembers is the online members that are not retiring: the members
	// sizing counts, and the ones a grow is measured from.
	ActiveMembers int
	// Waking, Retiring, Asleep and WakeFailed count the group's members by
	// power state, whatever their liveness and disabled flag.
	Waking, Retiring, Asleep, WakeFailed int
	// WakeCandidates are a named group's members to wake now, by worker ID, in
	// order: at most DesiredMachines less the active members (online, not
	// retiring) and the pending ones (waking, not disabled). Empty for a sized
	// group.
	WakeCandidates []string
	// RetireCandidates are worker IDs, longest idle first. Retiring all of
	// them never leaves fewer active members than min_machines, and never
	// retires a member that absorbed demand. Every candidate is awake.
	RetireCandidates []string
	// Mismatches are a sized group's online members whose registration falls
	// short of the group's profile, by worker ID. Evaluate leaves it empty;
	// the caller, which can see each member's registration, fills it. A
	// mismatched member is still a member.
	Mismatches  []Mismatch
	EvaluatedAt time.Time

	// Requested, Degraded and LastAction are what the actuator recorded for
	// the group: the size it last asked a sized group's provider for, why the
	// group is degraded (nil when it is not), and its latest provider action
	// (nil before the first). Evaluate leaves them empty.
	Requested  int
	Degraded   *Degraded
	LastAction *ActionRecord
}

// Degraded is why a group stopped growing, and until when.
type Degraded struct {
	Reason              string
	Since, BackoffUntil time.Time
}

// ActionRecord is one provider action and its result.
type ActionRecord struct {
	// Action is wake, grow, retire, list or configure.
	Action string
	// Result is ok, failed, refused, unsupported, timeout or expired.
	Result  string
	Message string
	At      time.Time
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
//   - Grow: a named group wakes the WakeCandidates, by name. A sized group
//     adds DesiredMachines − ActiveMembers machines, less the machines it has
//     already launched that have not registered yet, when that is positive.
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
// active is the group's online members that are not retiring, and pending a
// named group's waking members that are not disabled (zero for a sized
// group). Before any limit, the desired size is active + pending + need when
// the group's demand calls for need > 0 more machines (need = DemandCores over
// one machine's cores, rounded up), and kept, the active members that are not
// retire candidates, when it calls for none. min_machines then raises it, so
// on a cold start the machines min asks for and the machines demand asks for
// are the same machines.
//
// The limits compare two numbers. current adds the machines asked for but not
// yet registered to active: it is the larger of active + pending and, while
// the last step-up is within g's BootTimeout, LastDesired[g]. The desired size
// may then be:
//
//   - outside the scale-up cooldown, at most max(current, active + MaxStepUp):
//     what was asked for is held, and new growth is paced by machines that
//     actually registered, so a step-up whose machines never register costs
//     one step and does not compound;
//   - inside the cooldown, at most current.
//
// Falling demand lowers the desired size below both. min_machines and
// max_machines apply last, max_machines less the retiring members still
// online, since max_machines caps the machines running; that cap wins over
// min_machines. The free slots demand may fill are likewise max_machines less
// the online and pending members, retiring ones included. A recorded time up
// to one window (cooldown or boot timeout) in the future, as after the wall
// clock steps back, counts as inside that window.
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
		plans[i] = newGroupPlan(g, s.Members[g.ID], s.Now)
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

// wakeFailedBackoff is how many boot timeouts a member whose wake failed waits
// before it is wakeable again.
const wakeFailedBackoff = 2

// takesWork reports whether w's free cores absorb demand now: online,
// enabled and awake.
func takesWork(w store.Worker) bool {
	return online(w) && !w.Disabled && w.EffectivePowerState() == store.PowerAwake
}

// Pending reports whether w is pending capacity: a machine asked to start
// that has not registered yet, and is not disabled.
func Pending(w store.Worker) bool {
	return !online(w) && !w.Disabled && w.EffectivePowerState() == store.PowerWaking
}

// Wakeable reports whether w could be woken at now, bootTimeout being its
// group's: offline, not disabled,
// and awake or asleep, or wake_failed for at least wakeFailedBackoff boot
// timeouts. A wake_failed member with no recorded time is wakeable, and one
// recorded in the future holds for at most one backoff, as [within] does.
func Wakeable(w store.Worker, bootTimeout time.Duration, now time.Time) bool {
	if online(w) || w.Disabled {
		return false
	}
	switch w.EffectivePowerState() {
	case store.PowerAwake, store.PowerAsleep:
		return true
	case store.PowerWakeFailed:
		backoff := time.Duration(math.MaxInt64)
		if bootTimeout < backoff/wakeFailedBackoff {
			backoff = wakeFailedBackoff * bootTimeout
		}
		return w.PowerStateAt == nil || !within(now.Sub(*w.PowerStateAt), backoff)
	default:
		return false
	}
}

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

// absorbers lists the workers that can take demand, in absorption order: the
// online, enabled, awake workers (s.Others by ID, then each group's online
// members by ID in group order) with their free cores, then each named group's
// pending members by ID in group order with all their cores. A worker listed
// more than once appears once, where first seen.
func absorbers(s Snapshot, plans []*groupPlan) []*absorber {
	var out []*absorber
	seen := map[string]bool{}
	add := func(w store.Worker, free int64) {
		if seen[w.ID] {
			return
		}
		seen[w.ID] = true
		if free > 0 {
			out = append(out, &absorber{w: w, free: free})
		}
	}
	addOnline := func(m Member) {
		if takesWork(m.Worker) {
			add(m.Worker, machineCores(m.Worker)-int64(m.CommittedCores))
		}
	}
	for _, m := range sortedMembers(s.Others) {
		addOnline(m)
	}
	for _, p := range plans {
		for _, m := range p.members {
			addOnline(m)
		}
	}
	for _, p := range plans {
		for _, w := range p.pendingMembers {
			add(w, machineCores(w))
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
	// online and offline count the members by liveness.
	online  int
	offline int
	// active is the online members that are not retiring: the ones sizing
	// counts.
	active int
	// pendingMembers are a named group's waking members that are not
	// disabled, by worker ID: machines asked for that have not registered
	// yet. None for a sized group, whose machines asked for are its launch
	// record, not waking members.
	pendingMembers []store.Worker
	// byPower counts the members by power state.
	byPower map[store.PowerState]int
	// wakeable are a named group's members that could be woken now: the
	// machines it could add.
	wakeable []store.Worker
	// taken are the demand items a named group took any of, which its wake
	// candidates must be able to run.
	taken []DemandItem
	// profile stands for one machine of a sized group.
	profile store.Worker
	// perMachine is the cores one new machine brings: a sized group's
	// profile, a named group's mean member, never below 1.
	perMachine int64
	// room is the cores the group's free machine slots can still hold.
	room   int64
	demand int64
}

// pending is how many machines a named group has asked for that have not
// registered yet.
func (p *groupPlan) pending() int { return len(p.pendingMembers) }

func newGroupPlan(g store.CapacityGroup, members []Member, now time.Time) *groupPlan {
	p := &groupPlan{g: g, members: sortedMembers(members), byPower: map[store.PowerState]int{}}
	isNamed := g.Kind == store.CapacityGroupNamed
	var cores int64
	for _, m := range p.members {
		w := m.Worker
		cores = satAdd(cores, int64(max(w.CPUCount, 0)))
		p.byPower[w.EffectivePowerState()]++
		switch {
		case online(w):
			p.online++
			if w.EffectivePowerState() != store.PowerRetiring {
				p.active++
			}
		default:
			p.offline++
			if isNamed && Pending(w) {
				p.pendingMembers = append(p.pendingMembers, w)
			}
			if isNamed && Wakeable(w, g.BootTimeout, now) {
				p.wakeable = append(p.wakeable, w)
			}
		}
	}
	// max_machines caps the machines running, so a retiring member still
	// online takes a slot until it is gone.
	slots := int64(max(g.MaxMachines-p.online-p.pending(), 0))
	if isNamed {
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
	if took > 0 && p.g.Kind == store.CapacityGroupNamed {
		p.taken = append(p.taken, item)
	}
	return took
}

// serves reports whether the group's queue allow-list admits queueID.
func (p *groupPlan) serves(queueID string) bool {
	return len(p.g.QueueIDs) == 0 || slices.Contains(p.g.QueueIDs, queueID)
}

// status picks the group's retire and wake candidates and sizes it.
func (p *groupPlan) status(s Snapshot, absorbed map[string]bool) Status {
	idle := p.retirable(s.Now, absorbed)
	kept := p.active - len(idle)
	// Invariant: retiring every candidate leaves at least max(min, kept)
	// active members, so never fewer than min_machines, and every candidate
	// is awake, idle past the idle timeout and absorbed none of this
	// evaluation's demand; the members that did are among the kept.
	retire := idle[:max(p.active-max(p.g.MinMachines, kept), 0)]
	desired := p.desired(s, kept)
	st := Status{
		GroupID:         p.g.ID,
		GroupName:       p.g.Name,
		DemandCores:     toInt(p.demand),
		DesiredMachines: desired,
		OnlineMembers:   p.online,
		OfflineMembers:  p.offline,
		ActiveMembers:   p.active,
		Waking:          p.byPower[store.PowerWaking],
		Retiring:        p.byPower[store.PowerRetiring],
		Asleep:          p.byPower[store.PowerAsleep],
		WakeFailed:      p.byPower[store.PowerWakeFailed],
		WakeCandidates:  p.wakeCandidates(s, desired),
		EvaluatedAt:     s.Now,
	}
	for _, m := range retire {
		st.RetireCandidates = append(st.RetireCandidates, m.Worker.ID)
	}
	return st
}

// wakeCandidates picks a named group's members to wake (see [Status]): the
// wakeable members that could run an item the group took, in wake order, as
// many as desired leaves room for beside the active and pending members. Any
// other wakeable member makes up what min_machines still asks for after them.
// The capable members alone bound the rest: demand never wakes a machine that
// could run none of it.
func (p *groupPlan) wakeCandidates(s Snapshot, desired int) []string {
	if p.g.Kind != store.CapacityGroupNamed {
		return nil
	}
	room := desired - p.active - p.pending()
	if room <= 0 {
		return nil
	}
	var capable, others []store.Worker
	for _, w := range p.wakeable {
		if slices.ContainsFunc(p.taken, func(item DemandItem) bool { return runs(s, w, item) }) {
			capable = append(capable, w)
		} else {
			others = append(others, w)
		}
	}
	slices.SortStableFunc(capable, wakeOrder)
	slices.SortStableFunc(others, wakeOrder)
	fromCapable := min(room, len(capable))
	short := max(p.g.MinMachines-p.active-p.pending()-fromCapable, 0)
	fromOthers := min(room-fromCapable, short, len(others))
	var out []string
	for _, w := range capable[:fromCapable] {
		out = append(out, w.ID)
	}
	for _, w := range others[:fromOthers] {
		out = append(out, w.ID)
	}
	return out
}

// wakeOrder orders wakeable members: the smallest machine first, one that
// reports no cores last, then the one down longest (see [downSince]), then by
// worker ID.
func wakeOrder(a, b store.Worker) int {
	cores := func(w store.Worker) int {
		if w.CPUCount <= 0 {
			return math.MaxInt
		}
		return w.CPUCount
	}
	return cmp.Or(
		cmp.Compare(cores(a), cores(b)),
		compareDown(downSince(a), downSince(b)),
		cmp.Compare(a.ID, b.ID),
	)
}

// downSince is the earlier of w's PowerStateAt and LastHeartbeatAt, or nil
// when neither is recorded.
func downSince(w store.Worker) *time.Time {
	at, hb := w.PowerStateAt, w.LastHeartbeatAt
	switch {
	case at == nil:
		return hb
	case hb == nil || at.Before(*hb):
		return at
	default:
		return hb
	}
}

// compareDown orders two down-since times, earliest first, a member with none
// recorded before any.
func compareDown(a, b *time.Time) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return -1
	case b == nil:
		return 1
	default:
		return a.Compare(*b)
	}
}

// retirable returns the online, enabled, awake members idle for at least the
// idle timeout that absorbed no demand, longest idle first, then by worker ID.
func (p *groupPlan) retirable(now time.Time, absorbed map[string]bool) []Member {
	var out []Member
	for _, m := range p.members {
		w := m.Worker
		if !takesWork(w) || m.IdleSince == nil || absorbed[w.ID] {
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

// desired is the group's desired size (see [Evaluate]): the active and
// pending members plus the machines its demand calls for, or the members it
// keeps when it calls for none, limited and clamped. It records the result in
// s.LastDesired and a step-up in s.LastStepUp.
func (p *groupPlan) desired(s Snapshot, kept int) int {
	g := p.g
	last, stepped := s.LastStepUp[g.ID]
	cooling := stepped && within(s.Now.Sub(last), g.ScaleUpCooldown)
	current := p.current(s)

	limit := int64(current)
	if !cooling {
		limit = max(limit, satAdd(int64(p.active), int64(max(g.MaxStepUp, 0))))
	}
	computed := int64(kept)
	if need := ceilDiv(p.demand, p.perMachine); need > 0 {
		// Retire candidates are present until retired, so growth counts them.
		computed = satAdd(int64(p.active+p.pending()), need)
	}
	desired := toInt(min(computed, limit, int64(g.MaxMachines)))
	desired = max(desired, g.MinMachines)
	// max_machines caps the machines running: a retiring member still online
	// counts toward it, even against min_machines.
	desired = min(desired, max(g.MaxMachines-(p.online-p.active), 0))

	if desired > current && !cooling && s.LastStepUp != nil {
		s.LastStepUp[g.ID] = s.Now
	}
	if s.LastDesired != nil {
		s.LastDesired[g.ID] = desired
	}
	return desired
}

// current is the group's size for limiting a step-up: its active and pending
// members, or the size it last asked for while that request is younger than
// the boot timeout and its machines may still be booting.
func (p *groupPlan) current(s Snapshot) int {
	present := p.active + p.pending()
	last, stepped := s.LastStepUp[p.g.ID]
	if !stepped || !within(s.Now.Sub(last), p.g.BootTimeout) {
		return present
	}
	return max(s.LastDesired[p.g.ID], present)
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
