// SPDX-License-Identifier: AGPL-3.0-or-later

package capacity

import (
	"fmt"
	"math"
	"slices"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store"
)

var now = time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)

// canRun stands in for the scheduler's couldRun: the step's Name, when set,
// names a tag the worker must carry; a worker with a queue affinity runs only
// that queue; declared cores must fit the machine.
func canRun(w store.Worker, item DemandItem) bool {
	if tag := item.Step.Name; tag != "" {
		if _, ok := w.Tags[tag]; !ok {
			return false
		}
	}
	if w.QueueID != "" && w.QueueID != item.Demand.QueueID {
		return false
	}
	req := item.Demand.RequiredCores
	return req <= 0 || w.CPUCount <= 0 || req <= w.CPUCount
}

func worker(id string, cpus int, tags ...string) store.Worker {
	w := store.Worker{
		ID: id, CPUCount: cpus, Status: store.WorkerStatusOnline, PowerState: store.PowerAwake,
		Tags: map[string]string{},
	}
	for _, t := range tags {
		w.Tags[t] = "1"
	}
	return w
}

// power puts w in the given power state, entered ago before now.
func power(w store.Worker, state store.PowerState, ago time.Duration) store.Worker {
	at := now.Add(-ago)
	w.PowerState, w.PowerStateAt = state, &at
	return w
}

// sleeper is an offline member asleep since ago before now.
func sleeper(w store.Worker, ago time.Duration) Member {
	return Member{Worker: power(offline(w), store.PowerAsleep, ago)}
}

// waking is an offline member woken ago before now.
func waking(w store.Worker, ago time.Duration) Member {
	return Member{Worker: power(offline(w), store.PowerWaking, ago)}
}

func offline(w store.Worker) store.Worker {
	w.Status = store.WorkerStatusOffline
	return w
}

func disabled(w store.Worker) store.Worker {
	w.Disabled = true
	return w
}

// busy is a member running work, committing the given cores.
func busy(w store.Worker, committed int) Member {
	return Member{Worker: w, CommittedCores: committed}
}

// idle is a member that has been idle for d.
func idle(w store.Worker, d time.Duration) Member {
	since := now.Add(-d)
	return Member{Worker: w, IdleSince: &since}
}

// item is a step with ready tasks each needing cores; tag, when set, must be
// carried by any worker that runs it.
func item(step, queue string, cores, ready int, tag string) DemandItem {
	return DemandItem{
		Demand: store.StepDemand{JobID: "job-" + step, StepID: step, QueueID: queue, RequiredCores: cores, Ready: ready},
		Job:    store.Job{ID: "job-" + step, Priority: 50, CreatedAt: now.Add(-time.Hour)},
		Step:   store.Step{ID: step, Name: tag},
	}
}

func group(id string, kind store.CapacityGroupKind, priority, cpus, minM, maxM, stepUp int, tags ...string) store.CapacityGroup {
	g := store.CapacityGroup{
		ID: id, Name: id, Kind: kind, Enabled: true,
		MinMachines: minM, MaxMachines: maxM, Priority: priority, MaxStepUp: stepUp,
		IdleTimeout: 10 * time.Minute, BootTimeout: 10 * time.Minute,
		Profile: store.MachineProfile{CPUCount: cpus, Tags: map[string]string{}},
	}
	for _, t := range tags {
		g.Profile.Tags[t] = "1"
	}
	return g
}

func sized(id string, priority, cpus, minM, maxM, stepUp int, tags ...string) store.CapacityGroup {
	return group(id, store.CapacityGroupSized, priority, cpus, minM, maxM, stepUp, tags...)
}

func named(id string, priority, minM, maxM, stepUp int) store.CapacityGroup {
	return group(id, store.CapacityGroupNamed, priority, 0, minM, maxM, stepUp)
}

func snapshot(groups []store.CapacityGroup, members map[string][]Member, others []Member, demand ...DemandItem) Snapshot {
	if members == nil {
		members = map[string][]Member{}
	}
	return Snapshot{
		Now: now, Groups: groups, Members: members, Others: others, Demand: demand,
		CanRun: canRun, LastStepUp: map[string]time.Time{}, LastDesired: map[string]int{},
	}
}

// brief is the part of a Status most tests assert on.
type brief struct {
	demand, desired int
	retire          []string
}

func briefs(st []Status) []brief {
	out := make([]brief, len(st))
	for i, s := range st {
		out[i] = brief{demand: s.DemandCores, desired: s.DesiredMachines, retire: s.RetireCandidates}
	}
	return out
}

func assertBriefs(t *testing.T, got []Status, want ...brief) {
	t.Helper()
	g := briefs(got)
	if len(g) != len(want) {
		t.Fatalf("got %d statuses, want %d: %+v", len(g), len(want), g)
	}
	for i := range want {
		if g[i].demand != want[i].demand || g[i].desired != want[i].desired || !slices.Equal(g[i].retire, want[i].retire) {
			t.Errorf("group %d (%s): got demand=%d desired=%d retire=%v, want demand=%d desired=%d retire=%v",
				i, got[i].GroupName, g[i].demand, g[i].desired, g[i].retire, want[i].demand, want[i].desired, want[i].retire)
		}
	}
}

func TestEvaluate_IneligibleDemandDrivesNothing(t *testing.T) {
	houdini := item("render", "q1", 1, 100, "houdini")
	tests := []struct {
		name    string
		groups  []store.CapacityGroup
		members map[string][]Member
		others  []Member
		want    []brief
	}{
		{
			name:   "no sized group has the tag",
			groups: []store.CapacityGroup{sized("local", 10, 8, 0, 10, 5), sized("cloud", 1, 32, 0, 50, 5, "nuke")},
			want:   []brief{{0, 0, nil}, {0, 0, nil}},
		},
		{
			name:   "busy outside worker with the tag does not make it a group's",
			groups: []store.CapacityGroup{sized("local", 10, 8, 0, 10, 5)},
			others: []Member{busy(worker("w1", 8, "houdini"), 8)},
			want:   []brief{{0, 0, nil}},
		},
		{
			name:   "named group whose members lack the tag",
			groups: []store.CapacityGroup{named("onprem", 10, 0, 10, 5)},
			members: map[string][]Member{"onprem": {
				busy(worker("m1", 8), 8), {Worker: offline(worker("m2", 8))},
			}},
			want: []brief{{0, 1, nil}},
		},
		{
			name:   "named group's only capable member is disabled",
			groups: []store.CapacityGroup{named("onprem", 10, 0, 10, 5)},
			members: map[string][]Member{"onprem": {
				{Worker: disabled(offline(worker("m1", 8, "houdini")))},
			}},
			want: []brief{{0, 0, nil}},
		},
		{
			name:   "capable member is already online and busy, nothing to wake",
			groups: []store.CapacityGroup{named("onprem", 10, 0, 10, 5)},
			members: map[string][]Member{"onprem": {
				busy(worker("m1", 8, "houdini"), 8), {Worker: offline(worker("m2", 8))},
			}},
			want: []brief{{0, 1, nil}},
		},
		{
			name:   "a later capable group still gets it, the higher one does not",
			groups: []store.CapacityGroup{sized("local", 10, 8, 0, 10, 5), sized("cloud", 1, 10, 0, 50, 5, "houdini")},
			want:   []brief{{0, 0, nil}, {100, 5, nil}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assertBriefs(t, Evaluate(snapshot(tc.groups, tc.members, tc.others, houdini)), tc.want...)
		})
	}
}

func TestEvaluate_FreeCoresOutsideGroupsAbsorbFirst(t *testing.T) {
	cloud := sized("cloud", 1, 8, 0, 100, 100)
	tests := []struct {
		name    string
		groups  []store.CapacityGroup
		members map[string][]Member
		others  []Member
		demand  []DemandItem
		want    []brief
	}{
		{
			name:   "idle outside worker absorbs all",
			others: []Member{idle(worker("w1", 16), time.Hour)},
			demand: []DemandItem{item("a", "q1", 1, 10, "")},
			want:   []brief{{0, 0, nil}},
		},
		{
			name:   "fully busy outside worker absorbs nothing",
			others: []Member{busy(worker("w1", 16), 16)},
			demand: []DemandItem{item("a", "q1", 1, 10, "")},
			want:   []brief{{10, 2, nil}},
		},
		{
			name:   "partly busy worker absorbs its free cores",
			others: []Member{busy(worker("w1", 16), 10)},
			demand: []DemandItem{item("a", "q1", 1, 10, "")},
			want:   []brief{{4, 1, nil}},
		},
		{
			name:   "free cores too few for one task absorb nothing",
			others: []Member{busy(worker("w1", 16), 13)},
			demand: []DemandItem{item("a", "q1", 4, 2, "")},
			want:   []brief{{8, 1, nil}},
		},
		{
			name:   "a worker's free cores are spent once across items",
			others: []Member{idle(worker("w1", 8), time.Hour)},
			demand: []DemandItem{item("a", "q1", 1, 8, ""), item("b", "q1", 1, 8, "")},
			want:   []brief{{8, 1, nil}},
		},
		{
			name:   "a worker listed twice is spent once",
			others: []Member{idle(worker("w1", 8), time.Hour), idle(worker("w1", 8), time.Hour)},
			demand: []DemandItem{item("a", "q1", 1, 16, "")},
			want:   []brief{{8, 1, nil}},
		},
		{
			name:   "an outside worker that cannot run the item absorbs nothing",
			others: []Member{idle(worker("w1", 16), time.Hour)},
			demand: []DemandItem{item("a", "q1", 1, 10, "houdini")},
			groups: []store.CapacityGroup{sized("cloud", 1, 8, 0, 100, 100, "houdini")},
			want:   []brief{{10, 2, nil}},
		},
		{
			name:   "offline and disabled outside workers absorb nothing",
			others: []Member{idle(offline(worker("w1", 16)), time.Hour), idle(disabled(worker("w2", 16)), time.Hour)},
			demand: []DemandItem{item("a", "q1", 1, 10, "")},
			want:   []brief{{10, 2, nil}},
		},
		{
			name:   "an idle group member absorbs and is not retired",
			groups: []store.CapacityGroup{cloud},
			members: map[string][]Member{"cloud": {
				idle(worker("m1", 8), time.Hour), idle(worker("m2", 8), time.Hour),
			}},
			demand: []DemandItem{item("a", "q1", 1, 8, "")},
			want:   []brief{{0, 1, []string{"m2"}}},
		},
		{
			name:   "a lower group's idle member absorbs before the higher group sizes",
			groups: []store.CapacityGroup{sized("local", 10, 8, 0, 10, 10), cloud},
			members: map[string][]Member{"cloud": {
				idle(worker("m1", 8), time.Hour),
			}},
			demand: []DemandItem{item("a", "q1", 1, 12, "")},
			want:   []brief{{4, 1, nil}, {0, 1, nil}},
		},
		{
			name:   "a member also listed outside is spent once",
			groups: []store.CapacityGroup{cloud},
			members: map[string][]Member{"cloud": {
				idle(worker("w1", 8), time.Hour),
			}},
			others: []Member{idle(worker("w1", 8), time.Hour)},
			demand: []DemandItem{item("a", "q1", 1, 16, "")},
			want:   []brief{{8, 2, nil}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			groups := tc.groups
			if groups == nil {
				groups = []store.CapacityGroup{cloud}
			}
			assertBriefs(t, Evaluate(snapshot(groups, tc.members, tc.others, tc.demand...)), tc.want...)
		})
	}
}

func TestEvaluate_MaxAndStepUpCapAMillionTasks(t *testing.T) {
	members := map[string][]Member{"cloud": {
		busy(worker("m1", 32), 32), busy(worker("m2", 32), 32), busy(worker("m3", 32), 32),
	}}
	tests := []struct {
		name  string
		cores int
		ready int
		want  brief
	}{
		{"one core each", 1, 1_000_000, brief{(50 - 3) * 32, 3 + 5, nil}},
		{"undeclared cores", 0, 1_000_000, brief{(50 - 3) * 32, 3 + 5, nil}},
		{"largest ready count", 1, math.MaxInt, brief{(50 - 3) * 32, 3 + 5, nil}},
		{"declared cores filling the machine", 32, math.MaxInt, brief{(50 - 3) * 32, 3 + 5, nil}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := snapshot([]store.CapacityGroup{sized("cloud", 1, 32, 0, 50, 5)}, members, nil,
				item("a", "q1", tc.cores, tc.ready, ""))
			assertBriefs(t, Evaluate(s), tc.want)
		})
	}

	t.Run("work does not grow with the ready count", func(t *testing.T) {
		allocs := func(ready int) float64 {
			s := snapshot([]store.CapacityGroup{sized("cloud", 1, 32, 0, 50, 5)}, members, nil,
				item("a", "q1", 1, ready, ""))
			return testing.AllocsPerRun(20, func() { Evaluate(s) })
		}
		if one, million := allocs(1), allocs(1_000_000); one != million {
			t.Errorf("allocations: %v for 1 ready task, %v for 1,000,000", one, million)
		}
	})
}

func TestEvaluate_CooldownHoldsTheSize(t *testing.T) {
	members := map[string][]Member{"cloud": {busy(worker("m1", 8), 8), busy(worker("m2", 8), 8)}}
	demand := item("a", "q1", 1, 1000, "")
	tests := []struct {
		name       string
		minM       int
		last       *time.Time
		want       int
		wantLastAt *time.Time     // nil = no entry
		cooldown   *time.Duration // nil = 5m
	}{
		{"no step-up recorded", 0, nil, 2 + 3, &now, nil},
		{"inside the cooldown", 0, new(now.Add(-30 * time.Second)), 2, new(now.Add(-30 * time.Second)), nil},
		{"cooldown just ended", 0, new(now.Add(-5 * time.Minute)), 2 + 3, &now, nil},
		{"min still applies inside the cooldown", 4, new(now.Add(-30 * time.Second)), 4, new(now.Add(-30 * time.Second)), nil},
		{"a clock that went backwards holds", 0, new(now.Add(time.Minute)), 2, new(now.Add(time.Minute)), nil},
		{"a clock far behind holds no longer than a cooldown", 0, new(now.Add(time.Hour)), 2 + 3, &now, nil},
		{"no cooldown configured", 0, new(now.Add(time.Second)), 2 + 3, &now, new(time.Duration(0))},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := sized("cloud", 1, 8, tc.minM, 100, 3)
			g.ScaleUpCooldown = 5 * time.Minute
			if tc.cooldown != nil {
				g.ScaleUpCooldown = *tc.cooldown
			}
			s := snapshot([]store.CapacityGroup{g}, members, nil, demand)
			if tc.last != nil {
				s.LastStepUp["cloud"] = *tc.last
			}
			got := Evaluate(s)
			if got[0].DesiredMachines != tc.want {
				t.Errorf("desired = %d, want %d", got[0].DesiredMachines, tc.want)
			}
			at, ok := s.LastStepUp["cloud"]
			switch {
			case tc.wantLastAt == nil && ok:
				t.Errorf("LastStepUp set to %v, want no entry", at)
			case tc.wantLastAt != nil && (!ok || !at.Equal(*tc.wantLastAt)):
				t.Errorf("LastStepUp = %v (present %v), want %v", at, ok, *tc.wantLastAt)
			}
		})
	}

	t.Run("a second evaluation inside the cooldown does not step again", func(t *testing.T) {
		g := sized("cloud", 1, 8, 0, 100, 3)
		g.ScaleUpCooldown = 5 * time.Minute
		s := snapshot([]store.CapacityGroup{g}, members, nil, demand)
		if got := Evaluate(s)[0].DesiredMachines; got != 5 {
			t.Fatalf("first evaluation desired = %d, want 5", got)
		}
		s.Now = now.Add(time.Minute)
		// The three machines asked for are still booting, so the size holds
		// at 5 rather than stepping again.
		if got := Evaluate(s)[0].DesiredMachines; got != 5 {
			t.Errorf("second evaluation desired = %d, want 5", got)
		}
		if !s.LastStepUp["cloud"].Equal(now) {
			t.Errorf("LastStepUp moved to %v", s.LastStepUp["cloud"])
		}
	})

	t.Run("a nil LastStepUp map is tolerated", func(t *testing.T) {
		g := sized("cloud", 1, 8, 0, 100, 3)
		g.ScaleUpCooldown = 5 * time.Minute
		s := snapshot([]store.CapacityGroup{g}, members, nil, demand)
		s.LastStepUp = nil
		if got := Evaluate(s)[0].DesiredMachines; got != 5 {
			t.Errorf("desired = %d, want 5", got)
		}
	})
}

func TestEvaluate_PriorityLocalBeforeCloud(t *testing.T) {
	groups := []store.CapacityGroup{sized("cloud", 1, 8, 0, 100, 10), sized("local", 10, 8, 0, 4, 10)}
	// Groups arrive ordered by priority descending.
	slices.SortFunc(groups, func(a, b store.CapacityGroup) int { return b.Priority - a.Priority })
	tests := []struct {
		name  string
		ready int
		want  []brief
	}{
		{"local takes all it can hold", 20, []brief{{20, 3, nil}, {0, 0, nil}}},
		{"local fills to max before cloud sees any", 100, []brief{{32, 4, nil}, {68, 9, nil}}},
		{"exactly local's headroom", 32, []brief{{32, 4, nil}, {0, 0, nil}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assertBriefs(t, Evaluate(snapshot(groups, nil, nil, item("a", "q1", 1, tc.ready, ""))), tc.want...)
		})
	}

	t.Run("a group already at max passes everything on", func(t *testing.T) {
		members := map[string][]Member{"local": {
			busy(worker("l1", 8), 8), busy(worker("l2", 8), 8), busy(worker("l3", 8), 8), busy(worker("l4", 8), 8),
		}}
		got := Evaluate(snapshot(groups, members, nil, item("a", "q1", 1, 16, "")))
		assertBriefs(t, got, brief{0, 4, nil}, brief{16, 2, nil})
	})
}

func TestEvaluate_QueueAllowList(t *testing.T) {
	only := func(g store.CapacityGroup, queues ...string) store.CapacityGroup {
		g.QueueIDs = queues
		return g
	}
	tests := []struct {
		name   string
		groups []store.CapacityGroup
		demand []DemandItem
		want   []brief
	}{
		{
			name:   "unlisted queue drives nothing",
			groups: []store.CapacityGroup{only(sized("cloud", 1, 8, 0, 10, 10), "q1")},
			demand: []DemandItem{item("a", "q2", 1, 16, "")},
			want:   []brief{{0, 0, nil}},
		},
		{
			name:   "listed queue drives the group",
			groups: []store.CapacityGroup{only(sized("cloud", 1, 8, 0, 10, 10), "q0", "q1")},
			demand: []DemandItem{item("a", "q1", 1, 16, ""), item("b", "q2", 1, 16, "")},
			want:   []brief{{16, 2, nil}},
		},
		{
			name:   "an empty list serves every queue",
			groups: []store.CapacityGroup{sized("cloud", 1, 8, 0, 10, 10)},
			demand: []DemandItem{item("a", "q1", 1, 16, ""), item("b", "q2", 1, 16, "")},
			want:   []brief{{32, 4, nil}},
		},
		{
			name:   "work a higher group does not list passes to the next",
			groups: []store.CapacityGroup{only(sized("local", 10, 8, 0, 10, 10), "q1"), sized("cloud", 1, 8, 0, 10, 10)},
			demand: []DemandItem{item("a", "q1", 1, 8, ""), item("b", "q2", 1, 8, "")},
			want:   []brief{{8, 1, nil}, {8, 1, nil}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assertBriefs(t, Evaluate(snapshot(tc.groups, nil, nil, tc.demand...)), tc.want...)
		})
	}
}

func TestEvaluate_MinHoldsWithNoDemand(t *testing.T) {
	tests := []struct {
		name        string
		group       store.CapacityGroup
		members     []Member
		want        brief
		wantOnline  int
		wantOffline int
	}{
		{
			name:  "nothing online",
			group: sized("cloud", 1, 8, 2, 10, 1),
			want:  brief{0, 2, nil},
		},
		{
			name:  "idle members beyond min are candidates, min are kept",
			group: sized("cloud", 1, 8, 2, 10, 1),
			members: []Member{
				idle(worker("m1", 8), time.Hour), idle(worker("m2", 8), 2*time.Hour), idle(worker("m3", 8), 3*time.Hour),
			},
			want:       brief{0, 2, []string{"m3"}},
			wantOnline: 3,
		},
		{
			name:  "min above online with members offline",
			group: named("onprem", 1, 3, 10, 1),
			members: []Member{
				idle(worker("m1", 8), time.Hour), {Worker: offline(worker("m2", 8))}, {Worker: offline(worker("m3", 8))},
			},
			want:        brief{0, 3, nil},
			wantOnline:  1,
			wantOffline: 2,
		},
		{
			name:  "min equal to max",
			group: sized("cloud", 1, 8, 2, 2, 1),
			members: []Member{
				idle(worker("m1", 8), time.Hour), idle(worker("m2", 8), time.Hour),
			},
			want:       brief{0, 2, nil},
			wantOnline: 2,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := snapshot([]store.CapacityGroup{tc.group}, map[string][]Member{tc.group.ID: tc.members}, nil)
			got := Evaluate(s)
			assertBriefs(t, got, tc.want)
			if got[0].OnlineMembers != tc.wantOnline || got[0].OfflineMembers != tc.wantOffline {
				t.Errorf("online/offline = %d/%d, want %d/%d",
					got[0].OnlineMembers, got[0].OfflineMembers, tc.wantOnline, tc.wantOffline)
			}
			if got[0].GroupID != tc.group.ID || got[0].GroupName != tc.group.Name || !got[0].EvaluatedAt.Equal(now) {
				t.Errorf("identity = %q %q %v", got[0].GroupID, got[0].GroupName, got[0].EvaluatedAt)
			}
		})
	}
}

func TestEvaluate_RetireCandidatesLongestIdleFirstNeverBelowDesired(t *testing.T) {
	five := []Member{
		idle(worker("a", 8), 30*time.Minute),
		idle(worker("b", 8), 2*time.Hour),
		idle(worker("c", 8, "houdini"), time.Hour),
		busy(worker("d", 8), 8),
		idle(worker("e", 8), 5*time.Minute), // younger than IdleTimeout
	}
	tests := []struct {
		name    string
		minM    int
		maxM    int
		members []Member
		demand  []DemandItem
		want    brief
	}{
		{"longest idle first", 0, 10, five, nil, brief{0, 2, []string{"b", "c", "a"}}},
		{"never below min", 3, 10, five, nil, brief{0, 3, []string{"b", "c"}}},
		{
			"a member that absorbs demand is kept", 0, 10, five,
			[]DemandItem{item("h", "q1", 1, 4, "houdini")},
			brief{0, 3, []string{"b", "a"}},
		},
		{
			"demand for new machines grows the group past every candidate", 0, 10, five,
			[]DemandItem{item("h", "q1", 8, 2, "houdini")},
			// c absorbs one task; the other needs a new machine. a and b cannot
			// run it, so both are candidates, and they stay counted as present
			// until retired: desired is online + 1.
			brief{8, 6, []string{"b", "a"}},
		},
		{
			"equal idle times break by worker ID", 0, 10,
			[]Member{idle(worker("z", 8), time.Hour), idle(worker("y", 8), time.Hour)},
			nil,
			brief{0, 0, []string{"y", "z"}},
		},
		{
			"a disabled member is never a candidate", 0, 10,
			[]Member{idle(disabled(worker("x", 8)), time.Hour)},
			nil,
			brief{0, 1, nil},
		},
		{
			"an offline member is never a candidate", 0, 10,
			[]Member{idle(offline(worker("x", 8)), time.Hour)},
			nil,
			brief{0, 0, nil},
		},
		{
			"max below online retires only the idle", 0, 2,
			[]Member{
				busy(worker("p", 8), 8), busy(worker("q", 8), 8),
				busy(worker("r", 8), 8), idle(worker("s", 8), time.Hour),
			},
			nil,
			brief{0, 2, []string{"s"}},
		},
		{
			"exactly at the idle timeout counts", 0, 10,
			[]Member{idle(worker("t", 8), 10*time.Minute)},
			nil,
			brief{0, 0, []string{"t"}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := sized("cloud", 1, 8, tc.minM, tc.maxM, 10, "houdini")
			s := snapshot([]store.CapacityGroup{g}, map[string][]Member{"cloud": tc.members}, nil, tc.demand...)
			got := Evaluate(s)
			assertBriefs(t, got, tc.want)
			if left := got[0].OnlineMembers - len(got[0].RetireCandidates); left < tc.minM {
				t.Errorf("retiring %v leaves %d online, below min %d", got[0].RetireCandidates, left, tc.minM)
			}
		})
	}
}

func TestEvaluate_NoFlapOnAlternatingDemand(t *testing.T) {
	g := sized("cloud", 1, 8, 0, 10, 2)
	g.ScaleUpCooldown = time.Minute
	members := map[string][]Member{"cloud": {
		idle(worker("m1", 8), time.Minute), idle(worker("m2", 8), 3*time.Minute), busy(worker("m3", 8), 8),
	}}
	s := snapshot([]store.CapacityGroup{g}, members, nil)
	for i := range 12 {
		s.Now = now.Add(time.Duration(i) * 15 * time.Second)
		s.Demand = nil
		if i%2 == 0 {
			s.Demand = []DemandItem{item("a", "q1", 1, 40, "")}
		}
		st := Evaluate(s)[0]
		if len(st.RetireCandidates) != 0 {
			t.Fatalf("evaluation %d: retire candidates %v from members younger than the idle timeout", i, st.RetireCandidates)
		}
		if st.DesiredMachines < st.OnlineMembers {
			t.Fatalf("evaluation %d: desired %d dropped below online %d", i, st.DesiredMachines, st.OnlineMembers)
		}
	}
}

func TestEvaluate_UndeclaredCoresCostAWholeMachine(t *testing.T) {
	tests := []struct {
		name    string
		group   store.CapacityGroup
		members []Member
		others  []Member
		ready   int
		want    brief
	}{
		{
			name:  "sized group costs the profile's cores",
			group: sized("cloud", 1, 16, 0, 10, 10),
			ready: 3,
			want:  brief{48, 3, nil},
		},
		{
			name:   "an entirely free outside worker absorbs one task",
			group:  sized("cloud", 1, 16, 0, 10, 10),
			others: []Member{idle(worker("w1", 8), time.Hour)},
			ready:  3,
			want:   brief{32, 2, nil},
		},
		{
			name:   "a partly busy outside worker absorbs none",
			group:  sized("cloud", 1, 16, 0, 10, 10),
			others: []Member{busy(worker("w1", 8), 1)},
			ready:  3,
			want:   brief{48, 3, nil},
		},
		{
			name:   "a worker advertising no cores still takes one",
			group:  sized("cloud", 1, 16, 0, 10, 10),
			others: []Member{idle(worker("w1", 0), time.Hour)},
			ready:  3,
			want:   brief{32, 2, nil},
		},
		{
			name:  "named group costs the members' mean",
			group: named("onprem", 1, 0, 10, 10),
			members: []Member{
				{Worker: offline(worker("m1", 4))}, {Worker: offline(worker("m2", 9))}, {Worker: offline(worker("m3", 8))},
			},
			ready: 2,
			want:  brief{14, 2, nil},
		},
		{
			name:  "named group capped at the members it can wake",
			group: named("onprem", 1, 0, 10, 10),
			members: []Member{
				{Worker: offline(worker("m1", 8))}, {Worker: disabled(offline(worker("m2", 8)))},
			},
			ready: 5,
			want:  brief{8, 1, nil},
		},
		{
			name:  "named group with no cores reported costs one",
			group: named("onprem", 1, 0, 10, 10),
			members: []Member{
				{Worker: offline(worker("m1", 0))}, {Worker: offline(worker("m2", 0))},
			},
			ready: 2,
			want:  brief{2, 2, nil},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := snapshot([]store.CapacityGroup{tc.group}, map[string][]Member{tc.group.ID: tc.members}, tc.others,
				item("a", "q1", 0, tc.ready, ""))
			assertBriefs(t, Evaluate(s), tc.want)
		})
	}
}

func TestEvaluate_Deterministic(t *testing.T) {
	groups := []store.CapacityGroup{sized("local", 10, 8, 0, 3, 10), sized("cloud", 1, 8, 0, 100, 10)}
	members := map[string][]Member{"local": {idle(worker("l2", 8), time.Hour), idle(worker("l1", 8), time.Hour)}}
	others := []Member{idle(worker("w2", 4), time.Hour), idle(worker("w1", 4), time.Hour)}
	high := item("hi", "q1", 2, 10, "")
	high.Job.Priority = 90
	demand := []DemandItem{item("b", "q1", 1, 7, ""), high, item("a", "q2", 0, 3, "")}
	first := Evaluate(snapshot(groups, members, others, demand...))
	slices.Reverse(demand)
	slices.Reverse(others)
	slices.Reverse(members["local"])
	second := Evaluate(snapshot(groups, members, others, demand...))
	if !slices.EqualFunc(briefs(first), briefs(second), func(a, b brief) bool {
		return a.demand == b.demand && a.desired == b.desired && slices.Equal(a.retire, b.retire)
	}) {
		t.Errorf("input order changed the result: %+v then %+v", briefs(first), briefs(second))
	}
}

func TestEvaluate_NilCanRunDrivesNothing(t *testing.T) {
	s := snapshot([]store.CapacityGroup{sized("cloud", 1, 8, 0, 10, 10)}, nil, nil, item("a", "q1", 1, 100, ""))
	s.CanRun = nil
	assertBriefs(t, Evaluate(s), brief{0, 0, nil})
}

func TestEvaluate_NamedGroupWakesOnlyWhatItHas(t *testing.T) {
	groups := []store.CapacityGroup{named("onprem", 10, 0, 10, 10), sized("cloud", 1, 8, 0, 100, 10)}
	members := map[string][]Member{"onprem": {
		busy(worker("m0", 8), 8),
		{Worker: offline(worker("m1", 8))},
		{Worker: offline(worker("m2", 8))},
		{Worker: disabled(offline(worker("m3", 8)))},
	}}
	// Two steps each want two machines; onprem can wake only m1 and m2, so the
	// rest overflows to cloud.
	got := Evaluate(snapshot(groups, members, nil, item("a", "q1", 8, 2, ""), item("b", "q1", 8, 2, "")))
	assertBriefs(t, got, brief{16, 3, nil}, brief{16, 2, nil})
}

func TestEvaluate_BootingMachinesDoNotFlapTheSignal(t *testing.T) {
	g := sized("cloud", 1, 8, 0, 50, 5)
	g.ScaleUpCooldown = 5 * time.Minute
	g.BootTimeout = 10 * time.Minute
	s := snapshot([]store.CapacityGroup{g}, nil, nil, item("a", "q1", 1, 1000, ""))

	// Nothing is online throughout; the five asked for at now are booting.
	for _, step := range []struct {
		at   time.Duration
		want int
	}{
		{0, 5},
		{30 * time.Second, 5},
		{time.Minute, 5},
		{4*time.Minute + 59*time.Second, 5},
		// The cooldown is over and the five are still within the boot timeout:
		// they are held, but growth beyond them waits for machines to register.
		{5 * time.Minute, 5},
		{6 * time.Minute, 5},
	} {
		s.Now = now.Add(step.at)
		st := Evaluate(s)[0]
		if st.DesiredMachines != step.want || st.OnlineMembers != 0 {
			t.Fatalf("at +%v: desired %d online %d, want desired %d online 0",
				step.at, st.DesiredMachines, st.OnlineMembers, step.want)
		}
		if s.LastDesired["cloud"] != step.want {
			t.Fatalf("at +%v: LastDesired = %d, want %d", step.at, s.LastDesired["cloud"], step.want)
		}
	}
	if !s.LastStepUp["cloud"].Equal(now) {
		t.Errorf("LastStepUp = %v, want %v (holding is not a step-up)", s.LastStepUp["cloud"], now)
	}

	// Two machines register: the next step is measured from them.
	s.Members = map[string][]Member{"cloud": {busy(worker("m1", 8), 8), busy(worker("m2", 8), 8)}}
	s.Now = now.Add(7 * time.Minute)
	if got := Evaluate(s)[0].DesiredMachines; got != 2+5 {
		t.Errorf("after two registered: desired %d, want 7", got)
	}
}

func TestEvaluate_PendingExpiresAfterBootTimeout(t *testing.T) {
	tests := []struct {
		name        string
		lastStepUp  time.Duration // before now
		cooldown    time.Duration
		lastDesired int
		want        int
	}{
		// Cooldown longer than the boot timeout: the hold lasts only as long
		// as the machines may still be booting, then falls to online.
		{"booting, cooling", 9 * time.Minute, 15 * time.Minute, 5, 5},
		{"boot timeout passed, still cooling", 11 * time.Minute, 15 * time.Minute, 5, 0},
		{"exactly the boot timeout", 10 * time.Minute, 15 * time.Minute, 5, 0},
		// No cooldown: the step is measured from the booting machines only
		// while they may still be booting.
		{"booting, not cooling", 9 * time.Minute, 0, 20, 20},
		{"boot timeout passed, not cooling", 11 * time.Minute, 0, 20, 5},
		{"a clock far behind does not hold", -time.Hour, 0, 20, 5},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := sized("cloud", 1, 8, 0, 50, 5)
			g.ScaleUpCooldown = tc.cooldown
			g.BootTimeout = 10 * time.Minute
			s := snapshot([]store.CapacityGroup{g}, nil, nil, item("a", "q1", 1, 1000, ""))
			s.LastStepUp["cloud"] = now.Add(-tc.lastStepUp)
			s.LastDesired["cloud"] = tc.lastDesired
			if got := Evaluate(s)[0].DesiredMachines; got != tc.want {
				t.Errorf("desired = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestEvaluate_FallingDemandLowersAHeldSize(t *testing.T) {
	tests := []struct {
		name     string
		cooldown time.Duration
		ready    int
		want     int
	}{
		{"inside the cooldown", 5 * time.Minute, 16, 2},
		{"outside the cooldown", 0, 16, 2},
		{"to nothing", 5 * time.Minute, 0, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := sized("cloud", 1, 8, 0, 50, 5)
			g.ScaleUpCooldown = tc.cooldown
			g.BootTimeout = 10 * time.Minute
			s := snapshot([]store.CapacityGroup{g}, nil, nil, item("a", "q1", 1, tc.ready, ""))
			s.LastStepUp["cloud"] = now.Add(-time.Minute)
			s.LastDesired["cloud"] = 5
			if got := Evaluate(s)[0].DesiredMachines; got != tc.want {
				t.Errorf("desired = %d, want %d", got, tc.want)
			}
			if s.LastDesired["cloud"] != tc.want {
				t.Errorf("LastDesired = %d, want %d", s.LastDesired["cloud"], tc.want)
			}
			if !s.LastStepUp["cloud"].Equal(now.Add(-time.Minute)) {
				t.Errorf("LastStepUp moved to %v on a fall", s.LastStepUp["cloud"])
			}
		})
	}
}

func TestEvaluate_NilLastDesiredCountsNothingBooting(t *testing.T) {
	g := sized("cloud", 1, 8, 0, 50, 5)
	g.ScaleUpCooldown = 5 * time.Minute
	s := snapshot([]store.CapacityGroup{g}, nil, nil, item("a", "q1", 1, 1000, ""))
	s.LastDesired = nil
	s.LastStepUp["cloud"] = now.Add(-time.Minute)
	if got := Evaluate(s)[0].DesiredMachines; got != 0 {
		t.Errorf("desired = %d, want 0 (cooling, nothing known to be booting)", got)
	}
}

func TestEvaluate_HugeMaxStepUpDoesNotCollapseDesired(t *testing.T) {
	busyTwo := map[string][]Member{"cloud": {busy(worker("m1", 8), 8), busy(worker("m2", 8), 8)}}
	tests := []struct {
		name        string
		stepUp      int
		lastDesired int // with a step-up a minute ago when non-zero
		want        int
	}{
		{"largest max_step_up", math.MaxInt, 0, 50},
		{"largest max_step_up while booting", math.MaxInt, math.MaxInt, 50},
		{"largest held size", 5, math.MaxInt, 50},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := sized("cloud", 1, 8, 0, 50, tc.stepUp)
			s := snapshot([]store.CapacityGroup{g}, busyTwo, nil, item("a", "q1", 1, 1000, ""))
			if tc.lastDesired != 0 {
				s.LastStepUp["cloud"] = now.Add(-time.Minute)
				s.LastDesired["cloud"] = tc.lastDesired
			}
			if got := Evaluate(s)[0].DesiredMachines; got != tc.want {
				t.Errorf("desired = %d, want %d", got, tc.want)
			}
		})
	}

	t.Run("members' cores summed without wrapping", func(t *testing.T) {
		g := named("onprem", 1, 0, 10, 10)
		members := map[string][]Member{"onprem": {
			{Worker: offline(worker("m1", math.MaxInt))}, {Worker: offline(worker("m2", math.MaxInt))},
		}}
		st := Evaluate(snapshot([]store.CapacityGroup{g}, members, nil, item("a", "q1", 0, 1, "")))[0]
		if want := toInt(satAdd(math.MaxInt, math.MaxInt) / 2); st.DemandCores != want || st.DesiredMachines != 1 {
			t.Errorf("demand %d desired %d, want demand %d desired 1", st.DemandCores, st.DesiredMachines, want)
		}
	})
}

func TestEvaluate_IdleMemberThatCannotRunTheDemandIsReplaced(t *testing.T) {
	houdini := item("r", "q1", 8, 5, "houdini")
	tests := []struct {
		name    string
		group   store.CapacityGroup
		members []Member
		demand  DemandItem
		want    brief
	}{
		{
			name:  "named: wake the capable sleeper, retire the incapable idler",
			group: named("onprem", 10, 0, 10, 10),
			members: []Member{
				idle(worker("m2", 8), time.Hour),
				{Worker: offline(worker("m3", 8, "houdini"))},
			},
			demand: houdini,
			want:   brief{8, 2, []string{"m2"}},
		},
		{
			name:  "named: the incapable idler is held by min, the sleeper still wakes",
			group: named("onprem", 10, 1, 10, 10),
			members: []Member{
				idle(worker("m2", 8), time.Hour),
				{Worker: offline(worker("m3", 8, "houdini"))},
			},
			demand: houdini,
			want:   brief{8, 2, nil},
		},
		{
			name:    "sized: a member registered without the profile's tag",
			group:   sized("cloud", 1, 8, 0, 10, 10, "houdini"),
			members: []Member{idle(worker("m1", 8), time.Hour)},
			demand:  item("r", "q1", 8, 1, "houdini"),
			want:    brief{8, 2, []string{"m1"}},
		},
		{
			name:  "a member that absorbed demand is never a candidate",
			group: sized("cloud", 1, 8, 0, 10, 10, "houdini"),
			members: []Member{
				idle(worker("m1", 8, "houdini"), time.Hour), idle(worker("m2", 8), time.Hour),
			},
			demand: item("r", "q1", 8, 2, "houdini"),
			want:   brief{8, 3, []string{"m2"}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := snapshot([]store.CapacityGroup{tc.group}, map[string][]Member{tc.group.ID: tc.members}, nil, tc.demand)
			for tick := range 3 {
				s.Now = now.Add(time.Duration(tick) * time.Minute)
				assertBriefs(t, Evaluate(s), tc.want)
			}
		})
	}
}

func TestEvaluate_NeverRegisteringGroupStaysAtOneStep(t *testing.T) {
	g := sized("cloud", 1, 8, 0, 50, 5)
	g.ScaleUpCooldown = 2 * time.Minute
	g.BootTimeout = 10 * time.Minute
	s := snapshot([]store.CapacityGroup{g}, nil, nil, item("a", "q1", 1, 100000, ""))
	for m := range 61 {
		s.Now = now.Add(time.Duration(m) * time.Minute)
		if got := Evaluate(s)[0].DesiredMachines; got != 5 {
			t.Fatalf("at +%dm with nothing ever online: desired %d, want 5", m, got)
		}
	}
}

func TestEvaluate_TwoIncapableIdleMembersDoNotBlockGrowth(t *testing.T) {
	houdini := item("r", "q1", 8, 5, "houdini")
	incapable := []Member{idle(worker("m1", 8), time.Hour), idle(worker("m2", 8), time.Hour)}
	tests := []struct {
		name    string
		group   store.CapacityGroup
		members []Member
		demand  DemandItem
		want    brief
	}{
		{
			name:    "named: wake the capable sleeper past two incapable idlers",
			group:   named("onprem", 10, 0, 10, 10),
			members: append(slices.Clone(incapable), Member{Worker: offline(worker("m3", 8, "houdini"))}),
			demand:  houdini,
			want:    brief{8, 2 + 1, []string{"m1", "m2"}},
		},
		{
			name:    "sized: launch one past two members registered without the tag",
			group:   sized("cloud", 1, 8, 0, 10, 10, "houdini"),
			members: incapable,
			demand:  item("r", "q1", 8, 1, "houdini"),
			want:    brief{8, 2 + 1, []string{"m1", "m2"}},
		},
		{
			name:    "min holds one incapable idler, growth still happens",
			group:   sized("cloud", 1, 8, 1, 10, 10, "houdini"),
			members: incapable,
			demand:  item("r", "q1", 8, 1, "houdini"),
			want:    brief{8, 2 + 1, []string{"m1"}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := snapshot([]store.CapacityGroup{tc.group}, map[string][]Member{tc.group.ID: tc.members}, nil, tc.demand)
			st := Evaluate(s)
			assertBriefs(t, st, tc.want)
			if st[0].DesiredMachines <= st[0].OnlineMembers {
				t.Errorf("desired %d does not exceed online %d: nothing would be launched",
					st[0].DesiredMachines, st[0].OnlineMembers)
			}
		})
	}
}

func TestEvaluate_ColdStartDoesNotOvershootMin(t *testing.T) {
	sixAsleep := make([]Member, 6)
	for i := range sixAsleep {
		sixAsleep[i] = Member{Worker: offline(worker(fmt.Sprintf("n%d", i), 8))}
	}
	tests := []struct {
		name    string
		group   store.CapacityGroup
		members []Member
		demand  DemandItem
		want    brief
	}{
		{"sized: min covers the one machine needed", sized("cloud", 1, 8, 2, 10, 10), nil, item("r", "q1", 8, 1, ""), brief{8, 2, nil}},
		{"named: min and demand are the same machines", named("onprem", 10, 3, 10, 10), sixAsleep, item("r", "q1", 8, 3, ""), brief{24, 3, nil}},
		{"demand beyond min", sized("cloud", 1, 8, 2, 10, 10), nil, item("r", "q1", 8, 3, ""), brief{24, 3, nil}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := snapshot([]store.CapacityGroup{tc.group}, map[string][]Member{tc.group.ID: tc.members}, nil, tc.demand)
			assertBriefs(t, Evaluate(s), tc.want)
		})
	}
}

func TestEvaluate_BrokenImageGrowthIsBoundedByMax(t *testing.T) {
	// Every machine the group adds registers without the profile's tag, so it
	// idles and the demand that called for it stays. The group keeps growing,
	// but only as machines register, by at most max_step_up a step, and never
	// past max_machines.
	g := sized("cloud", 1, 8, 0, 20, 5, "houdini")
	s := snapshot([]store.CapacityGroup{g}, nil, nil, item("r", "q1", 8, 1, "houdini"))
	var members []Member
	for m := range 120 {
		s.Now = now.Add(time.Duration(m) * time.Minute)
		s.Members = map[string][]Member{"cloud": members}
		st := Evaluate(s)[0]
		if st.DesiredMachines > 20 || st.DesiredMachines > st.OnlineMembers+5 {
			t.Fatalf("at +%dm: desired %d with %d online, max 20 and step 5", m, st.DesiredMachines, st.OnlineMembers)
		}
		for len(members) < st.DesiredMachines {
			since := s.Now
			members = append(members, Member{Worker: worker(fmt.Sprintf("m%03d", len(members)), 8), IdleSince: &since})
		}
	}
	if len(members) != 20 {
		t.Errorf("%d machines arrived, want the group to stop at max 20", len(members))
	}
}

// assertWake checks a Status's wake candidates, in order.
func assertWake(t *testing.T, st Status, want ...string) {
	t.Helper()
	if !slices.Equal(st.WakeCandidates, want) {
		t.Errorf("%s: wake candidates %v, want %v", st.GroupName, st.WakeCandidates, want)
	}
}

func TestEvaluate_WakingIsPendingCapacity(t *testing.T) {
	fourWaking := []Member{
		waking(worker("w1", 8), time.Minute), waking(worker("w2", 8), time.Minute),
		waking(worker("w3", 8), time.Minute), waking(worker("w4", 8), time.Minute),
		sleeper(worker("s1", 8), time.Hour), sleeper(worker("s2", 8), time.Hour),
		sleeper(worker("s3", 8), time.Hour), sleeper(worker("s4", 8), time.Hour),
	}
	tests := []struct {
		name     string
		members  []Member
		demand   DemandItem
		want     brief
		wantWake []string
	}{
		// With no machines left to add, desired is the members kept, which
		// leaves the waking members out: it dips while they boot.
		{"four waking members cover the demand", fourWaking, item("r", "q1", 8, 4, ""), brief{0, 0, nil}, nil},
		{"the rest wakes beside them", fourWaking, item("r", "q1", 8, 6, ""), brief{16, 4 + 2, nil}, []string{"s1", "s2"}},
		{
			"a waking member absorbs only what it can run",
			[]Member{waking(worker("w1", 8), time.Minute), sleeper(worker("s1", 8, "houdini"), time.Hour)},
			item("r", "q1", 8, 1, "houdini"),
			brief{8, 1 + 1, nil},
			[]string{"s1"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := named("onprem", 10, 0, 10, 10)
			st := Evaluate(snapshot([]store.CapacityGroup{g}, map[string][]Member{"onprem": tc.members}, nil, tc.demand))
			assertBriefs(t, st, tc.want)
			assertWake(t, st[0], tc.wantWake...)
		})
	}

	t.Run("a sized group's waking member absorbs nothing", func(t *testing.T) {
		g := sized("cloud", 1, 8, 0, 10, 10)
		members := []Member{waking(worker("w1", 8), time.Minute)}
		st := Evaluate(snapshot([]store.CapacityGroup{g}, map[string][]Member{"cloud": members}, nil, item("r", "q1", 8, 1, "")))
		assertBriefs(t, st, brief{8, 1, nil})
	})

	t.Run("counts by power state", func(t *testing.T) {
		g := named("onprem", 10, 0, 10, 10)
		st := Evaluate(snapshot([]store.CapacityGroup{g}, map[string][]Member{"onprem": fourWaking}, nil))[0]
		if st.Waking != 4 || st.Asleep != 4 || st.Retiring != 0 || st.WakeFailed != 0 {
			t.Errorf("waking/asleep/retiring/wake_failed = %d/%d/%d/%d, want 4/4/0/0",
				st.Waking, st.Asleep, st.Retiring, st.WakeFailed)
		}
		if st.OnlineMembers != 0 || st.OfflineMembers != 8 {
			t.Errorf("online/offline = %d/%d, want 0/8", st.OnlineMembers, st.OfflineMembers)
		}
	})
}

func TestEvaluate_WakeCandidatesOnlyCapable(t *testing.T) {
	members := []Member{
		sleeper(worker("a", 8), time.Hour),
		sleeper(worker("b", 8, "houdini"), time.Hour),
		sleeper(worker("c", 8, "houdini"), time.Hour),
		busy(worker("m", 8), 8),
	}
	tests := []struct {
		name   string
		demand []DemandItem
		want   brief
		wake   []string
	}{
		{"one machine's worth", []DemandItem{item("r", "q1", 8, 1, "houdini")}, brief{8, 2, nil}, []string{"b"}},
		{"more than the capable members", []DemandItem{item("r", "q1", 8, 5, "houdini")}, brief{16, 3, nil}, []string{"b", "c"}},
		{
			// Each step alone fits the two capable members, so together they
			// ask for three machines; the third is never a member that can
			// run neither step.
			"two steps overcounting the capable members",
			[]DemandItem{item("r1", "q1", 8, 2, "houdini"), item("r2", "q1", 8, 2, "houdini")},
			brief{24, 4, nil},
			[]string{"b", "c"},
		},
		{"demand no member can run", []DemandItem{item("r", "q1", 8, 1, "nuke")}, brief{0, 1, nil}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := named("onprem", 10, 0, 10, 10)
			st := Evaluate(snapshot([]store.CapacityGroup{g}, map[string][]Member{"onprem": members}, nil, tc.demand...))
			assertBriefs(t, st, tc.want)
			assertWake(t, st[0], tc.wake...)
		})
	}
}

func TestEvaluate_WakeCandidatesBestFitThenLongestDown(t *testing.T) {
	heartbeat := func(w store.Worker, ago time.Duration) store.Worker {
		at := now.Add(-ago)
		w.LastHeartbeatAt = &at
		return w
	}
	// The mean member is 11 cores: one new machine's worth.
	members := []Member{
		sleeper(worker("big", 32), 10*time.Hour),
		sleeper(worker("b", 8), time.Hour),
		sleeper(worker("c", 8), 3*time.Hour),
		{Worker: offline(worker("d", 8))}, // no time down on record
		sleeper(heartbeat(worker("e", 8), 5*time.Hour), 30*time.Minute),
		sleeper(worker("f", 8), time.Hour),
		{Worker: heartbeat(offline(worker("g", 8)), 2*time.Hour)},
	}
	all := []string{"d", "e", "c", "g", "b", "f", "big"}
	tests := []struct {
		name   string
		demand DemandItem
		want   []string
	}{
		{"all of them", item("r", "q1", 8, 10, ""), all},
		{"the first two", item("r", "q1", 8, 2, ""), []string{"d", "e"}},
		{"only the big machine fits", item("r", "q1", 10, 1, ""), []string{"big"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := named("onprem", 10, 0, 10, 10)
			st := Evaluate(snapshot([]store.CapacityGroup{g}, map[string][]Member{"onprem": members}, nil, tc.demand))
			assertWake(t, st[0], tc.want...)
		})
	}

	t.Run("a member reporting no cores goes last", func(t *testing.T) {
		g := named("onprem", 10, 0, 10, 10)
		members := []Member{
			sleeper(worker("z", 0), 10*time.Hour), sleeper(worker("a", 8), time.Hour), sleeper(worker("b", 8), time.Hour),
		}
		st := Evaluate(snapshot([]store.CapacityGroup{g}, map[string][]Member{"onprem": members}, nil, item("r", "q1", 0, 3, "")))
		assertWake(t, st[0], "a", "b", "z")
	})

	t.Run("input order does not matter", func(t *testing.T) {
		g := named("onprem", 10, 0, 10, 10)
		reversed := slices.Clone(members)
		slices.Reverse(reversed)
		st := Evaluate(snapshot([]store.CapacityGroup{g}, map[string][]Member{"onprem": reversed}, nil, item("r", "q1", 8, 10, "")))
		assertWake(t, st[0], all...)
	})
}

func TestEvaluate_WakeFailedBackoff(t *testing.T) {
	tests := []struct {
		name string
		ago  time.Duration // since the wake failed; BootTimeout is 10m
		want []string
	}{
		{"just inside the backoff", 20*time.Minute - time.Second, nil},
		{"exactly at the backoff", 20 * time.Minute, []string{"m"}},
		{"just past the backoff", 20*time.Minute + time.Second, []string{"m"}},
		{"a clock that went backwards holds", -time.Minute, nil},
		{"a clock far behind holds no longer than the backoff", -time.Hour, []string{"m"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := named("onprem", 10, 0, 10, 10)
			members := []Member{{Worker: power(offline(worker("m", 8)), store.PowerWakeFailed, tc.ago)}}
			st := Evaluate(snapshot([]store.CapacityGroup{g}, map[string][]Member{"onprem": members}, nil,
				item("r", "q1", 8, 1, "")))[0]
			assertWake(t, st, tc.want...)
			if st.DemandCores != 8*len(tc.want) {
				t.Errorf("demand %d, want %d", st.DemandCores, 8*len(tc.want))
			}
			if st.WakeFailed != 1 {
				t.Errorf("wake_failed count %d, want 1", st.WakeFailed)
			}
		})
	}
}

func TestEvaluate_RetiringNeitherAbsorbsNorCounts(t *testing.T) {
	retiringIdle := func(id string) Member {
		return idle(power(worker(id, 8), store.PowerRetiring, time.Minute), time.Hour)
	}
	tests := []struct {
		name         string
		group        store.CapacityGroup
		members      []Member
		demand       []DemandItem
		want         brief
		wake         []string
		wantRetiring int
	}{
		{
			name:         "sized: a retiring member's free cores take nothing",
			group:        sized("cloud", 1, 8, 0, 10, 10),
			members:      []Member{retiringIdle("r"), busy(worker("m", 8), 8)},
			demand:       []DemandItem{item("a", "q1", 8, 1, "")},
			want:         brief{8, 2, nil},
			wantRetiring: 1,
		},
		{
			name:  "named: a retiring member is neither capacity nor wakeable",
			group: named("onprem", 10, 0, 10, 10),
			members: []Member{
				retiringIdle("r"), busy(worker("m", 8), 8), sleeper(worker("s", 8), time.Hour),
				{Worker: power(offline(worker("o", 8)), store.PowerRetiring, time.Minute)},
			},
			demand:       []DemandItem{item("a", "q1", 8, 1, "")},
			want:         brief{8, 2, nil},
			wake:         []string{"s"},
			wantRetiring: 2,
		},
		{
			name:         "a retiring member is not kept",
			group:        sized("cloud", 1, 8, 0, 10, 10),
			members:      []Member{retiringIdle("r"), idle(worker("a", 8), time.Hour)},
			want:         brief{0, 0, []string{"a"}},
			wantRetiring: 1,
		},
		{
			name:         "a retiring member does not count toward min",
			group:        sized("cloud", 1, 8, 1, 10, 10),
			members:      []Member{retiringIdle("r"), idle(worker("a", 8), time.Hour)},
			want:         brief{0, 1, nil},
			wantRetiring: 1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := snapshot([]store.CapacityGroup{tc.group}, map[string][]Member{tc.group.ID: tc.members}, nil, tc.demand...)
			st := Evaluate(s)
			assertBriefs(t, st, tc.want)
			assertWake(t, st[0], tc.wake...)
			if st[0].Retiring != tc.wantRetiring {
				t.Errorf("retiring count %d, want %d", st[0].Retiring, tc.wantRetiring)
			}
			live := 0
			for _, m := range tc.members {
				if online(m.Worker) {
					live++
				}
			}
			if st[0].OnlineMembers != live {
				t.Errorf("online members %d, want %d: liveness counts retiring members", st[0].OnlineMembers, live)
			}
		})
	}
}

func TestEvaluate_MaxCountsRetiringMembers(t *testing.T) {
	nineBusy := func() []Member {
		out := make([]Member, 9)
		for i := range out {
			out[i] = busy(worker(fmt.Sprintf("c%d", i), 8), 8)
		}
		return out
	}
	retiring := busy(power(worker("r", 8), store.PowerRetiring, time.Minute), 8)
	retired := Member{Worker: power(offline(worker("r", 8)), store.PowerAsleep, 0)}
	sleepers := []Member{sleeper(worker("s1", 8), time.Hour), sleeper(worker("s2", 8), time.Hour)}
	tests := []struct {
		name       string
		group      store.CapacityGroup
		members    []Member
		want       brief
		wake       []string
		wantActive int
	}{
		{
			name:       "sized: at max with one retiring, no growth",
			group:      sized("cloud", 1, 8, 0, 10, 10),
			members:    append(nineBusy(), retiring),
			want:       brief{0, 9, nil},
			wantActive: 9,
		},
		{
			name:       "sized: growth resumes once the retiring member is gone",
			group:      sized("cloud", 1, 8, 0, 10, 10),
			members:    nineBusy(),
			want:       brief{8, 10, nil},
			wantActive: 9,
		},
		{
			name:       "sized: min does not push past max while one retires",
			group:      sized("cloud", 1, 8, 10, 10, 10),
			members:    append(nineBusy(), retiring),
			want:       brief{0, 9, nil},
			wantActive: 9,
		},
		{
			name:       "named: at max with one retiring, no wake candidates",
			group:      named("onprem", 10, 0, 10, 10),
			members:    append(append(nineBusy(), retiring), sleepers...),
			want:       brief{0, 9, nil},
			wantActive: 9,
		},
		{
			name:       "named: a wake resumes once the retiring member is asleep",
			group:      named("onprem", 10, 0, 10, 10),
			members:    append(append(nineBusy(), retired), sleepers...),
			want:       brief{8, 10, nil},
			wake:       []string{"s1"},
			wantActive: 9,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := snapshot([]store.CapacityGroup{tc.group}, map[string][]Member{tc.group.ID: tc.members}, nil,
				item("a", "q1", 8, 5, ""))
			st := Evaluate(s)
			assertBriefs(t, st, tc.want)
			assertWake(t, st[0], tc.wake...)
			if st[0].ActiveMembers != tc.wantActive {
				t.Errorf("active members %d, want %d", st[0].ActiveMembers, tc.wantActive)
			}
			running := st[0].DesiredMachines + st[0].OnlineMembers - st[0].ActiveMembers
			if running > tc.group.MaxMachines {
				t.Errorf("desired %d plus %d retiring online exceeds max %d",
					st[0].DesiredMachines, st[0].OnlineMembers-st[0].ActiveMembers, tc.group.MaxMachines)
			}
		})
	}
}

func TestEvaluate_RetireCandidatesAwakeOnly(t *testing.T) {
	idleIn := func(id string, state store.PowerState) Member {
		return idle(power(worker(id, 8), state, time.Minute), time.Hour)
	}
	members := []Member{
		idleIn("a", store.PowerAwake), idleIn("b", store.PowerAsleep), idleIn("c", store.PowerWakeFailed),
		idleIn("d", store.PowerWaking), idleIn("e", store.PowerRetiring),
	}
	for _, g := range []store.CapacityGroup{sized("cloud", 1, 8, 0, 10, 10), named("onprem", 1, 0, 10, 10)} {
		t.Run(string(g.Kind), func(t *testing.T) {
			st := Evaluate(snapshot([]store.CapacityGroup{g}, map[string][]Member{g.ID: members}, nil))
			// Five online, one retiring: four active, of which only a can go.
			assertBriefs(t, st, brief{0, 3, []string{"a"}})
		})
	}
}

func TestEvaluate_WakesThatNeverRegisterCostOneStep(t *testing.T) {
	g := named("onprem", 10, 0, 20, 5)
	members := make([]Member, 10)
	for i := range members {
		members[i] = sleeper(worker(fmt.Sprintf("n%d", i), 8), time.Hour)
	}
	s := snapshot([]store.CapacityGroup{g}, map[string][]Member{"onprem": members}, nil, item("r", "q1", 8, 10, ""))
	st := Evaluate(s)[0]
	assertWake(t, st, "n0", "n1", "n2", "n3", "n4")
	if st.DesiredMachines != 5 {
		t.Fatalf("first evaluation desired %d, want 5", st.DesiredMachines)
	}

	// The actuator marks them waking; none ever registers.
	for i := range 5 {
		members[i].Worker = power(members[i].Worker, store.PowerWaking, 0)
	}
	for _, d := range []time.Duration{time.Minute, 9 * time.Minute, 11 * time.Minute, 19 * time.Minute} {
		s.Now = now.Add(d)
		st := Evaluate(s)[0]
		if len(st.WakeCandidates) != 0 || st.DesiredMachines != 5 || st.Waking != 5 {
			t.Fatalf("at +%v: candidates %v desired %d waking %d, want none, 5, 5",
				d, st.WakeCandidates, st.DesiredMachines, st.Waking)
		}
	}

	// Expiry marks them failed at +20m; the next step wakes the others.
	for i := range 5 {
		members[i].Worker = power(members[i].Worker, store.PowerWakeFailed, -20*time.Minute)
	}
	s.Now = now.Add(21 * time.Minute)
	st = Evaluate(s)[0]
	assertWake(t, st, "n5", "n6", "n7", "n8", "n9")
	if st.DesiredMachines != 5 {
		t.Errorf("after the failures desired %d, want 5", st.DesiredMachines)
	}
}

func TestEvaluate_DisabledMemberNeverAWakeCandidate(t *testing.T) {
	tests := []struct {
		name    string
		members []Member
		demand  DemandItem
		want    []string
	}{
		{
			name: "a disabled sleeper, though the best fit",
			members: []Member{
				{Worker: disabled(power(offline(worker("d", 4, "houdini")), store.PowerAsleep, 5*time.Hour))},
				sleeper(worker("m", 8, "houdini"), time.Hour),
			},
			demand: item("r", "q1", 4, 1, "houdini"),
			want:   []string{"m"},
		},
		{
			name: "a disabled waking member is not pending capacity",
			members: []Member{
				{Worker: disabled(power(offline(worker("d", 8, "houdini")), store.PowerWaking, time.Minute))},
				sleeper(worker("m", 8, "houdini"), time.Hour),
			},
			demand: item("r", "q1", 8, 1, "houdini"),
			want:   []string{"m"},
		},
		{
			name: "only disabled members",
			members: []Member{
				{Worker: disabled(power(offline(worker("d", 8, "houdini")), store.PowerAsleep, time.Hour))},
			},
			demand: item("r", "q1", 8, 1, "houdini"),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := named("onprem", 10, 0, 10, 10)
			st := Evaluate(snapshot([]store.CapacityGroup{g}, map[string][]Member{"onprem": tc.members}, nil, tc.demand))
			assertWake(t, st[0], tc.want...)
		})
	}
}

func TestEvaluate_MinWakesAnyWakeableMember(t *testing.T) {
	tests := []struct {
		name    string
		group   store.CapacityGroup
		members []Member
		demand  []DemandItem
		want    brief
		wake    []string
	}{
		{
			name:  "no demand",
			group: named("onprem", 10, 2, 10, 10),
			members: []Member{
				sleeper(worker("a", 8), time.Hour), sleeper(worker("b", 8, "houdini"), time.Hour),
				{Worker: disabled(power(offline(worker("c", 8)), store.PowerAsleep, 5*time.Hour))},
			},
			want: brief{0, 2, nil},
			wake: []string{"a", "b"},
		},
		{
			name:  "capable members first, then any for min",
			group: named("onprem", 10, 3, 10, 10),
			members: []Member{
				sleeper(worker("a", 8), 5*time.Hour),
				sleeper(worker("b", 8, "houdini"), time.Hour), sleeper(worker("c", 8, "houdini"), time.Hour),
			},
			demand: []DemandItem{item("r", "q1", 8, 1, "houdini")},
			want:   brief{8, 3, nil},
			wake:   []string{"b", "c", "a"},
		},
		{
			name:  "min already met by waking members",
			group: named("onprem", 10, 2, 10, 10),
			members: []Member{
				waking(worker("w1", 8), time.Minute), waking(worker("w2", 8), time.Minute),
				sleeper(worker("a", 8), time.Hour),
			},
			want: brief{0, 2, nil},
		},
		{
			name:    "a sized group has none",
			group:   sized("cloud", 1, 8, 2, 10, 10),
			members: []Member{sleeper(worker("a", 8), time.Hour)},
			demand:  []DemandItem{item("r", "q1", 8, 1, "")},
			want:    brief{8, 2, nil},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := snapshot([]store.CapacityGroup{tc.group}, map[string][]Member{tc.group.ID: tc.members}, nil, tc.demand...)
			st := Evaluate(s)
			assertBriefs(t, st, tc.want)
			assertWake(t, st[0], tc.wake...)
		})
	}
}
