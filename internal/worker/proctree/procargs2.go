// SPDX-License-Identifier: AGPL-3.0-or-later

package proctree

import (
	"bytes"
	"encoding/binary"
	"errors"
)

// errProcArgs2 is a KERN_PROCARGS2 buffer that ends early.
var errProcArgs2 = errors.New("proctree: truncated kern.procargs2 buffer")

// parseProcArgs2 returns the environment from a macOS KERN_PROCARGS2 buffer:
// argc as a native (little-endian) int32, the executable path, NUL padding,
// argc NUL-terminated arguments, then the NUL-terminated environment, which
// ends at the first empty string. It is platform-neutral so it can be tested
// anywhere.
func parseProcArgs2(b []byte) ([]string, error) {
	if len(b) < 4 {
		return nil, errProcArgs2
	}
	argc := int(int32(binary.LittleEndian.Uint32(b))) //nolint:gosec // G115: argc is a C int
	rest := b[4:]
	// The executable path, then its padding.
	i := bytes.IndexByte(rest, 0)
	if i < 0 {
		return nil, errProcArgs2
	}
	rest = rest[i:]
	for len(rest) > 0 && rest[0] == 0 {
		rest = rest[1:]
	}
	for range argc {
		i := bytes.IndexByte(rest, 0)
		if i < 0 {
			return nil, errProcArgs2
		}
		rest = rest[i+1:]
	}
	var env []string
	for len(rest) > 0 {
		i := bytes.IndexByte(rest, 0)
		if i <= 0 {
			break
		}
		env = append(env, string(rest[:i]))
		rest = rest[i+1:]
	}
	return env, nil
}
