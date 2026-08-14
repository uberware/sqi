// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"context"
	"strconv"
	"testing"
	"time"
)

// TestCheckpoint_DoesNotBlockOnAReader is the reason [Store.Checkpoint] runs in
// PASSIVE mode rather than TRUNCATE.
//
// Before reads moved to their own pool there was only one connection, so a
// checkpoint never had a reader to wait for and TRUNCATE was free. Now a reader
// holding a WAL snapshot makes TRUNCATE invoke SQLite's busy handler, which
// waits out the ENTIRE busy_timeout (5s) on the write connection before giving
// up — stalling job submission, lease grants and every task-status write, on a
// five-minute timer, and still failing to truncate.
//
// That is the same multi-second write stall the separate read pool exists to
// remove, reintroduced through the back door. PASSIVE never invokes the busy
// handler: it transfers what it can and returns immediately.
//
// The assertion is deliberately far below busy_timeout. A regression to
// TRUNCATE does not make this slow — it makes it take the full 5s.
func TestCheckpoint_DoesNotBlockOnAReader(t *testing.T) {
	ctx := context.Background()
	st := openTestStoreWB(t)

	// Put frames in the WAL so the checkpoint has real work to do.
	seedCheckpointRows(t, ctx, st)

	// Hold a read snapshot open on the read pool for the whole checkpoint. This
	// is what a concurrent ListJobs looks like to SQLite.
	conn, err := st.rdb.Conn(ctx)
	if err != nil {
		t.Fatalf("read conn: %v", err)
	}
	defer conn.Close()

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin read tx: %v", err)
	}
	var n int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM farms").Scan(&n); err != nil {
		t.Fatalf("read: %v", err)
	}
	defer tx.Rollback() //nolint:errcheck // test cleanup

	start := time.Now()
	if _, _, err := st.Checkpoint(ctx); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	elapsed := time.Since(start)

	// busy_timeout is 5s; TRUNCATE against a held reader burns all of it.
	if elapsed > time.Second {
		t.Errorf("Checkpoint took %v with a reader holding a WAL snapshot; "+
			"PASSIVE must not invoke the busy handler (busy_timeout is 5s, so this "+
			"is almost certainly a regression to TRUNCATE)", elapsed)
	}
}

// TestCheckpointTruncate_IsStillAvailableForShutdown pins that the truncating
// form still exists and works, since shutdown genuinely does want a WAL
// truncated to zero and can afford to wait for it.
func TestCheckpointTruncate_IsStillAvailableForShutdown(t *testing.T) {
	ctx := context.Background()
	st := openTestStoreWB(t)
	seedCheckpointRows(t, ctx, st)

	wal, done, err := st.CheckpointTruncate(ctx)
	if err != nil {
		t.Fatalf("CheckpointTruncate: %v", err)
	}
	// With no reader holding a snapshot, a truncating checkpoint transfers
	// everything it found.
	if wal != done {
		t.Errorf("checkpointed %d of %d WAL pages with no reader holding a "+
			"snapshot; a quiescent TRUNCATE should transfer all of them", done, wal)
	}
}

// seedCheckpointRows writes enough rows to put frames in the WAL.
func seedCheckpointRows(t *testing.T, ctx context.Context, st *Store) {
	t.Helper()
	for i := range 50 {
		if _, err := st.db.ExecContext(
			ctx,
			`INSERT INTO farms (id, name, created_at, updated_at) VALUES (?, ?, ?, ?)`,
			"farm-"+strconv.Itoa(i), "f"+strconv.Itoa(i), timeToText(time.Now().UTC()), timeToText(time.Now().UTC()),
		); err != nil {
			t.Fatalf("seed farm %d: %v", i, err)
		}
	}
}
