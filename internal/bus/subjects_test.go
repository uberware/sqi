// SPDX-License-Identifier: AGPL-3.0-or-later

package bus

import "testing"

func TestSubjectHelpers(t *testing.T) {
	tests := []struct {
		name string
		got  string
		want string
	}{
		{"work lease", WorkLeaseSubject("q-123"), "work.lease.q-123"},
		{"task status", TaskStatusSubject("job-abc"), "task.status.job-abc"},
		{"task logs", TaskLogsSubject("task-xyz"), "task.logs.task-xyz"},
		{"task cancel", TaskCancelSubject("task-xyz"), "task.cancel.task-xyz"},
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
		{"work lease", SubjectWorkLeasePrefix, WorkLeaseSubject("x")},
		{"task status", SubjectTaskStatusPrefix, TaskStatusSubject("x")},
		{"task logs", SubjectTaskLogsPrefix, TaskLogsSubject("x")},
		{"task cancel", SubjectTaskCancelPrefix, TaskCancelSubject("x")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			want := tt.prefix + ".x"
			if tt.full != want {
				t.Fatalf("full subject %q does not compose from prefix %q", tt.full, tt.prefix)
			}
		})
	}
}
