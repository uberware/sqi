// SPDX-License-Identifier: AGPL-3.0-only

package openjd

import (
	"encoding/json"
	"fmt"
	"strconv"

	"gopkg.in/yaml.v3"
)

// Format identifies the wire format of a raw template submission.
type Format int

const (
	// FormatYAML means the raw bytes are YAML.
	FormatYAML Format = iota
	// FormatJSON means the raw bytes are JSON.
	FormatJSON
)

// Parse decodes raw YAML or JSON bytes into a [JobTemplate].
// It returns a low-level decode error for malformed documents; call
// [Validate] afterward to check semantic correctness.
func Parse(data []byte, f Format) (*JobTemplate, error) {
	var raw map[string]any
	if err := unmarshal(data, f, &raw); err != nil {
		return nil, f.wrapError(err)
	}
	return decodeJobTemplate(raw)
}

// ─── format helpers ──────────────────────────────────────────────────────────

func unmarshal(data []byte, f Format, dst any) error {
	switch f {
	case FormatYAML:
		return yaml.Unmarshal(data, dst)
	case FormatJSON:
		return json.Unmarshal(data, dst)
	default:
		return fmt.Errorf("openjd: unknown format %d", int(f))
	}
}

func (f Format) wrapError(err error) error {
	switch f {
	case FormatYAML:
		return fmt.Errorf("openjd: parse YAML: %w", err)
	default:
		return fmt.Errorf("openjd: parse JSON: %w", err)
	}
}

// ─── top-level decoder ───────────────────────────────────────────────────────

func decodeJobTemplate(raw map[string]any) (*JobTemplate, error) {
	t := &JobTemplate{}
	t.SpecificationVersion = getString(raw, "specificationVersion")
	t.Name = getString(raw, "name")
	t.Description = getString(raw, "description")
	t.Extensions = getStringSlice(raw, "extensions")

	// parameterDefinitions
	if defs, ok := raw["parameterDefinitions"]; ok {
		items, err := toSliceOfMaps(defs, "parameterDefinitions")
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			p, err := decodeJobParameter(item)
			if err != nil {
				return nil, err
			}
			t.ParameterDefinitions = append(t.ParameterDefinitions, p)
		}
	}

	// jobEnvironments
	if envs, ok := raw["jobEnvironments"]; ok {
		items, err := toSliceOfMaps(envs, "jobEnvironments")
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			e, err := decodeEnvironment(item)
			if err != nil {
				return nil, err
			}
			t.JobEnvironments = append(t.JobEnvironments, e)
		}
	}

	// steps (required)
	if steps, ok := raw["steps"]; ok {
		items, err := toSliceOfMaps(steps, "steps")
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			s, err := decodeStepTemplate(item)
			if err != nil {
				return nil, err
			}
			t.Steps = append(t.Steps, s)
		}
	}

	return t, nil
}

// ─── job parameter decoder ───────────────────────────────────────────────────

func decodeJobParameter(raw map[string]any) (JobParameter, error) {
	p := JobParameter{
		Name:        getString(raw, "name"),
		Type:        JobParamType(getString(raw, "type")),
		Description: getString(raw, "description"),
	}

	// default
	if v, ok := raw["default"]; ok && v != nil {
		s := anyToString(v)
		p.Default = &s
	}

	// allowedValues
	if v, ok := raw["allowedValues"]; ok && v != nil {
		items, _ := toAnySlice(v)
		for _, item := range items {
			p.AllowedValues = append(p.AllowedValues, anyToString(item))
		}
	}

	// minValue / maxValue (INT, FLOAT)
	if v, ok := raw["minValue"]; ok && v != nil {
		s := anyToString(v)
		p.MinValue = &s
	}
	if v, ok := raw["maxValue"]; ok && v != nil {
		s := anyToString(v)
		p.MaxValue = &s
	}

	// minLength / maxLength (STRING, PATH)
	if v, ok := raw["minLength"]; ok && v != nil {
		n := anyToInt(v)
		p.MinLength = &n
	}
	if v, ok := raw["maxLength"]; ok && v != nil {
		n := anyToInt(v)
		p.MaxLength = &n
	}

	return p, nil
}

// ─── environment decoder ─────────────────────────────────────────────────────

func decodeEnvironment(raw map[string]any) (Environment, error) {
	e := Environment{
		Name:        getString(raw, "name"),
		Description: getString(raw, "description"),
	}

	// variables
	if v, ok := raw["variables"]; ok && v != nil {
		switch m := v.(type) {
		case map[string]any:
			e.Variables = make(map[string]string, len(m))
			for k, val := range m {
				e.Variables[k] = anyToString(val)
			}
		case map[any]any:
			// yaml.v3 may produce this for non-string keys
			e.Variables = make(map[string]string, len(m))
			for k, val := range m {
				e.Variables[fmt.Sprintf("%v", k)] = anyToString(val)
			}
		}
	}

	// script
	if s, ok := raw["script"]; ok && s != nil {
		sm, err := toMap(s, "environment.script")
		if err != nil {
			return e, err
		}
		script, err := decodeEnvironmentScript(sm)
		if err != nil {
			return e, err
		}
		e.Script = &script
	}

	return e, nil
}

func decodeEnvironmentScript(raw map[string]any) (EnvironmentScript, error) {
	s := EnvironmentScript{}

	if ef, ok := raw["embeddedFiles"]; ok && ef != nil {
		items, err := toSliceOfMaps(ef, "embeddedFiles")
		if err != nil {
			return s, err
		}
		for _, item := range items {
			s.EmbeddedFiles = append(s.EmbeddedFiles, decodeEmbeddedFile(item))
		}
	}

	// actions (required)
	if a, ok := raw["actions"]; ok && a != nil {
		am, err := toMap(a, "environment.script.actions")
		if err != nil {
			return s, err
		}
		actions, err := decodeEnvironmentActions(am)
		if err != nil {
			return s, err
		}
		s.Actions = actions
	}

	return s, nil
}

func decodeEnvironmentActions(raw map[string]any) (EnvironmentActions, error) {
	a := EnvironmentActions{}
	if v, ok := raw["onEnter"]; ok && v != nil {
		m, err := toMap(v, "onEnter")
		if err != nil {
			return a, err
		}
		action := decodeAction(m)
		a.OnEnter = &action
	}
	if v, ok := raw["onExit"]; ok && v != nil {
		m, err := toMap(v, "onExit")
		if err != nil {
			return a, err
		}
		action := decodeAction(m)
		a.OnExit = &action
	}
	return a, nil
}

// ─── step template decoder ───────────────────────────────────────────────────

func decodeStepTemplate(raw map[string]any) (StepTemplate, error) {
	s := StepTemplate{
		Name:        getString(raw, "name"),
		Description: getString(raw, "description"),
	}

	// script
	if v, ok := raw["script"]; ok && v != nil {
		m, err := toMap(v, "step.script")
		if err != nil {
			return s, err
		}
		script, err := decodeStepScript(m)
		if err != nil {
			return s, err
		}
		s.Script = &script
	}

	// stepEnvironments
	envs, err := decodeStepEnvironmentList(raw)
	if err != nil {
		return s, err
	}
	s.StepEnvironments = envs

	// parameterSpace
	if v, ok := raw["parameterSpace"]; ok && v != nil {
		m, err := toMap(v, "parameterSpace")
		if err != nil {
			return s, err
		}
		ps, err := decodeStepParameterSpace(m)
		if err != nil {
			return s, err
		}
		s.ParameterSpace = &ps
	}

	// hostRequirements
	if v, ok := raw["hostRequirements"]; ok && v != nil {
		m, err := toMap(v, "hostRequirements")
		if err != nil {
			return s, err
		}
		hr, err := decodeHostRequirements(m)
		if err != nil {
			return s, err
		}
		s.HostRequirements = &hr
	}

	// dependencies
	deps, err := decodeStepDependencyList(raw)
	if err != nil {
		return s, err
	}
	s.Dependencies = deps

	return s, nil
}

func decodeStepEnvironmentList(raw map[string]any) ([]Environment, error) {
	v, ok := raw["stepEnvironments"]
	if !ok || v == nil {
		return nil, nil
	}
	items, err := toSliceOfMaps(v, "stepEnvironments")
	if err != nil {
		return nil, err
	}
	envs := make([]Environment, 0, len(items))
	for _, item := range items {
		e, err := decodeEnvironment(item)
		if err != nil {
			return nil, err
		}
		envs = append(envs, e)
	}
	return envs, nil
}

func decodeStepDependencyList(raw map[string]any) ([]StepDependency, error) {
	v, ok := raw["dependencies"]
	if !ok || v == nil {
		return nil, nil
	}
	items, err := toSliceOfMaps(v, "dependencies")
	if err != nil {
		return nil, err
	}
	deps := make([]StepDependency, 0, len(items))
	for _, item := range items {
		deps = append(deps, StepDependency{DependsOn: getString(item, "dependsOn")})
	}
	return deps, nil
}

func decodeStepScript(raw map[string]any) (StepScript, error) {
	s := StepScript{}

	if v, ok := raw["embeddedFiles"]; ok && v != nil {
		items, err := toSliceOfMaps(v, "embeddedFiles")
		if err != nil {
			return s, err
		}
		for _, item := range items {
			s.EmbeddedFiles = append(s.EmbeddedFiles, decodeEmbeddedFile(item))
		}
	}

	// actions (required)
	if v, ok := raw["actions"]; ok && v != nil {
		m, err := toMap(v, "step.script.actions")
		if err != nil {
			return s, err
		}
		actions, err := decodeStepActions(m)
		if err != nil {
			return s, err
		}
		s.Actions = actions
	}

	return s, nil
}

func decodeStepActions(raw map[string]any) (StepActions, error) {
	a := StepActions{}
	if v, ok := raw["onRun"]; ok && v != nil {
		m, err := toMap(v, "onRun")
		if err != nil {
			return a, err
		}
		a.OnRun = decodeAction(m)
	}
	return a, nil
}

// ─── host requirements decoder ───────────────────────────────────────────────

func decodeHostRequirements(raw map[string]any) (HostRequirements, error) {
	hr := HostRequirements{}

	if v, ok := raw["amounts"]; ok && v != nil {
		items, err := toSliceOfMaps(v, "hostRequirements.amounts")
		if err != nil {
			return hr, err
		}
		for _, item := range items {
			a := AmountRequirement{Name: getString(item, "name")}
			if mv, ok := item["min"]; ok && mv != nil {
				s := anyToString(mv)
				a.Min = &s
			}
			if mv, ok := item["max"]; ok && mv != nil {
				s := anyToString(mv)
				a.Max = &s
			}
			hr.Amounts = append(hr.Amounts, a)
		}
	}

	if v, ok := raw["attributes"]; ok && v != nil {
		items, err := toSliceOfMaps(v, "hostRequirements.attributes")
		if err != nil {
			return hr, err
		}
		for _, item := range items {
			a := AttributeRequirement{Name: getString(item, "name")}
			a.AnyOf = getStringSlice(item, "anyOf")
			a.AllOf = getStringSlice(item, "allOf")
			hr.Attributes = append(hr.Attributes, a)
		}
	}

	return hr, nil
}

// ─── parameter space decoder ─────────────────────────────────────────────────

func decodeStepParameterSpace(raw map[string]any) (StepParameterSpace, error) {
	ps := StepParameterSpace{}

	if v, ok := raw["combination"]; ok && v != nil {
		s := anyToString(v)
		ps.Combination = &s
	}

	if v, ok := raw["taskParameterDefinitions"]; ok && v != nil {
		items, err := toSliceOfMaps(v, "taskParameterDefinitions")
		if err != nil {
			return ps, err
		}
		for _, item := range items {
			tp, err := decodeTaskParamDefinition(item)
			if err != nil {
				return ps, err
			}
			ps.TaskParameterDefinitions = append(ps.TaskParameterDefinitions, tp)
		}
	}

	return ps, nil
}

func decodeTaskParamDefinition(raw map[string]any) (TaskParamDefinition, error) {
	tp := TaskParamDefinition{
		Name: getString(raw, "name"),
		Type: TaskParamType(getString(raw, "type")),
	}

	// range field — either a string expression (INT) or a list of values
	if v, ok := raw["range"]; ok && v != nil {
		switch rv := v.(type) {
		case string:
			tp.RangeExpr = &rv
		default:
			items, _ := toAnySlice(v)
			for _, item := range items {
				tp.RangeList = append(tp.RangeList, anyToString(item))
			}
		}
	}

	// chunks (CHUNK[INT] only)
	if v, ok := raw["chunks"]; ok && v != nil {
		m, err := toMap(v, "chunks")
		if err != nil {
			return tp, err
		}
		chunks := TaskChunks{
			RangeConstraint: "CONTIGUOUS", // spec default
		}
		if dc, ok := m["defaultTaskCount"]; ok {
			chunks.DefaultTaskCount = anyToInt(dc)
		}
		if trs, ok := m["targetRuntimeSeconds"]; ok && trs != nil {
			n := anyToInt(trs)
			chunks.TargetRuntimeSeconds = &n
		}
		if rc, ok := m["rangeConstraint"]; ok && rc != nil {
			chunks.RangeConstraint = anyToString(rc)
		}
		tp.Chunks = &chunks
	}

	return tp, nil
}

// ─── embedded file decoder ───────────────────────────────────────────────────

func decodeEmbeddedFile(raw map[string]any) EmbeddedFile {
	ef := EmbeddedFile{
		Name:      getString(raw, "name"),
		Filename:  getString(raw, "filename"),
		Data:      getString(raw, "data"),
		EndOfLine: getString(raw, "endOfLine"),
	}
	if v, ok := raw["runnable"]; ok {
		if b, ok := v.(bool); ok {
			ef.Runnable = b
		}
	}
	return ef
}

// ─── action decoder ───────────────────────────────────────────────────────────

func decodeAction(raw map[string]any) Action {
	a := Action{
		Command: getString(raw, "command"),
	}

	if v, ok := raw["args"]; ok && v != nil {
		items, _ := toAnySlice(v)
		for _, item := range items {
			a.Args = append(a.Args, anyToString(item))
		}
	}

	if v, ok := raw["timeout"]; ok && v != nil {
		a.TimeoutSeconds = anyToInt(v)
	}

	if v, ok := raw["cancelation"]; ok && v != nil {
		if m, ok := toMapOK(v); ok {
			cm := &CancelationMethod{
				Mode: CancelationMode(getString(m, "mode")),
			}
			if np, ok := m["notifyPeriodInSeconds"]; ok && np != nil {
				cm.NotifyPeriodSeconds = anyToInt(np)
			}
			a.Cancelation = cm
		}
	}

	return a
}

// ─── low-level helpers ────────────────────────────────────────────────────────

// getString returns the string value for key in m, or "" if absent/not a string.
func getString(m map[string]any, key string) string {
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

// getStringSlice returns a []string for key in m, or nil.
func getStringSlice(m map[string]any, key string) []string {
	v, ok := m[key]
	if !ok || v == nil {
		return nil
	}
	items, _ := toAnySlice(v)
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, anyToString(item))
	}
	return out
}

// toSliceOfMaps converts v to []map[string]any, returning an error
// if v is not a slice or any element is not a map.
func toSliceOfMaps(v any, field string) ([]map[string]any, error) {
	items, ok := toAnySlice(v)
	if !ok {
		return nil, fmt.Errorf("openjd: %s must be a sequence", field)
	}
	out := make([]map[string]any, 0, len(items))
	for i, item := range items {
		m, ok := toMapOK(item)
		if !ok {
			return nil, fmt.Errorf("openjd: %s[%d] must be a mapping", field, i)
		}
		out = append(out, m)
	}
	return out, nil
}

// toMap converts v to map[string]any, returning an error on failure.
func toMap(v any, field string) (map[string]any, error) {
	m, ok := toMapOK(v)
	if !ok {
		return nil, fmt.Errorf("openjd: %s must be a mapping", field)
	}
	return m, nil
}

// toMapOK converts v to map[string]any without allocating an error.
func toMapOK(v any) (map[string]any, bool) {
	switch m := v.(type) {
	case map[string]any:
		return m, true
	case map[any]any:
		// yaml.v3 can return this for non-string keys; normalize it.
		out := make(map[string]any, len(m))
		for k, val := range m {
			out[fmt.Sprintf("%v", k)] = val
		}
		return out, true
	default:
		return nil, false
	}
}

// toAnySlice coerces v into a []interface{}.
func toAnySlice(v any) ([]any, bool) {
	switch s := v.(type) {
	case []any:
		return s, true
	default:
		return nil, false
	}
}

// anyToString converts an arbitrary value to its string representation.
func anyToString(v any) string {
	if v == nil {
		return ""
	}
	switch s := v.(type) {
	case string:
		return s
	case int:
		return strconv.Itoa(s)
	case int64:
		return strconv.FormatInt(s, 10)
	case float64:
		// Use %g to avoid trailing zeros while preserving precision.
		return strconv.FormatFloat(s, 'g', -1, 64)
	case bool:
		if s {
			return "true"
		}
		return "false"
	default:
		return fmt.Sprintf("%v", v)
	}
}

// anyToInt converts an arbitrary value to an int, returning 0 on failure.
func anyToInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	case string:
		if i, err := strconv.Atoi(n); err == nil {
			return i
		}
	}
	return 0
}
