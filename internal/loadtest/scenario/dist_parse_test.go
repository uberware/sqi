// SPDX-License-Identifier: AGPL-3.0-or-later

package scenario

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestDist_UnmarshalYAML(t *testing.T) {
	tests := []struct {
		name    string
		doc     string
		want    Dist
		wantErr string
	}{
		{"constant duration", "20s", Dist{Kind: DistConstant, Value: 20, Unit: "s"}, ""},
		{"constant minutes", "1m30s", Dist{Kind: DistConstant, Value: 90, Unit: "s"}, ""},
		{"constant count", "200", Dist{Kind: DistConstant, Value: 200}, ""},
		{"constant zero is a count", "0", Dist{Kind: DistConstant}, ""},
		{"constant map", "{dist: constant, value: 5s}", Dist{Kind: DistConstant, Value: 5, Unit: "s"}, ""},
		{"uniform durations", "{dist: uniform, min: 5s, max: 1m}", Dist{Kind: DistUniform, Min: 5, Max: 60, Unit: "s"}, ""},
		{"uniform counts", "{dist: uniform, min: 10, max: 50}", Dist{Kind: DistUniform, Min: 10, Max: 50}, ""},
		{"lognormal durations", "{dist: lognormal, median: 20s, p95: 90s}", Dist{Kind: DistLognormal, Median: 20, P95: 90, Unit: "s"}, ""},
		{"lognormal counts", "{dist: lognormal, median: 200, p95: 2000}", Dist{Kind: DistLognormal, Median: 200, P95: 2000}, ""},
		{"lognormal inferred", "{median: 200, p95: 2000}", Dist{Kind: DistLognormal, Median: 200, P95: 2000}, ""},
		{"uniform inferred", "{min: 1s, max: 3s}", Dist{Kind: DistUniform, Min: 1, Max: 3, Unit: "s"}, ""},
		{"unknown dist", "{dist: pareto, median: 1, p95: 2}", Dist{}, `unknown dist "pareto"`},
		{"cannot infer", "{median: 1}", Dist{}, "median and p95"},
		{"missing field", "{dist: lognormal, median: 1}", Dist{}, "p95"},
		{"foreign field", "{dist: uniform, min: 1, max: 2, median: 1}", Dist{}, `"median"`},
		{"mixed units", "{dist: uniform, min: 1s, max: 5}", Dist{}, "unit"},
		{"uniform inverted", "{dist: uniform, min: 9, max: 3}", Dist{}, "min"},
		{"lognormal p95 below median", "{dist: lognormal, median: 9, p95: 3}", Dist{}, "p95"},
		{"lognormal zero median", "{dist: lognormal, median: 0, p95: 3}", Dist{}, "median"},
		{"negative", "-5", Dist{}, "negative"},
		{"not a number", "soon", Dist{}, "soon"},
		{"sequence", "[1, 2]", Dist{}, "dist"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got Dist
			err := yaml.Unmarshal([]byte(tc.doc), &got)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("got %+v, want an error mentioning %q", got, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error %q does not mention %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unmarshal %q: %v", tc.doc, err)
			}
			if got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}
