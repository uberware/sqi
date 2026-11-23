// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import "strings"

// reprShellFuncs is the repr_* shell quoting group: repr_sh, repr_cmd
// and repr_pwsh.
//
// It is separated from repr_py and repr_json (funcsreprdata.go): everything in
// this file produces text that will be executed as part of a command line, so a
// quoting bug here is a command-injection bug. The two serialization functions
// next door produce data, where the same class of bug is merely malformed
// output.
//
// Cost: section 1.3.10 rule 2 names all five repr_* functions by name
// ("repr_sh(), repr_py(), repr_json(), repr_pwsh(), repr_cmd()"), and rule 3
// additionally names repr_sh() by name. Every string/path row here declares
// Cost{ArgBytes: []int{0}}, and every list row declares Cost{ArgElements:
// []int{0}}, on all five repr_* functions. The reference charges the same plus
// one unit for the string it builds, which sqi does not (rule 3 charges what
// a function processes, not what it produces); see
// test/oracle/baseline-ops.txt's repr_* section and
// TestOperationCount_ReprFunctionsListChargesElementsDespiteReferenceOmission.
var reprShellFuncs = map[string][]Shape{
	"repr_sh": {
		{Params: []Type{TString}, Ret: TString, Cost: Cost{ArgBytes: []int{0}}, Fn: func(args []Value) (Value, error) {
			return boundedString(shellQuote(args[0].AsStr()))
		}},
		{Params: []Type{TPath}, Ret: TString, Cost: Cost{ArgBytes: []int{0}}, Fn: func(args []Value) (Value, error) {
			return boundedString(shellQuote(pathText(args[0])))
		}},
		{Params: []Type{ListOf(TString)}, Ret: TString, Cost: Cost{ArgElements: []int{0}}, Fn: func(args []Value) (Value, error) {
			return joinValues(args[0].AsList(), " ", func(v Value) string { return shellQuote(v.AsStr()) })
		}},
		{Params: []Type{ListOf(TPath)}, Ret: TString, Cost: Cost{ArgElements: []int{0}}, Fn: func(args []Value) (Value, error) {
			return joinValues(args[0].AsList(), " ", func(v Value) string { return shellQuote(pathText(v)) })
		}},
	},
	"repr_cmd": {
		{Params: []Type{TString}, Ret: TString, Cost: Cost{ArgBytes: []int{0}}, Fn: func(args []Value) (Value, error) {
			return boundedString(cmdQuote(args[0].AsStr()))
		}},
		// Divergence from the reference: see this var block's own cost comment.
		{Params: []Type{ListOf(TString)}, Ret: TString, Cost: Cost{ArgElements: []int{0}}, Fn: func(args []Value) (Value, error) {
			return joinValues(args[0].AsList(), " ", func(v Value) string { return cmdQuote(v.AsStr()) })
		}},
	},
	"repr_pwsh": {
		{Params: []Type{TString}, Ret: TString, Cost: Cost{ArgBytes: []int{0}}, Fn: func(args []Value) (Value, error) {
			return boundedString(pwshQuote(args[0].AsStr()))
		}},
		{Params: []Type{TPath}, Ret: TString, Cost: Cost{ArgBytes: []int{0}}, Fn: func(args []Value) (Value, error) {
			return boundedString(pwshQuote(pathText(args[0])))
		}},
		// Cost{}: neither rule fires for a range_expr operand. Rule 3 covers
		// only "a string or path value" -- a range_expr is neither -- and the
		// reference agrees: repr_pwsh(range_expr(...)) stays flat at the same
		// count for a 7-byte range text and a 4444-byte one, confirmed in the
		// probe.
		{Params: []Type{TRangeExpr}, Ret: TString, Fn: func(args []Value) (Value, error) {
			return boundedString(pwshQuote(args[0].String()))
		}},
		// Cost{}: int/float/bool render from a fixed-shape payload with no
		// input to scan; confirmed flat at 1 (rule 1 only) in the reference.
		{Params: []Type{TInt}, Ret: TString, Fn: func(args []Value) (Value, error) {
			return String(args[0].String()), nil
		}},
		{Params: []Type{TFloat}, Ret: TString, Fn: func(args []Value) (Value, error) {
			return String(args[0].String()), nil
		}},
		{Params: []Type{TBool}, Ret: TString, Fn: func(args []Value) (Value, error) {
			if args[0].AsBool() {
				return String("$true"), nil
			}
			return String("$false"), nil
		}},
		// Divergence from the reference: see this var block's own cost comment.
		{Params: []Type{ListOf(varT)}, Ret: TString, Cost: Cost{ArgElements: []int{0}}, Fn: func(args []Value) (Value, error) {
			elems := args[0].AsList()
			body, err := joinValues(elems, ", ", pwshElement)
			if err != nil {
				return Value{}, err
			}
			return boundedString("@(" + pwshUnaryComma(elems) + body.AsStr() + ")")
		}},
	},
}

// shellQuote is Python's shlex.quote, which RFC 0006 names as repr_sh's
// behavior.
//
// The safe set is shlex's own, and it is ASCII-only: shlex tests it
// with re.ASCII, so any non-ASCII character forces quoting. Reproducing that
// matters, because "café" quoted and "café" bare are different arguments to a
// shell with a different locale.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if !strings.ContainsFunc(s, shellUnsafe) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

func shellUnsafe(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return false
	case strings.ContainsRune("_@%+=:,./-", r):
		return false
	}
	return true
}

// cmdQuote implements RFC 0006's cmd.exe rules verbatim.
//
// The newline stripping comes first and is a security rule rather than a
// formatting one: cmd.exe has no escape sequence for a literal newline inside a
// quoted argument, so anything after one would be parsed as a new command. The
// spec calls stripping "the only safe option".
func cmdQuote(s string) string {
	s = strings.NewReplacer("\n", "", "\r", "").Replace(s)
	if s != "" && !strings.ContainsAny(s, "&|<>^\"()%! \t") {
		return s
	}
	// Inside the quotes: ^ and " take a caret prefix, % doubles for .bat
	// contexts, and ! becomes ^^! because cmd.exe processes caret escapes
	// before delayed expansion.
	r := strings.NewReplacer("^", "^^", "\"", "^\"", "%", "%%", "!", "^^!")
	return `"` + r.Replace(s) + `"`
}

// pwshQuote is PowerShell's single-quoted literal: the only escape inside one
// is a doubled quote.
func pwshQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// pwshElement renders one member of a PowerShell array literal.
//
// CodeList gets its own case rather than falling to the default: without it, a
// nested list renders as sqi's own "[a, b]" text quoted as a single PowerShell
// string — repr_pwsh([['a'],['b']]) would give "@('[a]', '[b]')" — a nested
// list of text, not a nested array, and not runnable PowerShell for anything
// that expects array elements. Recursing through pwshElement instead builds a
// nested "@(...)" literal, matching how the top-level ListOf(varT) row itself
// is built.
//
// A nested list also picks up its own unary comma when it needs one, since
// the decision is per-list; see pwshUnaryComma.
func pwshElement(v Value) string {
	switch v.Type.Code {
	case CodeBool:
		if v.AsBool() {
			return "$true"
		}
		return "$false"
	case CodeInt, CodeFloat:
		return v.String()
	case CodeNull:
		return "$null"
	case CodeList:
		elems := v.AsList()
		parts := make([]string, len(elems))
		for i, elem := range elems {
			parts[i] = pwshElement(elem)
		}
		return "@(" + pwshUnaryComma(elems) + strings.Join(parts, ", ") + ")"
	default:
		return pwshQuote(v.String())
	}
}

// pwshUnaryComma returns the "," that section 2.2.6 requires in front of a
// one-element array whose only element is itself an array, and "" for every
// other list.
//
// PowerShell flattens "@(@(1, 2))" to "@(1, 2)", so the nesting is lost on
// the round trip; the unary comma operator, "@(,@(1, 2))", preserves it. The
// test is on the element count, not on depth: "@(@(1, 2), @(3))" needs no
// comma because two elements already force an array, and a one-element list
// of scalars must not get one -- "@('a')" is unambiguous already.
//
// openjd-specifications#176 states this rule outright; the reference
// implementation refuses lists nested more than two deep, so it cannot answer
// the recursive case at all.
func pwshUnaryComma(elems []Value) string {
	if len(elems) == 1 && elems[0].Type.Code == CodeList {
		return ","
	}
	return ""
}
