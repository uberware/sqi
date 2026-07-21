// SPDX-License-Identifier: AGPL-3.0-or-later

package ldap

import (
	"context"
	"errors"

	ldapv3 "github.com/go-ldap/ldap/v3"
)

// bindKey identifies one bind attempt by BOTH the DN and the password.
//
// Keying on the DN alone would make the password invisible to every
// assertion: a verifier that re-bound the user with the *service account's*
// password — authenticating everyone as long as the service credentials were
// right — would still satisfy a DN-only fake. The credential is the one thing
// a bind actually proves, so it belongs in the key.
type bindKey struct {
	DN string
	PW string
}

// fakeConn is a scripted conn. Every verifier test drives this rather than a
// real directory, so the suite needs no network and no container.
type fakeConn struct {
	// bindErr maps a (DN, password) pair to the error its bind returns; a pair
	// absent from the map binds successfully. Keying on the pair is what makes
	// binding with the wrong password observable.
	bindErr map[bindKey]error
	// binds records every (DN, password) bound, in order — the assertion
	// surface for "did it re-bind as the user, with the USER's password?".
	binds []bindKey
	// ops records binds and searches in one ordered log ("bind:<dn>",
	// "search:<filter>"), so a test can assert not just that an operation
	// happened but that it happened on the right side of a bind — which is
	// what "the nested lookup runs on the service connection" means.
	ops []string
	// searchResult is returned by every Search call not overridden by
	// searchResultAt.
	searchResult *ldapv3.SearchResult
	// searchResultAt overrides searchResult for specific 1-based Search call
	// indexes. It exists so a test can let the user search hit while the
	// nested-group expansion returns a successful but EMPTY result — the
	// misscoped-base_dn case, which no error field can express because there
	// is no error.
	searchResultAt map[int]*ldapv3.SearchResult
	// searchErr, when set, is returned by Search calls from searchErrFrom on.
	searchErr error
	// searchErrFrom is the 1-based index of the first Search call searchErr
	// applies to; 0 means every call. It exists so a test can fail only the
	// nested-group lookup while letting the user search succeed.
	searchErrFrom int
	// searchFilters records the filter of every Search call, so escaping can
	// be asserted at the point it actually matters.
	searchFilters []string
	// searchAttrs records the requested attribute list of every Search call.
	// entryUUID is an OPERATIONAL attribute that most directories omit unless
	// it is named explicitly, so "did the request ask for it?" is a distinct
	// question from "did the parse read it?" and needs its own surface.
	searchAttrs [][]string
	closed      bool
}

func (f *fakeConn) Bind(dn, pw string) error {
	f.binds = append(f.binds, bindKey{DN: dn, PW: pw})
	f.ops = append(f.ops, "bind:"+dn)
	if err, ok := f.bindErr[bindKey{DN: dn, PW: pw}]; ok {
		return err
	}
	return nil
}

func (f *fakeConn) Search(req *ldapv3.SearchRequest) (*ldapv3.SearchResult, error) {
	f.searchFilters = append(f.searchFilters, req.Filter)
	f.searchAttrs = append(f.searchAttrs, req.Attributes)
	f.ops = append(f.ops, "search:"+req.Filter)
	if f.searchErr != nil && len(f.searchFilters) >= f.searchErrFrom {
		return nil, f.searchErr
	}
	if r, ok := f.searchResultAt[len(f.searchFilters)]; ok {
		return r, nil
	}
	if f.searchResult == nil {
		return &ldapv3.SearchResult{}, nil
	}
	return f.searchResult, nil
}

func (f *fakeConn) Close() error {
	f.closed = true
	return nil
}

// dialTo returns a dialFunc handing out the given conn.
func dialTo(c conn) dialFunc {
	return func(context.Context) (conn, error) { return c, nil }
}

// dialFail returns a dialFunc that always fails, simulating an unreachable
// domain controller.
func dialFail() dialFunc {
	return func(context.Context) (conn, error) { return nil, errors.New("dial: connection refused") }
}

// entry builds a search-result entry with the given DN and attributes.
//
// Built through ldapv3.NewEntryAttribute so both Values and ByteValues are
// populated, which is what go-ldap does when it decodes a real response.
// Setting only Values would make every raw-value read return nothing here and
// something on the wire — precisely the class of fake/real divergence this
// package's tests are supposed to rule out.
func entry(dn string, attrs map[string][]string) *ldapv3.Entry {
	e := &ldapv3.Entry{DN: dn}
	for name, vals := range attrs {
		e.Attributes = append(e.Attributes, ldapv3.NewEntryAttribute(name, vals))
	}
	return e
}

// withRawAttr appends an attribute whose value is raw bytes rather than text,
// modeling Active Directory's objectGUID: a 16-byte octet string that is not
// valid UTF-8 and therefore cannot be carried in Values at all.
func withRawAttr(e *ldapv3.Entry, name string, raw []byte) *ldapv3.Entry {
	e.Attributes = append(e.Attributes, &ldapv3.EntryAttribute{
		Name:       name,
		ByteValues: [][]byte{raw},
	})
	return e
}
