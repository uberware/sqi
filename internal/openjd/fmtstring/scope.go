// SPDX-License-Identifier: AGPL-3.0-or-later

package fmtstring

// Scope resolves a fully-qualified OpenJD variable name (for example
// "Task.Param.Frame") to its string value. Lookup reports ok=false when the
// name is not present in the scope; callers must distinguish a missing name
// from a name whose value is the empty string.
type Scope interface {
	Lookup(name string) (value string, ok bool)
}

// MapScope is a [Scope] backed by a plain map from fully-qualified variable
// name to value.
type MapScope map[string]string

// Lookup implements [Scope].
func (m MapScope) Lookup(name string) (string, bool) {
	v, ok := m[name]
	return v, ok
}

// multiScope tries each underlying scope in order and returns the first match.
type multiScope []Scope

// MultiScope composes scopes into a single [Scope]. Lookup consults the scopes
// in the order given and returns the value from the first scope that contains
// the name, so earlier scopes take precedence over later ones.
func MultiScope(scopes ...Scope) Scope {
	return multiScope(scopes)
}

// Lookup implements [Scope].
func (ms multiScope) Lookup(name string) (string, bool) {
	for _, s := range ms {
		if v, ok := s.Lookup(name); ok {
			return v, true
		}
	}
	return "", false
}
