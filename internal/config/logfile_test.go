// SPDX-License-Identifier: AGPL-3.0-or-later

package config_test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/config"
)

func clearLogEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"SQI_LOG_FILE", "SQI_LOG_MAX_SIZE_MB", "SQI_LOG_MAX_BACKUPS", "SQI_LOG_LEVEL", "SQI_LOG_FORMAT"} {
		t.Setenv(k, "")
	}
}

func TestLoad_LogFileDefaults(t *testing.T) {
	clearLogEnv(t)
	cfg := config.DefaultConfig()
	if cfg.Log.File != "" || cfg.Log.MaxSizeMB != 100 || cfg.Log.MaxBackups != 5 {
		t.Fatalf("defaults = %+v, want file empty, 100 MB, 5 backups", cfg.Log)
	}
}

func TestLoad_LogFileFromFileAndEnv(t *testing.T) {
	clearLogEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "sqi-server.yaml")
	logPath := filepath.Join(dir, "server.log")
	// strconv.Quote yields a YAML double-quoted scalar, so a Windows path's
	// backslashes survive.
	yaml := "log:\n  file: " + strconv.Quote(logPath) + "\n  max_size_mb: 7\n  max_backups: 2\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path, config.FlagOverrides{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Log.File != logPath || cfg.Log.MaxSizeMB != 7 || cfg.Log.MaxBackups != 2 {
		t.Fatalf("file layer = %+v", cfg.Log)
	}

	t.Setenv("SQI_LOG_MAX_SIZE_MB", "9")
	t.Setenv("SQI_LOG_MAX_BACKUPS", "0")
	t.Setenv("SQI_LOG_FILE", filepath.Join(dir, "env.log"))
	cfg, err = config.Load(path, config.FlagOverrides{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Log.File != filepath.Join(dir, "env.log") || cfg.Log.MaxSizeMB != 9 || cfg.Log.MaxBackups != 0 {
		t.Fatalf("env layer = %+v", cfg.Log)
	}
}

func TestLoad_LogMaxSizeEnvRejectsNonInteger(t *testing.T) {
	clearLogEnv(t)
	t.Setenv("SQI_LOG_MAX_SIZE_MB", "big")
	if _, err := config.Load("", config.FlagOverrides{}); err == nil || !strings.Contains(err.Error(), "SQI_LOG_MAX_SIZE_MB") {
		t.Fatalf("err = %v, want one naming SQI_LOG_MAX_SIZE_MB", err)
	}
}

func TestValidate_LogFileAndRotation(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name      string
		mutate    func(*config.LogConfig)
		wantField string // "" = valid
	}{
		{"defaults valid", func(*config.LogConfig) {}, ""},
		{"file in existing dir", func(l *config.LogConfig) { l.File = filepath.Join(dir, "s.log") }, ""},
		{"file in missing dir", func(l *config.LogConfig) { l.File = filepath.Join(dir, "nope", "s.log") }, "log.file"},
		{"zero max size", func(l *config.LogConfig) { l.MaxSizeMB = 0 }, "log.max_size_mb"},
		{"negative backups", func(l *config.LogConfig) { l.MaxBackups = -1 }, "log.max_backups"},
		{"zero backups valid", func(l *config.LogConfig) { l.MaxBackups = 0 }, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.DefaultConfig()
			tc.mutate(&cfg.Log)
			var fields []string
			for _, e := range config.Validate(cfg) {
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
