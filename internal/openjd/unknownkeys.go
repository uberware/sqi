// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import (
	"fmt"
	"sort"
	"strconv"
)

// unknownKey is a key of some template object that the object's schema does
// not define: Pointer locates the key, Object names the schema for the error.
type unknownKey struct {
	Pointer string
	Object  string
}

// keySchema is one template object's allowed keys. A nil child is a leaf key;
// a non-nil one says what the key's value holds, so the walk can descend. An
// empty object name marks a level whose own keys are checked elsewhere (the
// template's top level and a step's own keys), so only its children are walked.
type keySchema struct {
	object string
	keys   map[string]*keyChild
}

// keyChild is the schema of a key's value, and whether the value is a list of
// such objects rather than one.
type keyChild struct {
	schema *keySchema
	list   bool
}

// templateKeySchema is the root of the table: the job template, whose own
// keys unknownTopLevelFields checks.
var templateKeySchema = buildKeySchemas()

func leafKeys(names ...string) map[string]*keyChild {
	m := make(map[string]*keyChild, len(names))
	for _, n := range names {
		m[n] = nil
	}
	return m
}

func one(s *keySchema) *keyChild  { return &keyChild{schema: s} }
func many(s *keySchema) *keyChild { return &keyChild{schema: s, list: true} }

// buildKeySchemas builds the table from the 2023-09 Template Schemas (§2–§6),
// plus the keys the supported extensions add (let from EXPR, chunks from
// TASK_CHUNKING, endOfLine from FEATURE_BUNDLE_1, which sqi decodes). Whether
// those extensions are declared is checked elsewhere. A job parameter and its
// userInterface accept the union of every parameter type's keys: this check
// catches misspellings, not keys that do not apply to the declared type.
//
// Environment variables are data (names chosen by the author), so the walk
// never descends into them.
func buildKeySchemas() *keySchema {
	fileFilter := &keySchema{"file filter", leafKeys("label", "patterns")}
	userInterface := &keySchema{"user interface", leafKeys("control", "label", "groupLabel", "decimals", "singleStepDelta")}
	userInterface.keys["fileFilters"] = many(fileFilter)
	userInterface.keys["fileFilterDefault"] = one(fileFilter)

	itemValueKeys := []string{"allowedValues", "minLength", "maxLength", "minValue", "maxValue"}
	// The decoder rejects an item: nested deeper than list[list[T]], so the
	// inner level allows item without descending into it.
	innerItem := &keySchema{"item constraint", leafKeys(append(itemValueKeys, "item")...)}
	outerItem := &keySchema{"item constraint", leafKeys(itemValueKeys...)}
	outerItem.keys["item"] = one(innerItem)

	jobParam := &keySchema{"job parameter definition", leafKeys(
		"name", "type", "description", "default", "allowedValues", "minLength", "maxLength",
		"minValue", "maxValue", "objectType", "dataFlow",
	)}
	jobParam.keys["item"] = one(outerItem)
	jobParam.keys["userInterface"] = one(userInterface)

	cancelation := &keySchema{"cancelation method", leafKeys("mode", "notifyPeriodInSeconds")}
	action := &keySchema{"action", leafKeys("command", "args", "timeout")}
	action.keys["cancelation"] = one(cancelation)
	embeddedFile := &keySchema{"embedded file", leafKeys("name", "type", "filename", "runnable", "data", "endOfLine")}

	envActions := &keySchema{"environment actions", map[string]*keyChild{"onEnter": one(action), "onExit": one(action)}}
	envScript := &keySchema{"environment script", map[string]*keyChild{
		"let": nil, "embeddedFiles": many(embeddedFile), "actions": one(envActions),
	}}
	environment := &keySchema{"environment", leafKeys("name", "description", "variables")}
	environment.keys["script"] = one(envScript)

	stepActions := &keySchema{"step actions", map[string]*keyChild{"onRun": one(action)}}
	stepScript := &keySchema{"step script", map[string]*keyChild{
		"let": nil, "embeddedFiles": many(embeddedFile), "actions": one(stepActions),
	}}

	chunks := &keySchema{"chunks", leafKeys("defaultTaskCount", "targetRuntimeSeconds", "rangeConstraint")}
	taskParam := &keySchema{"task parameter definition", leafKeys("name", "type", "range")}
	taskParam.keys["chunks"] = one(chunks)
	paramSpace := &keySchema{"parameter space", leafKeys("combination")}
	paramSpace.keys["taskParameterDefinitions"] = many(taskParam)

	hostReqs := &keySchema{"host requirements", map[string]*keyChild{
		"amounts":    many(&keySchema{"amount requirement", leafKeys("name", "min", "max")}),
		"attributes": many(&keySchema{"attribute requirement", leafKeys("name", "anyOf", "allOf")}),
	}}
	dependency := &keySchema{"step dependency", leafKeys("dependsOn")}

	step := &keySchema{"", map[string]*keyChild{
		"script": one(stepScript), "stepEnvironments": many(environment), "parameterSpace": one(paramSpace),
		"hostRequirements": one(hostReqs), "dependencies": many(dependency),
	}}
	return &keySchema{"", map[string]*keyChild{
		"parameterDefinitions": many(jobParam), "jobEnvironments": many(environment), "steps": many(step),
	}}
}

// keyFrame is one mapping the walk has yet to visit: its value, the JSON
// pointer to it, and the schema it is checked against.
type keyFrame struct {
	v      any
	ptr    string
	schema *keySchema
}

// findUnknownKeys returns every key, below the template's top level and a
// step's own keys, that its object's schema does not define, sorted by
// pointer. The walk descends only into keys the table knows, so its depth is
// bounded by the schema, and it keeps an explicit stack rather than recursing.
// A step-block extension name (SQI_LOG_CHECKS) is skipped: wherever it is found
// below a step, findMisplacedStepBlocks reports it.
func findUnknownKeys(raw map[string]any) []unknownKey {
	var out []unknownKey
	stack := []keyFrame{{v: raw, schema: templateKeySchema}}
	for len(stack) > 0 {
		f := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		m, ok := toMapOK(f.v)
		if !ok {
			continue // a non-mapping is the decoder's error to report
		}
		for k, v := range m {
			ptr := f.ptr + "/" + escapePointerToken(k)
			child, known := f.schema.keys[k]
			switch {
			case known:
				stack = pushKeyChild(stack, child, v, ptr)
			case f.schema.object == "" || isStepBlockName(k):
			default:
				out = append(out, unknownKey{Pointer: ptr, Object: f.schema.object})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Pointer < out[j].Pointer })
	return out
}

// pushKeyChild queues v for a visit under c's schema: once, or per element of
// a list. A leaf (nil c) and a list value that is not a list queue nothing.
func pushKeyChild(stack []keyFrame, c *keyChild, v any, ptr string) []keyFrame {
	if c == nil {
		return stack
	}
	if !c.list {
		return append(stack, keyFrame{v: v, ptr: ptr, schema: c.schema})
	}
	items, _ := toAnySlice(v)
	for i, item := range items {
		stack = append(stack, keyFrame{v: item, ptr: ptr + "/" + strconv.Itoa(i), schema: c.schema})
	}
	return stack
}

// validateUnknownKeys reports each key findUnknownKeys found. The decoders
// read only the keys they know, so without this a misspelled key (onExt,
// timout) would be dropped and its setting silently lost.
func validateUnknownKeys(t *JobTemplate) ValidationErrors {
	errs := make(ValidationErrors, 0, len(t.unknownKeys))
	for _, k := range t.unknownKeys {
		errs = append(errs, ValidationError{
			Pointer: k.Pointer,
			Message: fmt.Sprintf("unknown field; the %s schema defines no such key", k.Object),
		})
	}
	return errs
}
