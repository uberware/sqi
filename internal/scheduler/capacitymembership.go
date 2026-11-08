// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

// capacitymembership.go is how a worker becomes, and stops being, a member of a
// capacity group through its own messages.
//
// Joining happens at registration. With broker authentication on, the only
// source of membership is the worker's active credential: one enrolled with a
// group token carries the group, and the worker joins it as an ephemeral
// member. The worker's own worker.capacity_group setting is ignored there,
// because it would let any authenticated worker make itself ephemeral and so
// deletable. With authentication off there is no credential, and a worker
// joins by naming a sized group of its farm in that setting.
//
// Leaving for good is a deregistration whose kind is retired or preempted: an
// ephemeral member's row is deleted and its credential revoked. A
// deregistration with no kind is a restart and keeps both. A member that
// vanishes without deregistering is removed by the capacity sweep
// (capacity.go).
//
// A worker in no group registers as usual: with authentication off and no
// group named, none of this makes a store call; with it on, the credential
// comes back from the last-seen touch registration already made.

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/worker/protocol"
	"github.com/uberware/sqi/internal/ws"
)

// capacityGroupConfigKey is the worker configuration key that names the group
// a worker asks to join, as warnings name it.
const capacityGroupConfigKey = "worker.capacity_group"

// registrationContext is what [Scheduler.handleWorkerRegister] reads before
// it writes a registration, for capacity-group membership.
type registrationContext struct {
	// cred is the worker's active broker credential, after the last-seen
	// touch; hasCred is false with broker authentication off or no credential.
	cred    store.WorkerCredential
	hasCred bool
	// prior is the worker row as it was before this registration. It is read
	// only when the worker may be an ephemeral member, and nil otherwise or
	// when there is no row yet.
	prior *store.Worker
}

// beforeRegistration touches the worker's broker credential when broker
// authentication is on, and reads the worker's current row when the worker
// may be an ephemeral member: one that names a group with authentication off,
// or whose credential was enrolled into a group with it on. Both are
// best-effort and never stop a registration; a fenced touch is recorded, and
// the registration write that follows is fenced too.
//
// The touch comes before the registration write so that the credential is
// known in time to decide whether the row is worth reading. A registration
// then found stale has still touched the credential: the broker delivered a
// message the credential authorized, so it was seen.
func (s *Scheduler) beforeRegistration(ctx context.Context, m protocol.RegisterMsg, now time.Time) registrationContext {
	var rc registrationContext
	mayBeEphemeral := m.CapacityGroup != ""
	if s.cfg.NATSAuthEnabled {
		rc.cred, rc.hasCred = s.touchWorkerCredential(ctx, m.WorkerID, now)
		mayBeEphemeral = rc.hasCred && rc.cred.CapacityGroupID != ""
	}
	if !mayBeEphemeral {
		return rc
	}
	prior, err := s.store.GetWorker(ctx, m.WorkerID)
	switch {
	case err == nil:
		rc.prior = &prior
	case errors.Is(err, store.ErrNotFound):
	default:
		s.warnUnlessStopped(ctx, err, "scheduler: read worker before registration failed; cloned worker IDs go unreported",
			slog.String("worker_id", m.WorkerID))
	}
	return rc
}

// afterRegistration reports a cloned worker ID and makes a worker that is in
// no group a member of the one its credential or configuration names. stored
// is the row the registration wrote. A failure is logged and never fails the
// registration.
func (s *Scheduler) afterRegistration(
	ctx context.Context, m protocol.RegisterMsg, stored store.Worker, rc registrationContext, now time.Time,
) {
	s.warnOnClonedWorkerID(ctx, m, rc.prior, now)
	if s.cfg.NATSAuthEnabled && m.CapacityGroup != "" {
		s.warnCapacityOnce(ctx, m.WorkerID, "ignored", m.CapacityGroup,
			"scheduler: ignoring the worker's "+capacityGroupConfigKey+": with broker authentication on, "+
				"a worker joins a capacity group only by enrolling with the group's token",
			slog.String("key", capacityGroupConfigKey), slog.String("group", m.CapacityGroup))
	}
	if stored.CapacityGroupID != "" {
		return
	}
	switch {
	case s.cfg.NATSAuthEnabled:
		if rc.hasCred && rc.cred.CapacityGroupID != "" {
			g, err := s.store.GetCapacityGroup(ctx, rc.cred.CapacityGroupID)
			s.joinCapacityGroup(ctx, stored, g, err, rc.cred.CapacityGroupID, false)
		}
	case m.CapacityGroup != "":
		g, err := s.store.GetCapacityGroupByName(ctx, m.CapacityGroup)
		s.joinCapacityGroup(ctx, stored, g, err, m.CapacityGroup, true)
	}
}

// joinCapacityGroup makes w an ephemeral member of g, which the caller looked
// up by ref (an ID for a credential's group, a name for a configured one) with
// lookupErr as the result. byConfig is true for a group the worker named in its
// own configuration, and false for its credential's group. Either way the
// group must be sized and in the worker's farm: a group token stays valid
// after its group is changed to named, and a worker joining a named group as
// an ephemeral member would make itself deletable.
func (s *Scheduler) joinCapacityGroup(
	ctx context.Context, w store.Worker, g store.CapacityGroup, lookupErr error, ref string, byConfig bool,
) {
	source := "group token"
	if byConfig {
		source = capacityGroupConfigKey
	}
	refuse := func(reason string) {
		s.warnCapacityOnce(ctx, w.ID, "join", ref+": "+reason,
			"scheduler: worker not added to its capacity group: "+reason,
			slog.String("group", ref), slog.String("source", source))
	}
	switch {
	case errors.Is(lookupErr, store.ErrNotFound):
		refuse("no such group")
		return
	case lookupErr != nil:
		s.warnUnlessStopped(ctx, lookupErr, "scheduler: look up the worker's capacity group failed",
			slog.String("worker_id", w.ID), slog.String("group", ref))
		return
	case g.FarmID != w.FarmID:
		refuse("the group belongs to farm " + g.FarmID + ", the worker to farm " + w.FarmID)
		return
	case g.Kind != store.CapacityGroupSized:
		refuse("the group is not sized, and only a sized group takes ephemeral members; " +
			"add the worker to a named group through the API")
		return
	}

	_, err := s.store.SetWorkerCapacityGroup(ctx, w.ID, g.ID, true)
	switch {
	case err == nil:
		s.capWarned.Delete(capWarnKey{w.ID, "join"})
		s.logger.InfoContext(ctx, "scheduler: worker joined capacity group as an ephemeral member",
			slog.String("worker_id", w.ID), slog.String("group", g.Name), slog.String("source", source))
	case errors.Is(err, store.ErrConflict):
		// Another membership was written since the registration, or the group
		// was deleted while this write ran.
		refuse("the worker joined another group, or the group was removed, meanwhile")
	case errors.Is(err, store.ErrNotFound):
		refuse("the worker or the group was removed meanwhile")
	default:
		s.warnUnlessStopped(ctx, err, "scheduler: add worker to capacity group failed",
			slog.String("worker_id", w.ID), slog.String("group", g.Name))
	}
}

// capWarnKey identifies one capacity-membership warning about one worker.
type capWarnKey struct {
	workerID, topic string
}

// warnCapacityOnce logs msg at WARN for workerID unless the last warning on
// topic for that worker carried the same detail, so a worker re-registering on
// every reconnect is reported once per distinct problem.
func (s *Scheduler) warnCapacityOnce(ctx context.Context, workerID, topic, detail, msg string, attrs ...any) {
	key := capWarnKey{workerID, topic}
	if prev, ok := s.capWarned.Load(key); ok && prev == detail {
		return
	}
	s.capWarned.Store(key, detail)
	s.logger.WarnContext(ctx, msg, append([]any{slog.String("worker_id", workerID)}, attrs...)...)
}

// warnOnClonedWorkerID warns when an ephemeral member registers as a new
// process while the row, read before the registration, says its previous
// process is still online and heartbeated within WorkerTimeout. A restart
// deregisters first, so that is most likely a second machine started from an
// image that already carried a worker ID. The registration proceeds.
func (s *Scheduler) warnOnClonedWorkerID(ctx context.Context, m protocol.RegisterMsg, prior *store.Worker, now time.Time) {
	if prior == nil || !prior.Ephemeral || prior.Status != store.WorkerStatusOnline || prior.LastHeartbeatAt == nil {
		return
	}
	if prior.InstanceID == "" || m.InstanceID == "" || prior.InstanceID == m.InstanceID {
		return
	}
	if now.Sub(*prior.LastHeartbeatAt) >= s.cfg.WorkerTimeout {
		return
	}
	s.logger.WarnContext(ctx, "scheduler: an ephemeral worker registered as a new process while its previous "+
		"process is still online; two machines may share this worker ID",
		slog.String("worker_id", m.WorkerID),
		slog.String("instance_id", m.InstanceID),
		slog.String("previous_instance_id", prior.InstanceID),
		slog.String("remediation", "build machine images without a worker ID in the worker's data directory, "+
			"so each machine creates its own on first boot"))
}

// removeDepartedEphemeral deletes workerID when kind says it left for good and
// it is an ephemeral capacity-group member, revoking its credential, and
// reports whether it did. Any other kind, the empty one included, keeps the
// worker: it may come back. A worker that is not ephemeral is never deleted
// here, whatever the kind. fenced is true when the delete was fenced: the
// caller must stop and leave the message for the new leader.
func (s *Scheduler) removeDepartedEphemeral(
	ctx context.Context, workerID string, kind protocol.DeregisterKind,
) (removed, fenced bool) {
	if kind != protocol.DeregisterRetired && kind != protocol.DeregisterPreempted {
		return false, false
	}
	w, deleted, err := s.store.DeleteEphemeralWorker(ctx, workerID, time.Now().UTC())
	if s.stopIfFenced(ctx, err) {
		return false, true
	}
	if err != nil {
		s.warnUnlessStopped(ctx, err, "scheduler: remove departed ephemeral worker failed",
			slog.String("worker_id", workerID), slog.String("kind", string(kind)))
		return false, false
	}
	if !deleted {
		s.logger.DebugContext(ctx, "scheduler: departed worker is not an ephemeral capacity-group member; kept",
			slog.String("worker_id", workerID), slog.String("kind", string(kind)))
		return false, false
	}
	s.logger.InfoContext(ctx, "scheduler: ephemeral worker left for good; removed it and revoked its credential",
		slog.String("worker_id", workerID), slog.String("capacity_group_id", w.CapacityGroupID),
		slog.String("kind", string(kind)))
	s.announceRemovedWorker(w)
	s.revokeRemovedWorker(ctx, workerID)
	return true, false
}

// announceRemovedWorker fans out the removal of w to WebSocket clients.
func (s *Scheduler) announceRemovedWorker(w store.Worker) {
	s.notifier.NotifyWorker(ws.WorkerEvent{
		WorkerID: w.ID,
		Name:     w.Name,
		Hostname: w.Hostname,
		FarmID:   w.FarmID,
		Status:   ws.WorkerStatusRemoved,
	})
}

// revokeRemovedWorker calls [Config.RevokeWorker] for a worker
// [store.WorkerStore.DeleteEphemeralWorker] or
// [store.WorkerStore.DeleteVanishedEphemeralWorker] has just deleted, on a
// goroutine of its own: the store already revoked the credential, and dropping
// the broker connection takes seconds, which must not hold up the worker
// consumer or the sweep. The call keeps ctx's values (the leader epoch) but
// not its cancellation, so a scheduler stopping does not cut it short; the
// server bounds it, and [Scheduler.Run] waits for it, briefly, before it
// returns. Once Run has begun that wait, a removal drops no connection: the
// server is shutting down, and the store's revocation already applies.
func (s *Scheduler) revokeRemovedWorker(ctx context.Context, workerID string) {
	revoke := s.cfg.RevokeWorker
	if revoke == nil {
		return
	}
	ctx = context.WithoutCancel(ctx)
	s.revokes.Go(func() {
		err := revoke(ctx, workerID)
		if errors.Is(err, store.ErrNotFound) {
			return
		}
		s.warnUnlessStopped(ctx, err, "scheduler: revoke removed worker's broker access failed",
			slog.String("worker_id", workerID))
	})
}

// revokeDrainTimeout bounds how long [Scheduler.Run] waits, at shutdown, for
// [Config.RevokeWorker] calls still running. A broker revocation takes a few
// seconds; one that takes longer is left to finish, or not, on its own.
const revokeDrainTimeout = 5 * time.Second

// backgroundCalls runs calls on goroutines of their own until it is closed,
// and can then wait for those still running. Unlike a bare sync.WaitGroup, a
// call started while the owner is waiting is refused instead of racing the
// wait.
type backgroundCalls struct {
	mu     sync.Mutex
	closed bool
	wg     sync.WaitGroup
}

// Go runs f on a goroutine of its own and reports true, or reports false and
// runs nothing once Close has been called.
func (b *backgroundCalls) Go(f func()) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return false
	}
	b.wg.Go(f)
	return true
}

// Close refuses every later call and waits up to timeout for those still
// running. It reports whether they all finished in time.
func (b *backgroundCalls) Close(timeout time.Duration) bool {
	b.mu.Lock()
	b.closed = true
	b.mu.Unlock()
	done := make(chan struct{})
	go func() {
		b.wg.Wait()
		close(done)
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

// Wait waits for the calls running now, with no bound. Tests use it before
// reading what the calls did; it must not run alongside a call to Go.
func (b *backgroundCalls) Wait() {
	b.wg.Wait()
}
