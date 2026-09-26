// SPDX-License-Identifier: AGPL-3.0-or-later

// Package secretin reads one line of secret input from a cobra command's
// stdin, with console echo disabled on Windows when stdin is an interactive
// console, and a plain line read otherwise (pipes, redirected files, POSIX).
package secretin

import (
	"bufio"
	"io"
	"strings"
)

// readLineEcho is the plain line read the secret prompt has always used —
// no attempt to suppress terminal echo. It is the entire behavior on POSIX
// (see secretin_other.go) and the fallback on Windows whenever stdin is not an
// interactive console.
func readLineEcho(r io.Reader) (string, error) {
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}
