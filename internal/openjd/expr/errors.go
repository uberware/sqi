// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import "fmt"

// Error is a syntax or evaluation error in an expression. It carries the byte
// offset in the source text at which the problem was found, together with the
// source itself, so the rendered message can name a line and column.
//
// The spec requires errors to point at the failing part of an expression, so
// offsets are recorded on tokens by the lexer, carried onto tree nodes by the
// parser, and used by the evaluator to blame the operator that failed rather
// than the start of the expression.
type Error struct {
	// Msg describes what went wrong, without any position prefix.
	Msg string
	// Offset is the byte offset into Src where the problem starts.
	Offset int
	// Src is the expression source the offset refers to.
	Src string

	// err is the wrapped cause, when this Error decorates an operator error
	// with a position. It keeps errors.Is working through the wrap.
	err error
}

func (e *Error) Error() string {
	line, col := LineCol(e.Src, e.Offset)
	if line == 1 {
		return fmt.Sprintf("col %d: %s", col, e.Msg)
	}
	return fmt.Sprintf("line %d, col %d: %s", line, col, e.Msg)
}

// LineCol converts a byte offset into 1-based line and column numbers.
// Columns are counted in runes, not bytes, so a caret placed at the returned
// column lands under the intended character. An out-of-range offset is clamped
// to the start or end of src rather than reported as an error: a bad offset
// must never turn a real diagnostic into a panic.
func LineCol(src string, offset int) (line, col int) {
	if offset < 0 {
		offset = 0
	}
	if offset > len(src) {
		offset = len(src)
	}
	line, col = 1, 1
	for i, r := range src {
		if i >= offset {
			return line, col
		}
		if r == '\n' {
			line++
			col = 1
			continue
		}
		col++
	}
	return line, col
}

// errorAt builds an *Error for a position in src. Every error this package
// returns is constructed here, so every error carries a position.
func errorAt(src string, offset int, format string, args ...any) *Error {
	return &Error{Msg: fmt.Sprintf(format, args...), Offset: offset, Src: src}
}

// Unwrap returns the wrapped cause, or nil when there is none.
func (e *Error) Unwrap() error { return e.err }

// wrapAt decorates an operator error with the position of the operator that
// produced it. The cause is preserved so errors.Is still matches.
func wrapAt(src string, offset int, err error) *Error {
	return &Error{Msg: err.Error(), Offset: offset, Src: src, err: err}
}
