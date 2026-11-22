// SPDX-License-Identifier: AGPL-3.0-or-later

package configinit

import (
	"fmt"
	"os"
	"testing"
)

// TestMain expands the temporary directory to its long name before any test
// runs. config init writes and prints the canonical, long-name spelling of a
// path, so a test comparing it with a t.TempDir path would fail on a host
// whose TEMP is an 8.3 short name, such as C:\Users\RUNNER~1 on a GitHub
// runner.
func TestMain(m *testing.M) {
	long, err := longPathName(os.TempDir())
	if err != nil {
		fmt.Fprintln(os.Stderr, "configinit tests: long name of the temporary directory:", err)
		os.Exit(1)
	}
	for _, key := range []string{"TMP", "TEMP"} {
		if err := os.Setenv(key, long); err != nil {
			fmt.Fprintln(os.Stderr, "configinit tests:", err)
			os.Exit(1)
		}
	}
	os.Exit(m.Run())
}
