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
// The lowercase-first rule on <UserIdentifier> is load-bearing, not
// cosmetic: the spec states its purpose is to ensure user-defined names can
// never collide with the spec-defined symbols (Param, Task, Session, Env,
// RawParam), which always start with an uppercase letter. A later stage
// depends on that property to tell a let-bound name apart from a spec
// symbol without ambiguity, so preserving it here matters beyond this
// function.
//
// The name/expression split happens at the FIRST "=" byte in s, and the
// remainder is never re-scanned for a later "=": <UserIdentifier> admits no
// "=" character, so the first one in the string always ends the name, no
// matter how many more "=" the expression itself contains (e.g.
// "x = a == b" must yield the expression "a == b" intact). Only the
// surrounding <WS> around the name and around the "=" is trimmed; the
// expression's interior spacing is returned exactly as written, since what
// it means is the expression grammar's concern, not this function's.
//
// parseLetBinding is intentionally more lenient than the grammar at the
// string's outer edges: 3.6.1 places <WS>* only around the "=", not before
// <UserIdentifier> or after <Expression>, but leading/trailing space on the
// whole raw binding (e.g. from an author's YAML indentation habits) is
// trimmed anyway. This is a deliberate leniency, not an oversight and not a
// literal grammar reading -- do not "fix" it into a rejection.
//
// parseLetBinding does not use a regular expression to validate the name:
// 3.6.1 states three independent rules for <UserIdentifier> -- the first
// character, the subsequent characters, and the length -- and a
// hand-written scan can say which one a given name broke, instead of
// collapsing three distinct authoring mistakes into one generic mismatch.
//
// Errors returned here carry no "openjd: " prefix and, for an invalid name,
// no restatement of the raw binding text either: this is a leaf function
// whose caller (validateLetExtension's future sibling, in a later task)
// wraps the result as ValidationError{Pointer: ptr, Message: err.Error()},
// and the pointer already identifies which let: entry failed. A leaf error
// that also prefixes itself just duplicates what the caller adds, the same
// reasoning errRangeTooLarge in range.go documents for the same package.
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
