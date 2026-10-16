// SPDX-License-Identifier: AGPL-3.0-or-later

package storecopy

import "testing"

func TestDigest_SameRowsSameDigestAcrossRepresentations(t *testing.T) {
	a, b := newDigest(), newDigest()
	// SQLite reads a boolean as int64 0/1; PostgreSQL returns bool.
	a.row([]any{"id-1", int64(1), nil, "2026-10-12T00:00:00.000000000Z", int64(0)})
	b.row([]any{"id-1", true, nil, []byte("2026-10-12T00:00:00.000000000Z"), false})
	if a.sum() != b.sum() {
		t.Fatal("equal rows digest differently")
	}
}

func TestDigest_BoundariesCannotCollide(t *testing.T) {
	a, b := newDigest(), newDigest()
	a.row([]any{"ab", "c"})
	b.row([]any{"a", "bc"})
	if a.sum() == b.sum() {
		t.Fatal("length-prefixing failed: field boundaries collide")
	}
	c, d := newDigest(), newDigest()
	c.row([]any{nil})
	d.row([]any{""})
	if c.sum() == d.sum() {
		t.Fatal("NULL and empty string digest the same")
	}
	e, f := newDigest(), newDigest()
	e.row([]any{int64(1)})
	f.row([]any{"1"})
	if e.sum() == f.sum() {
		t.Fatal("an integer and its decimal text digest the same")
	}
}

func TestDigest_RowsAndOrderCount(t *testing.T) {
	a, b := newDigest(), newDigest()
	a.row([]any{"x"})
	a.row([]any{"y"})
	b.row([]any{"y"})
	b.row([]any{"x"})
	if a.sum() == b.sum() {
		t.Fatal("the same rows in another order digest the same")
	}
	c, d := newDigest(), newDigest()
	c.row([]any{"x", "y"})
	d.row([]any{"x"})
	d.row([]any{"y"})
	if c.sum() == d.sum() {
		t.Fatal("one two-column row and two one-column rows digest the same")
	}
}

func TestDigest_AnUnknownTypeNeverMatches(t *testing.T) {
	a, b := newDigest(), newDigest()
	a.row([]any{1.5})
	b.row([]any{"1.5"})
	if a.sum() == b.sum() {
		t.Fatal("a float digests as its text; an unexpected type must fail verification")
	}
}
