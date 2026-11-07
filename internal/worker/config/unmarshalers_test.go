// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"reflect"
	"slices"
	"testing"

	"github.com/uberware/sqi/internal/worker/capabilities"
	"github.com/uberware/sqi/internal/yamlstrict"
)

// TestWorkerConfig_CustomUnmarshalersReviewed pins the custom UnmarshalYAML
// types reachable from the worker's config file. yaml.v3's node.Decode does not
// inherit the file decoder's strictness, so one that decodes a mapping must use
// yamlstrict.DecodeNode or every key below it is silently accepted. A new one
// fails this test until it has been checked and listed here.
func TestWorkerConfig_CustomUnmarshalersReviewed(t *testing.T) {
	reviewed := []reflect.Type{
		reflect.TypeFor[capabilities.EnvCheck](), // mapping via yamlstrict.DecodeNode
	}
	found := yamlstrict.Unmarshalers(reflect.TypeFor[WorkerConfig]())
	for _, typ := range found {
		if !slices.Contains(reviewed, typ) {
			t.Errorf("%v implements yaml.Unmarshaler and is not reviewed: decode a mapping with yamlstrict.DecodeNode, then list it here", typ)
		}
	}
	for _, typ := range reviewed {
		if !slices.Contains(found, typ) {
			t.Errorf("%v is listed but not reachable from the config; remove it from the list", typ)
		}
	}
}
