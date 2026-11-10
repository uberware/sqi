// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"slices"
	"strings"
	"testing"
)

func TestLoad_PowerDefaultsOff(t *testing.T) {
	t.Setenv("SQI_WORKER_NATS_URL", "nats://localhost:4222")
	cfg, err := Load("", FlagOverrides{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	p := cfg.Worker.Power
	if p.AllowRemoteSleep || len(p.SleepCommand) != 0 || len(p.ShutdownCommand) != 0 {
		t.Errorf("default power = %+v, want remote sleep off and no command overrides", p)
	}
	if errs := Validate(Default()); len(errs) != 0 {
		t.Errorf("the built-in defaults must be valid: %v", errs)
	}
}

func TestLoad_PowerFromFileAndEnv(t *testing.T) {
	path := writeTempYAML(t, "worker:\n  power:\n    allow_remote_sleep: true\n    sleep_command: [\"my-sleep\", \"--now\"]\n    shutdown_command: [\"my-off\"]\n")
	t.Setenv("SQI_WORKER_NATS_URL", "nats://localhost:4222")
	cfg, err := Load(path, FlagOverrides{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	p := cfg.Worker.Power
	if !p.AllowRemoteSleep || !slices.Equal(p.SleepCommand, []string{"my-sleep", "--now"}) ||
		!slices.Equal(p.ShutdownCommand, []string{"my-off"}) {
		t.Fatalf("from file: %+v", p)
	}

	t.Setenv("SQI_WORKER_POWER_ALLOW_REMOTE_SLEEP", "false")
	cfg, err = Load(path, FlagOverrides{})
	if err != nil {
		t.Fatalf("Load with env: %v", err)
	}
	if cfg.Worker.Power.AllowRemoteSleep {
		t.Error("the environment did not override allow_remote_sleep from the file")
	}
}

func TestLoad_PowerAllowFromEnvMalformed(t *testing.T) {
	t.Setenv("SQI_WORKER_NATS_URL", "nats://localhost:4222")
	t.Setenv("SQI_WORKER_POWER_ALLOW_REMOTE_SLEEP", "ture")
	_, err := Load("", FlagOverrides{})
	if err == nil || !strings.Contains(err.Error(), "SQI_WORKER_POWER_ALLOW_REMOTE_SLEEP") {
		t.Fatalf("Load error = %v, want it to name SQI_WORKER_POWER_ALLOW_REMOTE_SLEEP", err)
	}
}

func TestLoad_PowerRejectsUnknownKey(t *testing.T) {
	t.Setenv("SQI_WORKER_NATS_URL", "nats://localhost:4222")
	path := writeTempYAML(t, "worker:\n  power:\n    allow_remote_slep: true\n")
	_, err := Load(path, FlagOverrides{})
	if err == nil || !strings.Contains(err.Error(), "unknown key worker.power.allow_remote_slep") {
		t.Fatalf("Load error = %v, want the misspelled key named", err)
	}
}

func TestValidate_PowerCommands(t *testing.T) {
	tests := []struct {
		name      string
		sleep     []string
		shutdown  []string
		wantField string
	}{
		{name: "empty means the default"},
		{name: "plain argv", sleep: []string{"a", "b"}, shutdown: []string{"c"}},
		{name: "blank program in sleep", sleep: []string{" "}, wantField: "worker.power.sleep_command"},
		{name: "blank program in shutdown", shutdown: []string{""}, wantField: "worker.power.shutdown_command"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Default()
			cfg.Worker.Power.SleepCommand = tt.sleep
			cfg.Worker.Power.ShutdownCommand = tt.shutdown
			var got []string
			for _, e := range Validate(cfg) {
				got = append(got, e.Field)
			}
			if tt.wantField == "" {
				if len(got) != 0 {
					t.Fatalf("Validate = %v, want none", got)
				}
				return
			}
			if !slices.Contains(got, tt.wantField) {
				t.Fatalf("Validate fields = %v, want %s", got, tt.wantField)
			}
		})
	}
}
