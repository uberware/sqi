// SPDX-License-Identifier: AGPL-3.0-or-later

package envutil

import "testing"

// TestBaseEnv_WindowsEssentialsSurviveFiltering checks that the minimal base
// carries the variables a Windows process needs. Without COMSPEC and PATHEXT
// nothing that resolves a command through a shell works, which is most of
// what a render task does.
//
// It is in package envutil, not envutil_test, because minimalBase is
// unexported.
func TestBaseEnv_WindowsEssentialsSurviveFiltering(t *testing.T) {
	for _, name := range []string{
		"COMSPEC", "PATHEXT", "WINDIR", "SYSTEMDRIVE", "PROGRAMDATA",
		"PROGRAMFILES", "COMMONPROGRAMFILES", "NUMBER_OF_PROCESSORS",
		"PROCESSOR_ARCHITECTURE",
	} {
		if !minimalBase[name] {
			t.Errorf("minimalBase is missing %q; an isolated Windows task cannot run without it", name)
		}
	}
}

// TestBaseEnv_SecretsAreStillFiltered checks that the minimal base admits no
// daemon credentials.
func TestBaseEnv_SecretsAreStillFiltered(t *testing.T) {
	for _, name := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "SQI_WORKER_TOKEN"} {
		if minimalBase[name] {
			t.Errorf("minimalBase admits %q; daemon secrets must never reach job code", name)
		}
	}
}
