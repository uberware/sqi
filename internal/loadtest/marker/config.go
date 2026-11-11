// SPDX-License-Identifier: AGPL-3.0-or-later

package marker

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
)

// ConfigArgPrefix starts the no-op command's argument naming its noop.json.
// The managed target puts it first when a job class has real_task; without
// it the no-op prints one line and exits.
const ConfigArgPrefix = "--sqi-load-config="

// ConfigArg returns the argument naming the config file at path.
func ConfigArg(path string) string { return ConfigArgPrefix + path }

// ConfigFromArgs returns the path the last config argument names.
func ConfigFromArgs(args []string) (string, bool) {
	for _, arg := range slices.Backward(args) {
		if path, ok := strings.CutPrefix(arg, ConfigArgPrefix); ok {
			return path, true
		}
	}
	return "", false
}

// NoopConfig is noop.json: where real tasks write their markers and how each
// job class with real_task behaves. A class it does not list behaves as the
// default no-op.
type NoopConfig struct {
	MarkerDir string               `json:"marker_dir"`
	Classes   map[string]NoopClass `json:"classes"`
}

// NoopClass is one class's real_task.
type NoopClass struct {
	DurationMS  int  `json:"duration_ms"`
	LogEveryMS  int  `json:"log_every_ms"`
	ChattyChild bool `json:"chatty_child"`
	SilentChild bool `json:"silent_child"`
}

// WriteConfig writes cfg to path.
func WriteConfig(path string, cfg NoopConfig) error {
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marker: %w", err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o600); err != nil {
		return fmt.Errorf("marker: write %s: %w", path, err)
	}
	return nil
}

// ReadConfig reads the noop.json at path.
func ReadConfig(path string) (NoopConfig, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return NoopConfig{}, fmt.Errorf("marker: %w", err)
	}
	var cfg NoopConfig
	if err := json.Unmarshal(b, &cfg); err != nil {
		return NoopConfig{}, fmt.Errorf("marker: %s: %w", path, err)
	}
	if cfg.MarkerDir == "" {
		return NoopConfig{}, fmt.Errorf("marker: %s names no marker_dir", path)
	}
	return cfg, nil
}
