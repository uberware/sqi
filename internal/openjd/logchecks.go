// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import (
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/uberware/sqi/internal/openjd/logpattern"
)

// Limits on an SQI_LOG_CHECKS block, beyond the per-list ones in
// [logpattern.MaxPatterns] and [logpattern.MaxPatternBytes]. They bound what a
// worker compiles and runs against every output line of a task, so they are
// fixed rather than configured.
const (
	// maxLogCheckTemplateInsts bounds the programs all of a template's patterns
	// compile to, together. Validation builds each pattern's program to measure
	// it, so without a template-wide bound the cost of a submission grows with
	// its number of steps. It is one step's worst case (two full lists of
	// patterns at the size limit), so any step that is valid on its own is valid
	// in any template.
	maxLogCheckTemplateInsts = 2 * logpattern.MaxPatterns * logpattern.MaxProgramInsts
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
	for _, k := range slices.Sorted(maps.Keys(m)) {
		switch k {
		case "failOn":
			lc.FailOn = decodePatternList(&lc.problems, m[k], "/failOn")
		case "requireLine":
			lc.RequireLine = decodePatternList(&lc.problems, m[k], "/requireLine")
		default:
			lc.problems = append(lc.problems, logCheckProblem{message: fmt.Sprintf("unknown field %q", k)})
		}
	}
	return lc
}

// decodePatternList reads one pattern list of a step block (SQI_LOG_CHECKS,
// SQI_LOG_PROGRESS), appending to problems for a value that is not a list of
// strings. A null list is an empty one.
func decodePatternList(problems *[]logCheckProblem, v any, suffix string) []string {
	if v == nil {
		return nil
	}
	items, ok := toAnySlice(v)
	if !ok {
		*problems = append(*problems, logCheckProblem{suffix: suffix, message: "must be a list of strings"})
		return nil
	}
	out := make([]string, 0, len(items))
	for i, item := range items {
		s, ok := item.(string)
		if !ok {
			*problems = append(*problems, logCheckProblem{
				suffix:  fmt.Sprintf("%s/%d", suffix, i),
				message: "must be a string",
			})
			continue
		}
		out = append(out, s)
	}
	return out
}

// isStepBlockName reports whether name is a registered step-block extension.
func isStepBlockName(name string) bool {
	ext, ok := registry[name]
	return ok && ext.StepBlock
}

// escapePointerToken escapes one JSON pointer reference token (RFC 6901).
func escapePointerToken(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1")
}

// validateLogChecks enforces the SQI_LOG_CHECKS extension/block coupling and
// the block's own shape (a block found somewhere other than directly on a step
// is rejected by validateUnknownKeys). Structural (not gated by
// EnforceLimits), like validatePathTranslation.
func validateLogChecks(t *JobTemplate) ValidationErrors {
	declared := t.hasExtension(ExtensionLogChecks)
	var errs ValidationErrors
	anyBlock := false
	budget := maxLogCheckTemplateInsts
	for i, s := range t.Steps {
		if s.LogChecks == nil || !declared {
			continue
		}
		anyBlock = true
		errs = append(errs, validateLogChecksBlock(s.LogChecks, fmt.Sprintf("/steps/%d/%s", i, ExtensionLogChecks), &budget)...)
	}
	if declared && !anyBlock {
		errs = append(errs, ValidationError{
			Pointer: "/extensions",
			Message: "the SQI_LOG_CHECKS extension is declared but no step carries an SQI_LOG_CHECKS block",
		})
	}
	return errs
}

// validateLogChecksBlock checks one decoded block. ptr is its JSON pointer and
// budget the template's remaining instruction budget (see
// validateLogCheckPatterns).
func validateLogChecksBlock(lc *LogChecks, ptr string, budget *int) ValidationErrors {
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
	errs = append(errs, validateLogCheckPatterns(lc.FailOn, ptr+"/failOn", ExtensionLogChecks, budget)...)
	errs = append(errs, validateLogCheckPatterns(lc.RequireLine, ptr+"/requireLine", ExtensionLogChecks, budget)...)
	return errs
}

// validateLogCheckPatterns checks one pattern list against the count, length,
// RE2-syntax and compiled-program-size (logpattern.MaxProgramInsts) limits. The
// size limit is the one the worker enforces too, so a pattern accepted here is
// never refused at runtime.
//
// budget is what the template's patterns may still compile to, in
// instructions, shared by every list (maxLogCheckTemplateInsts). Each pattern
// that would be built is charged its estimated size before it is built; once
// the budget is spent, the error is reported once and no further pattern is
// built, so validation costs at most the budget however long the template.
// A negative budget means it has been spent and reported.
func validateLogCheckPatterns(patterns []string, ptr, ext string, budget *int) ValidationErrors {
	errs, _ := validateLogCheckPatternsCompiled(patterns, ptr, ext, budget)
	return errs
}

// validateLogCheckPatternsCompiled is [validateLogCheckPatterns] that also
// returns each pattern's compiled form, index-aligned with patterns. An entry is
// nil when the pattern was rejected or, once the budget is spent, not built.
// ext names the extension in the budget message.
func validateLogCheckPatternsCompiled(patterns []string, ptr, ext string, budget *int) (ValidationErrors, []*regexp.Regexp) {
	var errs ValidationErrors
	compiled := make([]*regexp.Regexp, len(patterns))
	if len(patterns) > logpattern.MaxPatterns {
		errs = append(errs, ValidationError{
			Pointer: ptr,
			Message: fmt.Sprintf("holds %d patterns; at most %d are allowed", len(patterns), logpattern.MaxPatterns),
		})
	}
	for i, p := range patterns {
		pp := fmt.Sprintf("%s/%d", ptr, i)
		switch {
		case p == "":
			errs = append(errs, ValidationError{Pointer: pp, Message: "pattern must not be empty"})
		case len(p) > logpattern.MaxPatternBytes:
			errs = append(errs, ValidationError{
				Pointer: pp,
				Message: fmt.Sprintf("pattern is %d bytes; at most %d bytes are allowed", len(p), logpattern.MaxPatternBytes),
			})
		default:
			re, e, ok := compileLogCheckPattern(p, pp, ext, budget)
			if !ok {
				errs = append(errs, e)
			}
			compiled[i] = re
		}
	}
	return errs, compiled
}

// compileLogCheckPattern checks one pattern against RE2 syntax and the size
// bound, charging budget (see validateLogCheckPatterns) for a pattern it
// builds. It returns the error to report, and false, when there is one.
func compileLogCheckPattern(p, ptr, ext string, budget *int) (*regexp.Regexp, ValidationError, bool) {
	if *budget < 0 {
		return nil, ValidationError{}, true
	}
	n, err := logpattern.Estimate(p)
	if err != nil {
		return nil, ValidationError{Pointer: ptr, Message: "invalid regular expression: " + err.Error()}, false
	}
	// A pattern over logpattern.EstimateCeiling is refused unbuilt, so only
	// the rest are charged.
	if n <= logpattern.EstimateCeiling {
		if n > *budget {
			*budget = -1
			return nil, ValidationError{
				Pointer: ptr,
				Message: fmt.Sprintf("pattern takes the template's %s patterns over %d compiled instructions in total, the most allowed; later patterns are not checked", ext, maxLogCheckTemplateInsts),
			}, false
		}
		*budget -= n
	}
	var tooLarge *logpattern.TooLargeError
	re, err := logpattern.Compile(p)
	switch {
	case errors.As(err, &tooLarge):
		return nil, ValidationError{Pointer: ptr, Message: "pattern " + tooLarge.Error()}, false
	case err != nil:
		return nil, ValidationError{Pointer: ptr, Message: "invalid regular expression: " + err.Error()}, false
	}
	return re, ValidationError{}, true
}
