// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd_test

import (
	"errors"
	"testing"

	"github.com/uberware/sqi/internal/openjd"
	"github.com/uberware/sqi/internal/store"
)

// TestSubmit_RacePath422MatchesPreRead drives both paths end to end for the same
// cause and compares what a client would read. The race path is an upstream lost
// between resolveDependencies' read and the submission's write; the pre-read is
// the same submission repeated once the upstream is already lost. Their 422
// bodies must be identical, and the race path must still wrap the store's typed
// error.
func TestSubmit_RacePath422MatchesPreRead(t *testing.T) {
	cases := []struct {
		name string
		lose func(t *testing.T, st *submitSpy, upstreamID string)
		want func(upstreamID string) string
	}{
		{
			name: "canceled",
			lose: func(t *testing.T, st *submitSpy, upstreamID string) {
				t.Helper()
				if err := st.CancelJobStatus(t.Context(), upstreamID); err != nil {
					t.Fatalf("CancelJobStatus: %v", err)
				}
			},
			want: func(id string) string {
				return `openjd: submit: depends_on job "` + id + `" already terminated unsuccessfully (canceled)`
			},
		},
		{
			name: "deleted",
			lose: func(t *testing.T, st *submitSpy, upstreamID string) {
				t.Helper()
				if err := st.DeleteJob(t.Context(), upstreamID); err != nil {
					t.Fatalf("DeleteJob: %v", err)
				}
			},
			want: func(id string) string {
				return `openjd: submit: depends_on job "` + id + `" not found`
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st, sub, farmID, queueID, upstreamID := newBlockedSubmitFixture(t)
			opts := openjd.SubmitOptions{FarmID: farmID, QueueID: queueID, DependsOn: []string{upstreamID}}

			// The race path: the upstream is lost after the pre-read passed.
			st.beforeSubmission = func() { c.lose(t, st, upstreamID) }
			_, raceErr := sub.Submit(t.Context(), minimalJSON("BlockedJob"), store.TemplateFormatJSON, opts)
			if _, ok := errors.AsType[*openjd.SubmitValidationError](raceErr); !ok {
				t.Fatalf("race-path Submit = %v, want a *SubmitValidationError", raceErr)
			}
			if !errors.Is(raceErr, store.ErrDependencyUnsatisfiable) {
				t.Errorf("race-path Submit = %v, want it to wrap ErrDependencyUnsatisfiable", raceErr)
			}
			if dep, ok := errors.AsType[*store.DependencyUnsatisfiableError](raceErr); !ok || dep.UpstreamID != upstreamID {
				t.Errorf("race-path Submit = %v, want it to wrap a DependencyUnsatisfiableError naming %q", raceErr, upstreamID)
			}

			// The pre-read: the same submission again, the upstream already lost.
			st.beforeSubmission = nil
			_, preErr := sub.Submit(t.Context(), minimalJSON("BlockedJob"), store.TemplateFormatJSON, opts)
			if _, ok := errors.AsType[*openjd.SubmitValidationError](preErr); !ok {
				t.Fatalf("pre-read Submit = %v, want a *SubmitValidationError", preErr)
			}

			want := c.want(upstreamID)
			if preErr.Error() != want {
				t.Errorf("pre-read message = %q, want %q", preErr.Error(), want)
			}
			if raceErr.Error() != want {
				t.Errorf("race-path message = %q, want the pre-read's %q", raceErr.Error(), want)
			}
		})
	}
}
