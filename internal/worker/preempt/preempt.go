// SPDX-License-Identifier: AGPL-3.0-or-later

// Package preempt watches a cloud provider's instance metadata service for the
// notice that a spot or preemptible machine is about to be reclaimed, so the
// worker running on it can hand its tasks back at once instead of being killed
// mid-task.
//
// The watcher is opt-in (worker.preemption.watch) and contacts nothing
// otherwise. It supports the three notices providers publish:
//
//   - aws: the IMDSv2 spot instance-action document.
//   - gcp: the instance/preempted flag.
//   - azure: a Preempt event in the Scheduled Events document.
//
// A failed poll never stops the watcher: a metadata service that is briefly
// unreachable must not leave a machine that is then reclaimed unwatched.
package preempt

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// Vendor names, as written in worker.preemption.watch.
const (
	VendorAWS   = "aws"
	VendorGCP   = "gcp"
	VendorAzure = "azure"
)

const (
	awsAzureBase = "http://169.254.169.254"
	gcpBase      = "http://metadata.google.internal"

	// requestTimeout bounds each metadata request. The services are link-local
	// and answer in milliseconds; a request that takes longer is a failure.
	requestTimeout = 2 * time.Second

	// warnAfter is the number of consecutive failed polls after which the
	// watcher logs a warning, and warnEvery the number of further failures
	// between repeats. At the default 5 s interval that is a first warning
	// after about 50 s and one every 10 minutes after.
	warnAfter = 10
	warnEvery = 120

	// maxBody bounds how much of a metadata response is read.
	maxBody = 1 << 20

	// awsTokenTTL is the lifetime requested for the IMDSv2 session token. A
	// fresh token is fetched on every poll, so it only has to outlive one.
	awsTokenTTL = "60"
)

// Watcher polls one vendor's metadata service for a preemption notice.
type Watcher struct {
	vendor   string
	base     string
	interval time.Duration
	client   *http.Client
	logger   *slog.Logger
	check    func(ctx context.Context) (bool, error)
}

// New builds a Watcher for vendor (aws, gcp or azure), polling every interval.
// endpoint, when not empty, replaces the vendor's default metadata base URL
// (for a proxy, or a test). client may be nil for the default client.
func New(vendor, endpoint string, interval time.Duration, client *http.Client, logger *slog.Logger) (*Watcher, error) {
	if interval <= 0 {
		return nil, fmt.Errorf("preempt: poll interval must be positive, got %s", interval)
	}
	if client == nil {
		client = &http.Client{}
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	w := &Watcher{vendor: vendor, interval: interval, client: client, logger: logger}

	switch vendor {
	case VendorAWS:
		w.base, w.check = awsAzureBase, w.checkAWS
	case VendorGCP:
		w.base, w.check = gcpBase, w.checkGCP
	case VendorAzure:
		w.base, w.check = awsAzureBase, w.checkAzure
	default:
		return nil, fmt.Errorf("preempt: unknown preemption vendor %q (want aws, gcp or azure)", vendor)
	}
	if endpoint != "" {
		w.base = strings.TrimRight(endpoint, "/")
	}
	return w, nil
}

// Base returns the metadata base URL the watcher polls.
func (w *Watcher) Base() string { return w.base }

// Run polls until the provider announces a preemption, calls onNotice exactly
// once, and returns. It also returns, without calling onNotice, when ctx ends.
// The first poll is immediate.
func (w *Watcher) Run(ctx context.Context, onNotice func()) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	failures := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}

		notice, err := w.check(ctx)
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			failures++
			w.logFailure(ctx, failures, err)
		case notice:
			w.logger.WarnContext(ctx, "preempt: preemption notice received",
				slog.String("vendor", w.vendor))
			onNotice()
			return
		default:
			failures = 0
		}
		timer.Reset(w.interval)
	}
}

// logFailure records a failed poll: always at debug, and as a warning once
// failures have run on for warnAfter polls, then again every warnEvery.
func (w *Watcher) logFailure(ctx context.Context, failures int, err error) {
	attrs := []any{
		slog.String("vendor", w.vendor),
		slog.Int("consecutive_failures", failures),
		slog.Any("error", err),
	}
	if failures == warnAfter || (failures > warnAfter && (failures-warnAfter)%warnEvery == 0) {
		w.logger.WarnContext(ctx, "preempt: metadata service has not answered; a preemption notice would be missed", attrs...)
		return
	}
	w.logger.DebugContext(ctx, "preempt: poll failed", attrs...)
}

// do sends one request with the per-request timeout and returns the status and
// the (bounded) body.
func (w *Watcher) do(ctx context.Context, method, path string, headers map[string]string) (status int, body []byte, err error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, w.base+path, http.NoBody)
	if err != nil {
		return 0, nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := w.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err = io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, body, nil
}

// checkAWS reads the IMDSv2 spot instance-action document: 200 is a notice,
// 404 means none is pending.
func (w *Watcher) checkAWS(ctx context.Context) (bool, error) {
	status, body, err := w.do(ctx, http.MethodPut, "/latest/api/token",
		map[string]string{"X-aws-ec2-metadata-token-ttl-seconds": awsTokenTTL})
	if err != nil {
		return false, fmt.Errorf("aws token: %w", err)
	}
	if status != http.StatusOK {
		return false, fmt.Errorf("aws token: unexpected status %d", status)
	}

	status, _, err = w.do(ctx, http.MethodGet, "/latest/meta-data/spot/instance-action",
		map[string]string{"X-aws-ec2-metadata-token": strings.TrimSpace(string(body))})
	if err != nil {
		return false, fmt.Errorf("aws instance-action: %w", err)
	}
	switch status {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("aws instance-action: unexpected status %d", status)
	}
}

// checkGCP reads the instance/preempted flag: a body of TRUE is a notice.
func (w *Watcher) checkGCP(ctx context.Context) (bool, error) {
	status, body, err := w.do(ctx, http.MethodGet, "/computeMetadata/v1/instance/preempted",
		map[string]string{"Metadata-Flavor": "Google"})
	if err != nil {
		return false, fmt.Errorf("gcp preempted: %w", err)
	}
	if status != http.StatusOK {
		return false, fmt.Errorf("gcp preempted: unexpected status %d", status)
	}
	return strings.EqualFold(strings.TrimSpace(string(body)), "TRUE"), nil
}

// checkAzure reads the Scheduled Events document: any event of type Preempt is
// a notice. Other events (Reboot, Redeploy, Freeze, Terminate) are platform
// maintenance, not a spot eviction.
func (w *Watcher) checkAzure(ctx context.Context) (bool, error) {
	status, body, err := w.do(ctx, http.MethodGet, "/metadata/scheduledevents?api-version=2020-07-01",
		map[string]string{"Metadata": "true"})
	if err != nil {
		return false, fmt.Errorf("azure scheduledevents: %w", err)
	}
	if status != http.StatusOK {
		return false, fmt.Errorf("azure scheduledevents: unexpected status %d", status)
	}
	var doc struct {
		Events []struct {
			EventType string `json:"EventType"`
		} `json:"Events"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return false, fmt.Errorf("azure scheduledevents: %w", err)
	}
	for _, ev := range doc.Events {
		if ev.EventType == "Preempt" {
			return true, nil
		}
	}
	return false, nil
}
