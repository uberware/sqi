// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import "testing"

// TestParseJobParamType covers every spelling RFC 0007 declares equivalent,
// plus the spellings it does NOT license. The RFC grants case-insensitivity
// only: internal whitespace ("list [int]") and deeper nesting
// ("list[list[list[int]]]") stay unknown types, because accepting more than
// the spec authorizes is an acceptance change.
func TestParseJobParamType(t *testing.T) {
	tests := []struct {
		raw  string
		want JobParamType
		ok   bool
	}{
		{"INT", JobParamTypeInt, true},
		{"int", JobParamTypeInt, true},
		{"Int", JobParamTypeInt, true},
		{"FLOAT", JobParamTypeFloat, true},
		{"Float", JobParamTypeFloat, true},
		{"STRING", JobParamTypeString, true},
		{"PATH", JobParamTypePath, true},
		{"pAtH", JobParamTypePath, true},
		{"BOOL", JobParamTypeBool, true},
		{"Bool", JobParamTypeBool, true},
		{"bool", JobParamTypeBool, true},
		{"RANGE_EXPR", JobParamTypeRangeExpr, true},
		{"range_expr", JobParamTypeRangeExpr, true},
		{"Range_Expr", JobParamTypeRangeExpr, true},
		{"LIST[STRING]", JobParamTypeListString, true},
		{"list[string]", JobParamTypeListString, true},
		{"List[String]", JobParamTypeListString, true},
		{"list[PATH]", JobParamTypeListPath, true},
		{"List[Int]", JobParamTypeListInt, true},
		{"LIST[float]", JobParamTypeListFloat, true},
		{"List[Bool]", JobParamTypeListBool, true},
		{"list[list[int]]", JobParamTypeListListInt, true},
		{"LIST[LIST[INT]]", JobParamTypeListListInt, true},

		{"", "", false},
		{"list [int]", "", false},
		{"LIST[ INT ]", "", false},
		{"list[list[list[int]]]", "", false},
		{"LIST[RANGE_EXPR]", "", false},
		{"MAP[STRING]", "", false},
		{"CHUNK[INT]", "", false}, // a TASK type; not legal on a job parameter
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			got, ok := parseJobParamType(tt.raw)
			if ok != tt.ok {
				t.Fatalf("parseJobParamType(%q) ok = %v, want %v", tt.raw, ok, tt.ok)
			}
			if got != tt.want {
				t.Errorf("parseJobParamType(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

// TestParseTaskParamType pins that task types are case-insensitive too (RFC
// 0007 covers job AND task type names) and that sqi's own CHUNK[INT] is
// included, while the job-only list types are not: RFC 0007 adds no task
// parameter types.
func TestParseTaskParamType(t *testing.T) {
	tests := []struct {
		raw  string
		want TaskParamType
		ok   bool
	}{
		{"INT", TaskParamTypeInt, true},
		{"int", TaskParamTypeInt, true},
		{"Float", TaskParamTypeFloat, true},
		{"string", TaskParamTypeString, true},
		{"PaTh", TaskParamTypePath, true},
		{"CHUNK[INT]", TaskParamTypeChunkInt, true},
		{"chunk[int]", TaskParamTypeChunkInt, true},
		{"Chunk[Int]", TaskParamTypeChunkInt, true},

		{"", "", false},
		{"BOOL", "", false},
		{"LIST[INT]", "", false},
		{"chunk [int]", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			got, ok := parseTaskParamType(tt.raw)
			if ok != tt.ok {
				t.Fatalf("parseTaskParamType(%q) ok = %v, want %v", tt.raw, ok, tt.ok)
			}
			if got != tt.want {
				t.Errorf("parseTaskParamType(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}
