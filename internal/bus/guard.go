// SPDX-License-Identifier: AGPL-3.0-or-later

package bus

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// GuardBucket is the JetStream KV bucket that holds the one-server-per-account
// guard in external broker mode.
const GuardBucket = "SQI_SERVER"

// guardKey is the key in [GuardBucket] whose holder is the active server.
const guardKey = "active"

// defaultGuardTTL is how long a holder's key survives without a refresh. It is
// the figure the refusal message quotes.
const defaultGuardTTL = 30 * time.Second

// guardOpTimeout bounds each KV request the guard makes, so a request lost to a
// partition cannot hold the refresh loop or a release.
const guardOpTimeout = 5 * time.Second

// guardSetupTimeout bounds creating the bucket when the caller's context has
// no deadline.
const guardSetupTimeout = 30 * time.Second

// guardTTL and guardRefresh are the bucket's per-key TTL and the refresh
// period. They are variables so tests can shorten them.
var (
	guardTTL     = defaultGuardTTL
	guardRefresh = 10 * time.Second
)

// jsErrWrongLastSequence is nats-server's code for a KV write whose expected
// revision does not match the key's (server/jetstream_errors_generated.go).
const jsErrWrongLastSequence jetstream.ErrorCode = 10071

// ErrGuardHeld is returned, wrapped with the holder, when another sqi-server
// already holds the broker account.
var ErrGuardHeld = errors.New("bus: another sqi-server holds this broker account")

// GuardHolder identifies the sqi-server that holds the guard. It is the JSON
// value stored under the guard key.
type GuardHolder struct {
	InstanceID string    `json:"instance_id"`
	Host       string    `json:"host"`
	PID        int       `json:"pid"`
	StartedAt  time.Time `json:"started_at"`
	// Purpose says what holds the account when it is not a running server:
	// [GuardPurposeRestore] for "sqi-server restore". Empty for a server.
	Purpose string `json:"purpose,omitempty"`
}

// GuardPurposeRestore is the [GuardHolder.Purpose] of "sqi-server restore",
// which holds the account while it purges sqi's state from it.
const GuardPurposeRestore = "restore"

// describe names the holder in messages.
func (h GuardHolder) describe() string {
	what := ""
	if h.Purpose == GuardPurposeRestore {
		what = "\"sqi-server restore\" on "
	}
	return fmt.Sprintf("%shost %s, pid %d, started %s (instance %s)",
		what, h.Host, h.PID, h.StartedAt.UTC().Format(time.RFC3339), h.InstanceID)
}

// NewInstanceID returns a random identifier for one server process.
func NewInstanceID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Guard is a held one-server-per-account lease. The holder refreshes it by
// compare-and-set until [Guard.Release]. The guard is deliberately a lease so
// that leader election can later turn "refuse to start" into "wait as standby".
type Guard struct {
	js     jetstream.JetStream
	kv     jetstream.KeyValue
	holder GuardHolder
	value  []byte
	logger *slog.Logger

	// pause is held by the refresh loop around each refresh; tests hold it to
	// stop refreshes without stopping the loop.
	pause sync.Mutex

	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once

	mu       sync.Mutex
	revision uint64
	lastOK   time.Time
	lost     error
}

// AcquireGuard creates the guard bucket if needed, with the stream replica
// count, and takes the guard for h. It returns an error wrapping
// [ErrGuardHeld] and naming the current holder when another server holds it.
// On success a goroutine refreshes the guard until [Guard.Release].
func AcquireGuard(ctx context.Context, js jetstream.JetStream, replicas int, h GuardHolder, logger *slog.Logger) (*Guard, error) {
	return acquireGuard(ctx, js, replicas, h, logger, true)
}

// acquireGuard is [AcquireGuard]. With reconcile false the guard bucket keeps
// the configuration it has, replica count included, once held: a holder that
// is not a server, such as a restore, leaves rescaling to the server.
func acquireGuard(ctx context.Context, js jetstream.JetStream, replicas int, h GuardHolder, logger *slog.Logger, reconcile bool) (*Guard, error) {
	value, err := json.Marshal(h)
	if err != nil {
		return nil, fmt.Errorf("bus: encode guard holder: %w", err)
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(guardSetupTimeout)
	}
	kv, err := guardBucket(ctx, js, replicas, deadline)
	if err != nil {
		return nil, err
	}
	rev, err := createGuardKey(ctx, kv, h.InstanceID, value, deadline)
	if err != nil {
		return nil, err
	}

	loopCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	g := &Guard{
		js: js, kv: kv, holder: h, value: value, logger: logger,
		cancel: cancel, done: make(chan struct{}),
		revision: rev, lastOK: time.Now(),
	}
	go g.refreshLoop(loopCtx)
	if !reconcile {
		return g, nil
	}

	// Only the holder may change the bucket: a refused newcomer configured
	// with other replicas must leave the holder's bucket as it is.
	if err := ensureGuardBucket(ctx, js, replicas, deadline); err != nil {
		if rerr := g.Release(context.WithoutCancel(ctx)); rerr != nil {
			logger.WarnContext(ctx, "bus: could not release the broker account guard", slog.Any("error", rerr))
		}
		return nil, err
	}
	return g, nil
}

func guardBucketConfig(replicas int) jetstream.KeyValueConfig {
	return jetstream.KeyValueConfig{
		Bucket:      GuardBucket,
		Description: "the sqi-server that owns this broker account",
		History:     1,
		TTL:         guardTTL,
		Replicas:    replicas,
	}
}

// guardBucket creates the guard bucket, or opens it as it is when it exists:
// it never changes an existing bucket's configuration, which belongs to the
// holder until the guard is taken.
func guardBucket(ctx context.Context, js jetstream.JetStream, replicas int, deadline time.Time) (jetstream.KeyValue, error) {
	var kv jetstream.KeyValue
	err := retryUntil(ctx, deadline, func() error {
		opCtx, cancel := context.WithTimeout(ctx, guardOpTimeout)
		defer cancel()
		var err error
		kv, err = js.CreateKeyValue(opCtx, guardBucketConfig(replicas))
		if errors.Is(err, jetstream.ErrBucketExists) {
			kv, err = js.KeyValue(opCtx, GuardBucket)
		}
		return err
	}, transientJetStreamError)
	if err != nil {
		return nil, fmt.Errorf("bus: guard bucket %s: %w", GuardBucket, err)
	}
	return kv, nil
}

// ensureGuardBucket brings the guard bucket's configuration, its replica
// count included, to the configured one. Call it only while holding the guard.
func ensureGuardBucket(ctx context.Context, js jetstream.JetStream, replicas int, deadline time.Time) error {
	err := retryUntil(ctx, deadline, func() error {
		opCtx, cancel := context.WithTimeout(ctx, guardOpTimeout)
		defer cancel()
		_, err := js.CreateOrUpdateKeyValue(opCtx, guardBucketConfig(replicas))
		return err
	}, transientJetStreamError)
	if err != nil {
		return fmt.Errorf("bus: guard bucket %s: %w", GuardBucket, err)
	}
	return nil
}

// createGuardKey takes the key, or reports who holds it. The key can expire
// between a refused Create and the read that names its holder, so that case
// retries.
func createGuardKey(ctx context.Context, kv jetstream.KeyValue, instanceID string, value []byte, deadline time.Time) (uint64, error) {
	var rev uint64
	var held error
	err := retryUntil(ctx, deadline, func() error {
		held = nil
		opCtx, cancel := context.WithTimeout(ctx, guardOpTimeout)
		defer cancel()
		var err error
		rev, err = kv.Create(opCtx, guardKey, value)
		if !errors.Is(err, jetstream.ErrKeyExists) {
			return err
		}
		entry, gerr := kv.Get(opCtx, guardKey)
		switch {
		case gerr == nil && valueOwnedBy(entry.Value(), instanceID):
			// An earlier Create that timed out had committed: the key is ours.
			rev = entry.Revision()
			return nil
		case gerr == nil:
			held = heldError(entry.Value())
			return nil
		case errors.Is(gerr, jetstream.ErrKeyNotFound):
			return jetstream.ErrKeyExists // expired meanwhile; try again
		default:
			return gerr
		}
	}, func(err error) bool {
		return errors.Is(err, jetstream.ErrKeyExists) || transientJetStreamError(err)
	})
	switch {
	case held != nil:
		return 0, held
	case err != nil:
		return 0, fmt.Errorf("bus: take guard: %w", err)
	}
	return rev, nil
}

// heldError describes the holder in value.
func heldError(value []byte) error {
	var h GuardHolder
	if err := json.Unmarshal(value, &h); err != nil {
		return fmt.Errorf("%w (its identity is unreadable); if it is not running it exited less than %s ago", ErrGuardHeld, defaultGuardTTL)
	}
	return fmt.Errorf("%w: %s; it is still running, or exited less than %s ago", ErrGuardHeld, h.describe(), defaultGuardTTL)
}

// Healthy returns nil while this server still holds the guard: the last
// refresh succeeded within the TTL, and no other server has taken the key.
func (g *Guard) Healthy() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.lost != nil {
		return g.lost
	}
	if since := time.Since(g.lastOK); since > guardTTL {
		return fmt.Errorf("bus: the broker account guard was last refreshed %s ago; another sqi-server may have taken the account", since.Round(time.Second))
	}
	return nil
}

// Release stops the refresh and deletes the key, unless it is no longer this
// server's, so a release never removes another holder's key. It is safe to
// call more than once.
func (g *Guard) Release(ctx context.Context) error {
	var err error
	g.once.Do(func() {
		g.stop()
		err = g.deleteOwnKey(ctx)
	})
	return err
}

// stop ends the refresh loop and waits for it.
func (g *Guard) stop() {
	g.cancel()
	<-g.done
}

func (g *Guard) deleteOwnKey(ctx context.Context) error {
	g.mu.Lock()
	rev, lost := g.revision, g.lost
	g.mu.Unlock()
	if lost != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, guardOpTimeout)
	defer cancel()
	err := g.kv.Delete(ctx, guardKey, jetstream.LastRevision(rev))
	if err == nil || !isWrongRevision(err) {
		return wrapRelease(err)
	}
	// An aborted refresh can have landed after the revision was recorded.
	// Delete the key if it is still this server's.
	entry, gerr := g.kv.Get(ctx, guardKey)
	if gerr != nil || !g.ownedBy(entry.Value()) {
		return nil
	}
	return wrapRelease(g.kv.Delete(ctx, guardKey, jetstream.LastRevision(entry.Revision())))
}

func wrapRelease(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("bus: release guard: %w", err)
}

func (g *Guard) ownedBy(value []byte) bool {
	return valueOwnedBy(value, g.holder.InstanceID)
}

func valueOwnedBy(value []byte, instanceID string) bool {
	var h GuardHolder
	return json.Unmarshal(value, &h) == nil && h.InstanceID == instanceID
}

func isWrongRevision(err error) bool {
	var apiErr *jetstream.APIError
	return errors.Is(err, jetstream.ErrKeyExists) || (errors.As(err, &apiErr) && apiErr.ErrorCode == jsErrWrongLastSequence)
}

// refreshLoop keeps the key alive until ctx ends or the guard is lost.
func (g *Guard) refreshLoop(ctx context.Context) {
	defer close(g.done)
	ticker := time.NewTicker(guardRefresh)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if g.refreshOnce(ctx) {
			return
		}
	}
}

// refreshOnce refreshes the key and reports whether the guard is lost for good.
func (g *Guard) refreshOnce(ctx context.Context) (lost bool) {
	g.pause.Lock()
	defer g.pause.Unlock()
	if ctx.Err() != nil {
		return false
	}
	opCtx, cancel := context.WithTimeout(ctx, guardOpTimeout)
	defer cancel()

	g.mu.Lock()
	rev := g.revision
	g.mu.Unlock()

	newRev, err := g.kv.Update(opCtx, guardKey, g.value, rev)
	switch {
	case err == nil:
		g.refreshed(newRev)
		return false
	case ctx.Err() != nil:
		return false
	case isWrongRevision(err):
		return g.reconcile(opCtx)
	default:
		g.logger.ErrorContext(
			ctx, "bus: could not refresh the broker account guard",
			slog.Any("error", err),
			slog.String("impact", "if the refresh keeps failing, another sqi-server may take this account; /readyz turns degraded"),
		)
		return false
	}
}

func (g *Guard) refreshed(rev uint64) {
	g.mu.Lock()
	g.revision, g.lastOK = rev, time.Now()
	g.mu.Unlock()
}

// reconcile handles a refused refresh: the key expired and nobody took it,
// which is retaken, or another server holds it, which is final.
func (g *Guard) reconcile(ctx context.Context) bool {
	entry, err := g.kv.Get(ctx, guardKey)
	switch {
	case err == nil && g.ownedBy(entry.Value()):
		g.refreshed(entry.Revision())
		return false
	case err == nil:
		g.markLost(ctx, entry.Value())
		return true
	case errors.Is(err, jetstream.ErrKeyNotFound):
		return g.retakeExpired(ctx)
	default:
		g.logger.ErrorContext(ctx, "bus: could not read the broker account guard", slog.Any("error", err))
		return false
	}
}

func (g *Guard) markLost(ctx context.Context, value []byte) {
	var h GuardHolder
	holder := "another sqi-server"
	if json.Unmarshal(value, &h) == nil {
		holder = h.describe()
	}
	if g.holder.Purpose == GuardPurposeRestore {
		lost := fmt.Errorf("%w: this restore no longer holds it; %s took it", ErrGuardHeld, holder)
		g.setLost(lost)
		g.logger.ErrorContext(
			ctx, "bus: the restore no longer holds the broker account; an sqi-server took it",
			slog.Any("error", lost),
			slog.String("impact", "the purge refuses, so the restore fails and the broker keeps its old messages"),
			slog.String("remediation", "stop that sqi-server, then run \"sqi-server restore --in-place\""),
		)
		return
	}
	lost := fmt.Errorf("%w: this sqi-server no longer holds it; %s took it", ErrGuardHeld, holder)
	g.setLost(lost)
	g.logger.ErrorContext(
		ctx, "bus: this sqi-server no longer holds the broker account; another sqi-server took it",
		slog.Any("error", lost),
		slog.String("impact", "two servers are now working on one account, each consuming a share of the other's messages"),
		slog.String("remediation", "stop one of the two servers; /readyz reports degraded until this one restarts"),
	)
}

func (g *Guard) setLost(err error) {
	g.mu.Lock()
	g.lost = err
	g.mu.Unlock()
}

// retakeExpired handles a key that is gone. If nothing has been written to the
// bucket since this server's last refresh, the key only expired, and it is
// retaken. If anything was, another server held the account in between, even
// if it has since left, so the guard is lost.
func (g *Guard) retakeExpired(ctx context.Context) bool {
	last, err := g.bucketLastSeq(ctx)
	if err != nil {
		g.logger.ErrorContext(ctx, "bus: could not read the broker account guard", slog.Any("error", err))
		return false
	}
	g.mu.Lock()
	rev := g.revision
	g.mu.Unlock()
	if last != rev {
		g.markLost(ctx, nil)
		return true
	}
	newRev, err := g.kv.Create(ctx, guardKey, g.value)
	if err != nil {
		g.logger.ErrorContext(ctx, "bus: could not retake the broker account guard", slog.Any("error", err))
		return false
	}
	g.refreshed(newRev)
	g.logger.WarnContext(ctx, "bus: the broker account guard had expired and was retaken",
		slog.String("impact", "no other sqi-server held this account in that window"))
	return false
}

// bucketLastSeq is the sequence of the last message ever stored in the
// bucket. It survives the expiry of the messages themselves.
func (g *Guard) bucketLastSeq(ctx context.Context) (uint64, error) {
	stream, err := g.js.Stream(ctx, "KV_"+GuardBucket)
	if err != nil {
		return 0, err
	}
	info, err := stream.Info(ctx)
	if err != nil {
		return 0, err
	}
	return info.State.LastSeq, nil
}
