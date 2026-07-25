// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/sys/windows"
)

// readSecretLine reads one line from cmd's stdin for `isolation
// set-credential`. When stdin is a real, interactive Windows console, the
// line is read with echo disabled so the secret never lands in the console
// screen buffer (and, transitively, in a screen-scrape or a terminal's
// scrollback). When stdin is not a console — redirected from a file or a
// pipe, exactly how the test harness and scripted provisioning invoke this
// command — GetConsoleMode fails and this falls through to the same plain
// line read the command has always used, unchanged.
//
// golang.org/x/term is not a dependency of this module and this is a narrow
// enough need not to add one: golang.org/x/sys/windows (already a direct
// dependency) exposes GetConsoleMode/SetConsoleMode directly.
func readSecretLine(cmd *cobra.Command) (string, error) {
	stdin := cmd.InOrStdin()
	f, ok := stdin.(*os.File)
	if !ok {
		return readSecretLineEcho(stdin)
	}

	handle := windows.Handle(f.Fd())
	var mode uint32
	if err := windows.GetConsoleMode(handle, &mode); err != nil {
		// Not a console — a redirected file or pipe. Preserve the existing
		// scripted/harness behavior exactly.
		return readSecretLineEcho(stdin)
	}

	noEcho := mode &^ windows.ENABLE_ECHO_INPUT
	if err := windows.SetConsoleMode(handle, noEcho); err != nil {
		return "", fmt.Errorf("disable console echo: %w", err)
	}
	defer func() {
		_ = windows.SetConsoleMode(handle, mode) //nolint:errcheck // best-effort: restore echo even if the read below fails; nothing actionable if it fails
	}()

	secret, err := readSecretLineEcho(stdin)
	// With ENABLE_ECHO_INPUT cleared the console does not advance the
	// cursor to a new line when Enter is pressed the way it would for an
	// echoed keystroke, so the next line printed (the "stored credential
	// for ..." confirmation, or a shell prompt if this errors out) would
	// otherwise run into the "Password for ...: " prompt. Print the newline
	// explicitly to match what typing anything else at this prompt would
	// have produced.
	fmt.Fprintln(cmd.OutOrStdout())
	return secret, err
}

// readSecretLineEcho is the plain line read set-credential has always used —
// no attempt to suppress terminal echo. It is the entire behavior on POSIX
// (see readsecret_other.go) and the fallback here whenever stdin is not an
// interactive Windows console.
func readSecretLineEcho(r io.Reader) (string, error) {
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}
