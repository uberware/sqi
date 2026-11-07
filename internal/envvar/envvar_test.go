// SPDX-License-Identifier: AGPL-3.0-or-later

package envvar_test

import (
	"strings"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/envvar"
)

const key = "SQI_ENVVAR_TEST"

// TestTypedSetters_MalformedValue pins that a value that does not parse is an
// error naming the variable, its value and its kind, and leaves dst unchanged.
func TestTypedSetters_MalformedValue(t *testing.T) {
	t.Setenv(key, "bogus")
	n, n64, d, b := 7, int64(7), time.Second, true
	for _, tc := range []struct {
		kind string
		err  error
	}{
		{"integer", envvar.Int(&n, key)},
		{"integer", envvar.Int64(&n64, key)},
		{"duration", envvar.Duration(&d, key)},
		{"boolean", envvar.Bool(&b, key)},
	} {
		want := key + ": invalid " + tc.kind + ` "bogus"`
		if tc.err == nil || !strings.HasPrefix(tc.err.Error(), want) {
			t.Errorf("error = %v, want prefix %q", tc.err, want)
		}
	}
	if n != 7 || n64 != 7 || d != time.Second || !b {
		t.Errorf("a malformed value changed dst: %d %d %v %v", n, n64, d, b)
	}
}

// TestTypedSetters_UnsetOrEmptyKeepsLowerLayer pins that an unset, empty or
// all-whitespace variable is a no-op.
func TestTypedSetters_UnsetOrEmptyKeepsLowerLayer(t *testing.T) {
	for _, v := range []string{"", "   "} {
		t.Setenv(key, v)
		n, d, b, s := 7, time.Second, true, "lower"
		if err := envvar.Int(&n, key); err != nil || n != 7 {
			t.Errorf("Int(%q) = %d, %v", v, n, err)
		}
		if err := envvar.Duration(&d, key); err != nil || d != time.Second {
			t.Errorf("Duration(%q) = %v, %v", v, d, err)
		}
		if err := envvar.Bool(&b, key); err != nil || !b {
			t.Errorf("Bool(%q) = %v, %v", v, b, err)
		}
		if v == "" {
			envvar.String(&s, key)
			if s != "lower" {
				t.Errorf("String(%q) = %q", v, s)
			}
		}
	}
}

// TestTypedSetters_TrimWhitespace pins that surrounding whitespace, which a
// unit or Compose file can carry, does not make a value fail.
func TestTypedSetters_TrimWhitespace(t *testing.T) {
	t.Setenv(key, " 12345 ")
	var n int64
	if err := envvar.Int64(&n, key); err != nil || n != 12345 {
		t.Errorf("Int64 = %d, %v; want 12345", n, err)
	}
	t.Setenv(key, " 7s ")
	var d time.Duration
	if err := envvar.Duration(&d, key); err != nil || d != 7*time.Second {
		t.Errorf("Duration = %v, %v; want 7s", d, err)
	}
}

// TestBool_Tokens pins the accepted spellings: 1/true/yes/on and
// 0/false/no/off, any case, surrounding whitespace ignored.
func TestBool_Tokens(t *testing.T) {
	tests := []struct {
		value string
		want  bool
	}{
		{"1", true},
		{"true", true},
		{"TRUE", true},
		{"yes", true},
		{"On", true},
		{" true ", true},
		{"0", false},
		{"false", false},
		{"FALSE", false},
		{"no", false},
		{"Off", false},
		{" off ", false},
	}
	for _, tc := range tests {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv(key, tc.value)
			got := !tc.want
			if err := envvar.Bool(&got, key); err != nil {
				t.Fatalf("Bool: %v", err)
			}
			if got != tc.want {
				t.Errorf("Bool(%q) = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}
