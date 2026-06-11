// SPDX-License-Identifier: AGPL-3.0-or-later

// Package openjd implements parsing, validation, and parameter-space expansion
// for Open Job Description (OpenJD) job templates.
//
// # Specification version
//
// This package targets the "jobtemplate-2023-09" specification revision, which
// is the version used by AWS Deadline Cloud and other OpenJD-compatible
// systems.  The specificationVersion field in a submitted template must equal
// that literal string.
//
// # Usage
//
// Parse a raw YAML or JSON submission, then validate and expand it:
//
//	tmpl, err := openjd.Parse(rawBytes, openjd.FormatYAML)
//	if err != nil {
//	    // low-level decode error (bad YAML/JSON)
//	}
//
//	if errs := openjd.Validate(tmpl); len(errs) > 0 {
//	    for _, e := range errs {
//	        fmt.Printf("%s: %s\n", e.Pointer, e.Message)
//	    }
//	}
//
//	for i, step := range tmpl.Steps {
//	    tasks, err := openjd.ExpandParameterSpace(step.ParameterSpace)
//	    // each task is a map[string]string of parameter name → value
//	}
//
// # Format strings
//
// OpenJD format strings use double-braces to reference job or task parameters,
// for example: "{{Param.FrameNumber}}" or "{{Task.Param.OutputFile}}".
// This package records format strings verbatim; interpolation is performed
// later by the worker when it executes the task.
//
// # Path-mapping rules
//
// PATH-typed job parameters and task parameters hold filesystem or S3 paths.
// At execution time the worker resolves them to concrete local paths using the
// path-mapping JSON file written into the session working directory.  The parser
// records PATH parameters as strings; no path resolution is performed here.
package openjd
