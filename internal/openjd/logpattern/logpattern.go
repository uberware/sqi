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
// compiles to ~226,000 instructions and takes seconds to match one 4 KiB line,
// on the reader goroutine that a canceled task waits for. Realistic patterns
// compile to 10 to 100 instructions; 2000 leaves room for a long alternation of
// literals.
const MaxProgramInsts = 2000

// EstimateCeiling is the estimate above which Compile rejects a pattern without
// compiling it. Measuring a program means building it, at a cost proportional to
// its size, which a submission would repeat for every pattern it carries. The
// estimate tracks the real size closely (see estimate), and twice the bound
// leaves room for where it overstates, so a pattern within MaxProgramInsts is
// never refused by it.
const EstimateCeiling = 2 * MaxProgramInsts

// TooLargeError reports a pattern whose compiled program exceeds
// MaxProgramInsts.
type TooLargeError struct {
	// Insts is the size of the pattern's compiled program, or its estimate
	// when Estimated is set.
	Insts int
	// Estimated reports that the program was too large to be worth building,
	// so Insts is estimate's figure rather than a count.
	Estimated bool
}

func (e *TooLargeError) Error() string {
	about := ""
	if e.Estimated {
		about = "about "
	}
	return fmt.Sprintf("compiles to %s%d instructions; at most %d are allowed", about, e.Insts, MaxProgramInsts)
}

// Estimate parses p and estimates the size of its compiled program without
// building it, at a cost linear in len(p). A syntax error is returned exactly
// as regexp.Compile would return it.
func Estimate(p string) (int, error) {
	re, err := syntax.Parse(p, syntax.Perl)
	if err != nil {
		return 0, err
	}
	return estimate(re), nil
}

// maxEstimate caps estimate's arithmetic. The parser already bounds nested
// repetition, so a real pattern stays far below it; the cap only keeps the
// products from overflowing.
const maxEstimate = 1 << 30

// estimate returns the approximate number of instructions re compiles to, as
// regexp/syntax's own parser estimates it before compiling (calcSize in
// parse.go): a repeat costs its upper bound times its operand, so the
// expansion Simplify would perform is counted rather than built.
func estimate(re *syntax.Regexp) int {
	sub := func(i int) int64 { return int64(estimate(re.Sub[i])) }
	var n int64
	switch re.Op {
	case syntax.OpLiteral:
		n = int64(len(re.Rune))
	case syntax.OpCapture, syntax.OpStar:
		n = 2 + sub(0)
	case syntax.OpPlus, syntax.OpQuest:
		n = 1 + sub(0)
	case syntax.OpConcat, syntax.OpAlternate:
		for i := range re.Sub {
			n = min(n+sub(i), maxEstimate)
		}
		if re.Op == syntax.OpAlternate {
			n += int64(len(re.Sub) - 1)
		}
	case syntax.OpRepeat:
		s := sub(0)
		switch {
		case re.Max == -1 && re.Min == 0:
			n = 2 + s
		case re.Max == -1:
			n = 1 + int64(re.Min)*s
		default:
			n = int64(re.Max)*s + int64(re.Max-re.Min)
		}
	}
	return int(min(max(n, 1), maxEstimate))
}

// Compile compiles p as regexp.Compile does, but first measures its program
// and returns a *TooLargeError, without building the matcher, when it exceeds
// MaxProgramInsts. A program whose estimate exceeds EstimateCeiling is refused
// without being built at all. A syntax error is returned exactly as
// regexp.Compile would return it.
func Compile(p string) (*regexp.Regexp, error) {
	re, err := syntax.Parse(p, syntax.Perl)
	if err != nil {
		return nil, err
	}
	if n := estimate(re); n > EstimateCeiling {
		return nil, &TooLargeError{Insts: n, Estimated: true}
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
