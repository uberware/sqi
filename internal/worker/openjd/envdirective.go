// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import "strings"

// ── Environment directive prefixes ────────────────────────────────────────────

// Recognized OpenJD environment-mutation directive prefixes.  These are emitted
// to stdout by environment onEnter/onExit actions to dynamically modify the
// session environment for all SUBSEQUENT actions.
const (
	prefixEnv         = "openjd_env:"
	prefixRedactedEnv = "openjd_redacted_env:"
	prefixUnsetEnv    = "openjd_unset_env:"
)

// ── EnvOp ─────────────────────────────────────────────────────────────────────

// EnvOpKind classifies an [EnvOp] as either a set (assign/override) or an unset
// (remove) operation on the session environment.
type EnvOpKind int

const (
	// EnvOpSet sets or overrides an environment variable to Value.
	EnvOpSet EnvOpKind = iota
	// EnvOpUnset removes an environment variable.
	EnvOpUnset
)

// EnvOp is a single parsed environment-mutation directive.
//
//   - Kind reports whether the variable is being set or unset.
//   - Name is the (validated) environment variable name.
//   - Value is the variable value (set only; empty for unset).
//   - Redacted is true when the value originated from openjd_redacted_env and
//     therefore must never be written to logs in cleartext.
type EnvOp struct {
	Kind     EnvOpKind
	Name     string
	Value    string
	Redacted bool
}

// ParseEnvDirective classifies a single stdout line as one of the three OpenJD
// environment directives — openjd_env, openjd_redacted_env, openjd_unset_env —
// and returns the parsed mutation. ok is false when the line is not a valid
// environment directive (unrecognized prefix, malformed payload, or an invalid
// environment variable name); callers should log and ignore such lines.
//
// Surrounding whitespace is trimmed. For set directives the payload is split on
// the FIRST '=' so values may themselves contain '='. For unset the payload is
// the bare variable name. Names must be non-empty, start with a non-digit, and
// contain only ASCII letters, digits, and underscores (OpenJD's env-name rule).
func ParseEnvDirective(line string) (EnvOp, bool) {
	line = strings.TrimSpace(line)

	// Order matters only in that each prefix is distinct; check redacted before
	// the plain env prefix for clarity (they do not overlap).
	switch {
	case strings.HasPrefix(line, prefixRedactedEnv):
		return parseSetDirective(strings.TrimPrefix(line, prefixRedactedEnv), true)
	case strings.HasPrefix(line, prefixEnv):
		return parseSetDirective(strings.TrimPrefix(line, prefixEnv), false)
	case strings.HasPrefix(line, prefixUnsetEnv):
		name := strings.TrimSpace(strings.TrimPrefix(line, prefixUnsetEnv))
		if !validEnvName(name) {
			return EnvOp{}, false
		}
		return EnvOp{Kind: EnvOpUnset, Name: name}, true
	}
	return EnvOp{}, false
}

// parseSetDirective parses the "NAME=VALUE" payload of a set directive.
func parseSetDirective(payload string, redacted bool) (EnvOp, bool) {
	name, value, ok := strings.Cut(strings.TrimSpace(payload), "=")
	if !ok || !validEnvName(name) {
		return EnvOp{}, false
	}
	return EnvOp{
		Kind:     EnvOpSet,
		Name:     name,
		Value:    value,
		Redacted: redacted,
	}, true
}

// validEnvName reports whether name is a plausible environment variable name per
// OpenJD's rule: non-empty, first character a non-digit, and every character an
// ASCII letter, digit, or underscore.
func validEnvName(name string) bool {
	if name == "" {
		return false
	}
	for i := range len(name) {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '_':
			// always allowed
		case c >= '0' && c <= '9':
			if i == 0 {
				return false // first character must not be a digit
			}
		default:
			return false
		}
	}
	return true
}
