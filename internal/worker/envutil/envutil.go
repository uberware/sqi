// SPDX-License-Identifier: AGPL-3.0-or-later

// Package envutil provides shared helpers for building process environment
// variable slices from os.Environ() plus a caller-supplied override map.
//
// It is intentionally small and depends only on the standard library so that
// any sub-package of internal/worker can import it without creating a cycle.
package envutil

import (
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Build constructs a process environment slice by starting from os.Environ()
// and applying overrides on top.  Keys present in overrides take precedence
// over the inherited environment.  When overrides is empty the raw
// os.Environ() slice is returned directly (no allocation).
//
// The output format is "KEY=VALUE", matching exec.Cmd.Env expectations.
// Output order is non-deterministic (map iteration); callers must not rely on
// any specific ordering.
func Build(overrides map[string]string) []string {
	if len(overrides) == 0 {
		return os.Environ()
	}
	return flattenEnv(mergedEnv(overrides, nil))
}

// BuildWithUnset is like [Build] but additionally removes every key in unset
// from the resulting environment — including keys inherited from os.Environ()
// and keys supplied via overrides.  This implements OpenJD's openjd_unset_env
// directive, where a variable must be absent from a subsequent action's
// environment even if it was set by the worker's own environment or by a
// static environment Variables entry.
//
// Removal is applied after overrides, so a key present in both overrides and
// unset is removed.  When unset is empty the call is equivalent to [Build].
//
// The output format is "KEY=VALUE"; output order is non-deterministic.
func BuildWithUnset(overrides map[string]string, unset map[string]bool) []string {
	if len(unset) == 0 {
		return Build(overrides)
	}
	return flattenEnv(mergedEnv(overrides, unset))
}

// mergedEnv indexes os.Environ() into a map keyed by variable name, applies
// overrides on top (which take precedence), then removes every key in unset.
// unset may be nil. Entries with no '=' separator (pathological hosts) map the
// whole entry to an empty value, matching strings.Cut.
func mergedEnv(overrides map[string]string, unset map[string]bool) map[string]string {
	base := os.Environ()
	merged := make(map[string]string, len(base)+len(overrides))
	for _, kv := range base {
		k, v, _ := strings.Cut(kv, "=")
		merged[k] = v
	}
	maps.Copy(merged, overrides)
	for k := range unset {
		delete(merged, k)
	}
	return merged
}

// flattenEnv renders an environment map as a "KEY=VALUE" slice. Output order is
// non-deterministic (map iteration).
func flattenEnv(merged map[string]string) []string {
	out := make([]string, 0, len(merged))
	for k, v := range merged {
		out = append(out, k+"="+v)
	}
	return out
}

// minimalBase is the set of variables always inherited from the daemon when a
// Policy is enabled, because a process cannot reasonably run without them.
// HOME/USERPROFILE are inherited here but MUST be rewritten by the caller to the
// target user's home when a credential is in effect — otherwise DCCs write
// their configs into a directory the task cannot access.
var minimalBase = map[string]bool{
	// POSIX
	"PATH": true, "HOME": true, "TMPDIR": true, "LANG": true, "SHELL": true,
	// Windows. Keys are uppercase because allowedName looks them up through
	// foldEnvName, which uppercases on Windows. COMSPEC and PATHEXT are not
	// optional: without them nothing that resolves a command through a shell
	// works, which is most of what a render task does. The rest are how
	// installed software is located. All are machine-scoped; none carries a
	// secret. APPDATA/LOCALAPPDATA are deliberately ABSENT — they must be
	// derived from the target's token, not inherited from the daemon (see
	// session.Manager.Create), or every DCC writes its preferences into
	// SYSTEM's profile.
	"USERPROFILE": true, "TEMP": true, "TMP": true, "SYSTEMROOT": true,
	"COMSPEC": true, "PATHEXT": true, "WINDIR": true, "SYSTEMDRIVE": true,
	"PROGRAMDATA": true, "PROGRAMFILES": true, "PROGRAMFILES(X86)": true,
	"COMMONPROGRAMFILES": true, "NUMBER_OF_PROCESSORS": true,
	"PROCESSOR_ARCHITECTURE": true,
}

// Policy controls how much of the worker daemon's own environment is inherited
// by job-supplied processes.
//
// Enabled is false in the pre-isolation configuration, where the entire daemon
// environment is inherited — the historical behavior. When true, only
// minimalBase plus Allowlist survive, which is what stops daemon credentials
// (AWS keys, mount secrets, license tokens) reaching arbitrary job code.
type Policy struct {
	// Allowlist holds variable NAME patterns inherited in addition to
	// minimalBase, matched with filepath.Match semantics. Matching is
	// case-insensitive on Windows, mirroring that platform's own rules.
	Allowlist []string
	// Enabled reports whether filtering applies at all.
	Enabled bool
}

// BaseEnv returns the portion of the daemon environment a job-supplied process
// may inherit. It governs ONLY inherited variables; variables the job itself
// supplies are never filtered — see BuildFromBase.
func BaseEnv(p Policy) map[string]string {
	raw := os.Environ()
	out := make(map[string]string, len(raw))
	for _, kv := range raw {
		k, v, _ := strings.Cut(kv, "=")
		if !p.Enabled || allowedName(k, p.Allowlist) {
			out[k] = v
		}
	}
	return out
}

// allowedName reports whether an inherited variable name survives filtering.
//
// Case handling follows each platform's real rules rather than a convenient
// uniform fold: POSIX environment names are case-sensitive, so uppercasing here
// would allowlist a variable literally named "home" or "path" as though it were
// HOME or PATH — a filter that does not do what it claims.
func allowedName(name string, allowlist []string) bool {
	if minimalBase[foldEnvName(name)] {
		return true
	}
	for _, pat := range allowlist {
		if ok, err := filepath.Match(foldEnvName(pat), foldEnvName(name)); err == nil && ok {
			return true
		}
	}
	return false
}

// foldEnvName normalises an environment variable name for comparison: Windows
// treats them case-insensitively, POSIX does not.
func foldEnvName(name string) string {
	if runtime.GOOS == "windows" {
		return strings.ToUpper(name)
	}
	return name
}

// BuildFromBase renders a process environment from an already-filtered base,
// applying job-supplied overrides on top and then removing every key in unset.
//
// Filtering happens exactly once, when base is built by BaseEnv at session
// creation. Everything in overrides is job data — static Environment.variables,
// dynamic openjd_env exports, task template variables — and passes through
// UNTOUCHED. Re-filtering here would eat a job's own exports, whose names no
// operator allowlist would contain.
func BuildFromBase(base, overrides map[string]string, unset map[string]bool) []string {
	merged := make(map[string]string, len(base)+len(overrides))
	maps.Copy(merged, base)
	maps.Copy(merged, overrides)
	for k := range unset {
		delete(merged, k)
	}
	return flattenEnv(merged)
}
