// SPDX-License-Identifier: AGPL-3.0-or-later

package winsvc

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// appendTrace appends one slog-shaped JSON line recording a service's fatal
// error, so a service that fails before (or after) building its own logger
// still leaves a line an operator can read. path's directory is created the
// way the default log directory is when it is missing.
func appendTrace(path string, cause error) error {
	if err := createLogDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("winsvc: create trace directory: %w", err)
	}
	return writeTrace(path, cause, nil)
}

// writeTrace appends the trace line for cause to path, whose directory must
// already exist. logErr, when set, records why the default log could not take
// the line (see fallbackTracePath).
func writeTrace(path string, cause, logErr error) error {
	entry := struct {
		Time     string `json:"time"`
		Level    string `json:"level"`
		Msg      string `json:"msg"`
		Error    string `json:"error"`
		LogError string `json:"log_error,omitempty"`
	}{Time: time.Now().UTC().Format(time.RFC3339Nano), Level: "ERROR", Msg: "service exited with error", Error: cause.Error()}
	if logErr != nil {
		entry.LogError = logErr.Error()
	}
	line, err := json.Marshal(entry)
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

// fallbackTracePath is where a service's trace goes when its default log
// cannot take it — typically because the default log directory cannot be
// created: <service-name>.trace.log in dir, the service's working directory.
// Every character of the service name outside [A-Za-z0-9._-] becomes '_', so
// the name cannot reach another directory or, with a ':', name an NTFS
// alternate data stream.
func fallbackTracePath(dir, serviceName string) string {
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			return r
		default:
			return '_'
		}
	}, serviceName)
	return filepath.Join(dir, safe+".trace.log")
}
