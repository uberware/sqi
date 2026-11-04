// SPDX-License-Identifier: AGPL-3.0-or-later

package observe

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/uberware/sqi/internal/loadtest/client"
)

// maxProfileBytes bounds one profile the observer will read.
const maxProfileBytes = 256 << 20

// cpuProfileSlack is how long past the profile's own duration a CPU profile
// request may take before it is abandoned.
const cpuProfileSlack = 30 * time.Second

// errPprofUnavailable marks a server that does not serve /debug/pprof to us:
// it is not enabled (404) or not permitted (401/403).
var errPprofUnavailable = errors.New("pprof is not available")

// profiler fetches pprof profiles from the server into a directory.
type profiler struct {
	c       *client.Client
	dir     string
	timeout time.Duration
}

// fetch saves GET /debug/pprof/<path> as dir/<name>. A 404, 401 or 403 is
// errPprofUnavailable.
func (p profiler) fetch(ctx context.Context, path, name string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	body, err := p.c.GetRaw(ctx, "/debug/pprof/"+path, maxProfileBytes)
	var apiErr *client.APIError
	if errors.As(err, &apiErr) && (apiErr.Status == http.StatusNotFound ||
		apiErr.Status == http.StatusForbidden || apiErr.Status == http.StatusUnauthorized) {
		return "", fmt.Errorf("%w: %w", errPprofUnavailable, err)
	}
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(p.dir, 0o750); err != nil {
		return "", err
	}
	dest := filepath.Join(p.dir, name)
	if err := os.WriteFile(dest, body, 0o600); err != nil {
		return "", err
	}
	return dest, nil
}

// stamp renders t as a file-name-safe UTC time.
func stamp(t time.Time) string { return t.UTC().Format("20060102T150405Z") }

// snapshot fetches the heap and goroutine profiles, tagging their names with
// phase. It stops at the first error.
func (p profiler) snapshot(ctx context.Context, t time.Time, phase string) error {
	for _, kind := range []string{"heap", "goroutine"} {
		name := fmt.Sprintf("%s-%s-%s.pb.gz", kind, stamp(t), phase)
		if _, err := p.fetch(ctx, kind, name, p.timeout); err != nil {
			return err
		}
	}
	return nil
}

// cpu fetches a CPU profile of d.
func (p profiler) cpu(ctx context.Context, t time.Time, d time.Duration) error {
	secs := max(int(d.Round(time.Second)/time.Second), 1)
	name := fmt.Sprintf("cpu-%s.pb.gz", stamp(t))
	_, err := p.fetch(ctx, "profile?seconds="+strconv.Itoa(secs), name, d+cpuProfileSlack)
	return err
}
