// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
)

// FileURI returns a file: URI DSN for the database at path, with sqi's
// busy_timeout, read-only (mode=ro) when readOnly is set. The path is made
// absolute and percent-escaped by url.URL, so a "?", "#", "%" or space in it
// stays part of the name (handed to the driver raw, "?" would start the query
// and open, or create, a truncated name), and a Windows drive path becomes
// file:///C:/..., the form SQLite reads as that drive.
func FileURI(path string, readOnly bool) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	p := filepath.ToSlash(abs)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	query := fmt.Sprintf("_pragma=busy_timeout(%d)", busyTimeoutMS)
	if readOnly {
		query = "mode=ro&" + query
	}
	u := url.URL{Scheme: "file", Path: p, RawQuery: query}
	return u.String(), nil
}

// DSN returns the name to hand the driver for the database at path. The driver
// reads everything after a name's first "?" as its own query, so a path
// holding one is opened by its [FileURI], which escapes it. Any other path is
// handed over unchanged — the driver reads no other character specially in a
// plain name — as are a "file:" URI the caller wrote and an in-memory or
// temporary database.
func DSN(path string) (string, error) {
	if !strings.Contains(path, "?") || isURI(path) || !isSharedPath(path) {
		return path, nil
	}
	return FileURI(path, false)
}

// isURI reports whether path is already a "file:" URI.
func isURI(path string) bool {
	return len(path) >= 5 && strings.EqualFold(path[:5], "file:")
}
