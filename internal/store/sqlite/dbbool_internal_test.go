// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import "testing"

func TestDBBool_Scan(t *testing.T) {
	cases := []struct {
		src     any
		want    bool
		wantErr bool
	}{
		{int64(0), false, false},
		{int64(1), true, false},
		{true, true, false},
		{false, false, false},
		{"1", false, true},
		{nil, false, true},
	}
	for _, c := range cases {
		var b dbBool
		err := b.Scan(c.src)
		if (err != nil) != c.wantErr {
			t.Fatalf("Scan(%#v) error = %v, want error %v", c.src, err, c.wantErr)
		}
		if err == nil && bool(b) != c.want {
			t.Fatalf("Scan(%#v) = %v, want %v", c.src, bool(b), c.want)
		}
	}
}
