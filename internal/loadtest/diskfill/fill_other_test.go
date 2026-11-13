// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !unix

package diskfill

import (
	"errors"
	"testing"
)

func TestFill_UnsupportedOffUnix(t *testing.T) {
	b, err := Fill(t.TempDir(), 0, false)
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Fill = %v, want ErrUnsupported", err)
	}
	if err := b.Free(); err != nil {
		t.Errorf("Free on the nil Ballast = %v", err)
	}
}
