// SPDX-License-Identifier: AGPL-3.0-or-later

package ldap

import (
	"context"
	"errors"

	ldapv3 "github.com/go-ldap/ldap/v3"
)

// fakeConn is a scripted conn. Every verifier test drives this rather than a
// real directory, so the suite needs no network and no container.
type fakeConn struct {
	// bindErr maps a DN to the error its bind returns; a DN absent from the
	// map binds successfully.
	bindErr map[string]error
	// binds records every DN bound, in order — the assertion surface for
	// "did it re-bind as the user after searching?".
	binds []string
	// searchResult is returned by every Search call.
	searchResult *ldapv3.SearchResult
	// searchErr, when set, is returned by Search calls from searchErrFrom on.
	searchErr error
	// searchErrFrom is the 1-based index of the first Search call searchErr
	// applies to; 0 means every call. It exists so a test can fail only the
	// nested-group lookup while letting the user search succeed.
	searchErrFrom int
	// searchFilters records the filter of every Search call, so escaping can
	// be asserted at the point it actually matters.
	searchFilters []string
	closed        bool
}

func (f *fakeConn) Bind(dn, _ string) error {
	f.binds = append(f.binds, dn)
	if err, ok := f.bindErr[dn]; ok {
		return err
	}
	return nil
}

func (f *fakeConn) Search(req *ldapv3.SearchRequest) (*ldapv3.SearchResult, error) {
	f.searchFilters = append(f.searchFilters, req.Filter)
	if f.searchErr != nil && len(f.searchFilters) >= f.searchErrFrom {
		return nil, f.searchErr
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
func entry(dn string, attrs map[string][]string) *ldapv3.Entry {
	e := &ldapv3.Entry{DN: dn}
	for name, vals := range attrs {
		e.Attributes = append(e.Attributes, &ldapv3.EntryAttribute{Name: name, Values: vals})
	}
	return e
}
