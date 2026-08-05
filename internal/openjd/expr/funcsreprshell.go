// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import "strings"

// reprShellFuncs is sub-project C3's SHELL quoting group: repr_sh, repr_cmd
// and repr_pwsh.
//
// It is separated from repr_py and repr_json (funcsreprdata.go) on purpose.
// Everything in this file produces text that will be EXECUTED as part of a
// command line, so a quoting bug here is a command-injection bug. The two
// serialization functions next door produce data, where the same class of bug
// is merely malformed output.
var reprShellFuncs = map[string][]Shape{
	"repr_sh": {
		{Params: []Type{TString}, Ret: TString, Fn: func(args []Value) (Value, error) {
			return boundedString(shellQuote(args[0].AsStr()))
		}},
		{Params: []Type{TPath}, Ret: TString, Fn: func(args []Value) (Value, error) {
			return boundedString(shellQuote(pathText(args[0])))
		}},
		{Params: []Type{ListOf(TString)}, Ret: TString, Fn: func(args []Value) (Value, error) {
			return joinRendered(args[0].AsList(), " ", func(v Value) string { return shellQuote(v.AsStr()) })
		}},
		{Params: []Type{ListOf(TPath)}, Ret: TString, Fn: func(args []Value) (Value, error) {
			return joinRendered(args[0].AsList(), " ", func(v Value) string { return shellQuote(pathText(v)) })
		}},
	},
	"repr_cmd": {
		{Params: []Type{TString}, Ret: TString, Fn: func(args []Value) (Value, error) {
			return boundedString(cmdQuote(args[0].AsStr()))
		}},
		{Params: []Type{ListOf(TString)}, Ret: TString, Fn: func(args []Value) (Value, error) {
			return joinRendered(args[0].AsList(), " ", func(v Value) string { return cmdQuote(v.AsStr()) })
		}},
	},
	"repr_pwsh": {
		{Params: []Type{TString}, Ret: TString, Fn: func(args []Value) (Value, error) {
			return boundedString(pwshQuote(args[0].AsStr()))
		}},
		{Params: []Type{TPath}, Ret: TString, Fn: func(args []Value) (Value, error) {
			return boundedString(pwshQuote(pathText(args[0])))
		}},
		{Params: []Type{TRangeExpr}, Ret: TString, Fn: func(args []Value) (Value, error) {
			return boundedString(pwshQuote(args[0].String()))
		}},
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
		{Params: []Type{ListOf(varT)}, Ret: TString, Fn: func(args []Value) (Value, error) {
			body, err := joinRendered(args[0].AsList(), ", ", pwshElement)
			if err != nil {
				return Value{}, err
			}
			return boundedString("@(" + body.AsStr() + ")")
		}},
	},
}

// shellQuote is Python's shlex.quote, which RFC 0006 names as repr_sh's
// behavior.
//
// The safe set is shlex's own, and it is ASCII-ONLY on purpose: shlex tests it
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
// The newline stripping comes FIRST and is a security rule rather than a
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
// CodeList gets its OWN case rather than falling to the default: without it,
// a nested list renders as sqi's own "[a, b]" text quoted as a single
// PowerShell STRING — repr_pwsh([['a'],['b']]) used to give
// "@('[a]', '[b]')" — a nested list of TEXT, not a nested array, and not
// runnable PowerShell for anything that expects array elements. Recursing
// through pwshElement instead builds a nested "@(...)" literal, matching how
// the top-level ListOf(varT) row itself is built.
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
		parts := make([]string, len(v.AsList()))
		for i, elem := range v.AsList() {
			parts[i] = pwshElement(elem)
		}
		return "@(" + strings.Join(parts, ", ") + ")"
	default:
		return pwshQuote(v.String())
	}
}

// joinRendered renders each value and joins with sep, bounding the result
// before building it.
//
// The separator contribution goes through checkRepeat for the same reason C2's
// join does: len(sep) * (n-1) is exactly the quantity being bounded, and
// forming that product first is not a check at all.
func joinRendered(vals []Value, sep string, render func(Value) string) (Value, error) {
	if len(vals) == 0 {
		return String(""), nil
	}
	total, err := checkRepeat(len(sep), int64(len(vals)-1), maxStringBytes)
	if err != nil {
		return Value{}, err
	}
	parts := make([]string, len(vals))
	for i, v := range vals {
		parts[i] = render(v)
		total += int64(len(parts[i]))
		if err := checkStringBytes(int(total)); err != nil {
			return Value{}, err
		}
	}
	return String(strings.Join(parts, sep)), nil
}
