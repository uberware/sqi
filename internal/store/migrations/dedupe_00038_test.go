// SPDX-License-Identifier: AGPL-3.0-or-later

package migrations_test

import (
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" driver for the PostgreSQL twin
	"github.com/pressly/goose/v3"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/migrations"
	"github.com/uberware/sqi/internal/store/postgres"
	"github.com/uberware/sqi/internal/store/storetest"
)

// dedupeLogChunks plants redelivered log chunks (one chunk stored several
// times under fresh row IDs) in a database rewound to 37, then runs up. The
// copy received first survives (the lowest id on a tie), a chunk stored once
// is untouched, and a further duplicate is refused with a unique violation.
// It is the body shared by the SQLite and PostgreSQL tests.
func dedupeLogChunks(t *testing.T, db *sql.DB, taskID, attemptID string, up func(), isUnique func(error) bool) {
	t.Helper()
	insert := func(id string, workerSeq, natsSeq int, receivedAt string) error {
		_, err := db.ExecContext(t.Context(), fmt.Sprintf(
			`INSERT INTO task_logs (id, task_id, attempt_id, worker_seq, nats_seq, at, received_at, stream, data)
			 VALUES ('%s', '%s', '%s', %d, %d, '2026-10-10T12:00:00.000000000Z', '%s', 'stdout', 'x')`,
			id, taskID, attemptID, workerSeq, natsSeq, receivedAt,
		))
		return err
	}
	plant := func(id string, workerSeq, natsSeq int, receivedAt string) {
		t.Helper()
		if err := insert(id, workerSeq, natsSeq, receivedAt); err != nil {
			t.Fatalf("plant %s: %v", id, err)
		}
	}
	// Inserted out of order so the survivor is chosen by received_at, not by
	// position: "mid" arrives first despite being written second.
	plant("late", 1, 12, "2026-10-10T12:00:03.000000000Z")
	plant("mid", 1, 10, "2026-10-10T12:00:01.000000000Z")
	plant("later", 1, 11, "2026-10-10T12:00:02.000000000Z")
	plant("other", 2, 13, "2026-10-10T12:00:04.000000000Z")
	// Two copies received at the same instant: the lowest id wins, whichever
	// was written first.
	plant("tie-b", 3, 14, "2026-10-10T12:00:05.000000000Z")
	plant("tie-a", 3, 15, "2026-10-10T12:00:05.000000000Z")

	up()

	rows := queryStrings(t, db, `SELECT id FROM task_logs WHERE attempt_id = '`+attemptID+`' ORDER BY id`)
	if want := []string{"mid", "other", "tie-a"}; !slices.Equal(rows, want) {
		t.Fatalf("surviving chunks = %v, want %v (the copy received first, the lowest id on a tie, and the untouched one)", rows, want)
	}
	if err := insert("again", 2, 99, "2026-10-10T12:00:06.000000000Z"); err == nil || !isUnique(err) {
		t.Fatalf("a duplicate (attempt_id, worker_seq) after 00038: err = %v, want a unique violation", err)
	}
}

func TestMigration00038_DedupesLogChunksAndEnforcesUniqueness(t *testing.T) {
	path, s, _ := openSeedable(t)
	task := submitJob(t, s, store.JobStatusRunning, store.StepStatusReady, store.TaskStatusRunning).Tasks[0]
	attempt := injectAttempt(t, s, task.ID, 1, store.AttemptStatusRunning)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	db := rewindTo(t, path, 37)
	dedupeLogChunks(t, db, task.ID, attempt.ID,
		func() {
			if err := goose.Up(db, "."); err != nil {
				t.Fatalf("goose.Up: %v", err)
			}
		},
		func(err error) bool { return strings.Contains(err.Error(), "UNIQUE constraint failed") })
}

// TestMigration00038_DeleteSearchesAnIndex pins that the migration's DELETE
// looks duplicates up through the plain (attempt_id, worker_seq) index it
// builds first; without it the correlated lookup is quadratic in an attempt's
// chunks. It plans the migration's own DELETE, taken from the file, and
// requires the lookup to SEARCH that index.
func TestMigration00038_DeleteSearchesAnIndex(t *testing.T) {
	path, s, _ := openSeedable(t)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	db := rewindTo(t, path, 37)
	body, err := fs.ReadFile(migrations.FS, "00038_task_logs_unique_seq.sql")
	if err != nil {
		t.Fatal(err)
	}
	up, _, _ := strings.Cut(string(body), "-- +goose Down")
	create := up[strings.Index(up, "CREATE INDEX task_logs_dedupe_00038"):]
	create = create[:strings.Index(create, ";")+1]
	del := up[strings.Index(up, "DELETE FROM"):]
	del = del[:strings.Index(del, "\n);")+3]
	if _, err := db.ExecContext(t.Context(), create); err != nil {
		t.Fatalf("create the dedupe index: %v", err)
	}
	rows, err := db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+del)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		lines = append(lines, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	plan := strings.Join(lines, "\n")
	t.Logf("plan:\n%s", plan)
	if !strings.Contains(plan, "SEARCH d USING") || !strings.Contains(plan, "task_logs_dedupe_00038") {
		t.Fatalf("the DELETE's lookup does not search the dedupe index; plan:\n%s", plan)
	}
}

// TestMigration00038_DedupesLogChunksOnPostgreSQL is the PostgreSQL twin: the
// same real duplicate rows, through the PostgreSQL migration file.
func TestMigration00038_DedupesLogChunksOnPostgreSQL(t *testing.T) {
	dsn := storetest.PostgresDSN(t)
	st, err := postgres.Open(t.Context(), dsn, postgres.DefaultOptions())
	if err != nil {
		t.Fatalf("postgres.Open: %v", err)
	}
	farm, err := st.CreateFarm(t.Context(), store.Farm{ID: uuid.NewString(), Name: uuid.NewString()})
	if err != nil {
		t.Fatalf("CreateFarm: %v", err)
	}
	queue, err := st.CreateQueue(t.Context(), store.Queue{ID: uuid.NewString(), FarmID: farm.ID, Name: uuid.NewString()})
	if err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}
	job := store.Job{
		ID: uuid.NewString(), FarmID: farm.ID, QueueID: queue.ID, Name: "j",
		Status: store.JobStatusRunning, TemplateFormat: store.TemplateFormatJSON,
	}
	step := store.Step{ID: uuid.NewString(), JobID: job.ID, Name: "s", Status: store.StepStatusReady}
	task := store.Task{ID: uuid.NewString(), JobID: job.ID, StepID: step.ID, Name: "t", Status: store.TaskStatusRunning}
	storetest.Submit(t, st, store.JobSubmission{Job: job, Steps: []store.Step{step}, Tasks: []store.Task{task}})
	now := time.Now().UTC()
	attempt := storetest.InjectAttempt(t, st, store.TaskAttempt{
		TaskID: task.ID, WorkerID: "w", AttemptNumber: 1, Status: store.AttemptStatusRunning, StartedAt: now,
	})
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	db := openPG(t, dsn)
	p, err := migrations.NewProvider(db, "postgres")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.DownTo(t.Context(), 37); err != nil {
		t.Fatalf("DownTo(37): %v", err)
	}
	dedupeLogChunks(t, db, task.ID, attempt.ID,
		func() {
			if _, err := p.Up(t.Context()); err != nil {
				t.Fatalf("Up: %v", err)
			}
		},
		func(err error) bool {
			var pgErr *pgconn.PgError
			return errors.As(err, &pgErr) && pgErr.Code == "23505"
		})
}

func openPG(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.PingContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	return db
}
