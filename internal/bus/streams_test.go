// SPDX-License-Identifier: AGPL-3.0-or-later

package bus

import (
	"testing"

	"github.com/nats-io/nats.go/jetstream"
)

func TestStreamDefs(t *testing.T) {
	defs := streamDefs()

	byName := make(map[string]jetstream.StreamConfig, len(defs))
	for _, d := range defs {
		if _, dup := byName[d.Name]; dup {
			t.Fatalf("duplicate stream definition for %q", d.Name)
		}
		byName[d.Name] = d
	}

	want := map[string]struct {
		subjects  []string
		retention jetstream.RetentionPolicy
	}{
		StreamTask:   {[]string{SubjectTaskStatusPrefix + ".>"}, jetstream.WorkQueuePolicy},
		StreamLogs:   {[]string{SubjectTaskLogsPrefix + ".>"}, jetstream.LimitsPolicy},
		StreamWorker: {[]string{SubjectWorkerRegister, SubjectWorkerHeartbeat, SubjectWorkerDeregister}, jetstream.WorkQueuePolicy},
		StreamCancel: {[]string{SubjectTaskCancelPrefix + ".>"}, jetstream.WorkQueuePolicy},
	}

	if len(defs) != len(want) {
		t.Fatalf("streamDefs returned %d streams, want %d", len(defs), len(want))
	}

	for name, exp := range want {
		t.Run(name, func(t *testing.T) {
			cfg, ok := byName[name]
			if !ok {
				t.Fatalf("missing stream definition %q", name)
			}
			if cfg.Retention != exp.retention {
				t.Errorf("%s retention = %v, want %v", name, cfg.Retention, exp.retention)
			}
			if cfg.Storage != jetstream.FileStorage {
				t.Errorf("%s storage = %v, want FileStorage", name, cfg.Storage)
			}
			if cfg.Replicas != 1 {
				t.Errorf("%s replicas = %d, want 1", name, cfg.Replicas)
			}
			if len(cfg.Subjects) != len(exp.subjects) {
				t.Fatalf("%s subjects = %v, want %v", name, cfg.Subjects, exp.subjects)
			}
			for i, s := range exp.subjects {
				if cfg.Subjects[i] != s {
					t.Errorf("%s subjects[%d] = %q, want %q", name, i, cfg.Subjects[i], s)
				}
			}
		})
	}
}
