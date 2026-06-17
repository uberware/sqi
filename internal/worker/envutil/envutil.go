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
