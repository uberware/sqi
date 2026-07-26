// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

// SpecVersion is the only specification version accepted by this package.
const SpecVersion = "jobtemplate-2023-09"

// ─── Top-level template ──────────────────────────────────────────────────────

// JobTemplate is the root document of an OpenJD submission.
type JobTemplate struct {
	// SpecificationVersion must equal [SpecVersion].
	SpecificationVersion string

	// Name is the human-readable job name.  Required; may contain format-string
	// references such as "Render {{Param.SceneName}}".
	Name string

	// Description is an optional free-form description (no functional effect).
	Description string

	// Extensions is the optional list of named OpenJD feature extensions to
	// enable (e.g. "feature-bundle-1").
	Extensions []string

	// PathTranslation is the parsed SQI_PATH_TRANSLATION extension block, or nil
	// when the extension is not declared.
	PathTranslation *PathTranslation

	// ParameterDefinitions declares the job-level parameters that submitters
	// must or may provide.  Parameter names must be unique.
	ParameterDefinitions []JobParameter
	// ParameterDefinitionsSet records whether the key was present, so a
	// declared but empty list is distinguishable from an omitted one.
	ParameterDefinitionsSet bool

	// JobEnvironments are environments that wrap every session in this job.
	// They are entered in order and exited in reverse order.
	JobEnvironments []Environment

	// Steps is the ordered list of step definitions.  At least one step is
	// required.
	Steps []StepTemplate
}

// ─── Job parameters ──────────────────────────────────────────────────────────

// JobParamType is the discriminator for a [JobParameter].
type JobParamType string

const (
	// JobParamTypeInt is a 64-bit integer parameter.
	JobParamTypeInt JobParamType = "INT"
	// JobParamTypeFloat is a decimal (float64) parameter.
	JobParamTypeFloat JobParamType = "FLOAT"
	// JobParamTypeString is an arbitrary UTF-8 string parameter.
	JobParamTypeString JobParamType = "STRING"
	// JobParamTypePath is a filesystem or S3 path parameter.
	JobParamTypePath JobParamType = "PATH"
)

// PathObjectType is the objectType discriminator for a PATH [JobParameter].
// An empty value means the field was not set; the OpenJD spec default when
// absent is DIRECTORY.  Only valid on PATH parameters.
type PathObjectType string

const (
	// PathObjectTypeFile means the path refers to a regular file.
	PathObjectTypeFile PathObjectType = "FILE"
	// PathObjectTypeDirectory means the path refers to a directory.
	// This is the spec default when objectType is absent.
	PathObjectTypeDirectory PathObjectType = "DIRECTORY"
)

// PathDataFlow is the dataFlow discriminator for a PATH [JobParameter].
// An empty value means the field was not set; the OpenJD spec default when
// absent is NONE.  Only valid on PATH parameters.
type PathDataFlow string

const (
	// PathDataFlowNone means the path has no specific data-flow direction.
	// This is the spec default when dataFlow is absent.
	PathDataFlowNone PathDataFlow = "NONE"
	// PathDataFlowIn means the path is read as input by the job.
	PathDataFlowIn PathDataFlow = "IN"
	// PathDataFlowOut means the path is written as output by the job.
	PathDataFlowOut PathDataFlow = "OUT"
	// PathDataFlowInOut means the path is both read and written.
	PathDataFlowInOut PathDataFlow = "INOUT"
)

// JobParameter is one entry in [JobTemplate.ParameterDefinitions].
type JobParameter struct {
	// Name is the parameter identifier; must match [A-Za-z_][A-Za-z0-9_]*.
	Name string
	// Type discriminates INT / FLOAT / STRING / PATH.
	Type JobParamType
	// Description is optional documentation shown in UI elements.
	Description string
	// Default is the value used when the submitter does not provide one.
	// Stored as a string regardless of Type to avoid lossy conversion.
	// Nil means no default (parameter is required).
	Default *string
	// AllowedValues, when non-nil, enumerates the only acceptable values.
	AllowedValues []string
	// AllowedValuesSet records whether the key was PRESENT in the template.
	// "allowedValues: null" and "allowedValues: []" both decode to an empty
	// slice, which is indistinguishable from the key being absent — but the
	// spec allows omitting the list entirely while requiring a declared one to
	// hold at least one value.
	AllowedValuesSet bool
	// MinValue / MaxValue constrain INT and FLOAT parameters.
	// Stored as strings for lossless round-tripping.
	MinValue *string
	MaxValue *string
	// MinLength / MaxLength constrain STRING and PATH parameters.
	MinLength *int
	MaxLength *int
	// ObjectType narrows how the PATH value is interpreted: FILE or DIRECTORY.
	// Empty means absent; the spec default is DIRECTORY.
	// Only valid for PATH parameters; validation rejects it on other types.
	ObjectType PathObjectType
	// DataFlow describes the data-flow direction of the PATH value.
	// Empty means absent; the spec default is NONE.
	// Only valid for PATH parameters; validation rejects it on other types.
	DataFlow PathDataFlow
	// UserInterface carries optional OpenJD base-spec presentation hints.
	// Nil when the parameter declares no userInterface object.
	UserInterface *ParameterUserInterface
	// FileFilters are the file-type choices offered by a CHOOSE_*_FILE dialog.
	// Only valid on PATH parameters; validation rejects them on other types.
	FileFilters []PathFileFilter
	// FileFilterDefault is the filter selected when the dialog opens.
	// Only valid on PATH parameters. Nil when absent.
	FileFilterDefault *PathFileFilter
}

// PathFileFilter is one named file type offered by an input or output file
// chooser dialog, e.g. {label: "Image Files", patterns: ["*.png", "*.exr"]}.
// It is presentation metadata: the server parses, validates, and carries it but
// never acts on it.
type PathFileFilter struct {
	// Label names the filter in the chooser dialog. Unlike a parameter's
	// userInterface label/groupLabel, this field has no enforced length cap.
	Label string
	// Patterns are the glob patterns the filter matches; at least one required.
	Patterns []string
}

// ControlType is the OpenJD base-spec userInterface control for a job parameter.
type ControlType string

const (
	// ControlLineEdit is a single-line text input.
	ControlLineEdit ControlType = "LINE_EDIT"
	// ControlMultilineEdit is a multi-line text input.
	ControlMultilineEdit ControlType = "MULTILINE_EDIT"
	// ControlDropdownList is a dropdown; requires allowedValues.
	ControlDropdownList ControlType = "DROPDOWN_LIST"
	// ControlCheckBox is a two-state checkbox; requires exactly two allowedValues.
	ControlCheckBox ControlType = "CHECK_BOX"
	// ControlHidden hides the parameter from the generated form.
	ControlHidden ControlType = "HIDDEN"
	// ControlSpinBox is a numeric spinner; valid only on INT/FLOAT.
	ControlSpinBox ControlType = "SPIN_BOX"
	// ControlChooseInputFile is a browse-for-an-existing-file dialog; PATH only.
	ControlChooseInputFile ControlType = "CHOOSE_INPUT_FILE"
	// ControlChooseOutputFile is a browse-for-an-output-file dialog; PATH only.
	ControlChooseOutputFile ControlType = "CHOOSE_OUTPUT_FILE"
	// ControlChooseDirectory is a browse-for-a-directory dialog; PATH only.
	ControlChooseDirectory ControlType = "CHOOSE_DIRECTORY"
)

// ParameterUserInterface is the OpenJD base-spec userInterface hint object on a
// job parameter. It is presentation metadata consumed by submission UIs; the
// server parses, validates, and carries it but does not act on it.
type ParameterUserInterface struct {
	// Control selects the input widget.
	Control ControlType
	// Label is the human-readable field label.
	Label string
	// GroupLabel groups related fields in the generated form.
	GroupLabel string
	// Decimals sets the precision for a SPIN_BOX on a FLOAT parameter.
	Decimals *int
	// SingleStepDelta is the increment a SPIN_BOX applies per step. Held as a
	// string so validation can tell an integer from a float: an INT parameter's
	// delta must itself be an integer.
	SingleStepDelta *string
	// LabelSet and GroupLabelSet record whether the key was PRESENT in the
	// template, as opposed to merely empty. Both fields are optional, but an
	// explicitly empty one is invalid, and the string alone cannot tell the two
	// apart.
	LabelSet      bool
	GroupLabelSet bool
}

// ─── Environments ────────────────────────────────────────────────────────────

// Environment is an OpenJD environment block used at the job or step level.
// Environments are entered once per session and exited in reverse order.
type Environment struct {
	// Name is unique within its containing list.
	Name string
	// Description is optional documentation.
	Description string
	// Script defines the onEnter and onExit actions.
	Script *EnvironmentScript
	// Variables are environment variables set for all actions in the session.
	Variables map[string]string
}

// EnvironmentScript holds the runnable actions for an [Environment].
type EnvironmentScript struct {
	// EmbeddedFiles are plain-text files materialized before actions run.
	EmbeddedFiles []EmbeddedFile
	// EmbeddedFilesSet records whether the key was present, so a declared but
	// empty list is distinguishable from an omitted one.
	EmbeddedFilesSet bool
	// Actions holds the onEnter and onExit lifecycle hooks.
	Actions EnvironmentActions
}

// EnvironmentActions are the lifecycle hooks for an [Environment].
// OnEnter is required by the OpenJD spec; only OnExit is optional.
type EnvironmentActions struct {
	OnEnter *Action
	OnExit  *Action
}

// ─── Steps ────────────────────────────────────────────────────────────────────

// StepTemplate defines one step within a [JobTemplate].
type StepTemplate struct {
	// Name identifies this step; must be unique within the job.
	Name string
	// Description is optional documentation.
	Description string
	// Script is the executable definition of the step's tasks.
	Script *StepScript
	// StepEnvironments are per-step environments entered after job environments.
	StepEnvironments []Environment
	// StepEnvironmentsSet records whether the key was present, so a declared
	// but empty list is distinguishable from an omitted one.
	StepEnvironmentsSet bool
	// ParameterSpace defines the task parameter combinations for this step.
	// Nil means the step produces exactly one task with no parameters.
	ParameterSpace *StepParameterSpace
	// HostRequirements declares the host capabilities required by this step.
	HostRequirements *HostRequirements
	// Dependencies lists the steps that must complete before this one starts.
	Dependencies []StepDependency
	// DependenciesSet records whether the key was present, so a declared but
	// empty list is distinguishable from an omitted one.
	DependenciesSet bool
}

// StepScript is the executable definition attached to a [StepTemplate].
type StepScript struct {
	// EmbeddedFiles are plain-text files written to disk before each action.
	EmbeddedFiles []EmbeddedFile
	// EmbeddedFilesSet records whether the key was present, so a declared but
	// empty list is distinguishable from an omitted one.
	EmbeddedFilesSet bool
	// Actions holds the task lifecycle hooks.
	Actions StepActions
}

// StepActions holds the lifecycle actions for tasks of a step.
type StepActions struct {
	// OnRun is executed once per task.
	OnRun Action
}

// StepDependency names a step that must reach [store.StepStatusCompleted]
// before this step's tasks may be scheduled.
type StepDependency struct {
	// DependsOn is the name of the prerequisite step.
	DependsOn string
}

// ─── Host requirements ────────────────────────────────────────────────────────

// HostRequirements declares the worker capabilities required by a step.
type HostRequirements struct {
	// Amounts are quantifiable requirements such as CPUs, RAM, or software slots.
	Amounts []AmountRequirement
	// Attributes are categorical requirements such as OS or CPU architecture.
	Attributes []AttributeRequirement
}

// AmountRequirement is a single quantifiable host capability requirement.
type AmountRequirement struct {
	// Name is the capability name (e.g. "amount.worker.vcpu").
	Name string
	// Min and Max bound the acceptable range.  Nil means no bound.
	// Stored as strings for lossless round-tripping of int and float values.
	Min *string
	Max *string
}

// AttributeRequirement is a categorical host capability requirement.
type AttributeRequirement struct {
	// Name is the capability name (e.g. "attr.worker.os.family").
	Name string
	// AnyOf requires the host attribute to match at least one value.
	AnyOf []string
	// AllOf requires the host attribute to match all listed values.
	AllOf []string
}

// ─── Parameter space ─────────────────────────────────────────────────────────

// StepParameterSpace defines the multidimensional task parameter space for a
// step.  Expanding it via [ExpandParameterSpace] yields one [TaskParams] per
// task.
type StepParameterSpace struct {
	// TaskParameterDefinitions declares all task parameters.  1–16 entries.
	TaskParameterDefinitions []TaskParamDefinition
	// Combination is an optional expression that controls how parameter ranges
	// are combined.  Identifiers connected with * form a Cartesian product;
	// identifiers grouped in parentheses (A,B,C) are zipped (association).
	// When nil, all parameters are joined with *.
	Combination *string
}

// TaskParamType is the discriminator for a [TaskParamDefinition].
type TaskParamType string

const (
	// TaskParamTypeInt is an integer task parameter.
	TaskParamTypeInt TaskParamType = "INT"
	// TaskParamTypeFloat is a float task parameter.
	TaskParamTypeFloat TaskParamType = "FLOAT"
	// TaskParamTypeString is a string task parameter.
	TaskParamTypeString TaskParamType = "STRING"
	// TaskParamTypePath is a path task parameter.
	TaskParamTypePath TaskParamType = "PATH"
	// TaskParamTypeChunkInt is a chunked integer task parameter that groups
	// multiple frame values into a single task.
	TaskParamTypeChunkInt TaskParamType = "CHUNK[INT]"
)

// TaskParamDefinition is one parameter declaration in a [StepParameterSpace].
type TaskParamDefinition struct {
	// Name is the parameter identifier.
	Name string
	// Type discriminates INT / FLOAT / STRING / PATH / CHUNK[INT].
	Type TaskParamType
	// RangeExpr holds the raw range string for INT and CHUNK[INT] parameters
	// when the template uses range expression syntax (e.g. "1-100:2").
	// Exactly one of RangeExpr or RangeList is set for each definition.
	RangeExpr *string
	// RangeList holds the explicit value list for all other types, and also for
	// INT/CHUNK[INT] when the template provides an array of values.
	RangeList []string
	// Chunks is non-nil only for CHUNK[INT] parameters.
	Chunks *TaskChunks
}

// TaskChunks configures chunked task grouping for a CHUNK[INT] parameter.
type TaskChunks struct {
	// DefaultTaskCount is the number of INT values to group per task.
	DefaultTaskCount int
	// TargetRuntimeSeconds is parsed and carried but not acted on. The spec
	// permits this explicitly: "A scheduler can ignore this, or dynamically
	// adjust the chunk task count to be closer to this value."
	TargetRuntimeSeconds *int
	// RangeConstraint is "CONTIGUOUS" or "NONCONTIGUOUS"; required, validated
	// in [validateChunks].
	RangeConstraint string
}

// ─── Embedded files ───────────────────────────────────────────────────────────

// EmbeddedFileType is the type discriminator for an [EmbeddedFile].
// In jobtemplate-2023-09 the only allowed value is [EmbeddedFileTypeText].
// An empty value means the field was absent in the source document.
type EmbeddedFileType string

const (
	// EmbeddedFileTypeText is the only supported embedded-file type in
	// jobtemplate-2023-09.  It means the file contains UTF-8 plain text.
	EmbeddedFileTypeText EmbeddedFileType = "TEXT"
)

// EmbeddedFile is a plain-text file embedded directly in the job template.
// The worker materializes it to the session working directory before running
// the associated actions.
type EmbeddedFile struct {
	// Name is the identifier used to reference this file.
	Name string
	// Filename is the on-disk name; if empty the worker generates one.
	Filename string
	// FilenameSet records whether the key was present, so an explicitly empty
	// filename is distinguishable from an omitted one.
	FilenameSet bool
	// Data is the file content (may contain format-string references).
	Data string
	// Type is the file-type discriminator.  In jobtemplate-2023-09 the only
	// allowed value is [EmbeddedFileTypeText].  Empty means absent; validation
	// rejects it as required.
	Type EmbeddedFileType
	// Runnable, when true, sets the file's execute permission.
	Runnable bool
	// EndOfLine is "AUTO", "LF", or "CRLF".  Empty means "AUTO".
	EndOfLine string
}

// ─── Actions ─────────────────────────────────────────────────────────────────

// Action is an executable command run as part of a step or environment script.
type Action struct {
	// Command is the executable (may be a format string).
	Command string
	// Args is the optional argument list (each entry may be a format string).
	Args []string
	// ArgsSet records whether the key was present, so a declared but empty
	// list is distinguishable from an omitted one.
	ArgsSet bool
	// TimeoutSeconds, when > 0, limits how long the action may run.
	TimeoutSeconds int
	// TimeoutSet records whether the key was present, so an explicit
	// "timeout: 0" (illegal) is distinguishable from an omitted one (legal).
	TimeoutSet bool
	// Cancelation describes how to stop a running action.
	Cancelation *CancelationMethod
}

// CancelationMode is the cancelation strategy for an [Action].
type CancelationMode string

const (
	// CancelModeTerminate sends SIGKILL / TerminateProcess immediately.
	CancelModeTerminate CancelationMode = "TERMINATE"
	// CancelModeNotifyThenTerminate sends SIGTERM first, then SIGKILL after
	// NotifyPeriodSeconds.
	CancelModeNotifyThenTerminate CancelationMode = "NOTIFY_THEN_TERMINATE"
)

// CancelationMethod configures how a running [Action] is canceled.
type CancelationMethod struct {
	// Mode selects the cancelation strategy.
	Mode CancelationMode
	// NotifyPeriodSeconds is the delay between SIGTERM and SIGKILL when Mode is
	// [CancelModeNotifyThenTerminate].  0 means use the spec default (120s for
	// onRun actions, 30s for others).
	NotifyPeriodSeconds int
	// NotifyPeriodSet records whether the key was present, so an explicit zero
	// is distinguishable from an omitted field.
	NotifyPeriodSet bool
}

// ─── Expanded task parameters ─────────────────────────────────────────────────

// TaskParams is one materialized row from a step's parameter-space expansion.
// Keys are parameter names; values are their string representations.
// An empty map means the step has no parameters (produces a single task).
type TaskParams map[string]string
