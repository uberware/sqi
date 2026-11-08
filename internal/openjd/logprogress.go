// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/uberware/sqi/internal/openjd/logpattern"
	"github.com/uberware/sqi/internal/openjd/logprogress"
)

// LogProgress is the parsed SQI_LOG_PROGRESS block of one step.
//
// Progress and Status are RE2 patterns (Go regexp), matched unanchored against
// each line the step's onRun action writes. A progress pattern's named groups
// say what the line reports (see package logprogress); a status pattern's
// optional group "status" carries free text. Patterns are literal text: they are
// not format strings.
type LogProgress struct {
	// FrameParameter names the INT or CHUNK[INT] task parameter the frame forms
	// are measured against. Empty means the step's only such parameter.
	FrameParameter string
	// Progress holds the progress patterns.
	Progress []string
	// Status holds the status-text patterns.
	Status []string

	// problems are decode-time findings, reported by validation with a JSON
	// pointer instead of aborting the parse.
	problems []logCheckProblem
}

// decodeLogProgress decodes a step's SQI_LOG_PROGRESS value. It never returns an
// error: a malformed block is recorded in the result for validateLogProgress to
// report with a pointer. A null block decodes as an empty one, which validation
// rejects for holding neither list.
func decodeLogProgress(v any) *LogProgress {
	lp := &LogProgress{}
	if v == nil {
		return lp
	}
	m, ok := toMapOK(v)
	if !ok {
		lp.problems = append(lp.problems, logCheckProblem{message: "must be a mapping with progress and/or status"})
		return lp
	}
	for _, k := range slices.Sorted(maps.Keys(m)) {
		switch k {
		case "frameParameter":
			if m[k] == nil {
				continue
			}
			s, ok := m[k].(string)
			if !ok {
				lp.problems = append(lp.problems, logCheckProblem{suffix: "/frameParameter", message: "must be a string"})
				continue
			}
			lp.FrameParameter = s
		case "progress":
			lp.Progress = decodeProgressList(lp, m[k], "/progress")
		case "status":
			lp.Status = decodeProgressList(lp, m[k], "/status")
		default:
			lp.problems = append(lp.problems, logCheckProblem{message: fmt.Sprintf("unknown field %q", k)})
		}
	}
	return lp
}

// decodeProgressList reads one pattern list, recording a problem for a value
// that is not a list of strings. A null list is an empty one.
func decodeProgressList(lp *LogProgress, v any, suffix string) []string {
	if v == nil {
		return nil
	}
	items, ok := toAnySlice(v)
	if !ok {
		lp.problems = append(lp.problems, logCheckProblem{suffix: suffix, message: "must be a list of strings"})
		return nil
	}
	out := make([]string, 0, len(items))
	for i, item := range items {
		s, ok := item.(string)
		if !ok {
			lp.problems = append(lp.problems, logCheckProblem{
				suffix:  fmt.Sprintf("%s/%d", suffix, i),
				message: "must be a string",
			})
			continue
		}
		out = append(out, s)
	}
	return out
}

// isFrameTaskParam reports whether a task parameter can carry frame numbers.
func isFrameTaskParam(d TaskParamDefinition) bool {
	return d.Type == TaskParamTypeInt || d.Type == TaskParamTypeChunkInt
}

// frameTaskParams returns the names of the step's INT and CHUNK[INT] task
// parameters, in declaration order.
func (s *StepTemplate) frameTaskParams() []string {
	if s.ParameterSpace == nil {
		return nil
	}
	var out []string
	for _, d := range s.ParameterSpace.TaskParameterDefinitions {
		if isFrameTaskParam(d) {
			out = append(out, d.Name)
		}
	}
	return out
}

// LogProgressFrameParameter returns the task parameter the step's
// SQI_LOG_PROGRESS frame forms are measured against: the named frameParameter
// or, when omitted, the step's only INT or CHUNK[INT] task parameter. It
// returns "" when the step has no block, no progress pattern uses frames, or the
// name cannot be resolved. It does not validate; call it on a template
// that has been (or is being re-parsed from one that was).
func (s *StepTemplate) LogProgressFrameParameter() string {
	lp := s.LogProgress
	if lp == nil {
		return ""
	}
	usesFrames := false
	for _, p := range lp.Progress {
		re, err := logpattern.Compile(p)
		if err != nil {
			continue
		}
		if form, err := logprogress.ClassifyProgress(re); err == nil && form.UsesFrames() {
			usesFrames = true
			break
		}
	}
	if !usesFrames {
		return ""
	}
	candidates := s.frameTaskParams()
	if lp.FrameParameter != "" {
		if slices.Contains(candidates, lp.FrameParameter) {
			return lp.FrameParameter
		}
		return ""
	}
	if len(candidates) == 1 {
		return candidates[0]
	}
	return ""
}

// validateLogProgress enforces the SQI_LOG_PROGRESS extension/block coupling and
// each block's own shape. Misplaced blocks are reported once, by
// validateLogChecks. Structural (not gated by EnforceLimits), like
// validateLogChecks.
func validateLogProgress(t *JobTemplate) ValidationErrors {
	declared := t.hasExtension(ExtensionLogProgress)
	var errs ValidationErrors
	anyBlock := false
	// Separate from SQI_LOG_CHECKS's budget: a step carrying both full blocks is
	// valid.
	budget := maxLogCheckTemplateInsts
	for i := range t.Steps {
		s := &t.Steps[i]
		if s.LogProgress == nil || !declared {
			continue
		}
		anyBlock = true
		errs = append(errs, validateLogProgressBlock(s, fmt.Sprintf("/steps/%d/%s", i, ExtensionLogProgress), &budget)...)
	}
	if declared && !anyBlock {
		errs = append(errs, ValidationError{
			Pointer: "/extensions",
			Message: "the SQI_LOG_PROGRESS extension is declared but no step carries an SQI_LOG_PROGRESS block",
		})
	}
	return errs
}

// validateLogProgressBlock checks one step's decoded block. ptr is its JSON
// pointer and budget the template's remaining instruction budget.
func validateLogProgressBlock(s *StepTemplate, ptr string, budget *int) ValidationErrors {
	lp := s.LogProgress
	var errs ValidationErrors
	for _, p := range lp.problems {
		errs = append(errs, ValidationError{Pointer: ptr + p.suffix, Message: p.message})
	}
	if len(lp.problems) == 0 && len(lp.Progress) == 0 && len(lp.Status) == 0 {
		errs = append(errs, ValidationError{
			Pointer: ptr,
			Message: "requires at least one of progress or status to hold a pattern",
		})
	}
	progErrs, progRes := validateLogCheckPatternsCompiled(lp.Progress, ptr+"/progress", ExtensionLogProgress, budget)
	statErrs, statRes := validateLogCheckPatternsCompiled(lp.Status, ptr+"/status", ExtensionLogProgress, budget)
	errs = append(errs, progErrs...)
	errs = append(errs, statErrs...)

	for i, re := range statRes {
		if re == nil {
			continue
		}
		if err := logprogress.CheckStatus(re); err != nil {
			errs = append(errs, ValidationError{Pointer: fmt.Sprintf("%s/status/%d", ptr, i), Message: err.Error()})
		}
	}
	errs = append(errs, validateProgressForms(progRes, ptr)...)
	errs = append(errs, validateFrameParameter(s, progRes, ptr)...)
	return errs
}

// classifyProgressPatterns classifies each compiled progress pattern. forms[i]
// is zero for a nil pattern or one with an invalid group set (reported in errs).
func classifyProgressPatterns(res []*regexp.Regexp, ptr string) (forms []logprogress.Form, errs ValidationErrors) {
	forms = make([]logprogress.Form, len(res))
	for i, re := range res {
		if re == nil {
			continue
		}
		f, err := logprogress.ClassifyProgress(re)
		if err != nil {
			errs = append(errs, ValidationError{Pointer: fmt.Sprintf("%s/progress/%d", ptr, i), Message: err.Error()})
			continue
		}
		forms[i] = f
	}
	return forms, errs
}

// validateProgressForms reports invalid group sets, and a within-frame pattern
// in a block with no pattern that names the frame it belongs to.
func validateProgressForms(res []*regexp.Regexp, ptr string) ValidationErrors {
	forms, errs := classifyProgressPatterns(res, ptr)
	namesFrame := false
	for _, f := range forms {
		if f == logprogress.FormFrame || f == logprogress.FormFrameDone {
			namesFrame = true
		}
	}
	if namesFrame {
		return errs
	}
	for i, f := range forms {
		if f != 0 && f.WithinFrame() {
			errs = append(errs, ValidationError{
				Pointer: fmt.Sprintf("%s/progress/%d", ptr, i),
				Message: "a within-frame pattern (framepercent, or framedone with frametotal) needs another progress pattern in the block that names the frame, with the group frame or framedone",
			})
		}
	}
	return errs
}

// validateFrameParameter checks that the frame forms in use can be measured
// against exactly one INT or CHUNK[INT] task parameter of the step, and that a
// named frameParameter is such a parameter.
func validateFrameParameter(s *StepTemplate, res []*regexp.Regexp, ptr string) ValidationErrors {
	lp := s.LogProgress
	if lp.FrameParameter != "" {
		for _, d := range paramDefs(s) {
			if d.Name != lp.FrameParameter {
				continue
			}
			if isFrameTaskParam(d) {
				return nil
			}
			return ValidationErrors{{
				Pointer: ptr + "/frameParameter",
				Message: fmt.Sprintf("task parameter %q is %s; frameParameter must name an INT or CHUNK[INT] task parameter", d.Name, d.Type),
			}}
		}
		return ValidationErrors{{
			Pointer: ptr + "/frameParameter",
			Message: fmt.Sprintf("names no task parameter of this step: %q", lp.FrameParameter),
		}}
	}
	usesFrames := false
	for _, re := range res {
		if re == nil {
			continue
		}
		if f, err := logprogress.ClassifyProgress(re); err == nil && f.UsesFrames() {
			usesFrames = true
			break
		}
	}
	if !usesFrames {
		return nil
	}
	switch c := s.frameTaskParams(); len(c) {
	case 1:
		return nil
	case 0:
		return ValidationErrors{{
			Pointer: ptr,
			Message: "a frame progress pattern needs an INT or CHUNK[INT] task parameter to measure against, and this step has none",
		}}
	default:
		return ValidationErrors{{
			Pointer: ptr,
			Message: fmt.Sprintf("the step has several INT or CHUNK[INT] task parameters (%s); set frameParameter to name the frame parameter", strings.Join(c, ", ")),
		}}
	}
}

// paramDefs returns the step's task parameter definitions.
func paramDefs(s *StepTemplate) []TaskParamDefinition {
	if s.ParameterSpace == nil {
		return nil
	}
	return s.ParameterSpace.TaskParameterDefinitions
}
