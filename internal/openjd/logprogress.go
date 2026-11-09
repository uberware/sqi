// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import (
	"fmt"
	"maps"
	"regexp"
	"regexp/syntax"
	"slices"
	"strings"

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
			lp.Progress = decodePatternList(&lp.problems, m[k], "/progress")
		case "status":
			lp.Status = decodePatternList(&lp.problems, m[k], "/status")
		default:
			lp.problems = append(lp.problems, logCheckProblem{message: fmt.Sprintf("unknown field %q", k)})
		}
	}
	return lp
}

// isFrameTaskParam reports whether a task parameter can carry frame numbers.
func isFrameTaskParam(d TaskParamDefinition) bool {
	return d.Type == TaskParamTypeInt || d.Type == TaskParamTypeChunkInt
}

// taskParamDefs returns the step's task parameter definitions.
func (s *StepTemplate) taskParamDefs() []TaskParamDefinition {
	if s.ParameterSpace == nil {
		return nil
	}
	return s.ParameterSpace.TaskParameterDefinitions
}

// frameTaskParams returns the names of the step's INT and CHUNK[INT] task
// parameters, in declaration order.
func (s *StepTemplate) frameTaskParams() []string {
	var out []string
	for _, d := range s.taskParamDefs() {
		if isFrameTaskParam(d) {
			out = append(out, d.Name)
		}
	}
	return out
}

// resolveFrameParameter applies the frame-parameter rule to the step's block:
// the named frameParameter, which must be an INT or CHUNK[INT] task parameter
// of the step, or, when omitted, the step's only such parameter. When the rule
// cannot be met it returns the error to report, at ptr (the block's pointer).
func (s *StepTemplate) resolveFrameParameter(ptr string) (string, *ValidationError) {
	if name := s.LogProgress.FrameParameter; name != "" {
		for _, d := range s.taskParamDefs() {
			if d.Name != name {
				continue
			}
			if isFrameTaskParam(d) {
				return name, nil
			}
			return "", &ValidationError{
				Pointer: ptr + "/frameParameter",
				Message: fmt.Sprintf("task parameter %q is %s; frameParameter must name an INT or CHUNK[INT] task parameter", d.Name, d.Type),
			}
		}
		return "", &ValidationError{
			Pointer: ptr + "/frameParameter",
			Message: fmt.Sprintf("names no task parameter of this step: %q", name),
		}
	}
	switch c := s.frameTaskParams(); len(c) {
	case 1:
		return c[0], nil
	case 0:
		return "", &ValidationError{
			Pointer: ptr,
			Message: "a frame progress pattern needs an INT or CHUNK[INT] task parameter to measure against, and this step has none",
		}
	default:
		return "", &ValidationError{
			Pointer: ptr,
			Message: fmt.Sprintf("the step has several INT or CHUNK[INT] task parameters (%s); set frameParameter to name the frame parameter", strings.Join(c, ", ")),
		}
	}
}

// usesFrames reports whether any of forms is measured against the task's
// frames. A zero entry (an unclassified pattern) does not count.
func usesFrames(forms []logprogress.Form) bool {
	return slices.ContainsFunc(forms, func(f logprogress.Form) bool { return f != 0 && f.UsesFrames() })
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
	// Only the group names are needed, so each pattern is parsed but no program
	// is built: this runs on every task assignment.
	forms := make([]logprogress.Form, len(lp.Progress))
	for i, p := range lp.Progress {
		re, err := syntax.Parse(p, syntax.Perl)
		if err != nil {
			continue
		}
		if f, err := logprogress.ClassifyNames(re.CapNames()); err == nil {
			forms[i] = f
		}
	}
	if !usesFrames(forms) {
		return ""
	}
	if name, verr := s.resolveFrameParameter(""); verr == nil {
		return name
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
	forms, formErrs := validateProgressForms(progRes, ptr)
	errs = append(errs, formErrs...)
	// A named frameParameter is checked whether or not a pattern uses frames.
	if s.LogProgress.FrameParameter != "" || usesFrames(forms) {
		if _, e := s.resolveFrameParameter(ptr); e != nil {
			errs = append(errs, *e)
		}
	}
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

// validateProgressForms classifies each compiled progress pattern (see
// classifyProgressPatterns) and reports invalid group sets, and a within-frame
// pattern in a block with no pattern that names the frame it belongs to.
func validateProgressForms(res []*regexp.Regexp, ptr string) ([]logprogress.Form, ValidationErrors) {
	forms, errs := classifyProgressPatterns(res, ptr)
	namesFrame := false
	for _, f := range forms {
		if f == logprogress.FormFrame || f == logprogress.FormFrameDone {
			namesFrame = true
		}
	}
	if namesFrame {
		return forms, errs
	}
	for i, f := range forms {
		if f != 0 && f.WithinFrame() {
			errs = append(errs, ValidationError{
				Pointer: fmt.Sprintf("%s/progress/%d", ptr, i),
				Message: "a within-frame pattern (framepercent, or framedone with frametotal) needs another progress pattern in the block that names the frame, with the group frame or framedone",
			})
		}
	}
	return forms, errs
}
