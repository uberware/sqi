// SPDX-License-Identifier: AGPL-3.0-or-later

package measure

import "time"

// Slope fits an ordinary least squares line of V against hours since the
// earliest included point, over the points with T >= from, and returns the
// slope in units per hour and the coefficient of determination r2 in [0, 1].
//
// It returns (0, 0) when fewer than two points are included or every included
// point shares one timestamp. When every included V is equal the slope is 0
// and r2 is defined as 0: there is no variance for the line to explain, so a
// flat series must not read as a strong trend.
func Slope(points []Point, from time.Time) (perHour, r2 float64) {
	var first time.Time
	n := 0
	for _, p := range points {
		if p.T.Before(from) {
			continue
		}
		if n == 0 || p.T.Before(first) {
			first = p.T
		}
		n++
	}
	if n < 2 {
		return 0, 0
	}
	var sumX, sumY float64
	for _, p := range points {
		if p.T.Before(from) {
			continue
		}
		sumX += p.T.Sub(first).Hours()
		sumY += p.V
	}
	meanX, meanY := sumX/float64(n), sumY/float64(n)
	var sxx, sxy, syy float64
	for _, p := range points {
		if p.T.Before(from) {
			continue
		}
		dx, dy := p.T.Sub(first).Hours()-meanX, p.V-meanY
		sxx += dx * dx
		sxy += dx * dy
		syy += dy * dy
	}
	if sxx == 0 {
		return 0, 0
	}
	perHour = sxy / sxx
	if syy == 0 {
		return perHour, 0
	}
	r2 = sxy * sxy / (sxx * syy)
	return perHour, min(max(r2, 0), 1)
}
