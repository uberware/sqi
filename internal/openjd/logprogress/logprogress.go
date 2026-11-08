// SPDX-License-Identifier: AGPL-3.0-or-later

// Package logprogress recognizes task progress and status text in a task's
// output lines. A progress pattern declares what its match means through its
// named capture groups (a Form); a status pattern captures free text.
//
// It is a stdlib-only leaf shared by the template validator and the worker.
package logprogress

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Limits on a pattern list.
const (
	// MaxPatterns is the most patterns one list may hold.
	MaxPatterns = 32
	// MaxPatternBytes is the longest a single pattern may be.
	MaxPatternBytes = 1024
	// MaxStatusBytes is the longest status text kept; longer text is cut on a
	// UTF-8 boundary.
	MaxStatusBytes = 200
)

// Form is the meaning of a progress pattern's named groups.
type Form int

// The six progress forms.
const (
	// FormPercent: group percent, 0 to 100.
	FormPercent Form = iota + 1
	// FormCounts: groups done and total.
	FormCounts
	// FormFrame: group frame, the frame now being worked on.
	FormFrame
	// FormFrameDone: group framedone, a frame just completed.
	FormFrameDone
	// FormFramePercent: group framepercent, completion of the current frame.
	FormFramePercent
	// FormFrameCounts: groups framedone and frametotal, within the current frame.
	FormFrameCounts
)

// formSets lists the accepted group sets, in the order error text shows them.
var formSets = []struct {
	form   Form
	groups []string
}{
	{FormPercent, []string{"percent"}},
	{FormCounts, []string{"done", "total"}},
	{FormFrame, []string{"frame"}},
	{FormFrameDone, []string{"framedone"}},
	{FormFramePercent, []string{"framepercent"}},
	{FormFrameCounts, []string{"framedone", "frametotal"}},
}

// UsesFrames reports whether the form is measured against the task's frames.
func (f Form) UsesFrames() bool { return f != FormPercent && f != FormCounts }

// WithinFrame reports whether the form measures progress inside the current
// frame rather than naming a frame.
func (f Form) WithinFrame() bool { return f == FormFramePercent || f == FormFrameCounts }

// namedGroups returns the sorted distinct non-empty group names of re.
func namedGroups(re *regexp.Regexp) []string {
	seen := map[string]bool{}
	var out []string
	for _, n := range re.SubexpNames() {
		if n != "" && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// ClassifyProgress returns the Form a progress pattern's named groups
// declare. Unnamed groups are ignored.
func ClassifyProgress(re *regexp.Regexp) (Form, error) {
	got := namedGroups(re)
	for _, s := range formSets {
		want := append([]string(nil), s.groups...)
		sort.Strings(want)
		if strings.Join(want, ",") == strings.Join(got, ",") {
			return s.form, nil
		}
	}
	found := "none"
	if len(got) > 0 {
		found = strings.Join(got, ", ")
	}
	var accepted []string
	for _, s := range formSets {
		accepted = append(accepted, "("+strings.Join(s.groups, ", ")+")")
	}
	return 0, fmt.Errorf("named groups found: %s; a progress pattern must have exactly one of these sets: %s",
		found, strings.Join(accepted, ", "))
}

// CheckStatus rejects a status pattern carrying any named group other than
// status.
func CheckStatus(re *regexp.Regexp) error {
	for _, n := range namedGroups(re) {
		if n != "status" {
			return fmt.Errorf("a status pattern may only name the group %q, found %q", "status", n)
		}
	}
	return nil
}
