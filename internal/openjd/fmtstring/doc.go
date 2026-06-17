// SPDX-License-Identifier: AGPL-3.0-or-later

// Package fmtstring resolves Open Job Description (OpenJD) format strings.
//
// OpenJD templates embed format-string references delimited by double braces,
// for example "{{ Param.FrameStart }}" or "{{Param.FrameStart}}" (surrounding
// whitespace inside the braces is trimmed, so the two forms are equivalent).
// Text outside the braces is literal and copied verbatim. There is no
// backslash escape in OpenJD: every "{{" begins a reference, and a "}}" with
// no preceding "{{" is literal text.
//
// A reference names a dotted OpenJD identifier such as Param.Foo,
// RawParam.Foo, Task.Param.Frame, Task.RawParam.Frame, Task.File.script,
// Env.File.env, Session.WorkingDirectory, Session.HasPathMappingRules, or
// Session.PathMappingRulesFile. Each dot-separated segment is an OpenJD
// identifier matching [A-Za-z_][A-Za-z0-9_]*.
//
// The package is intentionally a self-contained leaf with no dependencies on
// other internal packages, because it is used by BOTH the server (at job
// submit/expansion time) and the worker (at task-run time). It does not
// hardcode the available variable namespaces; instead callers supply a [Scope]
// that maps fully-qualified names (for example "Task.Param.Frame") to values.
//
// The two core operations are [Resolve] (substitute every reference) and
// [References] (list the names a string refers to without resolving them).
package fmtstring
