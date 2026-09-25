// SPDX-License-Identifier: AGPL-3.0-or-later

package config_test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	workerconfig "github.com/uberware/sqi/internal/worker/config"
)

func clearWorkerLogEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"SQI_WORKER_LOG_FILE", "SQI_WORKER_LOG_MAX_SIZE_MB", "SQI_WORKER_LOG_MAX_BACKUPS"} {
		t.Setenv(k, "")
	}
}

func writeWorkerYAML(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sqi-worker.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestWorkerLoad_LogFileDefaultsFileAndEnv(t *testing.T) {
	clearWorkerLogEnv(t)
	dir := t.TempDir()
	logPath := filepath.Join(dir, "w.log")

	cfg, err := workerconfig.Load(writeWorkerYAML(t, "worker:\n  name: a\n"), workerconfig.FlagOverrides{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Log.File != "" || cfg.Log.MaxSizeMB != 100 || cfg.Log.MaxBackups != 5 {
		t.Fatalf("defaults = %+v", cfg.Log)
	}

	body := "log:\n  file: " + strconv.Quote(logPath) + "\n  max_size_mb: 3\n  max_backups: 1\n"
	cfg, err = workerconfig.Load(writeWorkerYAML(t, body), workerconfig.FlagOverrides{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Log.File != logPath || cfg.Log.MaxSizeMB != 3 || cfg.Log.MaxBackups != 1 {
		t.Fatalf("file layer = %+v", cfg.Log)
	}

	t.Setenv("SQI_WORKER_LOG_FILE", filepath.Join(dir, "env.log"))
	t.Setenv("SQI_WORKER_LOG_MAX_SIZE_MB", "4")
	t.Setenv("SQI_WORKER_LOG_MAX_BACKUPS", "0")
	cfg, err = workerconfig.Load(writeWorkerYAML(t, body), workerconfig.FlagOverrides{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Log.File != filepath.Join(dir, "env.log") || cfg.Log.MaxSizeMB != 4 || cfg.Log.MaxBackups != 0 {
		t.Fatalf("env layer = %+v", cfg.Log)
	}
}

func TestWorkerValidate_LogFileAndRotation(t *testing.T) {
	clearWorkerLogEnv(t)
	dir := t.TempDir()
	tests := []struct {
		name      string
		mutate    func(*workerconfig.LogConfig)
		wantField string
	}{
		{"defaults valid", func(*workerconfig.LogConfig) {}, ""},
		{"file in existing dir", func(l *workerconfig.LogConfig) { l.File = filepath.Join(dir, "w.log") }, ""},
		{"file in missing dir", func(l *workerconfig.LogConfig) { l.File = filepath.Join(dir, "nope", "w.log") }, "log.file"},
		{"zero max size", func(l *workerconfig.LogConfig) { l.MaxSizeMB = 0 }, "log.max_size_mb"},
		{"negative backups", func(l *workerconfig.LogConfig) { l.MaxBackups = -1 }, "log.max_backups"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := workerconfig.Load(writeWorkerYAML(t, "worker:\n  name: a\n"), workerconfig.FlagOverrides{})
			if err != nil {
				t.Fatal(err)
			}
			tc.mutate(&cfg.Log)
			var fields []string
			for _, e := range workerconfig.Validate(cfg) {
				if strings.HasPrefix(e.Field, "log.") {
					fields = append(fields, e.Field)
				}
			}
			if tc.wantField == "" && len(fields) > 0 {
				t.Fatalf("unexpected log errors %v", fields)
			}
			if tc.wantField != "" && (len(fields) != 1 || fields[0] != tc.wantField) {
				t.Fatalf("log errors = %v, want exactly [%s]", fields, tc.wantField)
			}
		})
	}
}
