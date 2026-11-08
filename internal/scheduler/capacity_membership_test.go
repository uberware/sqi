// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

// Tests for capacity-group membership at registration, deregistration kinds,
// and the capacity sweep's removal of vanished ephemeral members.

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
	"github.com/uberware/sqi/internal/store/storetest"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// revokeRecorder records the workers the scheduler's RevokeWorker hook was
// called for.
type revokeRecorder struct {
	mu  sync.Mutex
	ids []string
}

func (r *revokeRecorder) revoke(_ context.Context, workerID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ids = append(r.ids, workerID)
	return nil
}

func (r *revokeRecorder) calls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.ids...)
}

// membershipFixture is a capacity fixture with broker authentication set to
// auth and a recording RevokeWorker hook.
type membershipFixture struct {
	st   *fake.Store
	s    *Scheduler
	logs *bytes.Buffer
	rev  *revokeRecorder
}

func newMembershipFixture(t *testing.T, auth bool) membershipFixture {
	t.Helper()
	st, s, logs := newCapacityFixture(t, nil)
	rev := &revokeRecorder{}
	s.cfg.NATSAuthEnabled = auth
	s.cfg.RevokeWorker = rev.revoke
	return membershipFixture{st: st, s: s, logs: logs, rev: rev}
}

// settle waits for the scheduler's background revocations, after which the
// log buffer and the recorder may be read.
func (f membershipFixture) settle() {
	f.s.revokes.Wait()
}

// register drives a registration of id through the handler and requires it
// acknowledged.
func (f membershipFixture) register(t *testing.T, id, instance, group string) {
	t.Helper()
	msg := &fakeJSMsg{
		subject: bus.WorkerRegisterSubject(id),
		data: workerMsgJSON(t, protocol.RegisterMsg{
			Version: protocol.ProtocolVersion, Type: protocol.TypeRegister,
			WorkerID: id, InstanceID: instance, FarmID: capFarm, Hostname: "host-" + id,
			OS: "linux", Arch: "amd64", CPUCount: 4, CapacityGroup: group,
		}),
	}
	f.s.handleWorkerMessage(msg)
	if !msg.acked {
		t.Fatalf("registration of %s was not acknowledged", id)
	}
}

// deregister drives a deregistration of id through the handler and requires
// it acknowledged.
func (f membershipFixture) deregister(t *testing.T, id, instance string, kind protocol.DeregisterKind) {
	t.Helper()
	msg := &fakeJSMsg{
		subject: bus.WorkerDeregisterSubject(id),
		data: workerMsgJSON(t, protocol.DeregisterMsg{
			Version: protocol.ProtocolVersion, Type: protocol.TypeDeregister,
			WorkerID: id, InstanceID: instance, Kind: kind,
		}),
	}
	f.s.handleWorkerMessage(msg)
	if !msg.acked {
		t.Fatalf("deregistration of %s was not acknowledged", id)
	}
}

func (f membershipFixture) worker(t *testing.T, id string) store.Worker {
	t.Helper()
	w, err := f.st.GetWorker(t.Context(), id)
	if err != nil {
		t.Fatalf("GetWorker %s: %v", id, err)
	}
	return w
}

// credential seeds an active broker credential for workerID, enrolled into
// groupID ("" for an ordinary credential).
func (f membershipFixture) credential(t *testing.T, workerID, groupID string) {
	t.Helper()
	if _, err := f.st.CreateWorkerCredential(t.Context(), store.WorkerCredential{
		ID: uuid.NewString(), WorkerID: workerID, PublicKey: "pub-" + uuid.NewString(),
		EnrolledAt: time.Now().UTC(), CapacityGroupID: groupID,
	}); err != nil {
		t.Fatalf("CreateWorkerCredential: %v", err)
	}
}

func (f membershipFixture) credentialActive(t *testing.T, workerID string) bool {
	t.Helper()
	_, err := storetest.ActiveWorkerCredential(t.Context(), f.st, workerID)
	switch {
	case err == nil:
		return true
	case errors.Is(err, store.ErrNotFound):
		return false
	default:
		t.Fatalf("ActiveWorkerCredential %s: %v", workerID, err)
		return false
	}
}

// warnCount counts the WARN lines that contain every one of parts.
func warnCount(logs string, parts ...string) int {
	n := 0
	for line := range strings.Lines(logs) {
		if !strings.Contains(line, "level=WARN") {
			continue
		}
		all := true
		for _, p := range parts {
			all = all && strings.Contains(line, p)
		}
		if all {
			n++
		}
	}
	return n
}

// membershipSpy counts the store calls capacity-group membership could make
// at registration.
type membershipSpy struct {
	store.Store

	mu    sync.Mutex
	calls map[string]int
}

func (s *membershipSpy) count(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls[name]++
}

func (s *membershipSpy) GetWorker(ctx context.Context, id string) (store.Worker, error) {
	s.count("GetWorker")
	return s.Store.GetWorker(ctx, id)
}

func (s *membershipSpy) GetCapacityGroup(ctx context.Context, id string) (store.CapacityGroup, error) {
	s.count("GetCapacityGroup")
	return s.Store.GetCapacityGroup(ctx, id)
}

func (s *membershipSpy) GetCapacityGroupByName(ctx context.Context, name string) (store.CapacityGroup, error) {
	s.count("GetCapacityGroupByName")
	return s.Store.GetCapacityGroupByName(ctx, name)
}

func (s *membershipSpy) SetWorkerCapacityGroup(ctx context.Context, w, g string, eph bool) (store.Worker, error) {
	s.count("SetWorkerCapacityGroup")
	return s.Store.SetWorkerCapacityGroup(ctx, w, g, eph)
}

func (s *membershipSpy) TouchWorkerCredential(ctx context.Context, id string, at time.Time) (store.WorkerCredential, error) {
	s.count("TouchWorkerCredential")
	return s.Store.TouchWorkerCredential(ctx, id, at)
}

func (s *membershipSpy) ListActiveWorkerCredentials(ctx context.Context) ([]store.WorkerCredential, error) {
	s.count("ListActiveWorkerCredentials")
	return s.Store.ListActiveWorkerCredentials(ctx)
}

// A worker in no group costs no membership call with broker authentication
// off, and with it on only the last-seen touch every registration makes, whose
// result carries the credential's group.
func TestRegister_NoGroupMakesNoMembershipCalls(t *testing.T) {
	tests := []struct {
		name string
		auth bool
		want map[string]int
	}{
		{"auth off", false, map[string]int{}},
		{"auth on", true, map[string]int{"TouchWorkerCredential": 2}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var spy *membershipSpy
			st, s, _ := newCapacityFixture(t, func(st *fake.Store) store.Store {
				spy = &membershipSpy{Store: st, calls: map[string]int{}}
				return spy
			})
			createGroup(t, st, sizedGroup("cloud", nil))
			f := membershipFixture{st: st, s: s, logs: &bytes.Buffer{}, rev: &revokeRecorder{}}
			s.cfg.NATSAuthEnabled = tc.auth
			if tc.auth {
				f.credential(t, "w1", "")
			}

			f.register(t, "w1", "inst-1", "")
			f.register(t, "w1", "inst-2", "")

			spy.mu.Lock()
			got := maps.Clone(spy.calls)
			spy.mu.Unlock()
			if !maps.Equal(got, tc.want) {
				t.Errorf("membership store calls = %v, want %v", got, tc.want)
			}
		})
	}
}

// With broker authentication off, a worker naming a sized group of its farm
// joins it, as an ephemeral member, and stays in it across re-registration.
func TestRegister_ConfigGroupJoinsWhenAuthOff(t *testing.T) {
	f := newMembershipFixture(t, false)
	g := createGroup(t, f.st, sizedGroup("cloud", nil))

	f.register(t, "w1", "inst-1", "cloud")
	f.register(t, "w1", "inst-1", "cloud")

	w := f.worker(t, "w1")
	if w.CapacityGroupID != g.ID || !w.Ephemeral {
		t.Fatalf("w1 = group %q ephemeral %v, want %s ephemeral", w.CapacityGroupID, w.Ephemeral, g.ID)
	}
	if w.Status != store.WorkerStatusOnline {
		t.Errorf("w1 status = %q, want online", w.Status)
	}
	f.settle()
	if n := warnCount(f.logs.String()); n != 0 {
		t.Errorf("WARN lines = %d, want none:\n%s", n, f.logs.String())
	}
}

// With broker authentication on, the worker's own configuration cannot make
// it a member: that would let any authenticated worker turn itself ephemeral
// and deletable. The key is named in one warning per worker.
func TestRegister_ConfigGroupIgnoredWhenAuthOn(t *testing.T) {
	tests := []struct {
		name       string
		credential bool
	}{
		{"ordinary credential", true},
		{"no credential", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newMembershipFixture(t, true)
			createGroup(t, f.st, sizedGroup("cloud", nil))
			if tc.credential {
				f.credential(t, "w1", "")
			}

			f.register(t, "w1", "inst-1", "cloud")
			f.register(t, "w1", "inst-1", "cloud")

			if w := f.worker(t, "w1"); w.CapacityGroupID != "" || w.Ephemeral {
				t.Fatalf("w1 = group %q ephemeral %v, want no membership", w.CapacityGroupID, w.Ephemeral)
			}
			f.settle()
			if n := warnCount(f.logs.String(), "worker.capacity_group", "worker_id=w1"); n != 1 {
				t.Errorf("warnings naming worker.capacity_group = %d, want 1:\n%s", n, f.logs.String())
			}
		})
	}
}

// With broker authentication on, a worker whose active credential was
// enrolled with a group token joins that group, whatever its configuration
// names.
func TestRegister_CredentialGroupJoinsWhenAuthOn(t *testing.T) {
	tests := []struct {
		name     string
		named    string
		wantWarn int
	}{
		{"no configured group", "", 0},
		{"configured group ignored", "other", 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newMembershipFixture(t, true)
			g := createGroup(t, f.st, sizedGroup("cloud", nil))
			createGroup(t, f.st, sizedGroup("other", nil))
			f.credential(t, "w1", g.ID)

			f.register(t, "w1", "inst-1", tc.named)

			w := f.worker(t, "w1")
			if w.CapacityGroupID != g.ID || !w.Ephemeral {
				t.Fatalf("w1 = group %q ephemeral %v, want %s ephemeral", w.CapacityGroupID, w.Ephemeral, g.ID)
			}
			f.settle()
			if n := warnCount(f.logs.String(), "worker.capacity_group"); n != tc.wantWarn {
				t.Errorf("warnings naming worker.capacity_group = %d, want %d:\n%s", n, tc.wantWarn, f.logs.String())
			}
		})
	}
}

// A group token stays valid after its group is changed to named, but a worker
// enrolled with it must not join the named group as an ephemeral, and so
// deletable, member. It registers with no membership and one warning.
func TestRegister_CredentialGroupNowNamedRefused(t *testing.T) {
	f := newMembershipFixture(t, true)
	g := createGroup(t, f.st, sizedGroup("cloud", nil))
	f.credential(t, "w1", g.ID)
	g.Kind, g.Profile = store.CapacityGroupNamed, store.MachineProfile{}
	if _, err := f.st.UpdateCapacityGroup(t.Context(), g); err != nil {
		t.Fatalf("UpdateCapacityGroup: %v", err)
	}

	f.register(t, "w1", "inst-1", "")
	f.register(t, "w1", "inst-1", "")

	w := f.worker(t, "w1")
	if w.CapacityGroupID != "" || w.Ephemeral {
		t.Fatalf("w1 = group %q ephemeral %v, want no membership", w.CapacityGroupID, w.Ephemeral)
	}
	if w.Status != store.WorkerStatusOnline {
		t.Errorf("w1 status = %q, want online: a refused group must not refuse the worker", w.Status)
	}
	f.settle()
	if n := warnCount(f.logs.String(), "worker_id=w1", "not sized", "source=\"group token\""); n != 1 {
		t.Errorf("warnings refusing the named group = %d, want 1:\n%s", n, f.logs.String())
	}
}

// With broker authentication off, only a sized group of the worker's own farm
// may be joined by name. Anything else registers the worker with no
// membership and one warning per worker.
func TestRegister_ConfigNamedGroupRefused(t *testing.T) {
	tests := []struct {
		name  string
		group func(t *testing.T, st store.Store) string
	}{
		{"named group", func(t *testing.T, st store.Store) string {
			createGroup(t, st, sizedGroup("onprem", func(g *store.CapacityGroup) {
				g.Kind, g.Profile = store.CapacityGroupNamed, store.MachineProfile{}
			}))
			return "onprem"
		}},
		{"unknown group", func(*testing.T, store.Store) string { return "nowhere" }},
		{"sized group of another farm", func(t *testing.T, st store.Store) string {
			if _, err := st.CreateFarm(t.Context(), store.Farm{ID: "farm-2", Name: "farm two"}); err != nil {
				t.Fatalf("CreateFarm: %v", err)
			}
			createGroup(t, st, sizedGroup("elsewhere", func(g *store.CapacityGroup) { g.FarmID = "farm-2" }))
			return "elsewhere"
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newMembershipFixture(t, false)
			name := tc.group(t, f.st)

			f.register(t, "w1", "inst-1", name)
			f.register(t, "w1", "inst-1", name)

			w := f.worker(t, "w1")
			if w.CapacityGroupID != "" || w.Ephemeral {
				t.Fatalf("w1 = group %q ephemeral %v, want no membership", w.CapacityGroupID, w.Ephemeral)
			}
			if w.Status != store.WorkerStatusOnline {
				t.Errorf("w1 status = %q, want online: a refused group must not refuse the worker", w.Status)
			}
			f.settle()
			if n := warnCount(f.logs.String(), "worker_id=w1", name); n != 1 {
				t.Errorf("warnings naming %s = %d, want 1:\n%s", name, n, f.logs.String())
			}
		})
	}
}

// joinedEphemeral registers id as an ephemeral member of a new sized group
// "cloud" through an auth-on credential enrolled into it, and returns the
// group.
func joinedEphemeral(t *testing.T, f membershipFixture, id string) store.CapacityGroup {
	t.Helper()
	g := createGroup(t, f.st, sizedGroup("cloud", nil))
	f.credential(t, id, g.ID)
	f.register(t, id, "inst-1", "")
	if w := f.worker(t, id); w.CapacityGroupID != g.ID || !w.Ephemeral {
		t.Fatalf("%s = group %q ephemeral %v, want an ephemeral member", id, w.CapacityGroupID, w.Ephemeral)
	}
	return g
}

// A retired or preempted ephemeral member is gone for good: its row is deleted
// and its credential revoked, in the store and through RevokeWorker.
func TestDeregister_RetiredEphemeralDeletedAndRevoked(t *testing.T) {
	for _, kind := range []protocol.DeregisterKind{protocol.DeregisterRetired, protocol.DeregisterPreempted} {
		t.Run(string(kind), func(t *testing.T) {
			f := newMembershipFixture(t, true)
			joinedEphemeral(t, f, "w1")

			f.deregister(t, "w1", "inst-1", kind)
			f.settle()

			if _, err := f.st.GetWorker(t.Context(), "w1"); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("GetWorker after %s = %v, want ErrNotFound", kind, err)
			}
			if f.credentialActive(t, "w1") {
				t.Error("credential still active after the worker was removed")
			}
			if got := f.rev.calls(); len(got) != 1 || got[0] != "w1" {
				t.Errorf("RevokeWorker calls = %v, want [w1]", got)
			}
			if !strings.Contains(f.logs.String(), "kind="+string(kind)) {
				t.Errorf("removal log does not name the kind %s:\n%s", kind, f.logs.String())
			}
		})
	}
}

// A deregistration that does not say the worker is gone for good (a service
// restart or upgrade) keeps the row, the membership and the credential.
func TestDeregister_PlainEphemeralKept(t *testing.T) {
	for _, kind := range []protocol.DeregisterKind{"", "restarting"} {
		t.Run("kind="+string(kind), func(t *testing.T) {
			f := newMembershipFixture(t, true)
			g := joinedEphemeral(t, f, "w1")

			f.deregister(t, "w1", "inst-1", kind)
			f.settle()

			w := f.worker(t, "w1")
			if w.Status != store.WorkerStatusOffline {
				t.Errorf("w1 status = %q, want offline", w.Status)
			}
			if w.CapacityGroupID != g.ID || !w.Ephemeral {
				t.Errorf("w1 = group %q ephemeral %v, want its membership kept", w.CapacityGroupID, w.Ephemeral)
			}
			if !f.credentialActive(t, "w1") {
				t.Error("credential revoked on a plain deregistration")
			}
			if got := f.rev.calls(); len(got) != 0 {
				t.Errorf("RevokeWorker calls = %v, want none", got)
			}
			// The kind is logged only when the worker sent one.
			seen := 0
			for line := range strings.Lines(f.logs.String()) {
				if !strings.Contains(line, "worker deregistered") {
					continue
				}
				seen++
				if hasKind := strings.Contains(line, "kind="); hasKind != (kind != "") {
					t.Errorf("deregistered line names a kind = %v, want %v:\n%s", hasKind, kind != "", line)
				}
			}
			if seen != 1 {
				t.Errorf("worker deregistered lines = %d, want 1:\n%s", seen, f.logs.String())
			}

			// It comes back with the same identity and is still a member.
			f.register(t, "w1", "inst-2", "")
			if w := f.worker(t, "w1"); w.Status != store.WorkerStatusOnline || w.CapacityGroupID != g.ID || !w.Ephemeral {
				t.Errorf("after restart w1 = %q group %q ephemeral %v, want an online ephemeral member",
					w.Status, w.CapacityGroupID, w.Ephemeral)
			}
		})
	}
}

// Only an ephemeral member is ever deleted: a named member and a worker in no
// group are kept, with their credentials, even when they say they retired.
func TestDeregister_NonEphemeralNeverDeleted(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, f membershipFixture)
	}{
		{"named member", func(t *testing.T, f membershipFixture) {
			g := createGroup(t, f.st, sizedGroup("onprem", func(g *store.CapacityGroup) {
				g.Kind, g.Profile = store.CapacityGroupNamed, store.MachineProfile{}
			}))
			if _, err := f.st.SetWorkerCapacityGroup(t.Context(), "w1", g.ID, false); err != nil {
				t.Fatalf("SetWorkerCapacityGroup: %v", err)
			}
		}},
		{"no group", func(*testing.T, membershipFixture) {}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newMembershipFixture(t, true)
			f.credential(t, "w1", "")
			f.register(t, "w1", "inst-1", "")
			tc.setup(t, f)

			f.deregister(t, "w1", "inst-1", protocol.DeregisterRetired)
			f.settle()

			if w := f.worker(t, "w1"); w.Status != store.WorkerStatusOffline {
				t.Errorf("w1 status = %q, want offline", w.Status)
			}
			if !f.credentialActive(t, "w1") {
				t.Error("credential revoked for a worker that is not ephemeral")
			}
			if got := f.rev.calls(); len(got) != 0 {
				t.Errorf("RevokeWorker calls = %v, want none", got)
			}
		})
	}
}

// vanishedMember makes id an ephemeral member of g whose last heartbeat was at
// lastSeen and which is now offline, as the heartbeat sweep leaves a machine
// that disappeared without deregistering.
func vanishedMember(t *testing.T, f membershipFixture, g store.CapacityGroup, id string, lastSeen time.Time) {
	t.Helper()
	f.credential(t, id, g.ID)
	if _, _, err := f.st.RegisterWorker(t.Context(), store.Worker{
		ID: id, InstanceID: "inst-1", FarmID: capFarm, CPUCount: 4, OS: "linux", Arch: "amd64",
		Status: store.WorkerStatusOnline, LastHeartbeatAt: &lastSeen,
	}); err != nil {
		t.Fatalf("RegisterWorker: %v", err)
	}
	if _, err := f.st.SetWorkerCapacityGroup(t.Context(), id, g.ID, true); err != nil {
		t.Fatalf("SetWorkerCapacityGroup: %v", err)
	}
	if _, _, err := f.st.OfflineStaleWorker(t.Context(), id, lastSeen.Add(time.Second), lastSeen.Add(time.Minute)); err != nil {
		t.Fatalf("OfflineStaleWorker: %v", err)
	}
	if w := f.worker(t, id); w.Status != store.WorkerStatusOffline {
		t.Fatalf("%s status = %q, want offline", id, w.Status)
	}
}

// An ephemeral member that vanished is removed once it has been offline for
// longer than its group's boot timeout, and not before. A disabled group's
// members are cleaned up too.
func TestCapacitySweep_VanishedEphemeralRemovedAfterBootTimeout(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		name := "enabled group"
		if !enabled {
			name = "disabled group"
		}
		t.Run(name, func(t *testing.T) {
			f := newMembershipFixture(t, false)
			g := createGroup(t, f.st, sizedGroup("cloud", func(g *store.CapacityGroup) { g.Enabled = enabled }))
			t0 := time.Now().UTC().Add(-time.Hour)
			vanishedMember(t, f, g, "gone", t0)

			f.s.sweepCapacityAt(t.Context(), t0.Add(g.BootTimeout-time.Second))
			f.settle()
			if _, err := f.st.GetWorker(t.Context(), "gone"); err != nil {
				t.Fatalf("member removed before its boot timeout: GetWorker = %v", err)
			}
			if got := f.rev.calls(); len(got) != 0 {
				t.Fatalf("RevokeWorker calls before the boot timeout = %v, want none", got)
			}

			f.s.sweepCapacityAt(t.Context(), t0.Add(g.BootTimeout+time.Second))
			f.s.sweepCapacityAt(t.Context(), t0.Add(g.BootTimeout+2*time.Second))
			f.settle()
			if _, err := f.st.GetWorker(t.Context(), "gone"); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("GetWorker after the boot timeout = %v, want ErrNotFound", err)
			}
			if f.credentialActive(t, "gone") {
				t.Error("credential still active after the member was removed")
			}
			if got := f.rev.calls(); len(got) != 1 || got[0] != "gone" {
				t.Errorf("RevokeWorker calls = %v, want [gone] exactly once", got)
			}
			if !strings.Contains(f.logs.String(), "worker_id=gone") {
				t.Errorf("removal not logged:\n%s", f.logs.String())
			}
		})
	}
}

// An ephemeral member that restarted (deregistered with no kind and came back
// within the boot timeout) is untouched by the sweep, however long after.
func TestCapacitySweep_RestartedEphemeralKept(t *testing.T) {
	f := newMembershipFixture(t, true)
	g := joinedEphemeral(t, f, "w1")

	f.deregister(t, "w1", "inst-1", "")
	f.register(t, "w1", "inst-2", "")
	f.s.sweepCapacityAt(t.Context(), time.Now().UTC().Add(g.BootTimeout+time.Minute))
	f.settle()

	w := f.worker(t, "w1")
	if w.Status != store.WorkerStatusOnline || w.CapacityGroupID != g.ID || !w.Ephemeral {
		t.Errorf("w1 = %q group %q ephemeral %v, want an online ephemeral member", w.Status, w.CapacityGroupID, w.Ephemeral)
	}
	if !f.credentialActive(t, "w1") {
		t.Error("credential revoked for a member that came back")
	}
	if got := f.rev.calls(); len(got) != 0 {
		t.Errorf("RevokeWorker calls = %v, want none", got)
	}
}

// An ephemeral member registering as a new process while the stored one is
// still online and heartbeating is probably a second machine cloned from an
// image that carried a worker ID: that is reported. A restart, which
// deregisters first, is not.
func TestRegister_ClonedEphemeralIDWarns(t *testing.T) {
	tests := []struct {
		name       string
		deregister bool
		want       int
	}{
		{"second process while the first is online", false, 1},
		{"deregistered, then re-registered", true, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newMembershipFixture(t, false)
			createGroup(t, f.st, sizedGroup("cloud", nil))
			f.register(t, "w1", "inst-1", "cloud")
			if tc.deregister {
				f.deregister(t, "w1", "inst-1", "")
			}

			f.register(t, "w1", "inst-2", "cloud")
			f.settle()

			if n := warnCount(f.logs.String(), "worker_id=w1", "worker ID"); n != tc.want {
				t.Errorf("cloned-ID warnings = %d, want %d:\n%s", n, tc.want, f.logs.String())
			}
		})
	}
}

// backgroundCalls waits for the calls running when it is closed, up to its
// bound, and refuses calls started after it was closed.
func TestBackgroundCalls_Close(t *testing.T) {
	tests := []struct {
		name     string
		block    time.Duration
		timeout  time.Duration
		wantDone bool
	}{
		{"finishes within the bound", 20 * time.Millisecond, 5 * time.Second, true},
		{"outlives the bound", time.Hour, 20 * time.Millisecond, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var b backgroundCalls
			release := make(chan struct{})
			defer close(release)
			var finished atomic.Bool
			if !b.Go(func() {
				select {
				case <-time.After(tc.block):
					finished.Store(true)
				case <-release:
				}
			}) {
				t.Fatal("Go before Close refused the call")
			}
			if got := b.Close(tc.timeout); got != tc.wantDone {
				t.Errorf("Close = %v, want %v", got, tc.wantDone)
			}
			if tc.wantDone && !finished.Load() {
				t.Error("Close returned before the running call finished")
			}
			var ran atomic.Bool
			if b.Go(func() { ran.Store(true) }) {
				t.Error("Go after Close accepted a call")
			}
			time.Sleep(10 * time.Millisecond)
			if ran.Load() {
				t.Error("a call started after Close ran")
			}
		})
	}
}
