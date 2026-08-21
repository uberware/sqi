// SPDX-License-Identifier: AGPL-3.0-or-later

package bus

import (
	"strings"
	"testing"
)

func TestSubjectHelpers(t *testing.T) {
	tests := []struct {
		name string
		got  string
		want string
	}{
		{"work lease", WorkLeaseSubject("w-1", "q-123"), "work.lease.w-1.q-123"},
		{"work lease wildcard queue", WorkLeaseSubject("w-1", WildcardQueueToken), "work.lease.w-1._any"},
		{"task status", TaskStatusSubject("w-1", "job-abc"), "task.status.w-1.job-abc"},
		{"task logs", TaskLogsSubject("w-1", "task-xyz"), "task.logs.w-1.task-xyz"},
		{"task cancel", TaskCancelSubject("task-xyz"), "task.cancel.task-xyz"},
		{"worker register", WorkerRegisterSubject("w-1"), "worker.register.w-1"},
		{"worker heartbeat", WorkerHeartbeatSubject("w-1"), "worker.heartbeat.w-1"},
		{"worker deregister", WorkerDeregisterSubject("w-1"), "worker.deregister.w-1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Fatalf("got %q, want %q", tt.got, tt.want)
			}
		})
	}
}

func TestSubjectPrefixConstants(t *testing.T) {
	// The full subject helpers must compose from their declared prefixes so the
	// JetStream stream subject-filters (prefix + ".>") stay in sync.
	tests := []struct {
		name   string
		prefix string
		full   string
	}{
		{"work lease", SubjectWorkLeasePrefix, WorkLeaseSubject("x", "y")},
		{"task status", SubjectTaskStatusPrefix, TaskStatusSubject("x", "y")},
		{"task logs", SubjectTaskLogsPrefix, TaskLogsSubject("x", "y")},
		{"task cancel", SubjectTaskCancelPrefix, TaskCancelSubject("x")},
		{"worker register", SubjectWorkerRegisterPrefix, WorkerRegisterSubject("x")},
		{"worker heartbeat", SubjectWorkerHeartbeatPrefix, WorkerHeartbeatSubject("x")},
		{"worker deregister", SubjectWorkerDeregisterPrefix, WorkerDeregisterSubject("x")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			want := tt.prefix + "."
			if !strings.HasPrefix(tt.full, want) || tt.full == want {
				t.Fatalf("full subject %q does not compose from prefix %q", tt.full, tt.prefix)
			}
		})
	}
}

func TestParseWorkerSubject(t *testing.T) {
	tests := []struct {
		subject    string
		wantWorker string
		wantLeaf   string
		wantOK     bool
	}{
		{"task.status.w1.j1", "w1", "j1", true},
		{"task.logs.w1.t1", "w1", "t1", true},
		{"worker.register.w1", "w1", "", true},
		{"worker.heartbeat.w1", "w1", "", true},
		{"worker.deregister.w1", "w1", "", true},
		{"work.lease.w1.q1", "w1", "q1", true},
		{"work.lease.w1._any", "w1", "_any", true},

		// Subject shapes that do not carry a worker identity. A publisher using
		// one of these is not a worker this server can attribute a message to,
		// so it must be rejected rather than parsed into a partial answer.
		{"task.status.j1", "", "", false},
		{"worker.register", "", "", false},
		{"task.cancel.t1", "", "", false}, // server → worker
		{"worker.diag.w1", "", "", false}, // core NATS, not a stream subject
		{"task.status.w1.j1.extra", "", "", false},
		{"task.status..j1", "", "", false},
		{"task.status.w1.", "", "", false},
		{"worker.register.", "", "", false},
		{"", "", "", false},

		// A wildcard token names a set of workers, not one, so it identifies
		// nobody. Callers feed this worker ID to an authorization decision, so
		// the parser must reject it here rather than lean on the broker's
		// refusal to publish on a wildcard subject.
		{"task.status.*.j1", "", "", false},
		{"task.status.w1.*", "", "", false},
		{"task.status.>.j1", "", "", false},
		{"task.status.w1.>", "", "", false},
		{"worker.register.*", "", "", false},
		{"worker.heartbeat.>", "", "", false},
		{"work.lease.*.q1", "", "", false},
		{"work.lease.w1.>", "", "", false},
		{"task.logs.w*1.t1", "", "", false},
		{"worker.deregister.w>1", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.subject, func(t *testing.T) {
			w, leaf, ok := ParseWorkerSubject(tt.subject)
			if w != tt.wantWorker || leaf != tt.wantLeaf || ok != tt.wantOK {
				t.Errorf("= (%q, %q, %v), want (%q, %q, %v)", w, leaf, ok, tt.wantWorker, tt.wantLeaf, tt.wantOK)
			}
		})
	}
}
