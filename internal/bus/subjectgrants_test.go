// SPDX-License-Identifier: AGPL-3.0-or-later

package bus

import (
	"strings"
	"testing"

	natsserver "github.com/nats-io/nats-server/v2/server"

	"github.com/uberware/sqi/internal/brokerauth"
)

// permAllows reports whether subject matches an entry in sp.Allow, honoring the
// two NATS subject wildcards: "*" matches exactly one token, and a trailing ">"
// matches one or more trailing tokens.
//
// This is a small local reimplementation rather than a shared helper: the point
// of the test below is to check one package's output against another package's
// data, so borrowing either package's own matcher would let a shared mistake
// agree with itself.
func permAllows(sp *natsserver.SubjectPermission, subject string) bool {
	if sp == nil {
		return false
	}
	for _, pattern := range sp.Allow {
		if permMatches(pattern, subject) {
			return true
		}
	}
	return false
}

func permMatches(pattern, subject string) bool {
	patternTokens := strings.Split(pattern, ".")
	subjectTokens := strings.Split(subject, ".")

	for i, pt := range patternTokens {
		if pt == ">" {
			// ">" is only valid as the final token and matches one or more
			// remaining subject tokens.
			return i == len(patternTokens)-1 && i < len(subjectTokens)
		}
		if i >= len(subjectTokens) {
			return false
		}
		if pt == "*" {
			continue
		}
		if pt != subjectTokens[i] {
			return false
		}
	}
	return len(patternTokens) == len(subjectTokens)
}

// TestSubjectHelpers_MatchWorkerPermissions binds this package's subject
// construction to the broker's per-worker publish grants.
//
// The two are written independently — [brokerauth.WorkerPermissions] builds its
// patterns by string concatenation and knows nothing about the helpers here —
// so nothing but this test stops a reordered or renamed token in subjects.go
// from silently drifting out of the grant it is supposed to fall under. Such a
// drift compiles, passes every other test, and surfaces only as a runtime
// authorization denial on a farm that has broker auth switched on, which is the
// exact failure the identity-carrying subject scheme exists to make impossible.
func TestSubjectHelpers_MatchWorkerPermissions(t *testing.T) {
	const me = "worker-a"
	const other = "worker-b"

	perms := brokerauth.WorkerPermissions(me)

	tests := []struct {
		name string
		// mine is the subject this worker publishes; theirs is the same
		// subject class published by a different worker.
		mine   string
		theirs string
	}{
		{"task status", TaskStatusSubject(me, "job-1"), TaskStatusSubject(other, "job-1")},
		{"task logs", TaskLogsSubject(me, "task-1"), TaskLogsSubject(other, "task-1")},
		{"worker register", WorkerRegisterSubject(me), WorkerRegisterSubject(other)},
		{"worker heartbeat", WorkerHeartbeatSubject(me), WorkerHeartbeatSubject(other)},
		{"worker deregister", WorkerDeregisterSubject(me), WorkerDeregisterSubject(other)},
		{"worker diag", WorkerDiagSubject(me), WorkerDiagSubject(other)},
		{"work lease", WorkLeaseSubject(me, "queue-1"), WorkLeaseSubject(other, "queue-1")},
		{"work lease wildcard queue", WorkLeaseSubject(me, WildcardQueueToken), WorkLeaseSubject(other, WildcardQueueToken)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !permAllows(perms.Publish, tt.mine) {
				t.Errorf(
					"subject %q is not granted by WorkerPermissions(%q) — the helper and the grant have drifted apart; allow list: %v",
					tt.mine, me, perms.Publish.Allow,
				)
			}
			// The negative half is what makes the positive half mean
			// something: a permission set of ">" would satisfy the check
			// above for every subject, and confining a worker to its own
			// traffic is the whole purpose of the scheme.
			if permAllows(perms.Publish, tt.theirs) {
				t.Errorf(
					"subject %q is granted by WorkerPermissions(%q) — a worker can publish as another worker",
					tt.theirs, me,
				)
			}
		})
	}
}
