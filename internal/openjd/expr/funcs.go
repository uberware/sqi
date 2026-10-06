// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

// functionShapes is the function registry: a name mapped to its accepted
// signatures, in the same Shape form ops.go uses for operators, so type-variable
// binding and cost-ranked overload selection are shared rather than
// reimplemented.
//
// Properties follow section 1.3.3's convention: the property p is the function
// __property_p__, registered under that name.
//
// The table is assembled from one map per RFC 0006 category rather than written
// as a single literal: the general, validation, math and list groups; the
// string library across four files (case and classification, trim and search,
// split and join, padding); regular expressions and the repr_* family; the path
// engine and its properties and functions; and the single session-dependent
// function apply_path_mapping (pathmapping.go). A new category adds a file and
// one argument here.
var functionShapes = mergeFuncs(
	convFuncs, mathFuncs, listFuncs,
	strCaseFuncs, strFindFuncs, strSplitFuncs, strPadFuncs,
	reFuncs, reprShellFuncs, reprDataFuncs,
	pathFuncs,
	pathMappingFuncs,
)

// mergeFuncs folds the per-category tables into one registry.
//
// It PANICS on a duplicate name rather than letting one table quietly win.
// Several tables write into a single namespace, and RFC 0006 reuses names across
// categories in ways that are easy to misread — "string" is a conversion while
// "str"-prefixed helpers are not, "list" is a conversion while the list
// FUNCTIONS are a separate group. A silent last-write-wins would delete a whole
// function's overload set and surface much later as an unexplained "no
// signature accepts" at a call site. This runs once at package initialization,
// so the panic is a build-time failure in practice.
func mergeFuncs(groups ...map[string][]Shape) map[string][]Shape {
	out := make(map[string][]Shape)
	for _, g := range groups {
		for name, shapes := range g {
			if _, dup := out[name]; dup {
				panic("expr: function " + name + " is registered by two categories")
			}
			out[name] = shapes
		}
	}
	return out
}
