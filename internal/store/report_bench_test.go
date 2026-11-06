// SPDX-License-Identifier: AGPL-3.0-or-later

package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/sqlite"
)

// benchRunningAttempts is how many running attempts the benchmark records
// progress against, round-robin.
const benchRunningAttempts = 500

// BenchmarkRecordAttemptProgress measures one progress snapshot write on
// SQLite with benchRunningAttempts attempts running, each iteration recording
// the next attempt's snapshot with a rising At.
func BenchmarkRecordAttemptProgress(b *testing.B) {
	ctx := context.Background()
	st, err := sqlite.Open(ctx, b.TempDir()+"/test.db", sqlite.DefaultOptions())
	if err != nil {
		b.Fatalf("sqlite.Open: %v", err)
	}
	b.Cleanup(func() { _ = st.Close() })

	running := make([]store.TaskStatus, benchRunningAttempts)
	for i := range running {
		running[i] = store.TaskStatusRunning
	}
	g := seedGraph(b, st, graphOpts{jobStatus: store.JobStatusRunning},
		stepSpec{name: "a", status: store.StepStatusReady, tasks: running})
	attempts := make([]store.TaskAttempt, 0, benchRunningAttempts)
	for _, task := range g.Tasks["a"] {
		attempts = append(attempts, g.Attempts[task.ID])
	}

	base := time.Now().UTC().Truncate(time.Microsecond)
	progress := 0
	b.ResetTimer()
	for i := range b.N {
		a := attempts[i%len(attempts)]
		progress = (progress + 1) % 100
		out, err := st.RecordAttemptProgress(ctx, store.AttemptProgress{
			AttemptID: a.ID, TaskID: a.TaskID, Progress: &progress, StatusText: "frame",
			At: base.Add(time.Duration(i+1) * time.Millisecond),
		})
		if err != nil || out != store.ProgressRecorded {
			b.Fatalf("RecordAttemptProgress = (%v, %v), want recorded", out, err)
		}
	}
}
