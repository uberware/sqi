// SPDX-License-Identifier: AGPL-3.0-or-later

package logprogress

import (
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

func mustCompile(t *testing.T, progress, status []string, frames string) *Matcher {
	t.Helper()
	m, err := Compile(progress, status, frames)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	return m
}

type step struct {
	line string
	want int // -1 = no progress
}

func runSteps(t *testing.T, m *Matcher, steps []step) {
	t.Helper()
	for _, s := range steps {
		u := m.Observe(s.line)
		if s.want < 0 {
			if u.HasProgress {
				t.Errorf("%q: got progress %d, want none", s.line, u.Progress)
			}
			continue
		}
		if !u.HasProgress || u.Progress != s.want {
			t.Errorf("%q: got (%d,%v), want %d", s.line, u.Progress, u.HasProgress, s.want)
		}
	}
}

func TestMatcher_Percent(t *testing.T) {
	m := mustCompile(t, []string{`(?P<percent>[\d.]+)%`}, nil, "")
	runSteps(t, m, []step{{"43.7%", 43}, {"150%", -1}, {"abc", -1}})
}

func TestMatcher_Counts(t *testing.T) {
	m := mustCompile(t, []string{`(?P<done>\d+)/(?P<total>\d+)`}, nil, "")
	runSteps(t, m, []step{{"12/48", 25}, {"0/0", -1}, {"5/4", -1}})
}

func TestMatcher_FrameOverChunk(t *testing.T) {
	m := mustCompile(t, []string{`frame (?P<frame>-?\d+)`}, nil, "1-10")
	runSteps(t, m, []step{{"frame 1", 0}, {"frame 6", 50}})
}

func TestMatcher_FrameStepped(t *testing.T) {
	m := mustCompile(t, []string{`frame (?P<frame>-?\d+)`}, nil, "1-19:2")
	runSteps(t, m, []step{{"frame 11", 50}, {"frame 2", -1}})
}

func TestMatcher_FrameListed(t *testing.T) {
	m := mustCompile(t, []string{`frame (?P<frame>-?\d+)`}, nil, "1-3,7,8")
	runSteps(t, m, []step{{"frame 7", 60}})
	m = mustCompile(t, []string{`frame (?P<frame>-?\d+)`}, nil, "-3--1")
	runSteps(t, m, []step{{"frame -2", 33}})
}

func TestMatcher_FrameDone(t *testing.T) {
	m := mustCompile(t, []string{`wrote (?P<framedone>\d+)`}, nil, "1-4")
	runSteps(t, m, []step{{"wrote 2", 50}})
}

func TestMatcher_WithinFrame(t *testing.T) {
	m := mustCompile(t, []string{
		`frame (?P<frame>\d+)`,
		`Sample (?P<framedone>\d+)/(?P<frametotal>\d+)`,
	}, nil, "1-4")
	runSteps(t, m, []step{
		{"Sample 1/8", -1},
		{"frame 2", 25},
		{"Sample 4/8", 37},
		{"frame 3", 50},
		{"Sample 8/8", 75},
	})
}

func TestMatcher_SingleFrameWithinFrame(t *testing.T) {
	m := mustCompile(t, []string{
		`frame (?P<frame>\d+)`,
		`(?P<framepercent>\d+)%`,
	}, nil, "5")
	runSteps(t, m, []step{{"frame 5", 0}, {"40%", 40}})
}

func TestMatcher_FrameOutsideChunkIgnored(t *testing.T) {
	m := mustCompile(t, []string{`frame (?P<frame>\d+)`}, nil, "1-4")
	runSteps(t, m, []step{{"frame 9", -1}})
}

func TestMatcher_ClampsAt100(t *testing.T) {
	m := mustCompile(t, []string{
		`frame (?P<frame>\d+)`,
		`(?P<framepercent>\d+)%`,
	}, nil, "1-4")
	runSteps(t, m, []step{{"frame 4", 75}, {"120%", 100}})
}

func TestMatcher_FirstPatternWins(t *testing.T) {
	m := mustCompile(t, []string{
		`a=(?P<percent>\d+)`,
		`(?P<percent>\d+)`,
	}, nil, "")
	runSteps(t, m, []step{{"a=10 20", 10}})
}

// TestMatcher_InvalidValueFallsThrough pins that the first pattern to yield a
// valid value wins, not the first to match: a match whose value is out of
// range leaves the line to the later patterns.
func TestMatcher_InvalidValueFallsThrough(t *testing.T) {
	m := mustCompile(t, []string{
		`(?P<percent>\d+)%`,
		`(?P<done>\d+)/(?P<total>\d+)`,
	}, nil, "")
	runSteps(t, m, []step{{"150% 3/4", 75}, {"150%", -1}})
}

func TestMatcher_Status(t *testing.T) {
	m := mustCompile(t, nil, []string{`Saving (?P<status>.+)`, `^Loading`}, "")
	if u := m.Observe("Saving file x"); !u.HasStatus || u.Status != "file x" {
		t.Errorf("group: %+v", u)
	}
	if u := m.Observe("Loading scene"); !u.HasStatus || u.Status != "Loading scene" {
		t.Errorf("whole line: %+v", u)
	}
	if u := m.Observe("nothing"); u.HasStatus {
		t.Errorf("no match: %+v", u)
	}
	long := strings.Repeat("é", 150)
	u := m.Observe("Saving " + long)
	if !u.HasStatus || len(u.Status) > MaxStatusBytes || !utf8.ValidString(u.Status) {
		t.Errorf("cut: len=%d valid=%v", len(u.Status), utf8.ValidString(u.Status))
	}
	if len(u.Status) != 200 {
		t.Errorf("expected 200 bytes (even rune boundary), got %d", len(u.Status))
	}
	odd := strings.Repeat("a", 199) + "é"
	if u := m.Observe("Saving " + odd); len(u.Status) != 199 {
		t.Errorf("rune split not avoided: %d", len(u.Status))
	}
}

func TestMatcher_ProgressAndStatusIndependent(t *testing.T) {
	m := mustCompile(t, []string{`(?P<percent>\d+)%`}, []string{`^(?P<status>\d+% .*)$`}, "")
	u := m.Observe("50% rendering")
	if !u.HasProgress || u.Progress != 50 || !u.HasStatus || u.Status != "50% rendering" {
		t.Errorf("%+v", u)
	}
}

func TestCompile_Errors(t *testing.T) {
	frame := []string{`frame (?P<frame>\d+)`}
	pct := []string{`(?P<percent>\d+)`}
	many := make([]string, 33)
	for i := range many {
		many[i] = `(?P<percent>\d+)`
	}
	tests := []struct {
		name             string
		progress, status []string
		frames           string
	}{
		{"empty frames", frame, nil, ""},
		{"bad frames", frame, nil, "x"},
		{"saturating frames", frame, nil, "-9223372036854775808-9223372036854775807"},
		{"bad regexp", []string{`(`}, nil, ""},
		{"too large", []string{`(?P<percent>(((a{1,1000}){1,1000}){1,1000}))`}, nil, ""},
		{"unclassifiable", []string{`\d+`}, nil, ""},
		{"bad status", nil, []string{`(?P<frame>\d)`}, ""},
		{"too many progress", many, nil, ""},
		{"too many status", nil, many, ""},
		{"pattern too long", []string{`(?P<percent>\d+)` + strings.Repeat("a", 1024)}, nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Compile(tt.progress, tt.status, tt.frames); err == nil {
				t.Fatal("expected error")
			}
		})
	}
	if _, err := Compile(pct, nil, "x"); err != nil {
		t.Errorf("frames must be ignored without frame forms: %v", err)
	}
}

func TestMatcher_Concurrent(t *testing.T) {
	m := mustCompile(t, []string{`frame (?P<frame>\d+)`}, []string{`s (.*)`}, "1-100")
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for i := 1; i <= 100; i++ {
				m.Observe("frame " + strings.Repeat("1", 1))
				m.Observe("s x")
			}
		})
	}
	wg.Wait()
}
