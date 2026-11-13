// SPDX-License-Identifier: AGPL-3.0-or-later

package scenario

import "testing"

func TestClassFromArgs(t *testing.T) {
	tests := []struct {
		name   string
		args   []string
		want   string
		wantOK bool
	}{
		{"none", []string{"-x", "frame-1"}, "", false},
		{"nil", nil, "", false},
		{"one", []string{"-x", ClassArg("shot-render")}, "shot-render", true},
		{"the last wins", []string{ClassArg("a"), "-x", ClassArg("b")}, "b", true},
		{"empty class", []string{ClassArg("")}, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ClassFromArgs(tt.args)
			if got != tt.want || ok != tt.wantOK {
				t.Errorf("ClassFromArgs(%q) = %q, %v; want %q, %v", tt.args, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestKeyArg_RoundTrip(t *testing.T) {
	tests := []struct {
		name   string
		args   []string
		want   string
		wantOK bool
	}{
		{"none", []string{"-x", ClassArg("a")}, "", false},
		{"one", []string{KeyArg("j1/step-1/3"), ClassArg("a")}, "j1/step-1/3", true},
		{"the last wins", []string{KeyArg("a"), "-x", KeyArg("b")}, "b", true},
		{"empty key", []string{KeyArg("")}, "", true},
		{"a class arg is not a key", []string{ClassArg("k")}, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := KeyFromArgs(tt.args)
			if got != tt.want || ok != tt.wantOK {
				t.Errorf("KeyFromArgs(%q) = %q, %v; want %q, %v", tt.args, got, ok, tt.want, tt.wantOK)
			}
		})
	}
	if got := KeyArg("k"); got != KeyArgPrefix+"k" {
		t.Errorf("KeyArg(k) = %q", got)
	}
}

func TestTaskKey(t *testing.T) {
	if got := TaskKey("j1", "step-2", "7", ""); got != "j1/step-2/7" {
		t.Errorf("TaskKey without a block = %q, want j1/step-2/7", got)
	}
	if got := TaskKey("j1", "step-2", "7", "3"); got != "j1/step-2/7/3" {
		t.Errorf("TaskKey with block 3 = %q, want j1/step-2/7/3", got)
	}
}
