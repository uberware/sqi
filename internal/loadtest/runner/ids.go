// SPDX-License-Identifier: AGPL-3.0-or-later

package runner

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"regexp"
	"time"
)

// maxRunIDLen bounds a run ID, which names the run's directory and farm and
// prefixes every simulated worker's ID.
const maxRunIDLen = 64

// runIDPattern is what a run ID may contain. A run ID ends up in a directory
// name (so no colon or separator, for Windows), in worker IDs that are NATS
// subject tokens (so no dot, space, '*' or '>'), and in a PostgreSQL
// database name.
var runIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

// NewRunID returns a run ID for a run starting at now: the UTC time as
// yyyymmdd-hhmmss and four random hex digits, like 20261104-170507-3fa2.
func NewRunID(now time.Time) string {
	var b [2]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail on supported platforms; the time alone
		// still names the run.
		return now.UTC().Format("20060102-150405") + "-0000"
	}
	return now.UTC().Format("20060102-150405") + "-" + hex.EncodeToString(b[:])
}

// ValidRunID reports why id cannot name a run, or nil when it can: it must
// be 1 to 64 letters, digits, dashes and underscores, starting with a letter
// or digit.
func ValidRunID(id string) error {
	switch {
	case id == "":
		return errors.New("the run ID is empty")
	case len(id) > maxRunIDLen:
		return fmt.Errorf("run ID %q is longer than %d characters", id, maxRunIDLen)
	case !runIDPattern.MatchString(id):
		return fmt.Errorf("run ID %q may hold only letters, digits, '-' and '_', starting with a letter or digit", id)
	}
	return nil
}

// SeedFor derives a run's default seed from its ID, so a run is reproducible
// from its ID alone. It is never zero.
func SeedFor(runID string) int64 {
	h := fnv.New64a()
	h.Write([]byte(runID))
	s := int64(h.Sum64() & (1<<63 - 1))
	if s == 0 {
		return 1
	}
	return s
}
