// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres_test

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/sqlite"
	"github.com/uberware/sqi/internal/store/storetest"
)

// TestSubmissionWriteTimes measures one CreateJobSubmission at 50k, 75k and 1M
// tasks on SQLite and PostgreSQL and logs each duration. Opt-in
// (SQI_TEST_PERF=1): it takes minutes and asserts nothing.
func TestSubmissionWriteTimes(t *testing.T) {
	if os.Getenv("SQI_TEST_PERF") != "1" {
		t.Skip("set SQI_TEST_PERF=1 to measure")
	}
	for _, n := range []int{50_000, 75_000, 1_000_000} {
		lite, err := sqlite.Open(t.Context(), t.TempDir()+"/test.db", sqlite.DefaultOptions())
		if err != nil {
			t.Fatal(err)
		}
		for name, st := range map[string]store.Store{"sqlite": lite, "postgres": storetest.Postgres(t)} {
			farm, err := st.CreateFarm(t.Context(), store.Farm{ID: "f", Name: "f"})
			if err != nil {
				t.Fatal(err)
			}
			queue, err := st.CreateQueue(t.Context(), store.Queue{ID: "q", FarmID: farm.ID, Name: "q"})
			if err != nil {
				t.Fatal(err)
			}
			seed := storetest.NewJob(fmt.Sprintf("perf-%d", n), farm.ID, queue.ID).Step("s", store.StepStatusReady)
			for i := range n {
				seed.Task(fmt.Sprintf("t-%d", i), "s", store.TaskStatusReady)
			}
			start := time.Now()
			if _, err := st.CreateJobSubmission(t.Context(), seed.Sub); err != nil {
				t.Fatalf("%s %d: %v", name, n, err)
			}
			t.Logf("%-8s %9d tasks: %s", name, n, time.Since(start).Round(time.Millisecond))
		}
		_ = lite.Close()
	}
}
