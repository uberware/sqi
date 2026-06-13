// SPDX-License-Identifier: AGPL-3.0-or-later

package bus

import "testing"

func TestSubjectHelpers(t *testing.T) {
	tests := []struct {
		name string
		got  string
		want string
	}{
		{"work assign", WorkAssignSubject("q-123"), "work.assign.q-123"},
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
		{"work assign", SubjectWorkAssignPrefix, WorkAssignSubject("x")},
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

func TestSanitizeConsumerToken(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"uuid with dashes preserved", "a1b2c3d4-e5f6", "a1b2c3d4-e5f6"},
		{"underscores and dots preserved", "queue_1.default", "queue_1.default"},
		{"alphanumeric preserved", "Queue42", "Queue42"},
		{"spaces replaced", "my queue", "my_queue"},
		{"slashes and stars replaced", "a/b*c", "a_b_c"},
		{"empty stays empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sanitizeConsumerToken(tt.in); got != tt.want {
				t.Fatalf("sanitizeConsumerToken(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestWorkConsumerName(t *testing.T) {
	tests := []struct {
		name    string
		queueID string
		want    string
	}{
		{"clean id", "default", "sqi-work-default"},
		{"id needing sanitize", "my queue", "sqi-work-my_queue"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := workConsumerName(tt.queueID); got != tt.want {
				t.Fatalf("workConsumerName(%q) = %q, want %q", tt.queueID, got, tt.want)
			}
		})
	}
}
