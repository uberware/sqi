// SPDX-License-Identifier: AGPL-3.0-or-later

package store_test

import (
	"slices"
	"testing"

	"github.com/uberware/sqi/internal/store"
)

func TestSearchTerms(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"   ", nil},
		{"Foo", []string{"foo"}},
		{"  Foo   BAR ", []string{"foo", "bar"}},
	}
	for _, tc := range cases {
		got := store.SearchTerms(tc.in)
		if !slices.Equal(got, tc.want) {
			t.Errorf("SearchTerms(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestMatchesSearch(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		query  string
		fields []string
		want   bool
	}{
		{"empty query matches", "", []string{"anything"}, true},
		{"single substring", "foo", []string{"fool"}, true},
		{"case-insensitive", "FOO", []string{"fool"}, true},
		// The "foo bar" rule: each word is an independent substring, ANDed.
		{"both terms in one field", "foo bar", []string{"fool-grabbing-rebar"}, true},
		{"reversed order in one field", "foo bar", []string{"barfoo"}, true},
		{"order-independent", "bar foo", []string{"barfoo"}, true},
		{"terms spread across fields", "foo bar", []string{"a foo thing", "some rebar"}, true},
		{"one term matches, other misses", "foo zzz", []string{"fool", "bar"}, false},
		{"no field matches", "foo", []string{"bar", "baz"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := store.MatchesSearch(tc.query, tc.fields...); got != tc.want {
				t.Errorf("MatchesSearch(%q, %v) = %v, want %v", tc.query, tc.fields, got, tc.want)
			}
		})
	}
}
