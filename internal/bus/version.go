// SPDX-License-Identifier: AGPL-3.0-or-later

package bus

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// MinBrokerVersion is the oldest nats-server release sqi will connect to. It
// is the lowest entry of [seriesMinPatch], named in errors for brokers that
// predate every supported series.
const MinBrokerVersion = "2.14.8"

// seriesMinPatch maps each 2.x minor series upstream still patches to the
// first patch release of that series that fixes the auth-callout
// "auth_users" bypass, which sqi's authentication depends on (2.14.8 and
// 2.15.1). Upstream normally backports security fixes to the two most recent
// series (https://github.com/nats-io/nats-general/blob/main/SECURITY.md).
// Minors below the lowest key are unsupported; minors above the highest are
// accepted.
var seriesMinPatch = map[int]int{14: 8, 15: 1}

// CheckBrokerVersion reports an error when v, a nats-server version as the
// broker advertises it (for example "2.15.2" or "2.15.3-RC.1"), is malformed
// or older than the minimum for its series. Versions are compared numerically
// as major.minor.patch; a pre-release suffix is ignored.
func CheckBrokerVersion(v string) error {
	got, err := parseBrokerVersion(v)
	if err != nil {
		return fmt.Errorf("bus: broker version %q is not recognized: %w", v, err)
	}
	major, minor, patch := got[0], got[1], got[2]
	if major > 2 {
		return nil
	}
	lowest, highest := lowestAndHighestSeries()
	if major < 2 || minor < lowest {
		return tooOld(v, MinBrokerVersion)
	}
	if minor > highest {
		return nil
	}
	if want := seriesMinPatch[minor]; patch < want {
		return tooOld(v, fmt.Sprintf("2.%d.%d", minor, want))
	}
	return nil
}

func tooOld(found, minimum string) error {
	return fmt.Errorf(
		"bus: nats-server %s is older than the minimum %s for its release series; "+
			"upgrade the broker to a release still receiving security fixes", found, minimum,
	)
}

func lowestAndHighestSeries() (lowest, highest int) {
	lowest = -1
	for minor := range seriesMinPatch {
		if lowest < 0 || minor < lowest {
			lowest = minor
		}
		if minor > highest {
			highest = minor
		}
	}
	return lowest, highest
}

func parseBrokerVersion(v string) ([3]int, error) {
	var out [3]int
	core := strings.TrimPrefix(v, "v")
	if i := strings.IndexAny(core, "-+"); i >= 0 {
		core = core[:i]
	}
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return out, errors.New("want major.minor.patch")
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, fmt.Errorf("component %q is not a non-negative integer", p)
		}
		out[i] = n
	}
	return out, nil
}
