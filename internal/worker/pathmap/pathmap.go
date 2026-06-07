// SPDX-License-Identifier: AGPL-3.0-only

// Package pathmap implements resolved-mode path substitution for sqi-worker.
//
// In resolved mode the server sends an ordered list of source→destination path
// rules alongside a task assignment. The worker uses those rules to:
//
//  1. Validate that every rule has a concrete destination path (task 62).
//  2. Apply string substitution to the task command and argument strings,
//     replacing every occurrence of a source path with its destination path
//     before launching the OS process (task 60).
//  3. Write the standard OpenJD path_mapping.json file into the session
//     working directory so that applications with native OpenJD path-mapping
//     support can translate internally referenced paths without additional
//     integration (task 61).
//
// # Lookup
//
// A [Lookup] wraps the ordered slice of [protocol.PathMapRule] values and
// exposes [Lookup.Apply] for string substitution and [Lookup.ApplyToAction]
// for substituting across a full [protocol.Action] (command + args).
//
// # Ordering
//
// Rules are applied in declaration order.  If a command string contains a
// source path that matches multiple rules the first matching rule wins for
// any given starting position — but because each rule is applied as a global
// string replacement in turn, the caller should ensure rule source paths are
// non-overlapping prefix/path pairs to avoid unexpected chained substitutions.
//
// # Empty path map
//
// An empty or nil PathMap is valid; [Parse] returns a non-nil, empty [Lookup]
// and nil error.  Calling [Lookup.Apply] or [Lookup.ApplyToAction] on an
// empty Lookup is a no-op that returns the input unchanged.
package pathmap

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/uberware/sqi/internal/worker/protocol"
)

// PathMappingFileName is the name of the OpenJD path mapping file written
// to each session working directory (task 61).
const PathMappingFileName = "path_mapping.json"

// ── Lookup ────────────────────────────────────────────────────────────────────

// Lookup is an ordered list of source-path-format → destination-path pairs
// derived from an [protocol.AssignMsg]'s PathMap field.
//
// Create a Lookup via [Parse].  The zero value is safe to use and behaves
// as an empty lookup (no substitutions).
type Lookup struct {
	rules []protocol.PathMapRule
}

// Parse validates and converts rules into a Lookup.
//
// It returns a non-nil error if any rule with a non-empty SourcePathFormat
// has an empty DestinationPath, identifying the offending source so operators
// can diagnose a misconfigured storage location (task 62).
//
// Rules where both SourcePathFormat and DestinationPath are empty are accepted
// silently — they carry no mapping information and [Lookup.Apply] skips them.
//
// An empty or nil rules slice is valid; Parse returns a non-nil empty Lookup
// and nil error.
func Parse(rules []protocol.PathMapRule) (*Lookup, error) {
	for _, r := range rules {
		// Only error if a named location (non-empty source) has no resolved path.
		// A rule with both fields empty is a no-op and is accepted without error.
		if r.SourcePathFormat != "" && r.DestinationPath == "" {
			return nil, fmt.Errorf(
				"pathmap: named location %q has an empty resolved path; "+
					"check that the storage location is correctly configured for the worker's compute location",
				r.SourcePathFormat,
			)
		}
	}
	// Copy the slice so the Lookup is independent of the caller's backing array.
	cp := make([]protocol.PathMapRule, len(rules))
	copy(cp, rules)
	return &Lookup{rules: cp}, nil
}

// Len returns the number of rules in the Lookup.
func (l *Lookup) Len() int {
	if l == nil {
		return 0
	}
	return len(l.rules)
}

// Apply performs resolved-mode path substitution on s (task 60).
//
// Each rule's SourcePathFormat is replaced with its DestinationPath, globally,
// in declaration order.  A source path that appears more than once in s is
// replaced every time.  Rules with an empty SourcePathFormat are skipped.
//
// If the Lookup is nil or empty, s is returned unchanged.
func (l *Lookup) Apply(s string) string {
	if l == nil || len(l.rules) == 0 {
		return s
	}
	for _, r := range l.rules {
		if r.SourcePathFormat == "" {
			// Skip rules with empty source; they cannot appear in a command
			// string and a global replace of "" would corrupt every character.
			continue
		}
		s = strings.ReplaceAll(s, r.SourcePathFormat, r.DestinationPath)
	}
	return s
}

// ApplyToAction returns a copy of action with [Apply] applied to
// action.Command and each element of action.Args (task 60).
//
// The original action is not modified.  If action is nil or the Lookup
// has no rules, action is returned unchanged.
func (l *Lookup) ApplyToAction(action *protocol.Action) *protocol.Action {
	if action == nil || l == nil || len(l.rules) == 0 {
		return action
	}
	// Shallow-copy the action so we can replace Command and Args without
	// touching the caller's fields (Cancelation, TimeoutSeconds, etc.).
	out := *action
	out.Command = l.Apply(action.Command)
	if len(action.Args) > 0 {
		out.Args = make([]string, len(action.Args))
		for i, arg := range action.Args {
			out.Args[i] = l.Apply(arg)
		}
	}
	return &out
}

// ── Path mapping file ─────────────────────────────────────────────────────────

// pathMappingEntry is the on-disk JSON representation of a single
// source→destination pair in the OpenJD path_mapping.json file.
// The field names match the OpenJD path mapping specification exactly.
type pathMappingEntry struct {
	SourcePathFormat string `json:"source_path_format"`
	DestinationPath  string `json:"destination_path"`
}

// WritePathMappingFile serializes rules as JSON and writes them to
// <workDir>/path_mapping.json (task 61).
//
// The file format is a JSON array of objects with "source_path_format" and
// "destination_path" keys, matching the OpenJD path mapping specification.
// Applications with native OpenJD support read this file to translate
// internally referenced paths without additional integration.
//
// If rules is empty, no file is written (an empty path map carries no
// mapping information and applications should treat a missing file as
// equivalent to an empty map).
//
// The file is written with mode 0644 (world-readable).  An existing file at
// the path is overwritten (open with O_TRUNC semantics via os.WriteFile).
func WritePathMappingFile(workDir string, rules []protocol.PathMapRule) error {
	if len(rules) == 0 {
		return nil
	}

	entries := make([]pathMappingEntry, len(rules))
	for i, r := range rules {
		entries[i] = pathMappingEntry{
			SourcePathFormat: r.SourcePathFormat,
			DestinationPath:  r.DestinationPath,
		}
	}

	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		// Unreachable in practice: entries contains only strings.
		return fmt.Errorf("pathmap: marshal %s: %w", PathMappingFileName, err)
	}

	path := filepath.Join(workDir, PathMappingFileName)
	// 0o644: world-readable so task processes running as a different OS user
	// than the worker can still read the file (common in hardened deployments).
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("pathmap: write %q: %w", path, err)
	}
	return nil
}
