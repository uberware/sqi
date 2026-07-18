// SPDX-License-Identifier: AGPL-3.0-or-later

// Hub manages active WebSocket clients, subscription routing, ring-buffer
// replay, and per-client backpressure for the /api/v1/ws endpoint.
//
// # Lifecycle
//
//  1. When a WebSocket connection is established the read loop calls
//     [Hub.Register] to create a [hubClient] and obtain a send channel.
//     A per-connection send goroutine reads from that channel and writes
//     frames to the wire.
//
//  2. When the client sends a TypeSubscribe message the read loop calls
//     [Hub.Subscribe].  The hub adds the client to the subject's fan-out set
//     and, when SinceSeq > 0, replays buffered envelopes from the subject's
//     ring buffer directly into the client's send channel before returning.
//
//  3. When the scheduler processes a NATS message it calls one of the
//     Notify* methods ([Notifier] implementation):
//       a. Build a TypePush [Envelope] with the subject-specific payload.
//       b. Assign a monotonically increasing hub-level sequence number.
//       c. Store the envelope in the subject's ring buffer.
//       d. Fan out to every client subscribed to that subject; if the client's
//          send buffer is full the message is dropped (per-client backpressure).
//
//  4. [Hub.Deregister] removes the client from all subscriptions and closes its
//     send channel so the send goroutine exits cleanly.
//
// # Sequence numbers
//
// Hub assigns a monotonically increasing per-subject sequence number to every
// TypePush envelope.  This sequence is global (not per-connection) so clients
// can pass the Seq of the last received push as a SinceSeq cursor when
// reconnecting and receive any missed events from the ring buffer.
//
// # Subjects
//
// Four subscription subjects are supported in Phase 1:
//
//	"jobs"              — aggregate job-summary events
//	"jobs/{id}/tasks"   — per-job task-level updates
//	"workers"           — worker status events
//	"tasks/{id}/logs"   — live task log chunks

package ws

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// ringBufSize is the maximum number of push envelopes retained per subject
// for SinceSeq replay on reconnect.
const ringBufSize = 256

// clientSendBuf is the send channel buffer size per client.  If this many
// envelopes are queued and the client has not drained them, new pushes are
// dropped for that client.
const clientSendBuf = 64

// ── Push payload types ────────────────────────────────────────────────────────

// JobSummaryPush is the TypePush payload for [SubjectJobs] subscriptions.
//
// The same payload type carries two kinds of event on the jobs subject,
// distinguished by TaskID:
//   - Job-status changes (from NotifyJob) leave TaskID empty; Status is the job
//     status.
//   - Task-status changes (from NotifyTask) set TaskID; Status is the task
//     status. List subscribers use TaskID to apply task-count deltas to the
//     parent job row instead of treating it as a job-status change.
type JobSummaryPush struct {
	JobID     string    `json:"job_id"`
	TaskID    string    `json:"task_id,omitempty"`
	Name      string    `json:"name,omitempty"`
	Owner     string    `json:"owner,omitempty"`
	QueueID   string    `json:"queue_id,omitempty"`
	Status    string    `json:"status"`
	UpdatedAt time.Time `json:"updated_at"`
}

// TaskUpdatePush is the TypePush payload for "jobs/{id}/tasks" subscriptions.
type TaskUpdatePush struct {
	JobID               string    `json:"job_id"`
	TaskID              string    `json:"task_id"`
	Name                string    `json:"name,omitempty"`
	Status              string    `json:"status"`
	WorkerID            string    `json:"worker_id,omitempty"`
	UnschedulableReason string    `json:"unschedulable_reason,omitempty"`
	UpdatedAt           time.Time `json:"updated_at"`
}

// WorkerStatusPush is the TypePush payload for [SubjectWorkers] subscriptions.
//
// Its field set and order must mirror [WorkerEvent] exactly: the hub builds the
// push via the direct struct conversion WorkerStatusPush(e) (see buildEnvelope
// call in NotifyWorker). Adding or reordering a field here without doing the
// same in WorkerEvent breaks that conversion at compile time.
type WorkerStatusPush struct {
	WorkerID string `json:"worker_id"`
	Name     string `json:"name,omitempty"`
	Hostname string `json:"hostname,omitempty"`
	FarmID   string `json:"farm_id,omitempty"`
	Status   string `json:"status"`
}

// TaskLogPush is the TypePush payload for "tasks/{id}/logs" subscriptions.
type TaskLogPush struct {
	TaskID    string    `json:"task_id"`
	AttemptID string    `json:"attempt_id,omitempty"`
	SeqNum    int64     `json:"seq_num"`
	Stream    string    `json:"stream"`
	Data      string    `json:"data"`
	At        time.Time `json:"at"`
}

// DiagnosticsPush is the TypePush payload for [SubjectDiagnostics] subscriptions.
type DiagnosticsPush struct {
	Component string            `json:"component"`
	Level     string            `json:"level"`
	Msg       string            `json:"msg"`
	Attrs     map[string]string `json:"attrs,omitempty"`
	At        time.Time         `json:"at"`
}

// ── hubClient ─────────────────────────────────────────────────────────────────

// hubClient holds per-connection state owned by the Hub.
type hubClient struct {
	id     string
	scope  Scope
	sendCh chan Envelope
}

// ── subjectRing ───────────────────────────────────────────────────────────────

// subjectRing is a bounded circular buffer of recent TypePush envelopes for a
// single subscription subject.  It assigns hub-level monotonic sequence numbers
// so clients can request replay of missed events on reconnect.
type subjectRing struct {
	mu      sync.Mutex
	buf     [ringBufSize]Envelope
	pos     int    // next write slot (0-based, wraps at ringBufSize)
	count   int    // number of valid entries (0..ringBufSize)
	nextSeq uint64 // hub-level seq to assign to the next entry; starts at 1
}

// add stores env in the ring, assigns the next hub seq (writing it into
// env.Seq), and returns the assigned seq.
func (r *subjectRing) add(env Envelope) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.nextSeq == 0 {
		r.nextSeq = 1 // seq 0 is reserved for ping/pong
	}
	seq := r.nextSeq
	r.nextSeq++
	env.Seq = seq

	r.buf[r.pos] = env
	r.pos = (r.pos + 1) % ringBufSize
	if r.count < ringBufSize {
		r.count++
	}
	return seq
}

// since returns all buffered envelopes with Seq strictly greater than
// sinceSeq, in chronological (oldest-first) order.  Returns nil when the ring
// is empty.  Callers pass 0 to replay all buffered messages (since all hub
// seqs start at 1, every buffered entry satisfies seq > 0).
func (r *subjectRing) since(sinceSeq uint64) []Envelope {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.count == 0 {
		return nil
	}

	// Start index of the oldest entry.
	start := (r.pos - r.count + ringBufSize) % ringBufSize
	var out []Envelope
	for i := range r.count {
		e := r.buf[(start+i)%ringBufSize]
		if e.Seq > sinceSeq {
			out = append(out, e)
		}
	}
	return out
}

// ── Hub ───────────────────────────────────────────────────────────────────────

// Hub is the WebSocket fan-out hub.  It implements [Notifier] and routes
// scheduler events to subscribed WebSocket clients.
//
// Create with [NewHub]; the zero value is not usable.
type Hub struct {
	logger *slog.Logger

	// mu guards clients, subs, and rings.  Notify* methods briefly acquire a
	// read lock to read subs, then send to channels without holding the lock
	// so slow clients cannot stall the hub.
	mu      sync.RWMutex
	clients map[string]*hubClient            // clientID → client
	subs    map[string]map[string]*hubClient // subject → clientID → client
	rings   map[string]*subjectRing          // subject → ring buffer

	// owners resolves jobID → owner for envelopes that carry a job id but not
	// its owner (task events). Never nil after NewHub; its lookup may be.
	owners *ownerCache
}

// NewHub creates a Hub ready for use. jobOwner resolves a job id to its owner
// username, used to scope task events; pass nil when ownership cannot be
// resolved, in which case scoped clients receive no task events (fail closed).
func NewHub(logger *slog.Logger, jobOwner func(jobID string) string) *Hub {
	return &Hub{
		logger:  logger,
		clients: make(map[string]*hubClient),
		subs:    make(map[string]map[string]*hubClient),
		rings:   make(map[string]*subjectRing),
		owners:  newOwnerCache(jobOwner),
	}
}

// Register creates a hubClient for clientID and returns its send channel.
// The caller must start a goroutine that drains the channel and writes frames
// to the WebSocket connection.  Call [Hub.Deregister] when the connection
// closes.
//
// Calling Register for an already-registered clientID returns the existing
// send channel without creating a duplicate (idempotent).
func (h *Hub) Register(clientID string, scope Scope) chan Envelope {
	h.mu.Lock()
	defer h.mu.Unlock()

	if existing, ok := h.clients[clientID]; ok {
		return existing.sendCh
	}
	c := &hubClient{
		id:     clientID,
		scope:  scope,
		sendCh: make(chan Envelope, clientSendBuf),
	}
	h.clients[clientID] = c
	return c.sendCh
}

// Deregister removes clientID from all subscriptions and closes its send
// channel so the send goroutine exits.  Safe to call multiple times.
func (h *Hub) Deregister(clientID string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	c, ok := h.clients[clientID]
	if !ok {
		return
	}
	delete(h.clients, clientID)

	for subject, members := range h.subs {
		delete(members, clientID)
		if len(members) == 0 {
			delete(h.subs, subject)
		}
	}

	close(c.sendCh)
}

// Subscribe registers clientID for push notifications on subject.
// If sinceSeq > 0, buffered ring entries with Seq > sinceSeq are replayed
// into the client's send channel immediately (best-effort; entries may be
// dropped if the buffer is full).
//
// Returns an error if subject is not a recognized subscription subject or if
// clientID is not currently registered.
func (h *Hub) Subscribe(clientID, subject string, sinceSeq uint64) error {
	if !isValidSubject(subject) {
		return fmt.Errorf("unrecognized subject %q", subject)
	}

	h.mu.Lock()
	c, ok := h.clients[clientID]
	if !ok {
		h.mu.Unlock()
		return fmt.Errorf("ws: hub: client %q not registered", clientID)
	}
	if h.subs[subject] == nil {
		h.subs[subject] = make(map[string]*hubClient)
	}
	h.subs[subject][clientID] = c
	ring := h.ensureRingLocked(subject)
	h.mu.Unlock()

	// Replay outside the hub lock; ring has its own mutex. The scope check
	// mirrors fanout's: replay writes directly to the client channel, so
	// without it every buffered event would bypass filtering.
	// sinceSeq==0 replays all buffered messages; sinceSeq>0 replays from that cursor.
	for _, env := range ring.since(sinceSeq) {
		if !c.scope.allows(env) {
			continue
		}
		select {
		case c.sendCh <- env:
		default:
			h.logger.WarnContext(
				context.Background(),
				"ws: hub: replay dropped (slow client)",
				slog.String("client_id", clientID),
				slog.String("subject", subject),
				slog.Uint64("seq", env.Seq),
			)
		}
	}

	h.logger.DebugContext(
		context.Background(),
		"ws: hub: subscribed",
		slog.String("client_id", clientID),
		slog.String("subject", subject),
		slog.Bool("replay", sinceSeq > 0),
	)
	return nil
}

// Unsubscribe removes clientID from subject's subscription set.
// No-op when clientID is not subscribed to subject.
func (h *Hub) Unsubscribe(clientID, subject string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	members, ok := h.subs[subject]
	if !ok {
		return
	}
	delete(members, clientID)
	if len(members) == 0 {
		delete(h.subs, subject)
	}
}

// ── Notifier implementation ───────────────────────────────────────────────────

// NotifyTask fans a task-status change to:
//   - SubjectJobs subscribers (job-summary view of the event)
//   - "jobs/{jobID}/tasks" subscribers (per-job task detail)
func (h *Hub) NotifyTask(e TaskEvent) {
	now := e.UpdatedAt
	if now.IsZero() {
		now = time.Now().UTC()
	}

	// Job-summary push (SubjectJobs) — ring it even when no clients are
	// currently subscribed, so a scoped client that subscribes moments later
	// can replay it via since_seq: 0 and have it filtered by Scope.allows,
	// rather than have it silently never buffered. That only matters when the
	// hub has an owner resolver at all (auth on): with auth off (h.owners.
	// lookup == nil) there is no such thing as a scoped client, so this must
	// fall back to the pre-B2 hasSubscribers guard — otherwise the highest-
	// frequency event in the system unconditionally marshals JSON and takes
	// the ring mutex on every task transition even with nobody connected.
	if h.owners.lookup != nil || h.hasSubscribers(SubjectJobs) {
		env, err := buildEnvelope(SubjectJobs, JobSummaryPush{
			JobID:     e.JobID,
			TaskID:    e.TaskID,
			Status:    e.Status,
			UpdatedAt: now,
		})
		if err == nil {
			env.ownerScoped = true
			env.owner = h.resolveOwner("", e.JobID)
			h.fanout(SubjectJobs, env)
		} else {
			h.logger.WarnContext(context.Background(), "ws: hub: NotifyTask jobs envelope", slog.Any("error", err))
		}
	}

	// Per-job task-update push — skip marshal when no subscribers.
	taskSubject := fmt.Sprintf(SubjectJobTasksFmt, e.JobID)
	if h.hasSubscribers(taskSubject) {
		if env, err := buildEnvelope(taskSubject, TaskUpdatePush{
			JobID:               e.JobID,
			TaskID:              e.TaskID,
			Name:                e.Name,
			Status:              e.Status,
			WorkerID:            e.WorkerID,
			UnschedulableReason: e.UnschedulableReason,
			UpdatedAt:           now,
		}); err == nil {
			h.fanout(taskSubject, env)
		} else {
			h.logger.WarnContext(context.Background(), "ws: hub: NotifyTask tasks envelope", slog.Any("error", err))
		}
	}
}

// NotifyWorker fans a worker-status change to all SubjectWorkers subscribers.
func (h *Hub) NotifyWorker(e WorkerEvent) {
	if !h.hasSubscribers(SubjectWorkers) {
		return
	}
	env, err := buildEnvelope(SubjectWorkers, WorkerStatusPush(e))
	if err != nil {
		h.logger.WarnContext(context.Background(), "ws: hub: NotifyWorker envelope", slog.Any("error", err))
		return
	}
	h.fanout(SubjectWorkers, env)
}

// NotifyLog fans a log chunk to clients subscribed to "tasks/{taskID}/logs".
// The envelope is always stored in the ring buffer so late subscribers can
// replay it via since_seq: 0 even when no clients are active at emit time.
func (h *Hub) NotifyLog(e LogEvent) {
	at := e.At
	if at.IsZero() {
		at = time.Now().UTC()
	}
	logSubject := fmt.Sprintf(SubjectTaskLogsFmt, e.TaskID)
	env, err := buildEnvelope(logSubject, TaskLogPush{
		TaskID:    e.TaskID,
		AttemptID: e.AttemptID,
		SeqNum:    e.SeqNum,
		Stream:    e.Stream,
		Data:      e.Data,
		At:        at,
	})
	if err != nil {
		h.logger.WarnContext(context.Background(), "ws: hub: NotifyLog envelope", slog.Any("error", err))
		return
	}
	h.fanout(logSubject, env)
}

// NotifyJob fans a job-status change to all SubjectJobs subscribers.
//
// When the hub has an owner resolver (auth on), the envelope is always
// stored in the ring buffer (like NotifyLog), even when no clients are
// currently subscribed: a scoped client that subscribes moments later must
// be able to replay it via since_seq: 0 and have it filtered by
// Scope.allows, rather than have it silently never buffered. With auth off
// (h.owners.lookup == nil) there are no scoped clients to replay for, so this
// falls back to the pre-B2 hasSubscribers guard to keep the auth-off hot path
// unchanged: zero marshal, zero ring-mutex acquisition, with nobody
// connected.
func (h *Hub) NotifyJob(e JobEvent) {
	if h.owners.lookup == nil && !h.hasSubscribers(SubjectJobs) {
		return
	}

	now := e.UpdatedAt
	if now.IsZero() {
		now = time.Now().UTC()
	}
	env, err := buildEnvelope(SubjectJobs, JobSummaryPush{
		JobID:     e.JobID,
		Name:      e.Name,
		Owner:     e.Owner,
		QueueID:   e.QueueID,
		Status:    e.Status,
		UpdatedAt: now,
	})
	if err != nil {
		h.logger.WarnContext(context.Background(), "ws: hub: NotifyJob envelope", slog.Any("error", err))
		return
	}
	env.ownerScoped = true
	env.owner = h.resolveOwner(e.Owner, e.JobID)
	h.fanout(SubjectJobs, env)
}

// NotifyDiag fans a diagnostic log record to all SubjectDiagnostics subscribers.
func (h *Hub) NotifyDiag(e DiagEvent) {
	if !h.hasSubscribers(SubjectDiagnostics) {
		return
	}
	at := e.At
	if at.IsZero() {
		at = time.Now().UTC()
	}
	env, err := buildEnvelope(SubjectDiagnostics, DiagnosticsPush{
		Component: e.Component,
		Level:     e.Level,
		Msg:       e.Msg,
		Attrs:     e.Attrs,
		At:        at,
	})
	if err != nil {
		h.logger.WarnContext(context.Background(), "ws: hub: NotifyDiag envelope", slog.Any("error", err))
		return
	}
	h.fanout(SubjectDiagnostics, env)
}

// ── internal helpers ──────────────────────────────────────────────────────────

// resolveOwner returns owner when it is already known (non-empty), otherwise
// falls back to the owner-cache lookup keyed by jobID.
//
// Shared by NotifyJob and NotifyTask so this fallback can't drift between two
// copies again: most production NotifyJob call sites never populate
// JobEvent.Owner (only the retry path does), and TaskEvent carries no owner
// field at all, so both always need the cache — NotifyJob just also gets a
// cheap shortcut when its caller happens to already have the owner in hand.
func (h *Hub) resolveOwner(owner, jobID string) string {
	if owner != "" {
		return owner
	}
	return h.owners.get(jobID)
}

// hasSubscribers reports whether subject currently has at least one subscribed
// client.  Used by Notify* methods to skip JSON marshaling when there is nobody
// to deliver to.
func (h *Hub) hasSubscribers(subject string) bool {
	h.mu.RLock()
	n := len(h.subs[subject])
	h.mu.RUnlock()
	return n > 0
}

// fanout assigns a hub-level seq to env, stores it in the subject's ring
// buffer, and delivers a copy to each subscribed client.  Clients whose send
// buffers are full receive a warning log entry; the message is dropped rather
// than blocking fanout.
func (h *Hub) fanout(subject string, env Envelope) {
	ring := h.getOrCreateRing(subject)
	// add assigns env.Seq and stores the envelope; returns the assigned seq.
	seq := ring.add(env)
	env.Seq = seq

	h.mu.RLock()
	members := h.subs[subject]
	// Copy pointers so we can release the read lock before sending.
	clients := make([]*hubClient, 0, len(members))
	for _, c := range members {
		clients = append(clients, c)
	}
	h.mu.RUnlock()

	for _, c := range clients {
		if !c.scope.allows(env) {
			continue
		}
		select {
		case c.sendCh <- env:
		default:
			h.logger.WarnContext(
				context.Background(),
				"ws: hub: push dropped (slow client)",
				slog.String("client_id", c.id),
				slog.String("subject", subject),
				slog.Uint64("seq", seq),
			)
		}
	}
}

// getOrCreateRing returns the ring buffer for subject, creating one if needed.
// Safe for concurrent callers.
func (h *Hub) getOrCreateRing(subject string) *subjectRing {
	h.mu.RLock()
	ring := h.rings[subject]
	h.mu.RUnlock()
	if ring != nil {
		return ring
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	return h.ensureRingLocked(subject)
}

// ensureRingLocked returns (creating if absent) the ring for subject.
// Caller must hold h.mu for writing.
func (h *Hub) ensureRingLocked(subject string) *subjectRing {
	if r, ok := h.rings[subject]; ok {
		return r
	}
	r := &subjectRing{}
	h.rings[subject] = r
	return r
}

// buildEnvelope constructs a TypePush Envelope for subject, marshaling payload
// to JSON.  Seq is set to zero here and overwritten by [subjectRing.add].
func buildEnvelope(subject string, payload any) (Envelope, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return Envelope{}, fmt.Errorf("marshal push payload for %q: %w", subject, err)
	}
	return Envelope{
		Type:    TypePush,
		Subject: subject,
		Payload: json.RawMessage(raw),
	}, nil
}

// isValidSubject reports whether subject is a recognized subscription subject.
// Resource IDs within subject patterns are validated for structural correctness
// only (non-empty); the store is not queried.
func isValidSubject(subject string) bool {
	switch subject {
	case SubjectJobs, SubjectWorkers, SubjectDiagnostics:
		return true
	}
	// "jobs/{id}/tasks" — e.g. "jobs/abc123/tasks"
	{
		const pre, suf = "jobs/", "/tasks"
		if len(subject) > len(pre)+len(suf) &&
			strings.HasPrefix(subject, pre) &&
			strings.HasSuffix(subject, suf) {
			return true
		}
	}
	// "tasks/{id}/logs" — e.g. "tasks/abc123/logs"
	{
		const pre, suf = "tasks/", "/logs"
		if len(subject) > len(pre)+len(suf) &&
			strings.HasPrefix(subject, pre) &&
			strings.HasSuffix(subject, suf) {
			return true
		}
	}
	return false
}
