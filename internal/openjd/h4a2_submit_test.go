// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import (
	"errors"
	"testing"

	"github.com/uberware/sqi/internal/store"
)

// TestUnsatisfiedDependency_MatchesThePreRead pins the F13 race path's 422
// body to the pre-read's words for the same cause.
func TestUnsatisfiedDependency_MatchesThePreRead(t *testing.T) {
	cases := []struct {
		err  *store.DependencyUnsatisfiableError
		want string
	}{
		{
			&store.DependencyUnsatisfiableError{UpstreamID: "up-1", Status: store.JobStatusFailed},
			`openjd: submit: depends_on job "up-1" already terminated unsuccessfully (failed)`,
		},
		{
			&store.DependencyUnsatisfiableError{UpstreamID: "up-2", Status: store.JobStatusCanceled},
			`openjd: submit: depends_on job "up-2" already terminated unsuccessfully (canceled)`,
		},
		{
			&store.DependencyUnsatisfiableError{UpstreamID: "up-3"},
			`openjd: submit: depends_on job "up-3" not found`,
		},
	}
	for _, c := range cases {
		got := unsatisfiedDependency(c.err)
		var sv *SubmitValidationError
		if !errors.As(got, &sv) {
			t.Fatalf("%v is not a SubmitValidationError", got)
		}
		if sv.Cause.Error() != c.want {
			t.Errorf("message = %q, want %q", sv.Cause.Error(), c.want)
		}
	}
	if !errors.Is(&store.DependencyUnsatisfiableError{UpstreamID: "x"}, store.ErrDependencyUnsatisfiable) {
		t.Fatal("DependencyUnsatisfiableError must match ErrDependencyUnsatisfiable")
	}
}
