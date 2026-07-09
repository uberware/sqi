// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package capabilities

import (
	"strings"

	"golang.org/x/sys/windows/registry"
)

// registryExists reports whether a "HIVE\\Subkey" path exists. Only HKLM/HKCU
// are supported; anything else returns false.
func registryExists(key string) bool {
	hive, sub, ok := strings.Cut(key, `\`)
	if !ok {
		return false
	}
	var root registry.Key
	switch strings.ToUpper(hive) {
	case "HKLM", "HKEY_LOCAL_MACHINE":
		root = registry.LOCAL_MACHINE
	case "HKCU", "HKEY_CURRENT_USER":
		root = registry.CURRENT_USER
	default:
		return false
	}
	k, err := registry.OpenKey(root, sub, registry.READ)
	if err != nil {
		return false
	}
	_ = k.Close()
	return true
}
