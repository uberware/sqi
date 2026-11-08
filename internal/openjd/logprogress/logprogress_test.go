// SPDX-License-Identifier: AGPL-3.0-or-later

package logprogress

import (
	"regexp"
	"strings"
	"testing"
)

func TestClassifyProgress(t *testing.T) {
	tests := []struct {
		pattern string
		want    Form
	}{
		{`percent=(?P<percent>\d+)`, FormPercent},
		{`(?P<done>\d+)/(?P<total>\d+)`, FormCounts},
		{`frame (?P<frame>-?\d+)`, FormFrame},
		{`wrote (?P<framedone>\d+)`, FormFrameDone},
		{`(?P<framepercent>\d+)%`, FormFramePercent},
		{`(?P<framedone>\d+)/(?P<frametotal>\d+)`, FormFrameCounts},
		{`(\d+) (?P<percent>\d+)`, FormPercent},
	}
	for _, tt := range tests {
		t.Run(tt.pattern, func(t *testing.T) {
			got, err := ClassifyProgress(regexp.MustCompile(tt.pattern))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("form = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestClassifyProgress_Errors(t *testing.T) {
	tests := []struct {
		name, pattern, wantSub string
	}{
		{"no named group", `\d+`, "named"},
		{"unnamed only", `(\d+)`, "named"},
		{"done alone", `(?P<done>\d+)`, "done"},
		{"mixed", `(?P<percent>\d)(?P<frame>\d)`, "frame"},
		{"status in progress", `(?P<status>.*)`, "status"},
		{"unknown name", `(?P<pct>\d+)`, "pct"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ClassifyProgress(regexp.MustCompile(tt.pattern))
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tt.wantSub) {
				t.Errorf("error %q does not mention %q", err, tt.wantSub)
			}
			if !strings.Contains(err.Error(), "framedone") {
				t.Errorf("error %q should list the accepted sets", err)
			}
		})
	}
}

func TestCheckStatus(t *testing.T) {
	for _, p := range []string{`Saving (.*)`, `(?P<status>.+)`, `plain`} {
		if err := CheckStatus(regexp.MustCompile(p)); err != nil {
			t.Errorf("%q: unexpected error %v", p, err)
		}
	}
	if err := CheckStatus(regexp.MustCompile(`(?P<frame>\d+)`)); err == nil {
		t.Error("expected error for a frame group in a status pattern")
	}
}

// TestClassify_DuplicateGroupName pins that a pattern naming one group twice is
// rejected: only one of the two submatches could ever be read, so a branch of
// the pattern would silently never report.
func TestClassify_DuplicateGroupName(t *testing.T) {
	_, err := ClassifyProgress(regexp.MustCompile(`a (?P<percent>\d+)%|b (?P<percent>\d+)%`))
	if err == nil || !strings.Contains(err.Error(), `"percent"`) || !strings.Contains(err.Error(), "more than once") {
		t.Errorf("ClassifyProgress duplicate: err = %v", err)
	}
	err = CheckStatus(regexp.MustCompile(`a (?P<status>x)|b (?P<status>y)`))
	if err == nil || !strings.Contains(err.Error(), "more than once") {
		t.Errorf("CheckStatus duplicate: err = %v", err)
	}
}

// TestClassifyNames pins that classifying from a parse's capture names, with no
// program built, agrees with classifying the compiled pattern.
func TestClassifyNames(t *testing.T) {
	for _, p := range []string{
		`percent=(?P<percent>\d+)`,
		`(?P<framedone>\d+)/(?P<frametotal>\d+)`,
		`(\d+) (?P<frame>\d+)`,
		`(?P<pct>\d+)`,
		`(?P<percent>1)|(?P<percent>2)`,
	} {
		re := regexp.MustCompile(p)
		want, wantErr := ClassifyProgress(re)
		got, gotErr := ClassifyNames(re.SubexpNames())
		if got != want || (gotErr == nil) != (wantErr == nil) {
			t.Errorf("%q: ClassifyNames = %v, %v; ClassifyProgress = %v, %v", p, got, gotErr, want, wantErr)
		}
	}
}

func TestForm_Predicates(t *testing.T) {
	tests := []struct {
		f      Form
		frames bool
		within bool
	}{
		{FormPercent, false, false},
		{FormCounts, false, false},
		{FormFrame, true, false},
		{FormFrameDone, true, false},
		{FormFramePercent, true, true},
		{FormFrameCounts, true, true},
	}
	for _, tt := range tests {
		if tt.f.UsesFrames() != tt.frames || tt.f.WithinFrame() != tt.within {
			t.Errorf("form %v: UsesFrames=%v WithinFrame=%v", tt.f, tt.f.UsesFrames(), tt.f.WithinFrame())
		}
	}
}
