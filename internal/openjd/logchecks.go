// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Limits on an SQI_LOG_CHECKS block. They bound what a worker compiles and runs
// against every output line of a task, so they are fixed rather than configured.
const (
	// maxLogCheckPatterns is the most patterns one list (failOn or requireLine)
	// may hold.
	maxLogCheckPatterns = 32
	// maxLogCheckPatternBytes is the longest a single pattern may be.
	maxLogCheckPatternBytes = 1024
)

// LogChecks is the parsed SQI_LOG_CHECKS block of one step.
//
// Both lists are RE2 patterns (Go regexp), matched unanchored against each line
// the step's onRun action writes. A task fails when any line matches a FailOn
// pattern, or when no line matched a RequireLine pattern by the time the process
// exited. Patterns are literal text: they are not format strings.
type LogChecks struct {
	// FailOn fails the task on the first output line any pattern matches.
	FailOn []string
	// RequireLine fails the task unless some output line matches each pattern.
	RequireLine []string

	// problems are decode-time findings, reported by validation with a JSON
	// pointer instead of aborting the parse. Unexported: they are not part of
	// the block's meaning and must not reach a wire type.
	problems []logCheckProblem
}

// logCheckProblem is one decode-time finding. suffix is appended to the block's
// own JSON pointer ("" for the block itself, "/failOn" for a list, ...).
type logCheckProblem struct {
	suffix  string
	message string
}

// decodeLogChecks decodes a step's SQI_LOG_CHECKS value. It never returns an
// error: a malformed block is recorded in the result for validateLogChecks to
// report with a pointer. A null block decodes as an empty one, which validation
// then rejects for holding neither list.
func decodeLogChecks(v any) *LogChecks {
	lc := &LogChecks{}
	if v == nil {
		return lc
	}
	m, ok := toMapOK(v)
	if !ok {
		lc.problems = append(lc.problems, logCheckProblem{message: "must be a mapping with failOn and/or requireLine"})
		return lc
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		switch k {
		case "failOn":
			lc.FailOn = decodeLogCheckList(lc, m[k], "/failOn")
		case "requireLine":
			lc.RequireLine = decodeLogCheckList(lc, m[k], "/requireLine")
		default:
			lc.problems = append(lc.problems, logCheckProblem{message: fmt.Sprintf("unknown field %q", k)})
		}
	}
	return lc
}

// decodeLogCheckList reads one pattern list, recording a problem for a value
// that is not a list of strings. A null list is an empty one.
func decodeLogCheckList(lc *LogChecks, v any, suffix string) []string {
	if v == nil {
		return nil
	}
	items, ok := toAnySlice(v)
	if !ok {
		lc.problems = append(lc.problems, logCheckProblem{suffix: suffix, message: "must be a list of strings"})
		return nil
	}
	out := make([]string, 0, len(items))
	for i, item := range items {
		s, ok := item.(string)
		if !ok {
			lc.problems = append(lc.problems, logCheckProblem{
				suffix:  fmt.Sprintf("%s/%d", suffix, i),
				message: "must be a string",
			})
			continue
		}
		out = append(out, s)
	}
	return out
}

// sqiStepKeys returns the sorted SQI_-prefixed keys of a step's raw mapping.
func sqiStepKeys(raw map[string]any) []string {
	var out []string
	for k := range raw {
		if strings.HasPrefix(k, "SQI_") {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// validateLogChecks enforces the SQI_LOG_CHECKS extension/block coupling, the
// block's own shape, and rejects any SQI_ step key that names no registered
// step-block extension. Structural (not gated by EnforceLimits), like
// validatePathTranslation.
func validateLogChecks(t *JobTemplate) ValidationErrors {
	declared := t.hasExtension(ExtensionLogChecks)
	var errs ValidationErrors
	anyBlock := false
	for i, s := range t.Steps {
		errs = append(errs, validateSQIStepKeys(t, s, i)...)
		if s.LogChecks == nil || !declared {
			continue
		}
		anyBlock = true
		errs = append(errs, validateLogChecksBlock(s.LogChecks, fmt.Sprintf("/steps/%d/%s", i, ExtensionLogChecks))...)
	}
	if declared && !anyBlock {
		errs = append(errs, ValidationError{
			Pointer: "/extensions",
			Message: "the SQI_LOG_CHECKS extension is declared but no step carries an SQI_LOG_CHECKS block",
		})
	}
	return errs
}

// validateSQIStepKeys checks each SQI_-prefixed key of one step. A key is
// accepted only if it names a registered, supported step-block extension that
// the template declares; anything else is an error, because the decoder would
// otherwise drop a misspelled block and run the step with its checks off.
func validateSQIStepKeys(t *JobTemplate, s StepTemplate, idx int) ValidationErrors {
	var errs ValidationErrors
	for _, key := range s.SQIKeys {
		ptr := fmt.Sprintf("/steps/%d/%s", idx, key)
		ext, ok := LookupExtension(key)
		switch {
		case !ok || ext.Status != StatusSupported || !ext.StepBlock:
			errs = append(errs, ValidationError{
				Pointer: ptr,
				Message: fmt.Sprintf("unknown sqi step block %q; no supported extension defines it", key),
			})
		case !t.hasExtension(key):
			errs = append(errs, ValidationError{
				Pointer: ptr,
				Message: fmt.Sprintf("%s block requires declaring the %s extension in extensions", key, key),
			})
		}
	}
	return errs
}

// validateLogChecksBlock checks one decoded block. ptr is its JSON pointer.
func validateLogChecksBlock(lc *LogChecks, ptr string) ValidationErrors {
	var errs ValidationErrors
	for _, p := range lc.problems {
		errs = append(errs, ValidationError{Pointer: ptr + p.suffix, Message: p.message})
	}
	if len(lc.problems) == 0 && len(lc.FailOn) == 0 && len(lc.RequireLine) == 0 {
		errs = append(errs, ValidationError{
			Pointer: ptr,
			Message: "requires at least one of failOn or requireLine to hold a pattern",
		})
	}
	errs = append(errs, validateLogCheckPatterns(lc.FailOn, ptr+"/failOn")...)
	errs = append(errs, validateLogCheckPatterns(lc.RequireLine, ptr+"/requireLine")...)
	return errs
}

// validateLogCheckPatterns checks one pattern list against the count, length and
// RE2-syntax limits.
func validateLogCheckPatterns(patterns []string, ptr string) ValidationErrors {
	var errs ValidationErrors
	if len(patterns) > maxLogCheckPatterns {
		errs = append(errs, ValidationError{
			Pointer: ptr,
			Message: fmt.Sprintf("holds %d patterns; at most %d are allowed", len(patterns), maxLogCheckPatterns),
		})
	}
	for i, p := range patterns {
		pp := fmt.Sprintf("%s/%d", ptr, i)
		switch {
		case p == "":
			errs = append(errs, ValidationError{Pointer: pp, Message: "pattern must not be empty"})
		case len(p) > maxLogCheckPatternBytes:
			errs = append(errs, ValidationError{
				Pointer: pp,
				Message: fmt.Sprintf("pattern is %d bytes; at most %d bytes are allowed", len(p), maxLogCheckPatternBytes),
			})
		default:
			if _, err := regexp.Compile(p); err != nil {
				errs = append(errs, ValidationError{Pointer: pp, Message: "invalid regular expression: " + err.Error()})
			}
		}
	}
	return errs
}
