// SPDX-License-Identifier: AGPL-3.0-or-later

package product

import (
	"embed"
	"fmt"
	"io/fs"
	"slices"
	"sort"

	"github.com/uberware/sqi/internal/store"
)

//go:embed builtins/*.yaml
var builtinFS embed.FS

var (
	builtinList   []store.Product
	builtinByName map[string]store.Product
)

func init() {
	list, byName, err := loadBuiltins()
	if err != nil {
		panic(fmt.Sprintf("product: loading built-ins: %v", err))
	}
	builtinList = list
	builtinByName = byName
}

func loadBuiltins() ([]store.Product, map[string]store.Product, error) {
	entries, err := fs.ReadDir(builtinFS, "builtins")
	if err != nil {
		return nil, nil, err
	}
	byName := make(map[string]store.Product, len(entries))
	list := make([]store.Product, 0, len(entries))
	for _, e := range entries {
		if e.Name()[0] == '.' {
			continue
		}
		data, readErr := builtinFS.ReadFile("builtins/" + e.Name())
		if readErr != nil {
			return nil, nil, readErr
		}
		p, parseErr := ParseDefinition(data)
		if parseErr != nil {
			return nil, nil, fmt.Errorf("builtins/%s: %w", e.Name(), parseErr)
		}
		p.Source = store.SourceBuiltin
		if _, dup := byName[p.Name]; dup {
			return nil, nil, fmt.Errorf("builtins: duplicate name %q", p.Name)
		}
		byName[p.Name] = p
		list = append(list, p)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	return list, byName, nil
}

// Builtins returns the embedded built-in products, sorted by name. The returned
// slice is a copy; callers may not mutate the shared definitions.
func Builtins() []store.Product {
	return slices.Clone(builtinList)
}
