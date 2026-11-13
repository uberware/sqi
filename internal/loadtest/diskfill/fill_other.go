// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !unix

package diskfill

// Fill is unsupported on this OS.
func Fill(string, int64, bool) (*Ballast, error) { return nil, ErrUnsupported }
