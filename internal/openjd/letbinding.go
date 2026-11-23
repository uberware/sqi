// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import (
	"errors"
	"fmt"
	"strings"
)

// maxLetBindingNameLength is the <UserIdentifier> length bound from Template
// Schemas 3.6.1: 1-512 characters.
const maxLetBindingNameLength = 512

// letBindingWS is Template Schemas 3.6.1's <WS>: tab or space, and nothing
// broader. It is passed to strings.Trim rather than using a general
// "whitespace" cutset (which would also strip newlines) because <WS> is
// scoped to exactly these two characters.
const letBindingWS = " \t"

// parseLetBinding splits one raw <LetBinding> string into its bound name and
// unparsed expression source. Template Schemas 3.6.1:
//
//	<LetBinding>     ::= <UserIdentifier><WS>*"="<WS>*<Expression>
//	<UserIdentifier> ::= [a-z_][A-Za-z0-9_]*
//	<WS>             ::= tab or space
//
// The spec states that the lowercase-first rule on <UserIdentifier> ensures
// user-defined names never collide with the spec-defined symbols (Param,
// Task, Session, Env, RawParam), which start with an uppercase letter.
// Symbol lookup relies on that to tell a let-bound name from a spec symbol.
//
// The name/expression split happens at the first "=" byte in s:
// <UserIdentifier> admits no "=", so the first one always ends the name, no
// matter how many the expression contains ("x = a == b" yields the
// expression "a == b"). Only the <WS> around the name and around the "=" is
// trimmed; the expression's interior spacing is returned as written.
//
// parseLetBinding is more lenient than the grammar at the string's outer
// edges: 3.6.1 places <WS>* only around the "=", but leading and trailing
// space on the whole raw binding (e.g. from YAML indentation) is trimmed
// too. This leniency is intended; do not turn it into a rejection.
//
// The name is checked by a hand-written scan rather than a regular
// expression: 3.6.1 states three rules for <UserIdentifier> (the first
// character, the subsequent characters, the length), and the scan reports
// which one a name broke.
//
// Errors returned here carry no "openjd: " prefix and, for an invalid name,
// do not restate the raw binding text: the caller (checkLetBindings,
// exprcheck.go) wraps the result as
// ValidationError{Pointer: ptr, Message: err.Error()}, and the pointer
// identifies which let: entry failed. errRangeTooLarge in range.go follows
// the same rule.
func parseLetBinding(s string) (name, exprSrc string, err error) {
	before, after, found := strings.Cut(s, "=")
	if !found {
		return "", "", fmt.Errorf("let binding %q: must be of the form \"name = expression\"", s)
	}

	rawName := strings.Trim(before, letBindingWS)
	exprSrc = strings.Trim(after, letBindingWS)

	if verr := validateLetBindingName(rawName); verr != nil {
		return "", "", verr
	}
	if exprSrc == "" {
		return "", "", fmt.Errorf("let binding %q: missing expression after \"=\"", s)
	}

	return rawName, exprSrc, nil
}

// validateLetBindingName checks name against Template Schemas 3.6.1's
// <UserIdentifier> ::= [a-z_][A-Za-z0-9_]*, 1-512 characters. It scans by
// hand, rule by rule, so the returned error names the specific rule a name
// violates rather than reporting a single undifferentiated mismatch.
func validateLetBindingName(name string) error {
	if name == "" {
		return errors.New("name must start with a lowercase letter or underscore")
	}
	if c := name[0]; !isLetBindingNameStart(c) {
		return fmt.Errorf("name %q must start with a lowercase letter or underscore", name)
	}
	for i := 1; i < len(name); i++ {
		if c := name[i]; !isLetBindingNameCont(c) {
			return fmt.Errorf("name %q: character %q must be a letter, digit, or underscore", name, c)
		}
	}
	if len(name) > maxLetBindingNameLength {
		return fmt.Errorf("name %q is %d characters, must be at most %d characters", name, len(name), maxLetBindingNameLength)
	}
	return nil
}

// isLetBindingNameStart reports whether c is a legal first character of a
// <UserIdentifier>: [a-z_].
func isLetBindingNameStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z')
}

// isLetBindingNameCont reports whether c is a legal non-first character of a
// <UserIdentifier>: [A-Za-z0-9_].
func isLetBindingNameCont(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}
