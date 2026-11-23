// SPDX-License-Identifier: AGPL-3.0-or-later

package secretin

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// TestReadLine_PipedInputUnchanged pins the scripted/harness provisioning
// path, stdin that is not an interactive console (a strings.Reader here
// stands in for a redirected file or a piped process, neither of which is an
// *os.File wrapping a real console): it reads the line up to '\n' with
// trailing CR/LF stripped and no attempt to suppress echo. Both platforms take
// this path: Windows whenever GetConsoleMode fails (secretin_windows.go),
// POSIX unconditionally (secretin_other.go).
func TestReadLine_PipedInputUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
		want  string
	}{
		{"lf", "hunter2\n", "hunter2"},
		{"crlf", "hunter2\r\n", "hunter2"},
		{"no-trailing-newline", "hunter2", "hunter2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := &cobra.Command{}
			cmd.SetIn(strings.NewReader(tc.input))

			got, err := ReadLine(cmd)
			if err != nil {
				t.Fatalf("ReadLine: %v", err)
			}
			if got != tc.want {
				t.Errorf("ReadLine = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestReadLine_EmptyInputErrors proves an immediately-closed stdin
// (EOF with nothing read) surfaces as an error rather than a silent empty
// secret — set-credential's own caller then reports "secret must not be
// empty" for the "read something, but it was blank" case; this is the
// distinct "read nothing at all" case.
func TestReadLine_EmptyInputErrors(t *testing.T) {
	cmd := &cobra.Command{}
	cmd.SetIn(strings.NewReader(""))

	if _, err := ReadLine(cmd); err == nil {
		t.Error("ReadLine on empty input = nil error, want an error (EOF with nothing read)")
	}
}
