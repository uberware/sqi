// SPDX-License-Identifier: AGPL-3.0-or-later

// Package envvar reads typed configuration overrides from environment
// variables. sqi-server (internal/config) and sqi-worker
// (internal/worker/config) both overlay their settings through it, so the two
// binaries accept the same spellings and report a malformed variable in the
// same words.
//
// It exists as a leaf because internal/config pulls internal/auth/oidc, so the
// worker binary can never import it. This package imports nothing internal.
//
// Every setter treats an unset or empty variable as a no-op, so the lower
// config layer survives. The typed setters trim surrounding whitespace and
// return an error naming the variable and its value when it does not parse,
// never a silent fallback.
package envvar

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// String sets dst from a string-valued variable, untrimmed.
func String(dst *string, key string) {
	if v := os.Getenv(key); v != "" {
		*dst = v
	}
}

// StringSlice sets dst from a comma-separated variable, trimming spaces and
// dropping empty entries. A variable holding only commas and blanks is a no-op
// like an unset one, so it cannot wipe a list the config file set.
func StringSlice(dst *[]string, key string) {
	var out []string
	for part := range strings.SplitSeq(os.Getenv(key), ",") {
		if s := strings.TrimSpace(part); s != "" {
			out = append(out, s)
		}
	}
	if len(out) > 0 {
		*dst = out
	}
}

// Int sets dst from an integer-valued variable.
func Int(dst *int, key string) error {
	return set(dst, key, "integer", "", strconv.Atoi)
}

// Int64 sets dst from an int64-valued variable.
func Int64(dst *int64, key string) error {
	return set(dst, key, "integer", "", func(s string) (int64, error) {
		return strconv.ParseInt(s, 10, 64)
	})
}

// Duration sets dst from a Go duration variable (e.g. "30s", "5m", "1h").
func Duration(dst *time.Duration, key string) error {
	return set(dst, key, "duration", " (want e.g. 30s, 5m, 1h)", time.ParseDuration)
}

// Bool sets dst from a boolean variable spelled as [ParseBool] accepts. A
// malformed security-relevant flag such as SQI_AUTH_ENABLED=enabled is
// therefore an error rather than silently false.
func Bool(dst *bool, key string) error {
	return set(dst, key, "boolean", " (want 1/true/yes/on or 0/false/no/off)", func(s string) (bool, error) {
		if b, ok := ParseBool(s); ok {
			return b, nil
		}
		return false, errNotBool
	})
}

var errNotBool = errors.New("not a boolean")

// ParseBool reads 1/true/yes/on as true and 0/false/no/off as false, in any
// case. ok is false for anything else, including surrounding whitespace.
func ParseBool(s string) (value, ok bool) {
	switch strings.ToLower(s) {
	case "1", "true", "yes", "on":
		return true, true
	case "0", "false", "no", "off":
		return false, true
	default:
		return false, false
	}
}

func set[T any](dst *T, key, kind, hint string, parse func(string) (T, error)) error {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return nil
	}
	x, err := parse(v)
	if err != nil {
		return fmt.Errorf("%s: invalid %s %q%s", key, kind, v, hint)
	}
	*dst = x
	return nil
}
