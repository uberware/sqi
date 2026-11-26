// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import (
	"sort"
	"strconv"
	"strings"
)

// unknownKey is a key of some template object that the object's schema does
// not define: Pointer locates the key, Object names the schema for the error.
// StepBlock marks a step-block extension key (SQI_LOG_CHECKS,
// SQI_LOG_PROGRESS) found below a step rather than directly on it, which has
// an error of its own.
type unknownKey struct {
	Pointer   string
	Object    string
	StepBlock bool
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
// such objects rather than one. With dataKeys, each object (or list element)
// is instead a mapping whose keys are data, such as a path delivery's name,
// and each of its values is checked against schema.
type keyChild struct {
	schema   *keySchema
	list     bool
	dataKeys bool
}

// templateKeySchema is the root of the table: the job template. It and a step
// list every key they define, but their own keys are checked by
// unknownTopLevelFields and unknownStepFields, which also accept extension
// blocks there.
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
// TASK_CHUNKING), whose declaration is checked elsewhere. endOfLine is
// FEATURE_BUNDLE_1's, which sqi does not support, but sqi decodes and applies
// it, so it is accepted without the declaration. A job parameter and its
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

	step := &keySchema{"", leafKeys("name", "description", "let")}
	step.keys["script"] = one(stepScript)
	step.keys["stepEnvironments"] = many(environment)
	step.keys["parameterSpace"] = one(paramSpace)
	step.keys["hostRequirements"] = one(hostReqs)
	step.keys["dependencies"] = many(dependency)

	// $schema is permitted so editors can attach a schema URL; the spec
	// ignores it (conformance fixtures 1.1--schema-field{,-ignored}.yaml).
	root := &keySchema{"", leafKeys("specificationVersion", "name", "description", "extensions", "$schema")}
	root.keys["parameterDefinitions"] = many(jobParam)
	root.keys["jobEnvironments"] = many(environment)
	root.keys["steps"] = many(step)
	return root
}

// stepKeySchema is a step template's keys, which unknownStepFields checks.
var stepKeySchema = templateKeySchema.keys["steps"].schema

// unknownFieldMessage is the error for a key the named object's schema does
// not define.
func unknownFieldMessage(object string) string {
	return "unknown field; the " + object + " schema defines no such key"
}

// extensionBlockKeySchemas are the top-level extension blocks whose keys the
// walk checks, by block name. Whether the block may appear at all is checked
// elsewhere (validatePathTranslation).
var extensionBlockKeySchemas = map[string]*keyChild{
	"SQI_PATH_TRANSLATION": one(&keySchema{"SQI_PATH_TRANSLATION block", map[string]*keyChild{
		// Each delivery is a bare name or a {name: settings} mapping.
		"deliveries": {
			schema:   &keySchema{"path delivery settings", leafKeys("pattern", "variable")},
			list:     true,
			dataKeys: true,
		},
	}}),
}

// maxUnknownKeys is the most unknown keys (misplaced step blocks included) one
// template reports, the first by pointer. One is enough to reject the
// template; the cap keeps a template stuffed with them from returning an error
// per copy.
const maxUnknownKeys = 16

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
//
// The supported top-level extension blocks (extensionBlockKeySchemas) are
// walked too.
//
// A step-block extension key (SQI_LOG_CHECKS, SQI_LOG_PROGRESS) found in any
// object other than a
// step (under script, actions.onRun, an environment, and so on) is marked
// StepBlock: the decoder reads the block only from its step-level slot, so the
// step would otherwise run with its checks off. One under an unknown key is
// not reached, and that key is reported instead.
func findUnknownKeys(raw map[string]any) []unknownKey {
	var out unknownKeyTop
	stack := []keyFrame{{v: raw, schema: templateKeySchema}}
	for name, c := range extensionBlockKeySchemas {
		if v, ok := raw[name]; ok {
			stack = pushKeyChild(stack, c, v, "/"+name)
		}
	}
	for len(stack) > 0 {
		f := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		m, ok := toMapOK(f.v)
		if !ok {
			continue // a non-mapping is the decoder's error to report
		}
		for k, v := range m {
			child, known := f.schema.keys[k]
			switch {
			case known:
				if child != nil {
					stack = pushKeyChild(stack, child, v, f.ptr+"/"+escapePointerToken(k))
				}
			case f.schema.object == "":
			case isStepBlockName(k):
				out.add(f.ptr, escapePointerToken(k), unknownKey{StepBlock: true})
			default:
				out.add(f.ptr, escapePointerToken(k), unknownKey{Object: f.schema.object})
			}
		}
	}
	return out
}

// unknownKeyTop holds the first maxUnknownKeys unknown keys by pointer, in
// order, so a template stuffed with unknown keys costs no more than sixteen.
type unknownKeyTop []unknownKey

// add records k, at parent/token, unless sixteen earlier keys are held.
func (top *unknownKeyTop) add(parent, token string, k unknownKey) {
	s := *top
	if len(s) == maxUnknownKeys && !joinedLess(parent, token, s[len(s)-1].Pointer) {
		return // checked before building the pointer, which most keys never need
	}
	k.Pointer = parent + "/" + token
	i := sort.Search(len(s), func(i int) bool { return k.Pointer < s[i].Pointer })
	if len(s) < maxUnknownKeys {
		s = append(s, unknownKey{})
	}
	copy(s[i+1:], s[i:len(s)-1])
	s[i] = k
	*top = s
}

// joinedLess reports whether parent+"/"+token sorts before s, without
// building the joined string.
func joinedLess(parent, token, s string) bool {
	for _, part := range [...]string{parent, "/", token} {
		n := min(len(part), len(s))
		if part[:n] != s[:n] {
			return part[:n] < s[:n]
		}
		if len(part) > len(s) {
			return false // s is a proper prefix of the joined string
		}
		s = s[n:]
	}
	return s != ""
}

// pushKeyChild queues v for a visit under c's schema: once, or per element of
// a list. A list value that is not a list queues nothing.
func pushKeyChild(stack []keyFrame, c *keyChild, v any, ptr string) []keyFrame {
	if !c.list {
		return pushKeyValue(stack, c, v, ptr)
	}
	items, _ := toAnySlice(v)
	for i, item := range items {
		stack = pushKeyValue(stack, c, item, ptr+"/"+strconv.Itoa(i))
	}
	return stack
}

// pushKeyValue queues one value under c's schema: itself, or with dataKeys
// each of its entries' values. A dataKeys value that is not a mapping (a bare
// delivery name) queues nothing.
func pushKeyValue(stack []keyFrame, c *keyChild, v any, ptr string) []keyFrame {
	if !c.dataKeys {
		return append(stack, keyFrame{v: v, ptr: ptr, schema: c.schema})
	}
	m, _ := toMapOK(v)
	for k, val := range m {
		stack = append(stack, keyFrame{v: val, ptr: ptr + "/" + escapePointerToken(k), schema: c.schema})
	}
	return stack
}

// validateUnknownKeys reports each key findUnknownKeys found. The decoders
// read only the keys they know, so without this a misspelled key (onExt,
// timout) would be dropped and its setting silently lost.
func validateUnknownKeys(t *JobTemplate) ValidationErrors {
	errs := make(ValidationErrors, 0, len(t.unknownKeys))
	for _, k := range t.unknownKeys {
		msg := unknownFieldMessage(k.Object)
		if k.StepBlock {
			name := k.Pointer[strings.LastIndexByte(k.Pointer, '/')+1:]
			msg = name + " is a step-level block and is ignored here; put it directly on the step, as a sibling of script"
		}
		errs = append(errs, ValidationError{Pointer: k.Pointer, Message: msg})
	}
	return errs
}
