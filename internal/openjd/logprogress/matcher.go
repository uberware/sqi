// SPDX-License-Identifier: AGPL-3.0-or-later

package logprogress

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"sync"
	"unicode/utf8"

	"github.com/uberware/sqi/internal/openjd/intrange"
	"github.com/uberware/sqi/internal/openjd/logpattern"
)

// Update is what one output line told the matcher.
type Update struct {
	Progress    int
	HasProgress bool
	Status      string
	HasStatus   bool
}

type progressPattern struct {
	re   *regexp.Regexp
	form Form
}

type statusPattern struct {
	re *regexp.Regexp
	ix int // submatch index of the status group, or -1
}

// Matcher turns output lines into progress and status updates. It is safe for
// concurrent use.
type Matcher struct {
	progress []progressPattern
	status   []statusPattern

	ranges []intrange.Range
	prefix []int // frames in the ranges before ranges[i]
	n      int   // total frames

	// clean, when set, cleans a status capture before it is tested for empty
	// and cut to MaxStatusBytes.
	clean func(string) string

	mu      sync.Mutex
	base    int
	hasBase bool
}

func checkList(kind string, patterns []string) error {
	if len(patterns) > logpattern.MaxPatterns {
		return fmt.Errorf("%s: at most %d patterns are allowed, got %d", kind, logpattern.MaxPatterns, len(patterns))
	}
	for i, p := range patterns {
		if len(p) > logpattern.MaxPatternBytes {
			return fmt.Errorf("%s[%d]: pattern is %d bytes; at most %d are allowed", kind, i, len(p), logpattern.MaxPatternBytes)
		}
	}
	return nil
}

// Compile builds a Matcher. frames is the task's frame-parameter value
// ("5", "1-10", "1-19:2", "1-3,7,8"); it must parse when any progress pattern
// uses frames and is ignored otherwise.
//
// clean, which may be nil, cleans each status capture before the matcher tests
// it for empty and cuts it to MaxStatusBytes, so text it drops (leading space,
// control characters) neither uses up the limit nor stops a later status
// pattern from being tried. The worker passes its status-text sanitizer; it is
// a parameter so this package stays stdlib-only.
func Compile(progress, status []string, frames string, clean func(string) string) (*Matcher, error) {
	if err := checkList("progress", progress); err != nil {
		return nil, err
	}
	if err := checkList("status", status); err != nil {
		return nil, err
	}
	m := &Matcher{clean: clean}
	usesFrames := false
	for i, p := range progress {
		re, err := logpattern.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("progress[%d]: %w", i, err)
		}
		form, err := ClassifyProgress(re)
		if err != nil {
			return nil, fmt.Errorf("progress[%d]: %w", i, err)
		}
		usesFrames = usesFrames || form.UsesFrames()
		m.progress = append(m.progress, progressPattern{re: re, form: form})
	}
	for i, p := range status {
		re, err := logpattern.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("status[%d]: %w", i, err)
		}
		if err := CheckStatus(re); err != nil {
			return nil, fmt.Errorf("status[%d]: %w", i, err)
		}
		m.status = append(m.status, statusPattern{re: re, ix: re.SubexpIndex("status")})
	}
	if usesFrames {
		if err := m.setFrames(frames); err != nil {
			return nil, err
		}
	}
	return m, nil
}

func (m *Matcher) setFrames(frames string) error {
	if frames == "" {
		return errors.New("a frame-based progress pattern needs the task's frames, which are empty")
	}
	rs, err := intrange.Parse(frames)
	if err != nil {
		return fmt.Errorf("frames %q: %w", frames, err)
	}
	total := 0
	for _, r := range rs {
		c := r.Count()
		if c == math.MaxInt || total > math.MaxInt-c {
			return fmt.Errorf("frames %q: too many frames", frames)
		}
		m.prefix = append(m.prefix, total)
		total += c
	}
	m.ranges, m.n = rs, total
	return nil
}

// frameIndex returns the position of frame f within the chunk.
func (m *Matcher) frameIndex(f int) (int, bool) {
	for i, r := range m.ranges {
		step := r.Step
		if step == 0 {
			step = 1
		}
		if step > 0 && (f < r.Start || f > r.End) || step < 0 && (f > r.Start || f < r.End) {
			continue
		}
		off := f - r.Start
		if off%step != 0 {
			continue
		}
		return m.prefix[i] + off/step, true
	}
	return 0, false
}

func number(s string) (float64, bool) {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, false
	}
	return v, true
}

func toPercent(v float64) int {
	// The epsilon absorbs binary rounding (0.29*100 = 28.999999999999996).
	return int(min(math.Floor(v+1e-9), 100))
}

// group reads the named group of a match. The group exists: the pattern's
// Form was classified from its names.
func (p *progressPattern) group(sub []string, name string) (float64, bool) {
	return number(sub[p.re.SubexpIndex(name)])
}

// Observe feeds one output line to the matcher.
func (m *Matcher) Observe(line string) Update {
	var u Update
	for i := range m.progress {
		p := &m.progress[i]
		sub := p.re.FindStringSubmatch(line)
		if sub == nil {
			continue
		}
		if v, ok := m.progressFor(p, sub); ok {
			u.Progress, u.HasProgress = v, true
			break
		}
	}
	for _, sp := range m.status {
		sub := sp.re.FindStringSubmatch(line)
		if sub == nil {
			continue
		}
		text := line
		if sp.ix > 0 {
			text = sub[sp.ix]
		}
		if m.clean != nil {
			text = m.clean(text)
		}
		if text == "" {
			continue
		}
		u.Status, u.HasStatus = cutUTF8(text, MaxStatusBytes), true
		break
	}
	return u
}

func (m *Matcher) progressFor(p *progressPattern, sub []string) (int, bool) {
	switch p.form {
	case FormPercent:
		v, ok := p.group(sub, "percent")
		if !ok || v < 0 || v > 100 {
			return 0, false
		}
		return toPercent(v), true
	case FormCounts:
		done, ok1 := p.group(sub, "done")
		total, ok2 := p.group(sub, "total")
		if !ok1 || !ok2 || total <= 0 || done < 0 || done > total {
			return 0, false
		}
		return toPercent(done / total * 100), true
	}
	return m.frameProgress(p, sub)
}

// withinFraction returns how far through the current frame a within-frame
// form's match says the task is, in [0,1].
func (p *progressPattern) withinFraction(sub []string) (float64, bool) {
	if p.form == FormFramePercent {
		v, ok := p.group(sub, "framepercent")
		if !ok || v < 0 {
			return 0, false
		}
		return min(v, 100) / 100, true
	}
	done, ok1 := p.group(sub, "framedone")
	total, ok2 := p.group(sub, "frametotal")
	if !ok1 || !ok2 || total <= 0 || done < 0 || done > total {
		return 0, false
	}
	return done / total, true
}

// frameNumber returns the chunk position a FormFrame or FormFrameDone match
// refers to, as a count of frames complete before it.
func (m *Matcher) frameNumber(p *progressPattern, sub []string) (int, bool) {
	name := "frame"
	if p.form == FormFrameDone {
		name = "framedone"
	}
	v, ok := p.group(sub, name)
	if !ok || v != math.Trunc(v) || math.Abs(v) > math.MaxInt32 {
		return 0, false
	}
	idx, found := m.frameIndex(int(v))
	if !found {
		return 0, false
	}
	if p.form == FormFrameDone {
		idx++
	}
	return idx, true
}

func (m *Matcher) frameProgress(p *progressPattern, sub []string) (int, bool) {
	if p.form.WithinFrame() {
		x, ok := p.withinFraction(sub)
		if !ok {
			return 0, false
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		if !m.hasBase {
			return 0, false
		}
		return toPercent((float64(m.base) + x) * 100 / float64(m.n)), true
	}
	idx, ok := m.frameNumber(p, sub)
	if !ok {
		return 0, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.base, m.hasBase = idx, true
	return toPercent(float64(idx) * 100 / float64(m.n)), true
}

// cutUTF8 shortens s to at most n bytes without splitting a rune.
func cutUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
