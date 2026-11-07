// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"strings"
	"testing"
)

// TestLoad_ConfigFileRejectsUnknownKeys pins that the worker decodes its config
// file strictly, YAML or JSON, down into custom detectors. A dropped key fails
// open: isolation: {requird: true} used to run with isolation off, and a
// misspelled env matches made the check match on presence alone. The decoder's
// own rules are pinned in internal/yamlstrict.
func TestLoad_ConfigFileRejectsUnknownKeys(t *testing.T) {
	const detector = "capabilities:\n  detect:\n    - tag: hou\n      checks:\n        - env: {name: HFS, matchs: x}\n"
	tests := []struct {
		name string
		file string
		body string
		want string // empty: the file is valid
	}{
		{"valid", "sqi-worker.yaml", "isolation:\n  required: true\n", ""},
		{"empty file", "sqi-worker.yaml", "", ""},
		{"misspelled nested key", "sqi-worker.yaml", "isolation:\n  requird: true\n", "unknown key isolation.requird"},
		{"json", "sqi-worker.json", `{"isolation": {"requird": true}}`, "unknown key isolation.requird"},
		{"custom detector env check", "sqi-worker.yaml", detector, "unknown key capabilities.detect[0].checks[0].env.matchs"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("SQI_WORKER_NATS_URL", "nats://localhost:4222")
			path := writeTempFile(t, tt.file, []byte(tt.body))
			_, err := Load(path, FlagOverrides{})
			switch {
			case tt.want == "" && err != nil:
				t.Fatalf("Load: %v", err)
			case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
				t.Fatalf("Load error %v does not contain %q", err, tt.want)
			}
		})
	}
}
