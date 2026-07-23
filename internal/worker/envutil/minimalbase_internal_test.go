// SPDX-License-Identifier: AGPL-3.0-or-later

package envutil

import "testing"

// TestBaseEnv_WindowsEssentialsSurviveFiltering proves the minimal base
// carries the variables a Windows process cannot function without. COMSPEC
// and PATHEXT are the sharpest: without them nothing that resolves a command
// through a shell works at all, which is most of what a render task does.
//
// This lives in an internal (package envutil) test file, rather than
// alongside the rest of the suite in envutil_test.go (package envutil_test),
// because minimalBase is unexported and asserting on it directly requires
// same-package access.
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

// TestBaseEnv_SecretsAreStillFiltered guards the addition above from becoming
// a hole: widening the base must not start admitting daemon credentials.
func TestBaseEnv_SecretsAreStillFiltered(t *testing.T) {
	for _, name := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "SQI_WORKER_TOKEN"} {
		if minimalBase[name] {
			t.Errorf("minimalBase admits %q; daemon secrets must never reach job code", name)
		}
	}
}
