// SPDX-License-Identifier: AGPL-3.0-or-later

package brokerauth_test

import (
	"strings"
	"testing"

	natsserver "github.com/nats-io/nats-server/v2/server"

	"github.com/uberware/sqi/internal/brokerauth"
)

// subjectAllowed reports whether subject matches an entry in sp.Allow. It
// supports the two NATS subject wildcards: "*" matches exactly one token, and
// a trailing ">" matches one or more trailing tokens. A subject with no
// matching entry is denied. This exists to check the permission data returned
// by WorkerPermissions, not to reimplement NATS subject matching in general.
func subjectAllowed(sp *natsserver.SubjectPermission, subject string) bool {
	if sp == nil {
		return false
	}
	for _, pattern := range sp.Allow {
		if subjectMatches(pattern, subject) {
			return true
		}
	}
	return false
}

func subjectMatches(pattern, subject string) bool {
	patternTokens := strings.Split(pattern, ".")
	subjectTokens := strings.Split(subject, ".")

	for i, pt := range patternTokens {
		if pt == ">" {
			// ">" must be the final token and matches one or more remaining
			// subject tokens.
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

func TestWorkerPermissions_ConfinesToOwnSubtree(t *testing.T) {
	const me = "worker-a"
	const other = "worker-b"
	p := brokerauth.WorkerPermissions(me)

	allowedPublish := []string{
		"task.status." + me + ".job-1",
		"task.logs." + me + ".task-1",
		"worker.register." + me,
		"worker.heartbeat." + me,
		"worker.deregister." + me,
		"worker.diag." + me,
		"work.lease." + me + ".queue-1",
	}
	for _, s := range allowedPublish {
		if !subjectAllowed(p.Publish, s) {
			t.Errorf("publish %q should be allowed", s)
		}
	}

	deniedPublish := []string{
		"task.status." + other + ".job-1",
		"task.logs." + other + ".task-1",
		"worker.deregister." + other,
		"work.lease." + other + ".queue-1",
		"task.cancel.task-1",
		"task.status.>",
	}
	for _, s := range deniedPublish {
		if subjectAllowed(p.Publish, s) {
			t.Errorf("publish %q must NOT be allowed", s)
		}
	}

	if subjectAllowed(p.Subscribe, "task.status.>") {
		t.Error("a worker must not be able to subscribe to task.status.>")
	}
	if !subjectAllowed(p.Subscribe, "task.cancel.task-1") {
		t.Error("a worker must be able to subscribe to task.cancel.<taskID>")
	}

	// Reply inboxes. A worker needs its own, because a work lease is
	// core-NATS request/reply — and must not have anyone else's, because the
	// lease reply carries the whole assignment batch.
	if !subjectAllowed(p.Subscribe, brokerauth.InboxPrefix(me)+".x.1") {
		t.Error("a worker must be able to subscribe to its own reply inbox")
	}
	if subjectAllowed(p.Subscribe, brokerauth.InboxPrefix(other)+".x.1") {
		t.Error("a worker must NOT be able to subscribe to another worker's reply inbox")
	}
	if subjectAllowed(p.Subscribe, "_INBOX.x.1") {
		t.Error("a worker must NOT be able to subscribe to the process-global _INBOX subtree")
	}
}

func TestValidWorkerIDToken(t *testing.T) {
	valid := []string{
		"0f1d2c3b-4a59-6879-8a9b-0c1d2e3f4a5b", // what LoadOrCreateWorkerID writes
		"worker-a",
		"render_01",
	}
	for _, s := range valid {
		if !brokerauth.ValidWorkerIDToken(s) {
			t.Errorf("ValidWorkerIDToken(%q) = false, want true", s)
		}
	}

	invalid := []string{
		"",             // no token at all
		"a.b",          // two tokens: widens every grant built from it
		"a b",          // whitespace is not legal in a subject
		"*",            // single-token wildcard
		">",            // multi-token wildcard
		"worker.>",     // both of the above
		"worker\tname", // other whitespace
	}
	for _, s := range invalid {
		if brokerauth.ValidWorkerIDToken(s) {
			t.Errorf("ValidWorkerIDToken(%q) = true, want false", s)
		}
	}
}
