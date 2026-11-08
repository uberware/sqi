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
	"slices"
	"sort"
	"strings"
)

// MaxStatusBytes is the longest status text kept; longer text is cut on a UTF-8
// boundary. The pattern-list limits are logpattern.MaxPatterns and
// logpattern.MaxPatternBytes.
const MaxStatusBytes = 200

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

// formKeys holds each form set's sorted group names joined with ",", the key
// ClassifyProgress compares a pattern's names against, index-aligned with
// formSets.
var formKeys = func() []string {
	keys := make([]string, len(formSets))
	for i, s := range formSets {
		keys[i] = strings.Join(slices.Sorted(slices.Values(s.groups)), ",")
	}
	return keys
}()

// UsesFrames reports whether the form is measured against the task's frames.
func (f Form) UsesFrames() bool { return f != FormPercent && f != FormCounts }

// WithinFrame reports whether the form measures progress inside the current
// frame rather than naming a frame.
func (f Form) WithinFrame() bool { return f == FormFramePercent || f == FormFrameCounts }

// namedGroups returns the sorted non-empty names among a pattern's group names
// (its SubexpNames, or a parse's CapNames). A name used twice is an error: RE2
// accepts it, but only one of the two submatches can be read, so a branch of
// the pattern would silently never report.
func namedGroups(names []string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, n := range names {
		if n == "" {
			continue
		}
		if seen[n] {
			return nil, fmt.Errorf("the named group %q appears more than once; each name may be used once", n)
		}
		seen[n] = true
		out = append(out, n)
	}
	sort.Strings(out)
	return out, nil
}

// ClassifyProgress returns the Form a progress pattern's named groups
// declare. Unnamed groups are ignored.
func ClassifyProgress(re *regexp.Regexp) (Form, error) {
	return ClassifyNames(re.SubexpNames())
}

// ClassifyNames is [ClassifyProgress] from a pattern's group names alone, as
// [regexp.Regexp.SubexpNames] or [regexp/syntax.Regexp.CapNames] return them,
// so a caller that needs only the form need not build the program.
func ClassifyNames(names []string) (Form, error) {
	got, err := namedGroups(names)
	if err != nil {
		return 0, err
	}
	if i := slices.Index(formKeys, strings.Join(got, ",")); i >= 0 {
		return formSets[i].form, nil
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
	groups, err := namedGroups(re.SubexpNames())
	if err != nil {
		return err
	}
	for _, n := range groups {
		if n != "status" {
			return fmt.Errorf("a status pattern may only name the group %q, found %q", "status", n)
		}
	}
	return nil
}
