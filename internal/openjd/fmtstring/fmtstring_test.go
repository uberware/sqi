// SPDX-License-Identifier: AGPL-3.0-or-later

package fmtstring_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/openjd/fmtstring"
)

func TestResolve(t *testing.T) {
	t.Parallel()

	scope := fmtstring.MapScope{
		"Param.FrameStart":             "1",
		"Param.FrameEnd":               "10",
		"Param.Empty":                  "",
		"RawParam.Foo":                 "bar",
		"Task.Param.Frame":             "7",
		"Task.RawParam.Frame":          "7",
		"Task.File.script":             "/tmp/script.sh",
		"Env.File.env":                 "/tmp/env",
		"Session.WorkingDirectory":     "/work",
		"Session.HasPathMappingRules":  "true",
		"Session.PathMappingRulesFile": "/work/rules.json",
	}

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "no references literal",
			input: "just some literal text",
			want:  "just some literal text",
		},
		{
			name:  "empty string",
			input: "",
			want:  "",
		},
		{
			name:  "single reference no whitespace",
			input: "{{Param.FrameStart}}",
			want:  "1",
		},
		{
			name:  "single reference with inner whitespace",
			input: "{{ Param.FrameStart }}",
			want:  "1",
		},
		{
			name:  "inner whitespace equals no whitespace",
			input: "{{   Param.FrameEnd   }}",
			want:  "10",
		},
		{
			name:  "reference surrounded by literal text",
			input: "frame=  {{ Param.FrameStart }}  end",
			want:  "frame=  1  end",
		},
		{
			name:  "multiple references",
			input: "{{Param.FrameStart}}-{{Param.FrameEnd}}",
			want:  "1-10",
		},
		{
			name:  "adjacent references",
			input: "{{Param.FrameStart}}{{Param.FrameEnd}}",
			want:  "110",
		},
		{
			name:  "dotted multi-segment task param",
			input: "{{ Task.Param.Frame }}",
			want:  "7",
		},
		{
			name:  "session working directory",
			input: "cd {{ Session.WorkingDirectory }}",
			want:  "cd /work",
		},
		{
			name:  "task file",
			input: "bash {{Task.File.script}}",
			want:  "bash /tmp/script.sh",
		},
		{
			name:  "raw param",
			input: "{{ RawParam.Foo }}",
			want:  "bar",
		},
		{
			name:  "env file",
			input: "{{Env.File.env}}",
			want:  "/tmp/env",
		},
		{
			name:  "session has path mapping rules",
			input: "{{Session.HasPathMappingRules}}",
			want:  "true",
		},
		{
			name:  "session path mapping rules file",
			input: "{{Session.PathMappingRulesFile}}",
			want:  "/work/rules.json",
		},
		{
			name:  "lone closing braces are literal",
			input: "echo }} done",
			want:  "echo }} done",
		},
		{
			name:  "closing braces before a reference",
			input: "}}{{Param.FrameStart}}",
			want:  "}}1",
		},
		{
			name:  "empty string scope value",
			input: "prefix{{Param.Empty}}suffix",
			want:  "prefixsuffix",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := fmtstring.Resolve(tc.input, scope)
			if err != nil {
				t.Fatalf("Resolve(%q) returned error: %v", tc.input, err)
			}
			if got != tc.want {
				t.Errorf("Resolve(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestResolveUnknownVariable(t *testing.T) {
	t.Parallel()

	scope := fmtstring.MapScope{"Param.Known": "1"}

	got, err := fmtstring.Resolve("{{ Param.Unknown }}", scope)
	if err == nil {
		t.Fatalf("Resolve returned no error, got %q", got)
	}
	if !strings.Contains(err.Error(), "Param.Unknown") {
		t.Errorf("error %q does not name the offending variable Param.Unknown", err.Error())
	}

	var unresolved *fmtstring.UnresolvedError
	if !errors.As(err, &unresolved) {
		t.Fatalf("error is not *UnresolvedError: %T", err)
	}
	if want := []string{"Param.Unknown"}; !reflect.DeepEqual(unresolved.Names, want) {
		t.Errorf("UnresolvedError.Names = %v, want %v", unresolved.Names, want)
	}
}

func TestResolveMultipleUnknownVariables(t *testing.T) {
	t.Parallel()

	scope := fmtstring.MapScope{}

	_, err := fmtstring.Resolve("{{Param.A}} {{Param.B}} {{Param.A}}", scope)
	if err == nil {
		t.Fatal("Resolve returned no error")
	}

	var unresolved *fmtstring.UnresolvedError
	if !errors.As(err, &unresolved) {
		t.Fatalf("error is not *UnresolvedError: %T", err)
	}
	// Verify first-appearance ordering: A before B, deduplicated.
	want := []string{"Param.A", "Param.B"}
	if !reflect.DeepEqual(unresolved.Names, want) {
		t.Errorf("UnresolvedError.Names = %v, want %v (first-appearance order, deduplicated)", unresolved.Names, want)
	}
	if !strings.Contains(err.Error(), "Param.A") || !strings.Contains(err.Error(), "Param.B") {
		t.Errorf("error %q should mention both unknown variables", err.Error())
	}
}

func TestResolveMalformed(t *testing.T) {
	t.Parallel()

	scope := fmtstring.MapScope{"Param.X": "1"}

	tests := []struct {
		name  string
		input string
	}{
		{name: "unclosed reference", input: "{{ Param.X"},
		{name: "unclosed at end after text", input: "prefix {{Param.X"},
		{name: "empty reference", input: "{{}}"},
		{name: "whitespace-only reference", input: "{{   }}"},
		{name: "invalid identifier char", input: "{{ Param.X! }}"},
		{name: "leading digit segment", input: "{{ Param.1bad }}"},
		{name: "trailing dot", input: "{{ Param. }}"},
		{name: "leading dot", input: "{{ .Param }}"},
		{name: "empty middle segment", input: "{{ Task..Frame }}"},
		{name: "space inside name", input: "{{ Param.Foo Bar }}"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := fmtstring.Resolve(tc.input, scope)
			if err == nil {
				t.Fatalf("Resolve(%q) returned no error", tc.input)
			}
			var malformed *fmtstring.MalformedError
			if !errors.As(err, &malformed) {
				t.Fatalf("Resolve(%q) error is not *MalformedError: %T (%v)", tc.input, err, err)
			}
		})
	}
}

func TestReferences(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  []string
	}{
		{
			name:  "no references",
			input: "literal only",
			want:  nil,
		},
		{
			name:  "single reference",
			input: "{{ Param.Foo }}",
			want:  []string{"Param.Foo"},
		},
		{
			name:  "multiple references in order",
			input: "{{Param.A}}-{{ Task.Param.B }}-{{Session.WorkingDirectory}}",
			want:  []string{"Param.A", "Task.Param.B", "Session.WorkingDirectory"},
		},
		{
			name:  "duplicates collapsed",
			input: "{{Param.A}} {{Param.A}} {{Param.B}}",
			want:  []string{"Param.A", "Param.B"},
		},
		{
			name:  "literal closing braces ignored",
			input: "}} {{Param.A}} }}",
			want:  []string{"Param.A"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := fmtstring.References(tc.input)
			if err != nil {
				t.Fatalf("References(%q) returned error: %v", tc.input, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("References(%q) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}

func TestReferencesMalformed(t *testing.T) {
	t.Parallel()

	for _, input := range []string{"{{ Param.X", "{{}}", "{{ bad! }}"} {
		_, err := fmtstring.References(input)
		if err == nil {
			t.Errorf("References(%q) returned no error", input)
		}
		var malformed *fmtstring.MalformedError
		if !errors.As(err, &malformed) {
			t.Errorf("References(%q) error is not *MalformedError: %T (%v)", input, err, err)
		}
	}
}

func TestMapScope(t *testing.T) {
	t.Parallel()

	scope := fmtstring.MapScope{"Param.A": "value"}

	if v, ok := scope.Lookup("Param.A"); !ok || v != "value" {
		t.Errorf("Lookup(Param.A) = %q, %v; want value, true", v, ok)
	}
	if _, ok := scope.Lookup("Param.Missing"); ok {
		t.Error("Lookup(Param.Missing) returned ok=true")
	}

	// Empty-string value should still be found.
	empty := fmtstring.MapScope{"Param.Empty": ""}
	if v, ok := empty.Lookup("Param.Empty"); !ok || v != "" {
		t.Errorf("Lookup(Param.Empty) = %q, %v; want \"\", true", v, ok)
	}
}

func TestMultiScopePrecedence(t *testing.T) {
	t.Parallel()

	first := fmtstring.MapScope{"Param.A": "first", "Param.Only1": "1"}
	second := fmtstring.MapScope{"Param.A": "second", "Param.Only2": "2"}

	scope := fmtstring.MultiScope(first, second)

	tests := []struct {
		name   string
		want   string
		wantOK bool
		lookup string
	}{
		{name: "first scope wins", lookup: "Param.A", want: "first", wantOK: true},
		{name: "only in first", lookup: "Param.Only1", want: "1", wantOK: true},
		{name: "only in second", lookup: "Param.Only2", want: "2", wantOK: true},
		{name: "in neither", lookup: "Param.None", want: "", wantOK: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := scope.Lookup(tc.lookup)
			if got != tc.want || ok != tc.wantOK {
				t.Errorf("Lookup(%q) = %q, %v; want %q, %v", tc.lookup, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestMultiScopeEmpty(t *testing.T) {
	t.Parallel()

	scope := fmtstring.MultiScope()
	if _, ok := scope.Lookup("anything"); ok {
		t.Error("empty MultiScope returned ok=true")
	}
}
