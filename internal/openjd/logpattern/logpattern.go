// SPDX-License-Identifier: AGPL-3.0-or-later

// Package logpattern compiles an SQI_LOG_CHECKS pattern under the size bound
// that both the server (submission validation, internal/openjd) and the worker
// (the checker it runs against every output line, internal/worker/executor)
// enforce. It is a stdlib-only leaf so that the worker, which must never import
// internal/openjd, shares the bound with it instead of keeping a copy that could
// drift.
package logpattern

import (
	"fmt"
	"regexp"
	"regexp/syntax"
)

// MaxProgramInsts is the most instructions a pattern's compiled program may
// hold.
//
// RE2 matching is linear in the length of the line, but the constant is the
// size of the compiled program: Go bounds nested repetition at 1000 but not
// concatenation, so the 905-byte pattern strings.Repeat(".{0,999}", 113)+"x"
// compiles to ~226,000 instructions and takes seconds to match one 4 KiB line
// (minutes for a 64 KiB one), per pattern, on the reader goroutine that a
// canceled task waits for. Realistic patterns compile to 10 to 100
// instructions; 2000 leaves ample room for a long alternation of literals.
const MaxProgramInsts = 2000

// TooLargeError reports a pattern whose compiled program exceeds
// MaxProgramInsts.
type TooLargeError struct {
	// Insts is the size of the pattern's compiled program.
	Insts int
}

func (e *TooLargeError) Error() string {
	return fmt.Sprintf("compiles to %d instructions; at most %d are allowed", e.Insts, MaxProgramInsts)
}

// Compile compiles p as regexp.Compile does, but first measures its program
// and returns a *TooLargeError, without building the matcher, when it exceeds
// MaxProgramInsts. A syntax error is returned exactly as regexp.Compile would
// return it.
func Compile(p string) (*regexp.Regexp, error) {
	re, err := syntax.Parse(p, syntax.Perl)
	if err != nil {
		return nil, err
	}
	prog, err := syntax.Compile(re.Simplify())
	if err != nil {
		return nil, err
	}
	if n := len(prog.Inst); n > MaxProgramInsts {
		return nil, &TooLargeError{Insts: n}
	}
	return regexp.Compile(p)
}
