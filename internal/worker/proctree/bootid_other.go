// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !linux && !darwin

package proctree

// bootID is empty where the ledger is not swept.
func bootID() (string, error) { return "", nil }
