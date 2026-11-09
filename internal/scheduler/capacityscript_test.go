// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/capacity"
	"github.com/uberware/sqi/internal/capacity/script"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// scriptModeVar turns the test binary into a capacity provider script. It
// reaches the child through script.Provider.Env, which is applied after the
// server's own SQI_ variables are filtered out, so a parent never passes it
// on by accident.
const scriptModeVar = "SQI_SCHEDULER_TEST_SCRIPT_MODE"

// scriptDirVar names the directory the child records each request in.
const scriptDirVar = "SCHEDULER_TEST_SCRIPT_DIR"

// TestMain runs the test binary as a provider script when scriptModeVar is
// set, and the tests otherwise.
func TestMain(m *testing.M) {
	if mode := os.Getenv(scriptModeVar); mode != "" {
		os.Exit(runScriptChild(mode))
	}
	os.Exit(m.Run())
}

// runScriptChild is the provider script: it appends the request it reads to
// requests.jsonl in scriptDirVar's directory and answers ok, listing one
// instance for a list.
func runScriptChild(mode string) int {
	body, err := io.ReadAll(os.Stdin)
	if err != nil || mode != "record" {
		return 3
	}
	var req script.Request
	if err := json.Unmarshal(body, &req); err != nil {
		return 3
	}
	f, err := os.OpenFile(filepath.Join(os.Getenv(scriptDirVar), "requests.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return 3
	}
	_, werr := f.Write(append(body, '\n'))
	if cerr := f.Close(); werr != nil || cerr != nil {
		return 3
	}
	resp := script.Response{Result: "ok", Message: req.Action + " accepted"}
	if req.Action == "list" {
		resp.InstanceIDs = []string{"i-orphan"}
	}
	if err := json.NewEncoder(os.Stdout).Encode(resp); err != nil {
		return 3
	}
	return 0
}

// ── fixtures ────────────────────────────────────────────────────────────────

// providerCall is one call a fakeProvider received.
type providerCall struct {
	action           string
	workerIDs        []string
	instanceIDs      []string
	desired, current int
}

// fakeProvider is a capacityProvider that records every call and answers ok
// unless told otherwise.
type fakeProvider struct {
	mu      sync.Mutex
	calls   []providerCall
	results map[string]actionResult
	listIDs []string
	// onCall, when set, runs inside each call before it answers.
	onCall func(providerCall)
}

func newFakeProvider() *fakeProvider {
	return &fakeProvider{results: map[string]actionResult{}}
}

func (p *fakeProvider) set(action string, r actionResult) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.results[action] = r
}

func (p *fakeProvider) setList(ids ...string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.listIDs = ids
}

func (p *fakeProvider) record(c providerCall) actionResult {
	p.mu.Lock()
	p.calls = append(p.calls, c)
	r, ok := p.results[c.action]
	hook := p.onCall
	p.mu.Unlock()
	if hook != nil {
		hook(c)
	}
	if !ok {
		r = actionResult{Result: resultOK, Message: c.action + " done"}
	}
	return r
}

func (p *fakeProvider) callsOf(action string) []providerCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []providerCall
	for _, c := range p.calls {
		if c.action == action {
			out = append(out, c)
		}
	}
	return out
}

func (p *fakeProvider) wake(_ context.Context, _ store.CapacityGroup, members []store.Worker) map[string]actionResult {
	return sameResult(members, p.record(providerCall{action: "wake", workerIDs: workerIDs(members)}))
}

func (p *fakeProvider) grow(_ context.Context, _ store.CapacityGroup, desired, current int) actionResult {
	return p.record(providerCall{action: "grow", desired: desired, current: current})
}

func (p *fakeProvider) retire(_ context.Context, _ store.CapacityGroup, members []store.Worker) map[string]actionResult {
	return sameResult(members, p.record(providerCall{action: "retire", workerIDs: workerIDs(members)}))
}

func (p *fakeProvider) retireInstances(_ context.Context, _ store.CapacityGroup, ids []string) actionResult {
	return p.record(providerCall{action: "retire_instances", instanceIDs: slices.Clone(ids)})
}

func (p *fakeProvider) list(context.Context, store.CapacityGroup) ([]string, actionResult) {
	r := p.record(providerCall{action: "list"})
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.listIDs), r
}

// scriptFixture is an actFixture whose script provider "cloud" is a
// fakeProvider, with one sized group using it.
type scriptFixture struct {
	*actFixture

	prov *fakeProvider
	g    store.CapacityGroup
}

// newScriptFixture builds a scriptFixture, the group changed by mut.
func newScriptFixture(t *testing.T, mut func(*store.CapacityGroup)) *scriptFixture {
	t.Helper()
	f := newActFixture(t, nil)
	prov := newFakeProvider()
	f.s.cfg.ScriptProviders = map[string]script.Provider{"cloud": {Name: "cloud", Command: []string{"unused"}}}
	f.s.capState.act.newScriptProvider = func(script.Provider) capacityProvider { return prov }
	g := createGroup(t, f.st, sizedGroup("cloud", func(g *store.CapacityGroup) {
		g.Provider = "script:cloud"
		if mut != nil {
			mut(g)
		}
	}))
	return &scriptFixture{actFixture: f, prov: prov, g: g}
}

// cloudMember registers an ephemeral member of groupID matching sizedGroup's
// profile, reporting instanceID.
func cloudMember(t *testing.T, st store.Store, groupID, id, instanceID string) store.Worker {
	t.Helper()
	return addMember(t, st, groupID, store.Worker{
		ID: id, CPUCount: 4, OS: "linux", Arch: "amd64", CloudInstanceID: instanceID,
	})
}

func groupState(t *testing.T, st store.Store, groupID string) store.CapacityGroupState {
	t.Helper()
	all, err := st.ListCapacityGroupStates(t.Context())
	if err != nil {
		t.Fatalf("ListCapacityGroupStates: %v", err)
	}
	return all[groupID]
}

func putGroupState(t *testing.T, st store.Store, s store.CapacityGroupState) {
	t.Helper()
	if err := st.PutCapacityGroupState(t.Context(), s); err != nil {
		t.Fatalf("PutCapacityGroupState: %v", err)
	}
}

// ── sized script groups ───────────────────────────────────────────────────────

// The launch record is written before the provider is asked: a provider
// call never sees a state that does not already hold its request.
func TestScriptActuator_GrowRecordsThenCalls(t *testing.T) {
	f := newScriptFixture(t, nil)
	submitReady(t, f.st, 3, store.Step{})
	t0 := time.Now().UTC()
	var seen store.CapacityGroupState
	f.prov.onCall = func(c providerCall) {
		if c.action == "grow" {
			seen = groupState(t, f.st, f.g.ID)
		}
	}

	f.sweep(t, t0)

	grows := f.prov.callsOf("grow")
	if len(grows) != 1 || grows[0].desired != 3 || grows[0].current != 0 {
		t.Fatalf("grow calls = %+v, want one grow{3, 0}", grows)
	}
	if seen.Requested != 3 || seen.RequestedAt == nil || !seen.RequestedAt.Equal(t0) {
		t.Errorf("state seen by the provider = %+v, want Requested 3 at %s", seen, t0)
	}
	if la := lastAction(t, f.s, f.g.ID); la.Action != "grow" || la.Result != "ok" {
		t.Errorf("LastAction = %+v, want grow ok", la)
	}
	if got := actionsTotal(t, f.actFixture, "cloud", "grow", "ok"); got != 1 {
		t.Errorf("actions_total{grow,ok} = %v, want 1", got)
	}
	if st, ok := f.s.CapacityStatus(f.g.ID); !ok || st.Requested != 3 {
		t.Errorf("status = %+v, want Requested 3", st)
	}

	// The request is not repeated by this process.
	f.sweep(t, t0.Add(15*time.Second))
	if n := len(f.prov.callsOf("grow")); n != 1 {
		t.Errorf("grow calls after a second sweep = %d, want 1", n)
	}
}

// A grow names the total wanted and the size last asked for (or the members
// online, when more), never a delta.
func TestScriptActuator_GrowRequestIsAbsolute(t *testing.T) {
	f := newScriptFixture(t, nil)
	submitReady(t, f.st, 2, store.Step{})
	t0 := time.Now().UTC()

	f.sweep(t, t0)
	cloudMember(t, f.st, f.g.ID, "c1", "i-1")
	cloudMember(t, f.st, f.g.ID, "c2", "i-2")
	submitReady(t, f.st, 4, store.Step{})
	f.sweep(t, t0.Add(time.Minute))

	grows := f.prov.callsOf("grow")
	if len(grows) != 2 {
		t.Fatalf("grow calls = %+v, want two", grows)
	}
	if grows[0].desired != 2 || grows[0].current != 0 {
		t.Errorf("first grow = %+v, want {2, 0}", grows[0])
	}
	if grows[1].desired != 6 || grows[1].current != 2 {
		t.Errorf("second grow = %+v, want {6, 2}", grows[1])
	}
	if got := groupState(t, f.st, f.g.ID); got.Requested != 6 {
		t.Errorf("Requested = %d, want 6", got.Requested)
	}
}

// Machines that do not register within the boot timeout degrade the group:
// the launch record falls back to the member rows, the degraded gauge is
// set, and no grow is made before the backoff ends.
func TestScriptActuator_DegradesAndBacksOff(t *testing.T) {
	f := newScriptFixture(t, nil)
	f.prov.set("list", actionResult{Result: resultOK})
	submitReady(t, f.st, 2, store.Step{})
	t0 := time.Now().UTC()

	f.sweep(t, t0)
	f.sweep(t, t0.Add(4*time.Minute))
	if got := groupState(t, f.st, f.g.ID); got.DegradedReason != "" {
		t.Fatalf("degraded before the boot timeout: %+v", got)
	}
	d := t0.Add(5*time.Minute + time.Second)
	f.sweep(t, d)

	got := groupState(t, f.st, f.g.ID)
	want := "2 of 2 machines did not register within boot_timeout (5m0s)"
	if got.Requested != 0 || got.DegradedReason != want || got.DegradedAt == nil || !got.DegradedAt.Equal(d) ||
		got.BackoffUntil == nil || !got.BackoffUntil.Equal(d.Add(10*time.Minute)) {
		t.Fatalf("state = %+v, want Requested 0, %q at %s, backoff until %s", got, want, d, d.Add(10*time.Minute))
	}
	st, ok := f.s.CapacityStatus(f.g.ID)
	if !ok || st.Degraded == nil || st.Degraded.Reason != want || st.Requested != 0 {
		t.Errorf("status = %+v, want degraded with the reason", st)
	}
	if v := metricValue(t, f.s.metrics.CapacityGroupDegraded.WithLabelValues("cloud")); v != 1 {
		t.Errorf("sqi_capacity_group_degraded = %v, want 1", v)
	}
	if !strings.Contains(f.logs.String(), "level=WARN") || !strings.Contains(f.logs.String(), "degraded") {
		t.Errorf("no degraded warning:\n%s", f.logs.String())
	}
	if n := len(f.prov.callsOf("list")); n != 1 {
		t.Errorf("list calls = %d, want 1", n)
	}

	f.sweep(t, d.Add(5*time.Minute))
	f.sweep(t, d.Add(10*time.Minute-time.Second))
	if n := len(f.prov.callsOf("grow")); n != 1 {
		t.Fatalf("grow calls inside the backoff = %d, want still 1", n)
	}
	f.sweep(t, d.Add(10*time.Minute))
	grows := f.prov.callsOf("grow")
	if len(grows) != 2 || grows[1].desired != 2 || grows[1].current != 0 {
		t.Fatalf("grow calls after the backoff = %+v, want a second grow{2, 0}", grows)
	}

	// Once the later grow is met, the degradation clears; the lapsed backoff
	// is left as it was.
	cloudMember(t, f.st, f.g.ID, "c1", "i-1")
	cloudMember(t, f.st, f.g.ID, "c2", "i-2")
	f.sweep(t, d.Add(11*time.Minute))
	if got := groupState(t, f.st, f.g.ID); got.DegradedReason != "" || got.DegradedAt != nil || got.RequestedAt != nil {
		t.Errorf("state after the machines registered = %+v, want the launch met and the degradation cleared", got)
	}
	if st, ok := f.s.CapacityStatus(f.g.ID); !ok || st.Degraded != nil {
		t.Errorf("status = %+v, want not degraded", st)
	}
	if v := metricValue(t, f.s.metrics.CapacityGroupDegraded.WithLabelValues("cloud")); v != 0 {
		t.Errorf("sqi_capacity_group_degraded = %v, want 0", v)
	}
}

// degradeWith grows f's group to three machines with c1 (reporting
// instanceID) online beside any member rows already there, and lets the boot
// timeout pass. It returns the time of the degrading sweep.
func degradeWith(t *testing.T, f *scriptFixture, instanceID string) time.Time {
	t.Helper()
	cloudMember(t, f.st, f.g.ID, "c1", instanceID)
	members, err := f.st.ListCapacityGroupMembers(t.Context(), f.g.ID)
	if err != nil {
		t.Fatalf("ListCapacityGroupMembers: %v", err)
	}
	rows := len(members)
	submitReady(t, f.st, 3, store.Step{})
	t0 := time.Now().UTC()
	f.sweep(t, t0)
	if grows := f.prov.callsOf("grow"); len(grows) != 1 || grows[0].desired != 3 || grows[0].current != rows {
		t.Fatalf("grow calls = %+v, want grow{3, %d}", grows, rows)
	}
	d := t0.Add(5*time.Minute + time.Second)
	f.sweep(t, d)
	if got := groupState(t, f.st, f.g.ID); got.Requested != rows || got.DegradedAt == nil {
		t.Fatalf("state = %+v, want degraded to Requested %d", got, rows)
	}
	return d
}

// After a degradation every listed instance no member reports is retired by
// instance ID alone; an ID that is empty, unprintable or overlong is never
// used.
func TestScriptActuator_ListRetiresOrphans(t *testing.T) {
	f := newScriptFixture(t, nil)
	f.prov.setList("i-1", "i-2", "i-3", "i-3", "", "bad\nid", strings.Repeat("x", 300))
	degradeWith(t, f, "i-1")

	retired := f.prov.callsOf("retire_instances")
	if len(retired) != 1 || !slices.Equal(retired[0].instanceIDs, []string{"i-2", "i-3"}) {
		t.Fatalf("instance retirements = %+v, want one of i-2 and i-3", retired)
	}
	if retired[0].workerIDs != nil {
		t.Errorf("orphans retired with worker IDs %v, want none", retired[0].workerIDs)
	}
	if n := len(f.prov.callsOf("retire")); n != 0 {
		t.Errorf("member retirements = %d, want 0", n)
	}
	if out := f.logs.String(); strings.Contains(out, "bad") || strings.Contains(out, strings.Repeat("x", 300)) {
		t.Errorf("an unusable instance ID reached the log:\n%s", out)
	}
	if got := actionsTotal(t, f.actFixture, "cloud", "retire", "ok"); got != 1 {
		t.Errorf("actions_total{retire,ok} = %v, want 1", got)
	}
}

// One online member registered without a cloud instance ID makes every listed
// instance look orphaned, its own among them: orphan cleanup is skipped for the
// whole group, with one warning naming the member.
func TestScriptActuator_OrphansSkippedWhenAMemberHasNoInstanceID(t *testing.T) {
	f := newScriptFixture(t, nil)
	f.prov.setList("i-1", "i-2")
	d := degradeWith(t, f, "")
	for i := 1; i <= 3; i++ {
		f.sweep(t, d.Add(time.Duration(i)*15*time.Second))
	}

	if retired := f.prov.callsOf("retire_instances"); len(retired) != 0 {
		t.Fatalf("instance retirements = %+v, want none", retired)
	}
	out := f.logs.String()
	if n := strings.Count(out, "orphan cleanup skipped"); n != 1 {
		t.Errorf("skip warning logged %d times, want 1:\n%s", n, out)
	}
	if !strings.Contains(out, "c1") {
		t.Errorf("the skip warning does not name c1:\n%s", out)
	}
	if got := f.worker(t, "c1"); got.PowerState != store.PowerAwake || got.Status != store.WorkerStatusOnline {
		t.Errorf("c1 = %s/%s, want awake and online", got.PowerState, got.Status)
	}
}

// A provider with no list action leaves the degraded reason naming the gap.
func TestScriptActuator_ListUnsupportedNamesTheGap(t *testing.T) {
	f := newScriptFixture(t, nil)
	f.prov.set("list", actionResult{Result: resultUnsupported, Message: "no list"})
	d := degradeWith(t, f, "i-1")

	want := "2 of 3 machines did not register within boot_timeout (5m0s); " +
		"instances that never registered may still be running; the provider has no list action"
	if got := groupState(t, f.st, f.g.ID); got.DegradedReason != want {
		t.Fatalf("DegradedReason = %q, want %q", got.DegradedReason, want)
	}
	f.sweep(t, d.Add(15*time.Second))
	if st, ok := f.s.CapacityStatus(f.g.ID); !ok || st.Degraded == nil || st.Degraded.Reason != want {
		t.Errorf("status = %+v, want the gap in the degraded reason", st)
	}
	if n := len(f.prov.callsOf("list")); n != 1 {
		t.Errorf("list calls = %d, want 1", n)
	}
	if la := lastAction(t, f.s, f.g.ID); la.Action != "list" || la.Result != "unsupported" {
		t.Errorf("LastAction = %+v, want list unsupported", la)
	}
}

// A retirement the script accepts finishes at once: the ephemeral member is
// deleted and its broker access revoked, and the launch record drops by one.
func TestScriptActuator_RetireOKDeletesEphemeralAndRevokes(t *testing.T) {
	f := newScriptFixture(t, nil)
	rev := &revokeRecorder{}
	f.s.cfg.RevokeWorker = rev.revoke
	cloudMember(t, f.st, f.g.ID, "c1", "i-1")
	t0 := time.Now().UTC()
	putGroupState(t, f.st, store.CapacityGroupState{GroupID: f.g.ID, Requested: 1, RequestedAt: new(t0.Add(-time.Hour))})

	f.sweep(t, t0)
	f.sweep(t, t0.Add(2*time.Minute))
	f.s.revokes.Wait()

	retires := f.prov.callsOf("retire")
	if len(retires) != 1 || !slices.Equal(retires[0].workerIDs, []string{"c1"}) {
		t.Fatalf("retire calls = %+v, want one for c1", retires)
	}
	if _, err := f.st.GetWorker(t.Context(), "c1"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetWorker(c1) = %v, want ErrNotFound", err)
	}
	if ids := rev.calls(); !slices.Equal(ids, []string{"c1"}) {
		t.Errorf("RevokeWorker calls = %v, want [c1]", ids)
	}
	if got := groupState(t, f.st, f.g.ID); got.Requested != 0 {
		t.Errorf("Requested = %d, want 0", got.Requested)
	}
	if n := len(f.prov.callsOf("grow")); n != 0 {
		t.Errorf("grow calls = %d, want 0", n)
	}
}

// A retirement the script does not carry out puts the member back to awake at
// once, with the script's message, and leaves the launch record alone.
func TestScriptActuator_RetireFailedAborts(t *testing.T) {
	f := newScriptFixture(t, nil)
	f.prov.set("retire", actionResult{Result: resultFailed, Message: "api said no"})
	cloudMember(t, f.st, f.g.ID, "c1", "i-1")
	t0 := time.Now().UTC()
	putGroupState(t, f.st, store.CapacityGroupState{GroupID: f.g.ID, Requested: 1, RequestedAt: new(t0.Add(-time.Hour))})

	f.sweep(t, t0)
	f.sweep(t, t0.Add(2*time.Minute))

	w := f.worker(t, "c1")
	if w.PowerState != store.PowerAwake || w.PowerReason != "api said no" {
		t.Fatalf("c1 = %s %q, want awake with the script's message", w.PowerState, w.PowerReason)
	}
	if got := groupState(t, f.st, f.g.ID); got.Requested != 1 {
		t.Errorf("Requested = %d, want 1", got.Requested)
	}
	if got := actionsTotal(t, f.actFixture, "cloud", "retire", "failed"); got != 1 {
		t.Errorf("actions_total{retire,failed} = %v, want 1", got)
	}
}

// A sized script group's member that reports no cloud instance ID is never
// retired: the script could not name its instance, so the request would fail
// and the member go back to awake on every idle timeout. It is warned about
// once.
func TestScriptActuator_MemberWithoutInstanceIDNeverRetired(t *testing.T) {
	f := newScriptFixture(t, nil)
	cloudMember(t, f.st, f.g.ID, "c1", "")
	cloudMember(t, f.st, f.g.ID, "c2", "i-2")
	t0 := time.Now().UTC()

	for i := range 6 {
		f.sweep(t, t0.Add(time.Duration(i)*time.Minute))
	}

	retires := f.prov.callsOf("retire")
	if len(retires) != 1 || !slices.Equal(retires[0].workerIDs, []string{"c2"}) {
		t.Fatalf("retire calls = %+v, want one, for c2 only", retires)
	}
	if w := f.worker(t, "c1"); w.PowerState != store.PowerAwake || w.Status != store.WorkerStatusOnline {
		t.Errorf("c1 = %s/%s, want awake and online", w.PowerState, w.Status)
	}
	out := f.logs.String()
	if n := strings.Count(out, "member reports no cloud instance ID"); n != 1 {
		t.Errorf("warning logged %d times, want 1:\n%s", n, out)
	}
	if !strings.Contains(out, "worker_id=c1") || !strings.Contains(out, "worker.cloud_instance_id.source") {
		t.Errorf("the warning does not name c1 and the configuration key:\n%s", out)
	}
}

// A retirement that begins and then aborts leaves a degradation in place: the
// member that came back is not a machine that registered late. One that
// really does still clears it.
func TestScriptActuator_AbortedRetirementKeepsDegradation(t *testing.T) {
	f := newScriptFixture(t, func(g *store.CapacityGroup) { g.MinMachines = 1 })
	f.prov.set("list", actionResult{Result: resultUnsupported})
	f.prov.set("retire", actionResult{Result: resultFailed, Message: "api said no"})
	cloudMember(t, f.st, f.g.ID, "c1", "i-1")
	cloudMember(t, f.st, f.g.ID, "c2", "i-2")
	t0 := time.Now().UTC()
	putGroupState(t, f.st, store.CapacityGroupState{GroupID: f.g.ID, Requested: 3, RequestedAt: new(t0.Add(-6 * time.Minute))})

	f.sweep(t, t0)
	if got := groupState(t, f.st, f.g.ID); got.DegradedReason == "" || got.Requested != 2 {
		t.Fatalf("state = %+v, want degraded at Requested 2", got)
	}

	// A sweep runs while the script works on the retirement, and sees the
	// member retiring.
	f.prov.onCall = func(c providerCall) {
		if c.action == "retire" {
			f.s.sweepCapacityAt(t.Context(), t0.Add(2*time.Minute+15*time.Second))
		}
	}
	f.sweep(t, t0.Add(2*time.Minute))
	if n := len(f.prov.callsOf("retire")); n != 1 {
		t.Fatalf("retire calls = %d, want 1", n)
	}
	f.prov.mu.Lock()
	f.prov.onCall = nil
	f.prov.mu.Unlock()
	f.sweep(t, t0.Add(2*time.Minute+30*time.Second))
	if got := groupState(t, f.st, f.g.ID); got.DegradedReason == "" {
		t.Fatalf("state = %+v, want still degraded: the aborted member is no late registrant", got)
	}

	cloudMember(t, f.st, f.g.ID, "c3", "i-3")
	f.sweep(t, t0.Add(2*time.Minute+45*time.Second))
	if got := groupState(t, f.st, f.g.ID); got.DegradedReason != "" || got.DegradedAt != nil {
		t.Errorf("state = %+v, want the degradation cleared by the late member", got)
	}
}

// A new leader that finds an unmet launch while retirements recorded before
// it are still under way repeats the launch at the member rows, which then
// outnumber the record: an absolute grow below them would shrink the group.
func TestScriptActuator_FailoverRepeatAtRowsAboveRequested(t *testing.T) {
	f := newScriptFixture(t, nil)
	f.prov.set("retire", actionResult{Result: resultFailed, Message: "api said no"})
	for i, id := range []string{"c1", "c2", "c3", "c4"} {
		cloudMember(t, f.st, f.g.ID, id, fmt.Sprintf("i-%d", i+1))
	}
	t0 := time.Now().UTC()
	for _, id := range []string{"c3", "c4"} {
		if _, ok, err := f.st.BeginRetirement(t.Context(), f.g.ID, id, t0.Add(-time.Second)); err != nil || !ok {
			t.Fatalf("BeginRetirement %s = %v, %v", id, ok, err)
		}
	}
	putGroupState(t, f.st, store.CapacityGroupState{GroupID: f.g.ID, Requested: 3, RequestedAt: new(t0.Add(-time.Minute))})

	f.sweep(t, t0)

	grows := f.prov.callsOf("grow")
	if len(grows) != 1 || grows[0].desired != 4 || grows[0].current != 4 {
		t.Fatalf("grow calls = %+v, want the recorded launch repeated once as grow{4, 4}", grows)
	}
}

// A new leader seeds the policy's pacing from the launch record, so it holds
// the machines already asked for rather than asking again from zero, and
// repeats the recorded request once.
func TestScriptActuator_SeedsPacingAfterFailover(t *testing.T) {
	f := newScriptFixture(t, func(g *store.CapacityGroup) { g.MaxStepUp = 2 })
	submitReady(t, f.st, 10, store.Step{})
	t0 := time.Now().UTC()
	putGroupState(t, f.st, store.CapacityGroupState{GroupID: f.g.ID, Requested: 4, RequestedAt: new(t0.Add(-time.Minute))})
	var seen store.CapacityGroupState
	f.prov.onCall = func(c providerCall) {
		if c.action == "grow" {
			seen = groupState(t, f.st, f.g.ID)
		}
	}

	f.sweep(t, t0)

	// The repeat is re-stamped before it is sent, so the machines it starts
	// get a whole boot timeout.
	if seen.Requested != 4 || seen.RequestedAt == nil || !seen.RequestedAt.Equal(t0) {
		t.Errorf("state seen by the repeated grow = %+v, want Requested 4 at %s", seen, t0)
	}
	if st, ok := f.s.CapacityStatus(f.g.ID); !ok || st.DesiredMachines != 4 || st.Requested != 4 {
		t.Fatalf("status = %+v, want DesiredMachines 4 held from the launch record", st)
	}
	grows := f.prov.callsOf("grow")
	if len(grows) != 1 || grows[0].desired != 4 || grows[0].current != 4 {
		t.Fatalf("grow calls = %+v, want the recorded grow{4, 4} repeated once", grows)
	}
	f.sweep(t, t0.Add(15*time.Second))
	if n := len(f.prov.callsOf("grow")); n != 1 {
		t.Errorf("grow calls after another sweep = %d, want 1", n)
	}
}

// A named group with a script provider (IPMI, Redfish) wakes its candidates
// through the script and puts a member to sleep when the script retires it.
func TestScriptActuator_NamedScriptWakesAndSleeps(t *testing.T) {
	f := newScriptFixture(t, nil)
	// Ahead of the fixture's sized group, so the demand below is this group's.
	g := createGroup(t, f.st, wolGroup("ipmi", func(g *store.CapacityGroup) { g.Provider, g.Priority = "script:cloud", 10 }))
	addNamedMember(t, f.st, g.ID, lanWorker("idle", "192.168.1.21", "aa:bb:cc:dd:ee:01"), true)
	addNamedMember(t, f.st, g.ID, lanWorker("off", "192.168.1.22", "aa:bb:cc:dd:ee:02"), false)
	t0 := time.Now().UTC()

	f.sweep(t, t0)
	f.sweep(t, t0.Add(2*time.Minute))

	retires := f.prov.callsOf("retire")
	if len(retires) != 1 || !slices.Equal(retires[0].workerIDs, []string{"idle"}) {
		t.Fatalf("retire calls = %+v, want one for idle", retires)
	}
	if w := f.worker(t, "idle"); w.PowerState != store.PowerAsleep || w.Status != store.WorkerStatusOffline {
		t.Fatalf("idle = %s/%s, want asleep and offline", w.PowerState, w.Status)
	}

	submitReady(t, f.st, 1, store.Step{})
	f.sweep(t, t0.Add(3*time.Minute))
	wakes := f.prov.callsOf("wake")
	if len(wakes) != 1 || len(wakes[0].workerIDs) != 1 {
		t.Fatalf("wake calls = %+v, want one for one member", wakes)
	}
	if w := f.worker(t, wakes[0].workerIDs[0]); w.PowerState != store.PowerWaking {
		t.Errorf("%s = %s, want waking", w.ID, w.PowerState)
	}
	if grows := f.prov.callsOf("grow"); len(grows) != 0 {
		t.Errorf("grow calls = %+v, want none for a named group", grows)
	}
}

// A sized group naming a script provider this server does not configure
// records why and asks nothing of anyone.
func TestScriptActuator_UnconfiguredProviderRecordsAndDoesNothing(t *testing.T) {
	f := newScriptFixture(t, func(g *store.CapacityGroup) { g.Provider = "script:missing" })
	submitReady(t, f.st, 3, store.Step{})
	t0 := time.Now().UTC()

	f.sweep(t, t0)
	f.sweep(t, t0.Add(time.Minute))

	f.prov.mu.Lock()
	calls := slices.Clone(f.prov.calls)
	f.prov.mu.Unlock()
	if len(calls) != 0 {
		t.Errorf("provider calls = %+v, want none", calls)
	}
	if got, err := f.st.ListCapacityGroupStates(t.Context()); err != nil || len(got) != 0 {
		t.Errorf("group states = %+v, %v, want none", got, err)
	}
	la := lastAction(t, f.s, f.g.ID)
	if la.Action != "configure" || la.Result != "failed" || la.Message != "provider script:missing is not configured on this server" {
		t.Errorf("LastAction = %+v, want configure failed naming the provider", la)
	}
}

// The real runner: a grow, then a degradation whose list names an instance no
// member reports, retired by instance ID with no worker ID on the wire.
func TestScriptActuator_EndToEndThroughRun(t *testing.T) {
	f := newActFixture(t, nil)
	dir := t.TempDir()
	f.s.cfg.ScriptProviders = map[string]script.Provider{"cloud": {
		Command: []string{os.Args[0]},
		Env:     map[string]string{scriptModeVar: "record", scriptDirVar: dir, "GORACE": "atexit_sleep_ms=0"},
		Timeout: time.Minute,
	}}
	g := createGroup(t, f.st, sizedGroup("cloud", func(g *store.CapacityGroup) { g.Provider = "script:cloud" }))
	submitReady(t, f.st, 2, store.Step{})
	t0 := time.Now().UTC()

	f.sweep(t, t0)
	f.sweep(t, t0.Add(5*time.Minute+time.Second))

	raw, err := os.ReadFile(filepath.Join(dir, "requests.jsonl"))
	if err != nil {
		t.Fatalf("read the recorded requests: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 3 {
		t.Fatalf("requests = %q, want grow, list and retire", lines)
	}
	var reqs []script.Request
	for _, l := range lines {
		var r script.Request
		if err := json.Unmarshal([]byte(l), &r); err != nil {
			t.Fatalf("decode %q: %v", l, err)
		}
		reqs = append(reqs, r)
	}
	wantGroup := script.GroupRef{ID: g.ID, Name: "cloud", Kind: "sized"}
	if r := reqs[0]; r.Version != script.Version || r.Action != "grow" || r.Desired != 2 || r.Current != 0 || r.Group != wantGroup {
		t.Errorf("first request = %+v, want grow{2, 0} for %+v", r, wantGroup)
	}
	if r := reqs[1]; r.Action != "list" || r.Group != wantGroup {
		t.Errorf("second request = %+v, want list", r)
	}
	if r := reqs[2]; r.Action != "retire" || len(r.Members) != 1 || r.Members[0].InstanceID != "i-orphan" {
		t.Errorf("third request = %+v, want a retire of i-orphan", r)
	}
	if strings.Contains(lines[2], "worker_id") {
		t.Errorf("the orphan retirement carries a worker_id: %s", lines[2])
	}
	if got := actionsTotal(t, f, "cloud", "grow", "ok"); got != 1 {
		t.Errorf("actions_total{grow,ok} = %v, want 1", got)
	}
}

// ── a met launch ──────────────────────────────────────────────────────────────

// handleDeregister handles a deregistration of workerID of kind, from any
// goroutine, and reports whether it was acknowledged.
func handleDeregister(s *Scheduler, workerID string, kind protocol.DeregisterKind) bool {
	data, err := json.Marshal(map[string]string{"worker_id": workerID, "reason": "test", "kind": string(kind)})
	if err != nil {
		return false
	}
	msg := &fakeJSMsg{subject: bus.WorkerDeregisterSubject(workerID), data: data}
	s.handleWorkerMessage(msg)
	return msg.acked
}

// A scale-down whose member leaves (its deregistration deleting the row)
// before the script answers, with a sweep in between, neither degrades the
// group nor lowers the launch record twice.
func TestScriptActuator_ScaleDownRowGoneEarlyNoDegradeExactRequested(t *testing.T) {
	f := newScriptFixture(t, func(g *store.CapacityGroup) { g.MinMachines = 1 })
	cloudMember(t, f.st, f.g.ID, "c1", "i-1")
	cloudMember(t, f.st, f.g.ID, "c2", "i-2")
	t0 := time.Now().UTC()
	putGroupState(t, f.st, store.CapacityGroupState{GroupID: f.g.ID, Requested: 2, RequestedAt: new(t0.Add(-time.Hour))})
	acked := true
	f.prov.onCall = func(c providerCall) {
		if c.action != "retire" {
			return
		}
		acked = handleDeregister(f.s, c.workerIDs[0], protocol.DeregisterRetired)
		f.s.sweepCapacityAt(t.Context(), t0.Add(2*time.Minute+15*time.Second))
	}

	f.sweep(t, t0)
	f.sweep(t, t0.Add(2*time.Minute))

	if !acked {
		t.Fatal("the deregistration was not acknowledged")
	}
	if retires := f.prov.callsOf("retire"); len(retires) != 1 {
		t.Fatalf("retire calls = %+v, want one", retires)
	}
	got := groupState(t, f.st, f.g.ID)
	if got.DegradedReason != "" || got.DegradedAt != nil {
		t.Errorf("state = %+v, want no degradation from a scale-down", got)
	}
	if got.Requested != 1 {
		t.Errorf("Requested = %d, want exactly 1", got.Requested)
	}
}

// After a met launch, a preempted member neither degrades the group nor sets
// off orphan cleanup (which would terminate the backend's replacement): the
// record follows the member rows and a normal grow replaces the machine.
func TestScriptActuator_PreemptionAfterMetLaunchRegrowsWithoutDegrading(t *testing.T) {
	f := newScriptFixture(t, nil)
	f.prov.setList("i-1", "i-2", "i-3", "i-4")
	for i, id := range []string{"c1", "c2", "c3"} {
		cloudMember(t, f.st, f.g.ID, id, fmt.Sprintf("i-%d", i+1))
	}
	submitReady(t, f.st, 3, store.Step{})
	t0 := time.Now().UTC()
	putGroupState(t, f.st, store.CapacityGroupState{GroupID: f.g.ID, Requested: 3, RequestedAt: new(t0.Add(-time.Hour))})

	f.sweep(t, t0)
	if n := len(f.prov.callsOf("grow")); n != 0 {
		t.Fatalf("grow calls with the launch met = %d, want 0", n)
	}
	if !handleDeregister(f.s, "c3", protocol.DeregisterPreempted) {
		t.Fatal("the deregistration was not acknowledged")
	}
	if _, err := f.st.GetWorker(t.Context(), "c3"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetWorker(c3) = %v, want it deleted", err)
	}
	// Long after the launch: the old code read this shortfall as a failed
	// launch.
	f.sweep(t, t0.Add(15*time.Second))
	// The backend's replacement registers within the regrow's boot timeout.
	cloudMember(t, f.st, f.g.ID, "c4", "i-4")
	f.sweep(t, t0.Add(10*time.Minute))

	got := groupState(t, f.st, f.g.ID)
	if got.DegradedReason != "" || got.DegradedAt != nil {
		t.Errorf("state = %+v, want no degradation from a preemption", got)
	}
	if calls := f.prov.callsOf("list"); len(calls) != 0 {
		t.Errorf("list calls = %d, want none", len(calls))
	}
	if calls := f.prov.callsOf("retire_instances"); len(calls) != 0 {
		t.Errorf("instance retirements = %+v, want none", calls)
	}
	grows := f.prov.callsOf("grow")
	if len(grows) != 1 || grows[0].desired != 3 || grows[0].current != 2 {
		t.Errorf("grow calls = %+v, want one grow{3, 2} replacing the machine", grows)
	}
}

// After a met launch, members that restart (deregistering to come back) or
// miss heartbeats keep their rows and their machines: the record must not
// fall below the rows, or an absolute grow would shrink the cloud group and
// terminate working instances.
func TestScriptActuator_RestartOrBlipAfterMetLaunchNeverShrinks(t *testing.T) {
	restart := func(t *testing.T, f *scriptFixture, id string) {
		t.Helper()
		if !handleDeregister(f.s, id, "") {
			t.Fatalf("the deregistration of %s was not acknowledged", id)
		}
	}
	blip := func(t *testing.T, f *scriptFixture, id string) {
		t.Helper()
		if _, _, err := f.st.OfflineWorker(t.Context(), id, "", time.Now().UTC()); err != nil {
			t.Fatalf("OfflineWorker %s: %v", id, err)
		}
	}
	tests := []struct {
		name    string
		mut     func(*store.CapacityGroup)
		demand  int
		offline []string
		lose    func(*testing.T, *scriptFixture, string)
	}{
		{"restart with demand", func(g *store.CapacityGroup) { g.MaxStepUp = 1 }, 4, []string{"c3", "c4"}, restart},
		{"heartbeat blip with demand", func(g *store.CapacityGroup) { g.MaxStepUp = 1 }, 4, []string{"c3", "c4"}, blip},
		{
			"whole group restarts, min_machines, no demand", func(g *store.CapacityGroup) { g.MinMachines = 2 }, 0,
			[]string{"c1", "c2", "c3", "c4"},
			restart,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newScriptFixture(t, tc.mut)
			for i, id := range []string{"c1", "c2", "c3", "c4"} {
				cloudMember(t, f.st, f.g.ID, id, fmt.Sprintf("i-%d", i+1))
			}
			if tc.demand > 0 {
				submitReady(t, f.st, tc.demand, store.Step{})
			}
			t0 := time.Now().UTC()
			putGroupState(t, f.st, store.CapacityGroupState{GroupID: f.g.ID, Requested: 4, RequestedAt: new(t0.Add(-time.Hour))})

			f.sweep(t, t0)
			for _, id := range tc.offline {
				tc.lose(t, f, id)
			}
			f.sweep(t, t0.Add(15*time.Second))
			f.sweep(t, t0.Add(30*time.Second))

			for _, g := range f.prov.callsOf("grow") {
				if g.desired < 4 || g.current < 4 {
					t.Errorf("grow %+v sent with 4 member rows: it would shrink the cloud group", g)
				}
			}
			if got := groupState(t, f.st, f.g.ID); got.Requested < 4 || got.DegradedReason != "" {
				t.Errorf("state = %+v, want Requested at least the 4 rows and no degradation", got)
			}
		})
	}
}

// The record counts every member row that is not retiring as present, and
// every row toward a grow's current.
func TestLaunchCounts(t *testing.T) {
	tests := []struct {
		name                       string
		online, offline, retiring  int
		wantPresent, wantRowsCount int
	}{
		{"all online", 4, 0, 0, 4, 4},
		{"some offline", 2, 2, 0, 4, 4},
		{"one retiring", 3, 0, 1, 2, 3},
		{"retiring offline", 1, 1, 1, 1, 2},
		{"none", 0, 0, 0, 0, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			present, rows := launchCounts(capacity.Status{OnlineMembers: tc.online, OfflineMembers: tc.offline, Retiring: tc.retiring})
			if present != tc.wantPresent || rows != tc.wantRowsCount {
				t.Errorf("launchCounts = %d, %d, want %d, %d", present, rows, tc.wantPresent, tc.wantRowsCount)
			}
		})
	}
}

// A retiring member does not meet an unmet launch: its retirement lowers the
// record before its row goes, so it must not also count as present.
func TestScriptActuator_RetiringMemberDoesNotMeetALaunch(t *testing.T) {
	f := newScriptFixture(t, nil)
	for i, id := range []string{"c1", "c2", "c3"} {
		cloudMember(t, f.st, f.g.ID, id, fmt.Sprintf("i-%d", i+1))
	}
	submitReady(t, f.st, 3, store.Step{})
	t0 := time.Now().UTC()
	putGroupState(t, f.st, store.CapacityGroupState{GroupID: f.g.ID, Requested: 3, RequestedAt: new(t0.Add(-time.Minute))})
	if _, ok, err := f.st.BeginRetirement(t.Context(), f.g.ID, "c3", t0); err != nil || !ok {
		t.Fatalf("BeginRetirement = %v, %v", ok, err)
	}

	f.sweep(t, t0)

	if got := groupState(t, f.st, f.g.ID); got.RequestedAt == nil {
		t.Errorf("state = %+v, want the launch still unmet with c3 retiring", got)
	}
}

// An offline member registered without a cloud instance ID would have its
// instance terminated as an orphan: cleanup is skipped, with one warning
// naming it.
func TestScriptActuator_OrphansSkippedWhenAnOfflineMemberHasNoInstanceID(t *testing.T) {
	f := newScriptFixture(t, nil)
	f.prov.setList("i-1", "i-2")
	addNamedMember(t, f.st, f.g.ID, store.Worker{ID: "c2", CPUCount: 4, OS: "linux", Arch: "amd64"}, false)
	d := degradeWith(t, f, "i-1")
	for i := 1; i <= 3; i++ {
		f.sweep(t, d.Add(time.Duration(i)*15*time.Second))
	}

	if retired := f.prov.callsOf("retire_instances"); len(retired) != 0 {
		t.Fatalf("instance retirements = %+v, want none", retired)
	}
	out := f.logs.String()
	if n := strings.Count(out, "orphan cleanup skipped"); n != 1 {
		t.Errorf("skip warning logged %d times, want 1:\n%s", n, out)
	}
	if !strings.Contains(out, "c2") {
		t.Errorf("the skip warning does not name c2:\n%s", out)
	}
}

// sqi never asked for more than max_machines, so a list naming more
// unreported instances than max_machines less the members is refused.
func TestScriptActuator_OrphansBeyondMaxMachinesRefused(t *testing.T) {
	f := newScriptFixture(t, func(g *store.CapacityGroup) { g.MaxMachines = 4 })
	f.prov.setList("i-1", "i-2", "i-3", "i-4", "i-5", "i-6")
	degradeWith(t, f, "i-1")

	if retired := f.prov.callsOf("retire_instances"); len(retired) != 0 {
		t.Fatalf("instance retirements = %+v, want none", retired)
	}
	if n := strings.Count(f.logs.String(), "orphan cleanup refused"); n != 1 {
		t.Errorf("refusal warning logged %d times, want 1:\n%s", n, f.logs.String())
	}
}

// Machines that register after the group degraded clear the degradation at
// once, but the backoff still runs out before the next grow.
func TestScriptActuator_LateRegistrationClearsDegradedKeepsBackoff(t *testing.T) {
	f := newScriptFixture(t, nil)
	f.prov.set("list", actionResult{Result: resultUnsupported})
	d := degradeWith(t, f, "i-1")
	backoff := groupState(t, f.st, f.g.ID).BackoffUntil
	if backoff == nil {
		t.Fatal("no backoff recorded")
	}

	f.sweep(t, d.Add(15*time.Second))
	if st, ok := f.s.CapacityStatus(f.g.ID); !ok || st.Degraded == nil {
		t.Fatalf("status = %+v, want still degraded with no late member", st)
	}
	cloudMember(t, f.st, f.g.ID, "c2", "i-2")
	f.sweep(t, d.Add(30*time.Second))

	got := groupState(t, f.st, f.g.ID)
	if got.DegradedReason != "" || got.DegradedAt != nil {
		t.Errorf("state = %+v, want the degradation cleared by the late member", got)
	}
	if got.BackoffUntil == nil || !got.BackoffUntil.Equal(*backoff) {
		t.Errorf("BackoffUntil = %v, want %s kept", got.BackoffUntil, backoff)
	}
	if st, ok := f.s.CapacityStatus(f.g.ID); !ok || st.Degraded != nil {
		t.Errorf("status = %+v, want not degraded", st)
	}
	if v := metricValue(t, f.s.metrics.CapacityGroupDegraded.WithLabelValues("cloud")); v != 0 {
		t.Errorf("sqi_capacity_group_degraded = %v, want 0", v)
	}
	f.sweep(t, backoff.Add(-time.Second))
	if n := len(f.prov.callsOf("grow")); n != 1 {
		t.Errorf("grow calls inside the backoff = %d, want still 1", n)
	}
	f.sweep(t, *backoff)
	if n := len(f.prov.callsOf("grow")); n != 2 {
		t.Errorf("grow calls once the backoff ran out = %d, want 2", n)
	}
}

// ── the provider ──────────────────────────────────────────────────────────────

// The script provider sends one request per action carrying every member's
// facts, applies the single answer to every member, maps each script result
// to an action result, and validates the instance IDs a list returns.
func TestScriptProvider_RequestsAndResults(t *testing.T) {
	var got []script.Request
	answer := script.Response{Result: "ok"}
	var runErr error
	p := &scriptProvider{
		p: script.Provider{Name: "cloud"},
		run: func(_ context.Context, _ script.Provider, req script.Request, _ *slog.Logger) (script.Response, error) {
			got = append(got, req)
			return answer, runErr
		},
	}
	g := store.CapacityGroup{ID: "g1", Name: "cloud", Kind: store.CapacityGroupNamed}
	m1 := lanWorker("m1", "192.168.1.21", "aa:bb:cc:dd:ee:01")
	m1.Hostname, m1.CloudInstanceID = "host-1", "i-1"
	m2 := store.Worker{ID: "m2"}
	ctx := t.Context()

	res := p.wake(ctx, g, []store.Worker{m1, m2})
	if len(got) != 1 || got[0].Action != "wake" || got[0].Version != script.Version ||
		got[0].Group != (script.GroupRef{ID: "g1", Name: "cloud", Kind: "named"}) || len(got[0].Members) != 2 {
		t.Fatalf("wake request = %+v", got)
	}
	want := script.Member{
		WorkerID: "m1", Hostname: "host-1", IPAddress: "192.168.1.21", InstanceID: "i-1",
		Interfaces: []script.NetInterface{{Name: "eth0", MAC: "aa:bb:cc:dd:ee:01", Addrs: []string{"192.168.1.21/24"}}},
	}
	if m := got[0].Members[0]; m.WorkerID != want.WorkerID || m.Hostname != want.Hostname || m.IPAddress != want.IPAddress ||
		m.InstanceID != want.InstanceID || len(m.Interfaces) != 1 || m.Interfaces[0].MAC != want.Interfaces[0].MAC ||
		!slices.Equal(m.Interfaces[0].Addrs, want.Interfaces[0].Addrs) {
		t.Errorf("member = %+v, want %+v", m, want)
	}
	if len(res) != 2 || res["m1"].Result != resultOK || res["m2"].Result != resultOK {
		t.Errorf("wake results = %+v, want ok for both", res)
	}

	tests := []struct {
		name   string
		answer script.Response
		err    error
		want   string
	}{
		{"ok", script.Response{Result: "ok", Message: "done"}, nil, resultOK},
		{"unsupported", script.Response{Result: "unsupported"}, nil, resultUnsupported},
		{"error", script.Response{Result: "error", Message: "boom"}, nil, resultFailed},
		{"timeout", script.Response{}, context.DeadlineExceeded, resultTimeout},
		{"cannot start", script.Response{}, errors.New("exec: not found"), resultFailed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			answer, runErr, got = tc.answer, tc.err, nil
			if r := p.grow(ctx, g, 5, 3); r.Result != tc.want || r.Message == "" {
				t.Errorf("grow = %+v, want %s with a message", r, tc.want)
			}
			if len(got) != 1 || got[0].Action != "grow" || got[0].Desired != 5 || got[0].Current != 3 {
				t.Errorf("grow request = %+v", got)
			}
		})
	}

	answer, runErr, got = script.Response{Result: "ok", InstanceIDs: []string{"i-1", "i-1", "", "a\x00b", "i-2", "a b", "x\u202ey"}}, nil, nil
	ids, r := p.list(ctx, g)
	if r.Result != resultOK || !slices.Equal(ids, []string{"i-1", "i-2"}) {
		t.Errorf("list = %v %+v, want [i-1 i-2] ok", ids, r)
	}
	if strings.Contains(r.Message, "a b") || strings.Contains(r.Message, "\x00") {
		t.Errorf("list message %q echoes an unusable ID", r.Message)
	}

	answer, got = script.Response{Result: "ok"}, nil
	if r := p.retireInstances(ctx, g, []string{"i-9"}); r.Result != resultOK {
		t.Errorf("retireInstances = %+v, want ok", r)
	}
	if len(got) != 1 || got[0].Action != "retire" || len(got[0].Members) != 1 ||
		!reflect.DeepEqual(got[0].Members[0], script.Member{InstanceID: "i-9"}) {
		t.Errorf("retireInstances request = %+v, want a retire of i-9 alone", got)
	}
}

// ── shutdown and series ───────────────────────────────────────────────────────

// The shutdown drain covers the longest call a provider can make.
func TestCapacityDrainTimeout_CoversLongestCall(t *testing.T) {
	tests := []struct {
		name      string
		providers map[string]script.Provider
		want      time.Duration
	}{
		{"wake_on_lan only", nil, min(relayRequestTimeout+2*controlRequestTimeout+capacityDrainMargin, maxCapacityDrain)},
		{
			"short script",
			map[string]script.Provider{"a": {Timeout: 5 * time.Second}},
			min(relayRequestTimeout+2*controlRequestTimeout+capacityDrainMargin, maxCapacityDrain),
		},
		{"default script timeout, capped", map[string]script.Provider{"a": {}}, maxCapacityDrain},
		{
			"longest of several, capped",
			map[string]script.Provider{"a": {Timeout: time.Minute}, "b": {Timeout: 10 * time.Minute}},
			maxCapacityDrain,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newMetricsScheduler(newCheckedFake(t), &recordBus{}, "")
			s.cfg.ScriptProviders = tc.providers
			if got := s.capacityDrainTimeout(); got != tc.want {
				t.Errorf("capacityDrainTimeout = %s, want %s", got, tc.want)
			}
			// Run's drains end inside the server's wait for it.
			if got, want := s.StopTimeout(), tc.want+revokeDrainTimeout; got != want || got >= StopCeiling {
				t.Errorf("StopTimeout = %s, want %s, below StopCeiling %s", got, want, StopCeiling)
			}
		})
	}
}

// A call still running when the shutdown drain gives up is logged once, and
// its result writes nothing: the store may be closing.
func TestActuator_ShutdownPastDrainWritesNothing(t *testing.T) {
	f := newActFixture(t, nil)
	g := createGroup(t, f.st, wolGroup("lan", nil))
	addNamedMember(t, f.st, g.ID, lanWorker("m1", "192.168.1.21", "aa:bb:cc:dd:ee:01"), true)
	release := make(chan struct{})
	asked := make(chan struct{}, 1)
	f.bus.setAnswer(func(string, protocol.ControlMsg) ([]byte, error) {
		asked <- struct{}{}
		<-release
		return controlReply(false, protocol.ControlRefusalBusy), nil
	})
	f.s.capState.act.drainTimeout = 20 * time.Millisecond
	t0 := time.Now().UTC()

	f.s.sweepCapacityAt(t.Context(), t0)
	f.s.sweepCapacityAt(t.Context(), t0.Add(2*time.Minute))
	select {
	case <-asked:
	case <-time.After(5 * time.Second):
		t.Fatal("no sleep was requested")
	}
	f.s.closeCapacityCalls(t.Context())
	close(release)
	f.s.capState.act.calls.Wait()

	if got := f.worker(t, "m1").PowerState; got != store.PowerRetiring {
		t.Errorf("m1 = %s, want retiring: a result after the drain must not write", got)
	}
	if n := strings.Count(f.logs.String(), "still running at shutdown"); n != 1 {
		t.Errorf("shutdown warning logged %d times, want 1:\n%s", n, f.logs.String())
	}
}

// A server that stops waiting for Run abandons the provider calls: a result
// arriving afterwards writes nothing, and a second abandon is harmless.
func TestActuator_AbandonedCallsWriteNothing(t *testing.T) {
	f := newActFixture(t, nil)
	g := createGroup(t, f.st, wolGroup("lan", nil))
	addNamedMember(t, f.st, g.ID, lanWorker("m1", "192.168.1.21", "aa:bb:cc:dd:ee:01"), true)
	release := make(chan struct{})
	asked := make(chan struct{}, 1)
	f.bus.setAnswer(func(string, protocol.ControlMsg) ([]byte, error) {
		asked <- struct{}{}
		<-release
		return controlReply(false, protocol.ControlRefusalBusy), nil
	})
	t0 := time.Now().UTC()

	f.s.sweepCapacityAt(t.Context(), t0)
	f.s.sweepCapacityAt(t.Context(), t0.Add(2*time.Minute))
	select {
	case <-asked:
	case <-time.After(5 * time.Second):
		t.Fatal("no sleep was requested")
	}
	f.s.AbandonCapacityCalls()
	f.s.AbandonCapacityCalls()
	close(release)
	f.s.capState.act.calls.Wait()

	if got := f.worker(t, "m1").PowerState; got != store.PowerRetiring {
		t.Errorf("m1 = %s, want retiring: a result after the abandon must not write", got)
	}
}

// Counters of a group that leaves the farm are deleted, even one never
// evaluated (a disabled group whose waking member expired).
func TestCapacitySweep_VanishedGroupCountersDeleted(t *testing.T) {
	f := newActFixture(t, nil)
	g := createGroup(t, f.st, wolGroup("lan", func(g *store.CapacityGroup) { g.Enabled = false }))
	addNamedMember(t, f.st, g.ID, lanWorker("m1", "192.168.1.21", "aa:bb:cc:dd:ee:01"), false)
	t0 := time.Now().UTC()
	if moved, err := f.st.MarkWaking(t.Context(), g.ID, []string{"m1"}, t0); err != nil || len(moved) != 1 {
		t.Fatalf("MarkWaking = %v, %v", moved, err)
	}
	f.sweep(t, t0.Add(g.BootTimeout+time.Second))
	if _, ok := capacitySeries(t, f.s.metrics)[`sqi_capacity_boot_timeouts_total{group=lan}`]; !ok {
		t.Fatalf("no boot-timeout series for the disabled group: %v", capacitySeries(t, f.s.metrics))
	}

	if _, err := f.st.SetWorkerCapacityGroup(t.Context(), "m1", "", false); err != nil {
		t.Fatalf("SetWorkerCapacityGroup: %v", err)
	}
	if err := f.st.DeleteCapacityGroup(t.Context(), g.ID); err != nil {
		t.Fatalf("DeleteCapacityGroup: %v", err)
	}
	f.sweep(t, t0.Add(g.BootTimeout+time.Minute))

	for key := range capacitySeries(t, f.s.metrics) {
		if strings.Contains(key, "group=lan") {
			t.Errorf("series %s outlived its group", key)
		}
	}
}

// A provider call that reports after its group was renamed or deleted makes no
// series under the name that is gone.
func TestActuator_LateResultForVanishedGroupMakesNoSeries(t *testing.T) {
	f := newActFixture(t, nil)
	g := createGroup(t, f.st, wolGroup("lan", nil))
	addNamedMember(t, f.st, g.ID, lanWorker("m1", "192.168.1.21", "aa:bb:cc:dd:ee:01"), true)
	release := make(chan struct{})
	asked := make(chan struct{}, 1)
	f.bus.setAnswer(func(string, protocol.ControlMsg) ([]byte, error) {
		asked <- struct{}{}
		<-release
		return controlReply(false, protocol.ControlRefusalBusy), nil
	})
	t0 := time.Now().UTC()

	f.s.sweepCapacityAt(t.Context(), t0)
	f.s.sweepCapacityAt(t.Context(), t0.Add(2*time.Minute))
	select {
	case <-asked:
	case <-time.After(5 * time.Second):
		t.Fatal("no sleep was requested")
	}
	g.Name = "lan2"
	if _, err := f.st.UpdateCapacityGroup(t.Context(), g); err != nil {
		t.Fatalf("UpdateCapacityGroup: %v", err)
	}
	f.s.sweepCapacityAt(t.Context(), t0.Add(2*time.Minute+15*time.Second))
	close(release)
	f.s.capState.act.calls.Wait()

	for key := range capacitySeries(t, f.s.metrics) {
		if strings.Contains(key, "group=lan,") || strings.HasSuffix(key, "group=lan}") {
			t.Errorf("series %s was made for the renamed group's old name", key)
		}
	}
}
