// SPDX-License-Identifier: AGPL-3.0-or-later

package capabilities

import (
	"strings"
	"testing"
	"testing/fstest"
)

// TestDetectorsFrom_RejectsUnknownKeys pins that the built-in detectors are
// decoded strictly: a misspelled key in a shipped detector fails the load
// rather than silently dropping the check it belonged to.
func TestDetectorsFrom_RejectsUnknownKeys(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string // empty: the detector is valid
	}{
		{"valid", "tag: x\nchecks:\n  - exe: x\n", ""},
		{"misspelled key", "tag: x\nchecks:\n  - exe: x\n    oss: windows\n", "unknown key checks[0].oss"},
		{"misspelled env key", "tag: x\nchecks:\n  - env: {name: X, matchs: y}\n", "unknown key checks[0].env.matchs"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fsys := fstest.MapFS{"builtins/x.yaml": {Data: []byte(tt.body)}}
			ds, err := detectorsFrom(fsys)
			switch {
			case tt.want == "" && (err != nil || len(ds) != 1):
				t.Fatalf("detectorsFrom = %v, %v", ds, err)
			case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
				t.Fatalf("detectorsFrom error %v does not contain %q", err, tt.want)
			}
		})
	}
}
