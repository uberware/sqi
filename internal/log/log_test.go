// SPDX-License-Identifier: AGPL-3.0-or-later

package log_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	applog "github.com/uberware/sqi/internal/log"
)

func TestParseLevel(t *testing.T) {
	cases := []struct {
		in      string
		want    slog.Level
		wantErr bool
	}{
		{"", slog.LevelInfo, false},
		{"info", slog.LevelInfo, false},
		{"INFO", slog.LevelInfo, false},
		{"debug", slog.LevelDebug, false},
		{"warn", slog.LevelWarn, false},
		{"error", slog.LevelError, false},
		{"nope", slog.LevelInfo, true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := applog.ParseLevel(tc.in)
			if tc.wantErr && err == nil {
				t.Fatalf("ParseLevel(%q): want error, got nil", tc.in)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("ParseLevel(%q): unexpected error %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("ParseLevel(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestNew_JSONFormatAndLevelFilter(t *testing.T) {
	var buf bytes.Buffer
	logger, err := applog.New("warn", "json", &buf)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	logger.InfoContext(t.Context(), "dropped") // below warn → filtered
	logger.WarnContext(t.Context(), "kept")
	if strings.Contains(buf.String(), "dropped") {
		t.Error("info line should have been filtered at warn level")
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(buf.String())), &rec); err != nil {
		t.Fatalf("output is not JSON: %v (%q)", err, buf.String())
	}
	if rec["msg"] != "kept" {
		t.Errorf("msg = %v, want kept", rec["msg"])
	}
}

func TestNew_TextFormat(t *testing.T) {
	var buf bytes.Buffer
	logger, err := applog.New("", "text", &buf)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	logger.InfoContext(t.Context(), "hi")
	if !strings.Contains(buf.String(), "msg=hi") {
		t.Errorf("text output = %q, want logfmt-style msg=hi", buf.String())
	}
}

func TestNew_InvalidLevelErrors(t *testing.T) {
	if _, err := applog.New("loud", "json", &bytes.Buffer{}); err == nil {
		t.Fatal("New with invalid level: want error, got nil")
	}
}
