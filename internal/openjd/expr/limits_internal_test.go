// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"errors"
	"testing"
)

func TestCheckElementCount(t *testing.T) {
	tests := []struct {
		name    string
		n       int
		wantErr bool
	}{
		{"zero", 0, false},
		{"one", 1, false},
		{"at the limit", maxElements, false},
		{"one over", maxElements + 1, true},
		{"negative means overflow", -1, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := checkElementCount(tc.n)
			if tc.wantErr != (err != nil) {
				t.Fatalf("checkElementCount(%d) = %v, wantErr %v", tc.n, err, tc.wantErr)
			}
			if err != nil && !errors.Is(err, errTooLarge) {
				t.Fatalf("checkElementCount(%d) = %v, want it to wrap errTooLarge", tc.n, err)
			}
		})
	}
}

func TestCheckStringBytes(t *testing.T) {
	tests := []struct {
		name    string
		n       int
		wantErr bool
	}{
		{"zero", 0, false},
		{"one", 1, false},
		{"at the limit", maxStringBytes, false},
		{"one over", maxStringBytes + 1, true},
		{"negative means overflow", -1, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := checkStringBytes(tc.n)
			if tc.wantErr != (err != nil) {
				t.Fatalf("checkStringBytes(%d) = %v, wantErr %v", tc.n, err, tc.wantErr)
			}
			if err != nil && !errors.Is(err, errTooLarge) {
				t.Fatalf("checkStringBytes(%d) = %v, want it to wrap errTooLarge", tc.n, err)
			}
		})
	}
}
