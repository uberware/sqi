// SPDX-License-Identifier: AGPL-3.0-or-later

package staging

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/uberware/sqi/internal/worker/protocol"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// TestStager_Resolution exercises Configured/effectiveScratch/useBuiltin
// across the unset/explicit/sentinel/defaults-off combinations.
func TestStager_Resolution(t *testing.T) {
	tmpDefault := filepath.Join(os.TempDir(), "sqi-staging")
	tests := []struct {
		name           string
		scratch, sync  string
		defaults       bool
		wantConfigured bool
		wantScratch    string // effective scratch base
		wantBuiltin    bool   // uses builtin copy vs shell
	}{
		{"unset defaults-on", "", "", true, true, tmpDefault, true},
		{"explicit shell", "/scr", "cp {src} {dest}", true, true, "/scr", false},
		{"builtin sentinel defaults-off", "", "builtin", false, true, tmpDefault, true},
		{"unset defaults-off", "", "", false, false, "", true /*unused*/},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := New(tt.scratch, tt.sync, tt.defaults, discardLogger())
			if s.Configured() != tt.wantConfigured {
				t.Fatalf("Configured()=%v want %v", s.Configured(), tt.wantConfigured)
			}
			if tt.wantConfigured {
				if got := s.effectiveScratch(); got != tt.wantScratch {
					t.Fatalf("effectiveScratch()=%q want %q", got, tt.wantScratch)
				}
				if s.useBuiltin() != tt.wantBuiltin {
					t.Fatalf("useBuiltin()=%v want %v", s.useBuiltin(), tt.wantBuiltin)
				}
			}
		})
	}
}

// TestStager_RoundTripBuiltin exercises defaults-on, built-in-copy staging
// end to end: StageIn copies an IN file into scratch; the task (simulated)
// writes an OUT file in scratch; StageOut copies it back to the real path.
func TestStager_RoundTripBuiltin(t *testing.T) {
	base := t.TempDir()
	s := New(filepath.Join(base, "scratch"), builtinSentinel, true, discardLogger())

	in := filepath.Join(base, "input.txt")
	if err := os.WriteFile(in, []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(base, "result.txt")
	entries := []protocol.StageEntry{
		{Path: in, Direction: "IN", ObjectType: "FILE"},
		{Path: out, Direction: "OUT", ObjectType: "FILE"},
	}

	rules, scratch, err := s.StageIn(context.Background(), "job1", "att1", entries)
	if err != nil {
		t.Fatalf("StageIn: %v", err)
	}
	// The IN file is now readable at its scratch destination.
	if b, err := os.ReadFile(rules[0].DestinationPath); err != nil || string(b) != "payload" {
		t.Fatalf("staged input: %q err=%v", b, err)
	}
	// Simulate the task writing the OUT file in scratch.
	if err := os.WriteFile(rules[1].DestinationPath, []byte("rendered"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.StageOut(context.Background(), scratch, entries); err != nil {
		t.Fatalf("StageOut: %v", err)
	}
	if b, err := os.ReadFile(out); err != nil || string(b) != "rendered" {
		t.Fatalf("copied-out: %q err=%v", b, err)
	}
}
