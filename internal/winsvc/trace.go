// SPDX-License-Identifier: AGPL-3.0-or-later

package winsvc

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// appendTrace appends one slog-shaped JSON line recording a service's fatal
// error, so a service that fails before (or after) building its own logger
// still leaves a line an operator can read.
func appendTrace(path string, cause error) error {
	if err := createLogDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("winsvc: create trace directory: %w", err)
	}
	line, err := json.Marshal(struct {
		Time  string `json:"time"`
		Level string `json:"level"`
		Msg   string `json:"msg"`
		Error string `json:"error"`
	}{time.Now().UTC().Format(time.RFC3339Nano), "ERROR", "service exited with error", cause.Error()})
	if err != nil {
		return fmt.Errorf("winsvc: encode trace: %w", err)
	}
	//nolint:gosec // G302: log files are group-readable so a log shipper in the service's group can tail them
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return fmt.Errorf("winsvc: open trace %s: %w", path, err)
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close() // returning the write error
		return fmt.Errorf("winsvc: write trace: %w", err)
	}
	return f.Close()
}
