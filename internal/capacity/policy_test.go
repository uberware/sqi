// SPDX-License-Identifier: AGPL-3.0-or-later

package capacity

import (
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
	w := store.Worker{ID: id, CPUCount: cpus, Status: store.WorkerStatusOnline, Tags: map[string]string{}}
	for _, t := range tags {
		w.Tags[t] = "1"
	}
	return w
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
			"demand for new machines keeps the count", 0, 10, five,
			[]DemandItem{item("h", "q1", 8, 2, "houdini")},
			// a and b cannot run it: both are candidates, and the count rises
			// by the machine being added while one of them stands in for it.
			brief{8, 5, []string{"b", "a"}},
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
