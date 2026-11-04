// SPDX-License-Identifier: AGPL-3.0-or-later

package report

import (
	"encoding/csv"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/uberware/sqi/internal/loadtest/measure"
)

// writeSeriesCSV writes every series in long format, one row per sample:
// series, unit, time_rfc3339 (UTC), value. Series are ordered by name; a
// series' samples keep their order.
func writeSeriesCSV(w io.Writer, series []measure.Series) error {
	sorted := slices.Clone(series)
	slices.SortStableFunc(sorted, func(a, b measure.Series) int { return strings.Compare(a.Name, b.Name) })
	cw := csv.NewWriter(w)
	if err := cw.Write([]string{"series", "unit", "time_rfc3339", "value"}); err != nil {
		return err
	}
	for _, s := range sorted {
		for _, p := range s.Points {
			row := []string{s.Name, s.Unit, p.T.UTC().Format(time.RFC3339Nano), strconv.FormatFloat(p.V, 'f', -1, 64)}
			if err := cw.Write(row); err != nil {
				return err
			}
		}
	}
	cw.Flush()
	return cw.Error()
}
