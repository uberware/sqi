// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !linux

package target

import (
	"errors"
	"testing"
)

func TestLimitServerFDs_Unsupported(t *testing.T) {
	m := newTestManaged(t, laneScenario(false))
	if _, err := m.LimitServerFDs(5); !errors.Is(err, errFDLimitUnsupported) {
		t.Errorf("LimitServerFDs = %v, want errFDLimitUnsupported", err)
	}
	if err := m.RestoreServerFDs(); !errors.Is(err, errFDLimitUnsupported) {
		t.Errorf("RestoreServerFDs = %v, want errFDLimitUnsupported", err)
	}
	if err := m.restoreFDsIfLimited(); err != nil {
		t.Errorf("restoreFDsIfLimited = %v, want nil: Close calls it on every platform", err)
	}
}
