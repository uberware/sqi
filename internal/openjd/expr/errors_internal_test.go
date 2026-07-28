// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import "testing"

func TestLineCol(t *testing.T) {
	tests := []struct {
		name      string
		src       string
		offset    int
		line, col int
	}{
		{"start of input", "1 + 2", 0, 1, 1},
		{"mid line", "1 + 2", 4, 1, 5},
		{"end of input", "1 + 2", 5, 1, 6},
		{"second line", "1 +\n2", 4, 2, 1},
		{"third line", "a\nb\nc", 4, 3, 1},
		{"columns count runes not bytes", "'é' + 1", 3, 1, 3},
		{"negative offset clamps to start", "abc", -5, 1, 1},
		{"offset past end clamps to end", "abc", 99, 1, 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			line, col := LineCol(tt.src, tt.offset)
			if line != tt.line || col != tt.col {
				t.Errorf("LineCol(%q, %d) = %d,%d; want %d,%d",
					tt.src, tt.offset, line, col, tt.line, tt.col)
			}
		})
	}
}

func TestError_Error(t *testing.T) {
	tests := []struct {
		name string
		err  *Error
		want string
	}{
		{
			name: "single line omits the line number",
			err:  &Error{Msg: "unexpected character '@'", Offset: 4, Src: "1 + @"},
			want: "col 5: unexpected character '@'",
		},
		{
			name: "multi line includes the line number",
			err:  &Error{Msg: "unexpected token", Offset: 4, Src: "1 +\n@"},
			want: "line 2, col 1: unexpected token",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.err.Error(); got != tt.want {
				t.Errorf("Error() = %q; want %q", got, tt.want)
			}
		})
	}
}

func TestErrorAt(t *testing.T) {
	err := errorAt("1 + 2", 2, "unsupported operand types for %s", "+")
	if err.Offset != 2 {
		t.Errorf("Offset = %d; want 2", err.Offset)
	}
	if err.Src != "1 + 2" {
		t.Errorf("Src = %q; want %q", err.Src, "1 + 2")
	}
	if got, want := err.Error(), "col 3: unsupported operand types for +"; got != want {
		t.Errorf("Error() = %q; want %q", got, want)
	}
}
