// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"strings"
	"testing"
	"time"
)

func TestDefault_Preemption(t *testing.T) {
	cfg := Default()
	if cfg.Worker.CapacityGroup != "" {
		t.Errorf("default capacity_group = %q, want empty", cfg.Worker.CapacityGroup)
	}
	want := PreemptionConfig{Watch: "none", PollInterval: 5 * time.Second}
	if cfg.Worker.Preemption != want {
		t.Errorf("default preemption = %+v, want %+v", cfg.Worker.Preemption, want)
	}
	if errs := Validate(cfg); len(errs) != 0 {
		t.Errorf("the built-in defaults must be valid: %v", errs)
	}
}

func TestLoad_CapacityGroupFromEnv(t *testing.T) {
	t.Setenv("SQI_WORKER_NATS_URL", "nats://localhost:4222")
	t.Setenv("SQI_WORKER_CAPACITY_GROUP", "cloud-spot")
	cfg, err := Load("", FlagOverrides{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Worker.CapacityGroup != "cloud-spot" {
		t.Errorf("CapacityGroup = %q, want cloud-spot", cfg.Worker.CapacityGroup)
	}
}

func TestLoad_PreemptionFromFileAndEnv(t *testing.T) {
	path := writeTempYAML(t, "worker:\n  capacity_group: g1\n  preemption:\n    watch: gcp\n    poll_interval: 7s\n    endpoint: http://127.0.0.1:9\n")
	t.Setenv("SQI_WORKER_NATS_URL", "nats://localhost:4222")
	cfg, err := Load(path, FlagOverrides{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := PreemptionConfig{Watch: "gcp", PollInterval: 7 * time.Second, Endpoint: "http://127.0.0.1:9"}
	if cfg.Worker.Preemption != want || cfg.Worker.CapacityGroup != "g1" {
		t.Fatalf("from file: group %q preemption %+v, want g1 and %+v", cfg.Worker.CapacityGroup, cfg.Worker.Preemption, want)
	}

	t.Setenv("SQI_WORKER_PREEMPTION_WATCH", "azure")
	t.Setenv("SQI_WORKER_PREEMPTION_POLL_INTERVAL", "2s")
	t.Setenv("SQI_WORKER_PREEMPTION_ENDPOINT", "http://proxy:80")
	cfg, err = Load(path, FlagOverrides{})
	if err != nil {
		t.Fatalf("Load with env: %v", err)
	}
	want = PreemptionConfig{Watch: "azure", PollInterval: 2 * time.Second, Endpoint: "http://proxy:80"}
	if cfg.Worker.Preemption != want {
		t.Errorf("env did not override the file: %+v, want %+v", cfg.Worker.Preemption, want)
	}
}

func TestLoad_PreemptionWatchInvalid(t *testing.T) {
	t.Setenv("SQI_WORKER_NATS_URL", "nats://localhost:4222")
	t.Setenv("SQI_WORKER_PREEMPTION_WATCH", "oracle")
	_, err := Load("", FlagOverrides{})
	if err == nil || !strings.Contains(err.Error(), "worker.preemption.watch") {
		t.Fatalf("Load error = %v, want it to name worker.preemption.watch", err)
	}
}

func TestValidate_PreemptionPollInterval(t *testing.T) {
	tests := []struct {
		name    string
		d       time.Duration
		wantErr bool
	}{
		{"min", time.Second, false},
		{"default", 5 * time.Second, false},
		{"max", 60 * time.Second, false},
		{"below min", 999 * time.Millisecond, true},
		{"zero", 0, true},
		{"negative", -time.Second, true},
		{"above max", 61 * time.Second, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Default()
			cfg.Worker.Preemption.Watch = "aws"
			cfg.Worker.Preemption.PollInterval = tt.d
			errs := Validate(cfg)
			if got := containsField(errs, "worker.preemption.poll_interval"); got != tt.wantErr {
				t.Errorf("poll_interval %s: flagged = %v, want %v (%v)", tt.d, got, tt.wantErr, errs)
			}
		})
	}
}

func TestLoad_PreemptionRejectsUnknownKey(t *testing.T) {
	t.Setenv("SQI_WORKER_NATS_URL", "nats://localhost:4222")
	path := writeTempYAML(t, "worker:\n  preemption:\n    wach: aws\n")
	_, err := Load(path, FlagOverrides{})
	if err == nil || !strings.Contains(err.Error(), "unknown key worker.preemption.wach") {
		t.Fatalf("Load error = %v, want the misspelled key named", err)
	}
}

func TestLoad_MalformedPreemptionPollIntervalEnv(t *testing.T) {
	t.Setenv("SQI_WORKER_NATS_URL", "nats://localhost:4222")
	t.Setenv("SQI_WORKER_PREEMPTION_POLL_INTERVAL", "fast")
	_, err := Load("", FlagOverrides{})
	if err == nil || !strings.Contains(err.Error(), "SQI_WORKER_PREEMPTION_POLL_INTERVAL") {
		t.Fatalf("Load error = %v, want it to name SQI_WORKER_PREEMPTION_POLL_INTERVAL", err)
	}
}

func TestDefault_CloudInstanceID(t *testing.T) {
	cfg := Default()
	want := CloudInstanceIDConfig{Source: "none"}
	if cfg.Worker.CloudInstanceID != want {
		t.Errorf("default cloud_instance_id = %+v, want %+v", cfg.Worker.CloudInstanceID, want)
	}
}

func TestLoad_CloudInstanceIDFromFileAndEnv(t *testing.T) {
	path := writeTempYAML(t, "worker:\n  cloud_instance_id:\n    source: gcp\n    endpoint: http://127.0.0.1:9\n")
	t.Setenv("SQI_WORKER_NATS_URL", "nats://localhost:4222")
	cfg, err := Load(path, FlagOverrides{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := CloudInstanceIDConfig{Source: "gcp", Endpoint: "http://127.0.0.1:9"}
	if cfg.Worker.CloudInstanceID != want {
		t.Fatalf("from file: %+v, want %+v", cfg.Worker.CloudInstanceID, want)
	}

	t.Setenv("SQI_WORKER_CLOUD_INSTANCE_ID_SOURCE", "azure")
	t.Setenv("SQI_WORKER_CLOUD_INSTANCE_ID_ENDPOINT", "http://proxy:80")
	cfg, err = Load(path, FlagOverrides{})
	if err != nil {
		t.Fatalf("Load with env: %v", err)
	}
	want = CloudInstanceIDConfig{Source: "azure", Endpoint: "http://proxy:80"}
	if cfg.Worker.CloudInstanceID != want {
		t.Errorf("env did not override the file: %+v, want %+v", cfg.Worker.CloudInstanceID, want)
	}
}

func TestLoad_CloudInstanceIDSourceInvalid(t *testing.T) {
	t.Setenv("SQI_WORKER_NATS_URL", "nats://localhost:4222")
	t.Setenv("SQI_WORKER_CLOUD_INSTANCE_ID_SOURCE", "oracle")
	_, err := Load("", FlagOverrides{})
	if err == nil || !strings.Contains(err.Error(), "worker.cloud_instance_id.source") {
		t.Fatalf("Load error = %v, want it to name worker.cloud_instance_id.source", err)
	}
}

func TestLoad_CloudInstanceIDRejectsUnknownKey(t *testing.T) {
	t.Setenv("SQI_WORKER_NATS_URL", "nats://localhost:4222")
	path := writeTempYAML(t, "worker:\n  cloud_instance_id:\n    sorce: aws\n")
	_, err := Load(path, FlagOverrides{})
	if err == nil || !strings.Contains(err.Error(), "unknown key worker.cloud_instance_id.sorce") {
		t.Fatalf("Load error = %v, want the misspelled key named", err)
	}
}
