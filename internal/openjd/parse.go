// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import (
	"encoding/json"
	"fmt"
	"math"
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

	pt, err := maybeDecodePathTranslation(raw)
	if err != nil {
		return nil, err
	}
	t.PathTranslation = pt

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
	if s, ok, err := scalarFieldStrict(raw, "default", "default"); err != nil {
		return p, err
	} else if ok {
		p.Default = &s
	}

	// Constraint fields (allowedValues, minValue/maxValue, minLength/maxLength)
	// are decoded by a helper to keep this function's cyclomatic complexity
	// within bounds.
	if err := decodeJobParamConstraints(raw, &p); err != nil {
		return p, err
	}

	// objectType / dataFlow (PATH only; stored verbatim — validation enforces
	// the allowed values and the PATH-only constraint).
	if v, ok := raw["objectType"]; ok && v != nil {
		p.ObjectType = PathObjectType(anyToString(v))
	}
	if v, ok := raw["dataFlow"]; ok && v != nil {
		p.DataFlow = PathDataFlow(anyToString(v))
	}

	// userInterface (base-spec presentation hints)
	if v, ok := raw["userInterface"]; ok && v != nil {
		ui, err := decodeParameterUserInterface(v)
		if err != nil {
			return p, err
		}
		p.UserInterface = ui
	}

	return p, nil
}

// decodeJobParamConstraints populates the allowedValues, minValue/maxValue, and
// minLength/maxLength fields of p from the raw decoded map.
func decodeJobParamConstraints(raw map[string]any, p *JobParameter) error {
	// allowedValues
	if v, ok := raw["allowedValues"]; ok && v != nil {
		items, _ := toAnySlice(v)
		for _, item := range items {
			p.AllowedValues = append(p.AllowedValues, anyToString(item))
		}
	}

	// minValue / maxValue (INT, FLOAT) — left lenient: validate.go re-checks these
	// as numbers, so a non-scalar coercion cannot slip through. Strict scalar
	// decoding is reserved for fields with no such downstream type check.
	if v, ok := raw["minValue"]; ok && v != nil {
		s := anyToString(v)
		p.MinValue = &s
	}
	if v, ok := raw["maxValue"]; ok && v != nil {
		s := anyToString(v)
		p.MaxValue = &s
	}

	// minLength / maxLength (STRING, PATH)
	if n, ok, err := intFieldStrict(raw, "minLength", "minLength"); err != nil {
		return err
	} else if ok {
		p.MinLength = &n
	}
	if n, ok, err := intFieldStrict(raw, "maxLength", "maxLength"); err != nil {
		return err
	} else if ok {
		p.MaxLength = &n
	}

	return nil
}

// decodeParameterUserInterface decodes the optional userInterface hint object on
// a job parameter. Unknown control values are accepted here and rejected during
// validation, mirroring how parse stays lenient and validate enforces.
func decodeParameterUserInterface(v any) (*ParameterUserInterface, error) {
	m, err := toMap(v, "parameterDefinition.userInterface")
	if err != nil {
		return nil, err
	}
	ui := &ParameterUserInterface{
		Control:    ControlType(getString(m, "control")),
		Label:      getString(m, "label"),
		GroupLabel: getString(m, "groupLabel"),
	}
	if n, ok, err := intFieldStrict(m, "decimals", "userInterface.decimals"); err != nil {
		return nil, err
	} else if ok {
		ui.Decimals = &n
	}
	if b, ok := m["singleStepRemoval"].(bool); ok {
		ui.SingleStepRemoval = &b
	}
	return ui, nil
}

// ─── environment decoder ─────────────────────────────────────────────────────

func decodeEnvironment(raw map[string]any) (Environment, error) {
	e := Environment{
		Name:        getString(raw, "name"),
		Description: getString(raw, "description"),
	}

	// variables
	if v, ok := raw["variables"]; ok && v != nil {
		vars, err := decodeEnvVars(v, e.Name)
		if err != nil {
			return e, err
		}
		e.Variables = vars
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

// decodeEnvVars decodes an environment's variables mapping, rejecting a
// non-scalar value rather than stringifying it into a "map[...]" environment
// value that the worker would export into the running task's session. envName
// labels the owning environment in error messages.
func decodeEnvVars(v any, envName string) (map[string]string, error) {
	m, ok := toMapOK(v) // normalizes yaml.v3 map[any]any to map[string]any
	if !ok {
		return nil, fmt.Errorf("openjd: environment %q variables must be a mapping", envName)
	}
	out := make(map[string]string, len(m))
	for k, val := range m {
		s, ok := scalarToString(val)
		if !ok {
			return nil, fmt.Errorf(
				"openjd: environment %q variable %q must be a string, number, or boolean", envName, k,
			)
		}
		out[k] = s
	}
	return out, nil
}

func decodeEnvironmentScript(raw map[string]any) (EnvironmentScript, error) {
	s := EnvironmentScript{}

	if ef, ok := raw["embeddedFiles"]; ok && ef != nil {
		files, err := decodeEmbeddedFiles(ef)
		if err != nil {
			return s, err
		}
		s.EmbeddedFiles = files
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
		action, err := decodeAction(m)
		if err != nil {
			return a, err
		}
		a.OnEnter = &action
	}
	if v, ok := raw["onExit"]; ok && v != nil {
		m, err := toMap(v, "onExit")
		if err != nil {
			return a, err
		}
		action, err := decodeAction(m)
		if err != nil {
			return a, err
		}
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
		files, err := decodeEmbeddedFiles(v)
		if err != nil {
			return s, err
		}
		s.EmbeddedFiles = files
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
		act, err := decodeAction(m)
		if err != nil {
			return a, err
		}
		a.OnRun = act
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
		for i, item := range items {
			a := AmountRequirement{Name: getString(item, "name")}
			field := fmt.Sprintf("hostRequirements.amounts[%d]", i)
			var err error
			if a.Min, err = decodeAmountBound(item, "min", field+".min"); err != nil {
				return hr, err
			}
			if a.Max, err = decodeAmountBound(item, "max", field+".max"); err != nil {
				return hr, err
			}
			hr.Amounts = append(hr.Amounts, a)
		}
	}

	if v, ok := raw["attributes"]; ok && v != nil {
		items, err := toSliceOfMaps(v, "hostRequirements.attributes")
		if err != nil {
			return hr, err
		}
		for i, item := range items {
			a := AttributeRequirement{Name: getString(item, "name")}
			field := fmt.Sprintf("hostRequirements.attributes[%d]", i)
			var err error
			if a.AnyOf, err = getStringSliceStrict(item, "anyOf", field+".anyOf"); err != nil {
				return hr, err
			}
			if a.AllOf, err = getStringSliceStrict(item, "allOf", field+".allOf"); err != nil {
				return hr, err
			}
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
			list, err := strictStringSlice(v, "range")
			if err != nil {
				return tp, err
			}
			tp.RangeList = list
		}
	}

	// chunks (CHUNK[INT] only)
	if v, ok := raw["chunks"]; ok && v != nil {
		chunks, err := decodeTaskChunks(v)
		if err != nil {
			return tp, err
		}
		tp.Chunks = chunks
	}

	return tp, nil
}

// decodeTaskChunks decodes a CHUNK[INT] parameter's chunks block, rejecting a
// non-integer defaultTaskCount or targetRuntimeSeconds rather than silently
// coercing it to 0.
func decodeTaskChunks(v any) (*TaskChunks, error) {
	m, err := toMap(v, "chunks")
	if err != nil {
		return nil, err
	}
	chunks := TaskChunks{
		RangeConstraint: "CONTIGUOUS", // spec default
	}
	if n, ok, err := intFieldStrict(m, "defaultTaskCount", "chunks.defaultTaskCount"); err != nil {
		return nil, err
	} else if ok {
		chunks.DefaultTaskCount = n
	}
	if n, ok, err := intFieldStrict(m, "targetRuntimeSeconds", "chunks.targetRuntimeSeconds"); err != nil {
		return nil, err
	} else if ok {
		chunks.TargetRuntimeSeconds = &n
	}
	if rc, ok := m["rangeConstraint"]; ok && rc != nil {
		chunks.RangeConstraint = anyToString(rc)
	}
	return &chunks, nil
}

// ─── embedded file decoder ───────────────────────────────────────────────────

// decodeEmbeddedFiles decodes an embeddedFiles sequence (shared by step and
// environment scripts).
func decodeEmbeddedFiles(v any) ([]EmbeddedFile, error) {
	items, err := toSliceOfMaps(v, "embeddedFiles")
	if err != nil {
		return nil, err
	}
	out := make([]EmbeddedFile, 0, len(items))
	for i, item := range items {
		ef, err := decodeEmbeddedFile(item, i)
		if err != nil {
			return nil, err
		}
		out = append(out, ef)
	}
	return out, nil
}

func decodeEmbeddedFile(raw map[string]any, idx int) (EmbeddedFile, error) {
	// data is the file's content, which becomes the script the task executes;
	// reject a non-scalar rather than coercing it to a "map[...]" body.
	data, _, err := scalarFieldStrict(raw, "data", fmt.Sprintf("embeddedFiles[%d].data", idx))
	if err != nil {
		return EmbeddedFile{}, err
	}
	ef := EmbeddedFile{
		Name:      getString(raw, "name"),
		Filename:  getString(raw, "filename"),
		Data:      data,
		Type:      EmbeddedFileType(getString(raw, "type")),
		EndOfLine: getString(raw, "endOfLine"),
	}
	if v, ok := raw["runnable"]; ok {
		if b, ok := v.(bool); ok {
			ef.Runnable = b
		}
	}
	return ef, nil
}

// ─── action decoder ───────────────────────────────────────────────────────────

func decodeAction(raw map[string]any) (Action, error) {
	a := Action{
		Command: getString(raw, "command"),
	}

	if v, ok := raw["args"]; ok && v != nil {
		args, err := strictStringSlice(v, "args")
		if err != nil {
			return a, err
		}
		a.Args = args
	}

	if n, ok, err := intFieldStrict(raw, "timeout", "timeout"); err != nil {
		return a, err
	} else if ok {
		a.TimeoutSeconds = n
	}

	if v, ok := raw["cancelation"]; ok && v != nil {
		if m, ok := toMapOK(v); ok {
			cm := &CancelationMethod{
				Mode: CancelationMode(getString(m, "mode")),
			}
			if n, ok, err := intFieldStrict(m, "notifyPeriodInSeconds", "notifyPeriodInSeconds"); err != nil {
				return a, err
			} else if ok {
				cm.NotifyPeriodSeconds = n
			}
			a.Cancelation = cm
		}
	}

	return a, nil
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

// intFieldStrict reads an integer field key from m, returning (value, present,
// error). An absent or nil value yields (0, false, nil). A present value that is
// not an integer (a mapping, sequence, boolean, fractional number, or
// non-numeric string) is an error rather than a silent coercion to 0 — these
// fields are typed integers carrying no template references, so a non-integer is
// malformed. field names the document path for error messages.
func intFieldStrict(m map[string]any, key, field string) (value int, present bool, err error) {
	v, ok := m[key]
	if !ok || v == nil {
		return 0, false, nil
	}
	n, ok := scalarToInt(v)
	if !ok {
		return 0, false, fmt.Errorf("openjd: %s must be an integer", field)
	}
	return n, true, nil
}

// scalarToInt converts a scalar integer value (int, int64, an integral float, or
// a base-10 integer string) to an int, reporting ok=false for any other value:
// a mapping, sequence, boolean, fractional number, or non-numeric string.
func scalarToInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		if n != math.Trunc(n) {
			return 0, false
		}
		return int(n), true
	case string:
		if i, err := strconv.Atoi(n); err == nil {
			return i, true
		}
		return 0, false
	default:
		return 0, false
	}
}

// decodeAmountBound reads a scalar min/max bound from item[key], rejecting a
// non-scalar value (a nested mapping or sequence) instead of stringifying it
// via fmt. A coerced bound like "map[nested:1]" is stored verbatim and — for a
// non-reserved capability name — never numerically validated, silently
// corrupting the reservation. field names the document path for error messages.
// Absent or nil values yield (nil, nil); a numeric or template-reference string
// bound is returned as-is.
func decodeAmountBound(item map[string]any, key, field string) (*string, error) {
	s, ok, err := scalarFieldStrict(item, key, field)
	if err != nil || !ok {
		return nil, err
	}
	return &s, nil
}

// scalarFieldStrict reads a scalar string field key from m, rejecting a
// non-scalar value (a mapping or sequence) instead of stringifying it via fmt.
// Such a coerced value (e.g. a "map[...]" default, env value, or embedded-file
// data) would otherwise be substituted verbatim into a running task's command,
// script, or environment. Absent or nil values yield ("", false, nil); a scalar
// is returned in string form. field names the document path for error messages.
func scalarFieldStrict(m map[string]any, key, field string) (value string, present bool, err error) {
	v, ok := m[key]
	if !ok || v == nil {
		return "", false, nil
	}
	s, ok := scalarToString(v)
	if !ok {
		return "", false, fmt.Errorf("openjd: %s must be a string, number, or boolean", field)
	}
	return s, true, nil
}

// getStringSliceStrict returns a []string for key in m, rejecting a non-scalar
// element instead of stringifying it. Absent or nil values yield (nil, nil).
func getStringSliceStrict(m map[string]any, key, field string) ([]string, error) {
	v, ok := m[key]
	if !ok || v == nil {
		return nil, nil
	}
	return strictStringSlice(v, field)
}

// strictStringSlice converts a sequence value into a []string, rejecting a
// non-scalar element rather than coercing it via fmt — a silent coercion that
// turns e.g. anyOf: [{min: 1}] into the meaningless value "map[min:1]". A value
// that is not a sequence is itself an error. field names the document path.
func strictStringSlice(v any, field string) ([]string, error) {
	items, ok := toAnySlice(v)
	if !ok {
		return nil, fmt.Errorf("openjd: %s must be a sequence", field)
	}
	out := make([]string, 0, len(items))
	for i, item := range items {
		s, ok := scalarToString(item)
		if !ok {
			return nil, fmt.Errorf("openjd: %s[%d] must be a string, number, or boolean", field, i)
		}
		out = append(out, s)
	}
	return out, nil
}

// scalarToString converts a scalar value (string, integer, float, or boolean)
// to its string form, reporting ok=false for any other type (mapping,
// sequence, or nil).
func scalarToString(v any) (string, bool) {
	switch s := v.(type) {
	case string:
		return s, true
	case int:
		return strconv.Itoa(s), true
	case int64:
		return strconv.FormatInt(s, 10), true
	case float64:
		return strconv.FormatFloat(s, 'g', -1, 64), true
	case bool:
		if s {
			return "true", true
		}
		return "false", true
	default:
		return "", false
	}
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

// anyToString converts an arbitrary value to its string representation. It is
// the lenient counterpart of [scalarToString]: scalars are formatted the same
// way, but a mapping or sequence falls back to fmt rather than being rejected.
func anyToString(v any) string {
	if s, ok := scalarToString(v); ok {
		return s
	}
	if v == nil {
		return ""
	}
	return fmt.Sprintf("%v", v)
}
