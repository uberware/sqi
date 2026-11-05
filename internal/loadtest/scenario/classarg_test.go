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
