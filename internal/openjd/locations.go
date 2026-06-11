// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import (
	"fmt"
	"regexp"
	"strings"
)

// ── Named-location URI syntax ─────────────────────────────────────────────────
//
// sqi uses a "loc://" URI scheme to embed named storage location references
// inside OpenJD template strings (command arguments, environment variables,
// job/task parameters, and embedded-file data).
//
// Syntax:
//
//	loc://<location-name>/<relative-path>
//
// Examples:
//
//	loc://nas_shows/projects/hero/scene.hip
//	loc://s3_output/renders/hero/beauty.####.exr
//
// At task-dispatch time sqi rewrites every loc:// reference to a concrete
// local path by looking up the named storage location's root for the target
// worker's compute location (or "default" if no compute-location-specific
// root is registered).
//
// This mechanism implements "resolved" path-translation mode (§8.3 of
// sqi.md): the worker receives only concrete paths and requires no OpenJD
// path-mapping support.

// locURIRE matches a loc:// URI and captures the location name and the
// remainder of the path (which may be empty for a bare root reference).
var locURIRE = regexp.MustCompile(`loc://([^/\s"']+)(/[^\s"']*)?`)

// LocRef is a single named-location reference extracted from a template string.
type LocRef struct {
	// LocationName is the symbolic storage-location name (e.g. "nas_shows").
	LocationName string
	// RelPath is the path portion after the location name, including the
	// leading slash, or empty for a bare root reference (e.g. "loc://nas_shows").
	RelPath string
	// Raw is the full original URI string.
	Raw string
}

// ExtractLocRefs scans s for all loc:// URI occurrences and returns them
// in order of appearance.  Duplicates are not deduplicated.
func ExtractLocRefs(s string) []LocRef {
	matches := locURIRE.FindAllStringSubmatch(s, -1)
	if len(matches) == 0 {
		return nil
	}
	refs := make([]LocRef, 0, len(matches))
	for _, m := range matches {
		refs = append(refs, LocRef{
			LocationName: m[1],
			RelPath:      m[2],
			Raw:          m[0],
		})
	}
	return refs
}

// ResolveLocURIs replaces every loc:// URI in s with the concrete path
// returned by resolve(locationName, relPath).  relPath includes the leading
// slash; it is empty when the URI is a bare "loc://name" reference.
//
// resolve is called once per unique URI occurrence; if resolve returns an error
// the function aborts and returns the error.
func ResolveLocURIs(s string, resolve func(name, relPath string) (string, error)) (string, error) {
	var firstErr error
	result := locURIRE.ReplaceAllStringFunc(s, func(raw string) string {
		if firstErr != nil {
			return raw
		}
		m := locURIRE.FindStringSubmatch(raw)
		if m == nil {
			return raw
		}
		concrete, err := resolve(m[1], m[2])
		if err != nil {
			firstErr = err
			return raw
		}
		return concrete
	})
	if firstErr != nil {
		return "", firstErr
	}
	return result, nil
}

// ExtractTemplateLocRefs returns the deduplicated set of location names
// referenced anywhere in the given job template (command args, environment
// variables, embedded file data, and task/job parameter defaults).
//
// Only the location names are returned; use [ExtractLocRefs] when the full
// [LocRef] details are needed.
func ExtractTemplateLocRefs(tmpl *JobTemplate) []string {
	seen := make(map[string]struct{})

	add := func(s string) {
		for _, ref := range ExtractLocRefs(s) {
			seen[ref.LocationName] = struct{}{}
		}
	}

	// Job parameter defaults.
	for _, p := range tmpl.ParameterDefinitions {
		if p.Default != nil {
			add(*p.Default)
		}
	}

	// Job environments.
	for _, e := range tmpl.JobEnvironments {
		addEnvRefs(e, add)
	}

	// Steps.
	for _, st := range tmpl.Steps {
		addStepRefs(st, add)
	}

	if len(seen) == 0 {
		return nil
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	return names
}

// ExtractStepLocRefs returns the deduplicated set of location names referenced
// in a single step (its script, environments, and parameter space range lists).
func ExtractStepLocRefs(st StepTemplate) []string {
	seen := make(map[string]struct{})
	addStepRefs(st, func(s string) {
		for _, ref := range ExtractLocRefs(s) {
			seen[ref.LocationName] = struct{}{}
		}
	})
	if len(seen) == 0 {
		return nil
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	return names
}

// addEnvRefs scans an environment for loc:// references and passes each
// raw string through add.
func addEnvRefs(e Environment, add func(string)) {
	for _, v := range e.Variables {
		add(v)
	}
	if e.Script != nil {
		addActionRefs(e.Script.Actions.OnEnter, add)
		addActionRefs(e.Script.Actions.OnExit, add)
		for _, f := range e.Script.EmbeddedFiles {
			add(f.Data)
		}
	}
}

// addActionRefs scans an action for loc:// references.
func addActionRefs(a *Action, add func(string)) {
	if a == nil {
		return
	}
	add(a.Command)
	for _, arg := range a.Args {
		add(arg)
	}
}

// addStepRefs scans a step template for loc:// references.
func addStepRefs(st StepTemplate, add func(string)) {
	if st.Script != nil {
		addActionRefs(&st.Script.Actions.OnRun, add)
		for _, f := range st.Script.EmbeddedFiles {
			add(f.Data)
		}
	}
	for _, e := range st.StepEnvironments {
		addEnvRefs(e, add)
	}
	if st.ParameterSpace != nil {
		for _, tp := range st.ParameterSpace.TaskParameterDefinitions {
			for _, v := range tp.RangeList {
				add(v)
			}
		}
	}
}

// ── Concrete-path resolution ──────────────────────────────────────────────────

// ResolveRoot returns the concrete root path for locationRoots given a
// compute location name.  It prefers the compute-location-specific root,
// falls back to "default", and returns an error if neither is present.
func ResolveRoot(locationName string, roots map[string]string, computeLocation string) (string, error) {
	if computeLocation != "" {
		if root, ok := roots[computeLocation]; ok {
			return root, nil
		}
	}
	if root, ok := roots["default"]; ok {
		return root, nil
	}
	return "", fmt.Errorf(
		"storage location %q has no root for compute location %q and no default root",
		locationName, computeLocation,
	)
}

// JoinPath joins a resolved root with the relative path portion of a loc://
// URI.  relPath includes the leading slash (or is empty).
//
// For S3 roots (e.g. "s3://bucket/prefix"), path separators are kept as "/".
// For filesystem roots, the OS-appropriate separator is used.
func JoinPath(root, relPath string) string {
	if relPath == "" {
		return root
	}
	// Strip leading slash from relPath for joining.
	rel := strings.TrimPrefix(relPath, "/")
	if rel == "" {
		return root
	}
	// S3 URIs always use forward slashes.
	if strings.HasPrefix(root, "s3://") {
		return strings.TrimRight(root, "/") + "/" + rel
	}
	// Filesystem: use the root's separator style.
	sep := "/"
	if strings.ContainsRune(root, '\\') && !strings.ContainsRune(root, '/') {
		sep = `\`
	}
	return strings.TrimRight(root, `/\`) + sep + strings.ReplaceAll(rel, "/", sep)
}
