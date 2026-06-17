// SPDX-License-Identifier: AGPL-3.0-or-later

// Package fmtres resolves OpenJD "{{...}}" format strings on the worker, where
// the session working directory — and therefore Session.WorkingDirectory — is
// only known at run time.
//
// The server resolves loc:// URIs and the job-parameter space before
// dispatching an assignment (see internal/scheduler/assign.go); this package
// performs the remaining, worker-local substitution of format-string references
// in command lines, argument lists, and environment-variable values.
//
// # Scopes
//
// Two resolution contexts exist, matching the OpenJD specification:
//
//   - Task-action context (a step's onRun command and args) exposes
//     Param.<n> / RawParam.<n> (job parameters), Task.Param.<n> /
//     Task.RawParam.<n> (task parameters), and Session.WorkingDirectory.
//   - Environment-action context (an environment's onEnter/onExit actions and
//     its variable values) exposes Param.<n> / RawParam.<n> and
//     Session.WorkingDirectory, but NOT Task.Param.* — environments are
//     session-scoped and entered once, not per task.
//
// [TaskScope] and [EnvScope] return a [fmtstring.MapScope] (a plain map) so
// callers can extend the scope with future keys — such as Task.File.*,
// Env.File.*, or Session.PathMappingRulesFile — before resolving.
package fmtres

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/uberware/sqi/internal/openjd/fmtstring"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// TaskScope builds the format-string scope for a task action (a step's onRun
// command and args).
//
// jobParams are the job-level parameter values (exposed as Param.<n> and
// RawParam.<n>); taskParams are the per-task parameter values (exposed as
// Task.Param.<n> and Task.RawParam.<n>); workDir is the session working
// directory (exposed as Session.WorkingDirectory).
//
// pathMapFile is the absolute path to the written OpenJD path-mapping file and
// hasPathMap reports whether the assignment carries any path-mapping rules;
// they populate Session.HasPathMappingRules ("true"/"false") and, when
// hasPathMap is true, Session.PathMappingRulesFile (otherwise that key is
// absent).
//
// Param.<n> and RawParam.<n> currently map to the same value; path-mapped
// (cooked) Param values are a later task. The returned map may be mutated by
// the caller to add additional keys before resolving.
func TaskScope(jobParams, taskParams map[string]string, workDir, pathMapFile string, hasPathMap bool) fmtstring.MapScope {
	// A task scope is the environment scope (job params + Session.* keys) plus
	// the per-task Task.Param.* / Task.RawParam.* keys.
	m := EnvScope(jobParams, workDir, pathMapFile, hasPathMap)
	for k, v := range taskParams {
		m["Task.Param."+k] = v
		m["Task.RawParam."+k] = v
	}
	return m
}

// EnvScope builds the format-string scope for an environment action (an
// environment's onEnter/onExit command and args) and for environment variable
// values.
//
// Unlike [TaskScope], the environment scope does NOT expose Task.Param.* /
// Task.RawParam.*: environments are session-scoped and entered once, not per
// task. jobParams are exposed as Param.<n> and RawParam.<n>; workDir is exposed
// as Session.WorkingDirectory. pathMapFile and hasPathMap populate
// Session.PathMappingRulesFile / Session.HasPathMappingRules exactly as in
// [TaskScope]. The returned map may be mutated by the caller to add additional
// keys before resolving.
func EnvScope(jobParams map[string]string, workDir, pathMapFile string, hasPathMap bool) fmtstring.MapScope {
	m := make(fmtstring.MapScope, 2*len(jobParams)+3)
	for k, v := range jobParams {
		m["Param."+k] = v
		m["RawParam."+k] = v
	}
	m["Session.WorkingDirectory"] = workDir
	addPathMappingKeys(m, pathMapFile, hasPathMap)
	return m
}

// addPathMappingKeys populates the Session.HasPathMappingRules and
// Session.PathMappingRulesFile format-string variables.
//
// Session.HasPathMappingRules is always set to "true" or "false".
// Session.PathMappingRulesFile is set to pathMapFile only when hasPathMap is
// true; when there are no rules the key is left absent so a reference to it
// fails loudly rather than resolving to an empty path.
func addPathMappingKeys(m fmtstring.MapScope, pathMapFile string, hasPathMap bool) {
	if hasPathMap {
		m["Session.HasPathMappingRules"] = "true"
		m["Session.PathMappingRulesFile"] = pathMapFile
	} else {
		m["Session.HasPathMappingRules"] = "false"
	}
}

// ── Embedded files ────────────────────────────────────────────────────────────

// EmbeddedFileName computes the on-disk filename for an embedded file: it uses
// f.Filename when set, falling back to f.Name. Filenames containing path
// separators or null bytes are rejected to prevent directory traversal (a
// defense-in-depth check, since the assignment is server-signed).
//
// This is the single source of truth for the materialized filename, shared by
// the embedded-file writer and by [AddFileVars], so the path placed into
// Task.File.* / Env.File.* matches exactly where the file is written.
func EmbeddedFileName(f protocol.EmbeddedFile) (string, error) {
	name := f.Filename
	if name == "" {
		name = f.Name
	}
	if name == "" {
		return "", errors.New("embedded file has no name or filename")
	}
	if filepath.Base(name) != name {
		return "", fmt.Errorf("filename %q must not contain path separators", name)
	}
	if strings.ContainsRune(name, 0) {
		return "", fmt.Errorf("filename %q must not contain null bytes", name)
	}
	return name, nil
}

// AddFileVars sets "<prefix>.<name>" in scope to the absolute on-disk path of
// each embedded file's materialized location (filepath.Join(workDir, filename),
// where filename is derived via [EmbeddedFileName]). prefix is "Task.File" or
// "Env.File"; the key uses each file's Name. workDir is the absolute session
// working directory, so the resulting values are absolute paths.
//
// If two files share the same Name, the later entry overwrites the earlier in
// the scope (last-wins); callers should ensure unique names upstream.
//
// AddFileVars returns an error naming the offending file when a filename is
// invalid. The scope is mutated in place.
func AddFileVars(scope fmtstring.MapScope, prefix string, files []protocol.EmbeddedFile, workDir string) error {
	for _, f := range files {
		name, err := EmbeddedFileName(f)
		if err != nil {
			return fmt.Errorf("embedded file %q: %w", f.Name, err)
		}
		scope[prefix+"."+f.Name] = filepath.Join(workDir, name)
	}
	return nil
}

// ResolveEmbeddedFiles returns copies of files with each file's Data resolved
// against scope. All other fields (Name, Filename, Runnable, EndOfLine) are
// copied unchanged. A nil slice returns (nil, nil).
//
// ResolveEmbeddedFiles returns a descriptive error — naming the offending file
// and reference — when a file's data is malformed or names a variable the scope
// does not provide. The input slice and its elements are never mutated.
func ResolveEmbeddedFiles(files []protocol.EmbeddedFile, scope fmtstring.Scope) ([]protocol.EmbeddedFile, error) {
	if files == nil {
		return nil, nil
	}
	out := make([]protocol.EmbeddedFile, len(files))
	for i, f := range files {
		data, err := fmtstring.Resolve(f.Data, scope)
		if err != nil {
			return nil, fmt.Errorf("embedded file %q: %w", f.Name, err)
		}
		f.Data = data // f is a copy; the original element is untouched
		out[i] = f
	}
	return out, nil
}

// ResolveAction returns a copy of action with its Command and every Args entry
// resolved against scope. All other fields (TimeoutSeconds, Cancelation) are
// copied unchanged. A nil action returns (nil, nil).
//
// ResolveAction returns a descriptive error — naming the offending reference —
// if any reference is malformed or names a variable the scope does not provide.
// The input action is never mutated.
func ResolveAction(action *protocol.Action, scope fmtstring.Scope) (*protocol.Action, error) {
	if action == nil {
		return nil, nil
	}

	cmd, err := fmtstring.Resolve(action.Command, scope)
	if err != nil {
		return nil, fmt.Errorf("command %q: %w", action.Command, err)
	}

	var args []string
	if action.Args != nil {
		args = make([]string, len(action.Args))
		for i, a := range action.Args {
			r, rerr := fmtstring.Resolve(a, scope)
			if rerr != nil {
				return nil, fmt.Errorf("arg %d %q: %w", i, a, rerr)
			}
			args[i] = r
		}
	}

	out := *action // shallow copy preserves TimeoutSeconds, Cancelation pointer
	out.Command = cmd
	out.Args = args
	return &out, nil
}

// ResolveVars returns a copy of vars with every value resolved against scope.
// Keys are copied unchanged. A nil map returns (nil, nil).
//
// ResolveVars returns a descriptive error — naming both the offending variable
// key and the reference — if any value is malformed or names a variable the
// scope does not provide. The input map is never mutated.
func ResolveVars(vars map[string]string, scope fmtstring.Scope) (map[string]string, error) {
	if vars == nil {
		return nil, nil
	}
	out := make(map[string]string, len(vars))
	for k, v := range vars {
		r, err := fmtstring.Resolve(v, scope)
		if err != nil {
			return nil, fmt.Errorf("variable %q: %w", k, err)
		}
		out[k] = r
	}
	return out, nil
}
