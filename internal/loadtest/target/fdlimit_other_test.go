// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !linux

package target

import (
	"errors"
	"testing"
)

func TestLimitServerFDs_Unsupported(t *testing.T) {
	if _, _, err := limitFDs(1, 5); !errors.Is(err, errFDLimitUnsupported) {
		t.Errorf("limitFDs = %v, want errFDLimitUnsupported", err)
	}
	m := newTestManaged(t, laneScenario(false))
	if err := m.RestoreServerFDs(); err != nil {
		t.Errorf("RestoreServerFDs = %v, want nil: Close calls it on every platform", err)
	}
}
