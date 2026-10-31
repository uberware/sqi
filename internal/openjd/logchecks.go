// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/uberware/sqi/internal/openjd/logpattern"
)

// Limits on an SQI_LOG_CHECKS block. They bound what a worker compiles and runs
// against every output line of a task, so they are fixed rather than configured.
const (
	// maxLogCheckPatterns is the most patterns one list (failOn or requireLine)
	// may hold.
	maxLogCheckPatterns = 32
	// maxLogCheckPatternBytes is the longest a single pattern may be.
	maxLogCheckPatternBytes = 1024
	// maxLogCheckTemplateInsts bounds the programs all of a template's patterns
	// compile to, together. Validation builds each pattern's program to measure
	// it, so without a template-wide bound the cost of a submission grows with
	// its number of steps. It is one step's worst case (two full lists of
	// patterns at the size limit), so any step that is valid on its own is valid
	// in any template.
	maxLogCheckTemplateInsts = 2 * maxLogCheckPatterns * logpattern.MaxProgramInsts
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

// maxMisplacedStepBlocks is the most misplaced step blocks one template reports.
// One is enough to reject the template; the cap keeps a template stuffed with
// them from costing a pointer per copy.
const maxMisplacedStepBlocks = 16

// stepBlockFrameRole says how the walk in findMisplacedStepBlocks treats the
// keys of one container.
type stepBlockFrameRole int

const (
	// roleAny: a step-block key here is misplaced.
	roleAny stepBlockFrameRole = iota
	// roleStepList: the steps list, whose elements are steps.
	roleStepList
	// roleStep: a step, where a step-block key is in its proper slot.
	roleStep
	// roleVariables: an environment's variables, whose keys are variable names
	// the author chooses, so a variable named SQI_LOG_CHECKS is not a block.
	roleVariables
)

// stepBlockFrame is one container the walk visits. parent indexes the frame it
// was found in (-1 for a root), and key or idx names it there.
type stepBlockFrame struct {
	v      any
	parent int
	key    string
	idx    int
	isIdx  bool
	role   stepBlockFrameRole
}

// findMisplacedStepBlocks returns the JSON pointers of step-block extension
// keys (SQI_LOG_CHECKS) found anywhere in the template's parameter definitions,
// job environments or steps other than directly on a step: under script,
// actions.onRun, an environment, and so on. The decoder reads such a block only
// from its step-level slot, so one anywhere else would be dropped silently and
// the step would run with its checks off. A top-level key is reported
// separately (unknownTopLevelMessage).
//
// The walk is iterative (no recursion, whatever the nesting depth) and visits
// each decoded container once, so its cost is bounded by what the decoder
// already built. Pointers are built only for the hits, at most
// maxMisplacedStepBlocks of them (which ones, past the cap, depends on map
// order; any one rejects the template), and returned sorted.
func findMisplacedStepBlocks(raw map[string]any) []string {
	w := &stepBlockWalker{}
	for _, k := range []string{"parameterDefinitions", "jobEnvironments", "steps"} {
		role := roleAny
		if k == "steps" {
			role = roleStepList
		}
		w.push(stepBlockFrame{v: raw[k], parent: -1, key: k, role: role})
	}
	for len(w.pending) > 0 {
		fi := w.pending[len(w.pending)-1]
		w.pending = w.pending[:len(w.pending)-1]
		w.visit(fi)
	}
	out := make([]string, 0, len(w.hits))
	for _, h := range w.hits {
		out = append(out, stepBlockFramePointer(w.frames, h.frame)+"/"+escapePointerToken(h.key))
	}
	sort.Strings(out)
	return out
}

// stepBlockWalker is findMisplacedStepBlocks's state. frames holds every
// container found (for parent links); pending indexes those not yet visited.
type stepBlockWalker struct {
	frames  []stepBlockFrame
	pending []int
	hits    []stepBlockHit
}

// stepBlockHit is a misplaced key: key, in the container frames[frame].
type stepBlockHit struct {
	frame int
	key   string
}

// push records f for a visit if its value is a container; scalars hold no keys.
func (w *stepBlockWalker) push(f stepBlockFrame) {
	switch f.v.(type) {
	case map[string]any, map[any]any, []any:
		w.pending = append(w.pending, len(w.frames))
		w.frames = append(w.frames, f)
	}
}

// visit checks the keys of frames[fi] and queues its child containers.
func (w *stepBlockWalker) visit(fi int) {
	switch v := w.frames[fi].v.(type) {
	case map[string]any:
		for k, val := range v {
			w.visitKey(fi, k, val)
		}
	case map[any]any:
		for k, val := range v {
			w.visitKey(fi, fmt.Sprint(k), val)
		}
	case []any:
		role := roleAny
		if w.frames[fi].role == roleStepList {
			role = roleStep
		}
		for i, val := range v {
			w.push(stepBlockFrame{v: val, parent: fi, idx: i, isIdx: true, role: role})
		}
	}
}

func (w *stepBlockWalker) visitKey(fi int, k string, val any) {
	if isStepBlockName(k) {
		switch w.frames[fi].role {
		case roleStep:
			return // its proper slot; validated on its own
		case roleVariables:
			// a variable name, not a block
		default:
			if len(w.hits) < maxMisplacedStepBlocks {
				w.hits = append(w.hits, stepBlockHit{frame: fi, key: k})
			}
		}
	}
	role := roleAny
	if k == "variables" {
		role = roleVariables
	}
	w.push(stepBlockFrame{v: val, parent: fi, key: k, role: role})
}

// isStepBlockName reports whether name is a registered step-block extension.
func isStepBlockName(name string) bool {
	ext, ok := registry[name]
	return ok && ext.StepBlock
}

// stepBlockFramePointer builds the JSON pointer of frames[fi].
func stepBlockFramePointer(frames []stepBlockFrame, fi int) string {
	var segs []string
	for ; fi >= 0; fi = frames[fi].parent {
		f := frames[fi]
		if f.isIdx {
			segs = append(segs, strconv.Itoa(f.idx))
		} else {
			segs = append(segs, escapePointerToken(f.key))
		}
	}
	slices.Reverse(segs)
	return "/" + strings.Join(segs, "/")
}

// escapePointerToken escapes one JSON pointer reference token (RFC 6901).
func escapePointerToken(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1")
}

// validateLogChecks enforces the SQI_LOG_CHECKS extension/block coupling, the
// block's own shape, and rejects any SQI_ step key that names no registered
// step-block extension, and any step block found somewhere other than directly
// on a step. Structural (not gated by EnforceLimits), like
// validatePathTranslation.
func validateLogChecks(t *JobTemplate) ValidationErrors {
	declared := t.hasExtension(ExtensionLogChecks)
	var errs ValidationErrors
	for _, ptr := range t.misplacedStepBlocks {
		name := ptr[strings.LastIndexByte(ptr, '/')+1:]
		errs = append(errs, ValidationError{
			Pointer: ptr,
			Message: name + " is a step-level block and is ignored here; put it directly on the step, as a sibling of script",
		})
	}
	anyBlock := false
	budget := maxLogCheckTemplateInsts
	for i, s := range t.Steps {
		errs = append(errs, validateSQIStepKeys(t, s, i)...)
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
	errs = append(errs, validateLogCheckPatterns(lc.FailOn, ptr+"/failOn", budget)...)
	errs = append(errs, validateLogCheckPatterns(lc.RequireLine, ptr+"/requireLine", budget)...)
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
func validateLogCheckPatterns(patterns []string, ptr string, budget *int) ValidationErrors {
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
			if e, ok := compileLogCheckPattern(p, pp, budget); !ok {
				errs = append(errs, e)
			}
		}
	}
	return errs
}

// compileLogCheckPattern checks one pattern against RE2 syntax and the size
// bound, charging budget (see validateLogCheckPatterns) for a pattern it
// builds. It returns the error to report, and false, when there is one.
func compileLogCheckPattern(p, ptr string, budget *int) (ValidationError, bool) {
	if *budget < 0 {
		return ValidationError{}, true
	}
	n, err := logpattern.Estimate(p)
	if err != nil {
		return ValidationError{Pointer: ptr, Message: "invalid regular expression: " + err.Error()}, false
	}
	// A pattern over logpattern.EstimateCeiling is refused unbuilt, so only
	// the rest are charged.
	if n <= logpattern.EstimateCeiling {
		if n > *budget {
			*budget = -1
			return ValidationError{
				Pointer: ptr,
				Message: fmt.Sprintf("pattern takes the template's SQI_LOG_CHECKS patterns over %d compiled instructions in total, the most allowed; later patterns are not checked", maxLogCheckTemplateInsts),
			}, false
		}
		*budget -= n
	}
	var tooLarge *logpattern.TooLargeError
	_, err = logpattern.Compile(p)
	switch {
	case errors.As(err, &tooLarge):
		return ValidationError{Pointer: ptr, Message: "pattern " + tooLarge.Error()}, false
	case err != nil:
		return ValidationError{Pointer: ptr, Message: "invalid regular expression: " + err.Error()}, false
	}
	return ValidationError{}, true
}
